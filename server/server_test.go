package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/elaous/gojiberry/docfile"
	"github.com/elaous/gojiberry/export"
	"github.com/elaous/talos/agent"
	"github.com/elaous/talos/config"
	"github.com/elaous/talos/llm"
	"github.com/elaous/talos/rag"
	"github.com/elaous/talos/web"
)

type final string

func (f final) Chat(context.Context, string, []llm.Message) (string, error) {
	return `{"final":"` + string(f) + `"}`, nil
}

func newServer(t *testing.T) http.Handler {
	var cfg config.Config
	s := &Server{
		Team: agent.NewTeam(cfg, final("pong"), &rag.Library{}, web.New("", false)),
		Lib:  &rag.Library{}, PDFDir: t.TempDir(),
		UI: fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}},
	}
	return s.Handler()
}

func do(h http.Handler, method, path, ctype, body string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAskThenPoll(t *testing.T) {
	h := newServer(t)
	w := do(h, "POST", "/api/ask", "application/json", `{"question":"ping"}`)
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var id struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &id)
	deadline := time.Now().Add(3 * time.Second)
	for {
		var st struct {
			Done   bool
			Answer string
			Events []agent.Event
		}
		json.Unmarshal(do(h, "GET", "/api/jobs/"+id.ID, "", "").Body.Bytes(), &st)
		if st.Done {
			if st.Answer != "pong" || len(st.Events) == 0 {
				t.Fatalf("%+v", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("job never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRefusals(t *testing.T) {
	h := newServer(t)
	if w := do(h, "POST", "/api/ask", "application/json", `{"question":"x"}`, "Origin", "https://evil.example"); w.Code != 403 {
		t.Errorf("cross-origin POST: %d", w.Code)
	}
	if w := do(h, "POST", "/api/ask", "text/plain", `{"question":"x"}`); w.Code != 415 {
		t.Errorf("form-style POST: %d", w.Code)
	}
	if w := do(h, "POST", "/api/ask", "application/json", `{"question":"  "}`); w.Code != 400 {
		t.Errorf("empty question: %d", w.Code)
	}
	if w := do(h, "GET", "/api/jobs/nope", "", ""); w.Code != 404 {
		t.Errorf("unknown job: %d", w.Code)
	}
	if w := do(h, "GET", "/", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), "ui") {
		t.Errorf("ui: %d", w.Code)
	}
}

func hostServer(t *testing.T, hosts ...string) (http.Handler, *Server) {
	var cfg config.Config
	s := &Server{
		Team: agent.NewTeam(cfg, final("pong"), &rag.Library{}, web.New("", false)),
		Lib:  &rag.Library{}, PDFDir: t.TempDir(), AllowedHosts: hosts,
		UI: fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}},
	}
	return s.Handler(), s
}

func req(h http.Handler, method, path, host string, hdr map[string]string, ctype, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = host
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestDNSRebindingAndCrossSiteAreRefused(t *testing.T) {
	h, _ := hostServer(t, "127.0.0.1", "localhost")
	ask := `{"question":"x"}`
	cases := []struct {
		name, method, host string
		hdr                map[string]string
		want               int
	}{
		{"own host", "POST", "127.0.0.1:8080", nil, 202},
		{"own host, same origin", "POST", "localhost:8080", map[string]string{"Origin": "http://localhost:8080"}, 202},
		{"rebound host, matching origin", "POST", "evil.test", map[string]string{"Origin": "http://evil.test"}, 403},
		{"rebound host, GET", "GET", "evil.test", nil, 403},
		{"rebound host, library GET", "GET", "evil.test:8080", nil, 403},
		{"cross-origin", "POST", "127.0.0.1:8080", map[string]string{"Origin": "http://evil.test"}, 403},
		{"null origin", "POST", "127.0.0.1:8080", map[string]string{"Origin": "null"}, 403},
		{"fetch metadata cross-site", "POST", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"fetch metadata cross-site GET", "GET", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "cross-site"}, 403},
		{"fetch metadata same-origin", "POST", "127.0.0.1:8080", map[string]string{"Sec-Fetch-Site": "same-origin"}, 202},
		{"no Origin (curl)", "POST", "[::1]:8080", nil, 202},
		{"trailing dot is the same name", "GET", "localhost.:8080", nil, 200},
		{"upper case", "GET", "LOCALHOST:8080", nil, 200},
	}
	for _, c := range cases {
		path, body, ct := "/api/ask", ask, "application/json"
		if c.method == "GET" {
			path, body, ct = "/api/library", "", ""
		}
		if w := req(h, c.method, path, c.host, c.hdr, ct, body); w.Code != c.want {
			t.Errorf("%s: %d, want %d", c.name, w.Code, c.want)
		}
	}
}

func TestPollRejectsBadSince(t *testing.T) {
	h, _ := hostServer(t)
	var id struct{ ID string }
	json.Unmarshal(req(h, "POST", "/api/ask", "x", nil, "application/json", `{"question":"q"}`).Body.Bytes(), &id)
	for _, since := range []string{"9223372036854775808", "-1", "abc", "1e3"} {
		if w := req(h, "GET", "/api/jobs/"+id.ID+"?since="+since, "x", nil, "", ""); w.Code != 400 {
			t.Errorf("since=%s: %d", since, w.Code)
		}
	}
	if w := req(h, "GET", "/api/jobs/"+id.ID+"?since=999999", "x", nil, "", ""); w.Code != 200 {
		t.Errorf("since past the end: %d", w.Code)
	}
}

type blocker struct{ release chan struct{} }

func (b blocker) Chat(ctx context.Context, _ string, _ []llm.Message) (string, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
	}
	return `{"final":"x"}`, nil
}

func TestTooManyConcurrentQuestionsAreRefused(t *testing.T) {
	var cfg config.Config
	b := blocker{make(chan struct{})}
	defer close(b.release)
	s := &Server{Team: agent.NewTeam(cfg, b, &rag.Library{}, web.New("", false)), Lib: &rag.Library{},
		PDFDir: t.TempDir(), UI: fstest.MapFS{"index.html": {Data: []byte("x")}}}
	h := s.Handler()
	for i := 0; i < maxRunning; i++ {
		if w := req(h, "POST", "/api/ask", "x", nil, "application/json", `{"question":"q"}`); w.Code != 202 {
			t.Fatalf("question %d: %d", i, w.Code)
		}
	}
	if w := req(h, "POST", "/api/ask", "x", nil, "application/json", `{"question":"q"}`); w.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", w.Code)
	}
}

func TestHistoryAndQuestionAreBounded(t *testing.T) {
	h, _ := hostServer(t)
	huge := strings.Repeat("a", maxMsgLen+1)
	if w := req(h, "POST", "/api/ask", "x", nil, "application/json", `{"question":"`+huge+`"}`); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("huge question: %d", w.Code)
	}
}

func multipartBody(parts [][2]string) (string, string) {
	var sb strings.Builder
	for _, p := range parts {
		sb.WriteString("--B\r\nContent-Disposition: form-data; name=\"pdf\"; filename=\"" + p[0] + "\"\r\n\r\n" + p[1] + "\r\n")
	}
	sb.WriteString("--B--\r\n")
	return "multipart/form-data; boundary=B", sb.String()
}

func TestUpload(t *testing.T) {
	h, s := hostServer(t)
	post := func(parts [][2]string) *httptest.ResponseRecorder {
		ct, body := multipartBody(parts)
		return req(h, "POST", "/api/upload", "x", nil, ct, body)
	}
	read := func(name string) string { b, _ := os.ReadFile(filepath.Join(s.PDFDir, name)); return string(b) }

	if w := post([][2]string{{"../../etc/evil.pdf", "%PDF-1.4 a"}}); w.Code != 204 {
		t.Fatalf("traversal name is cleaned and saved inside the folder: %d", w.Code)
	}
	if read("evil.pdf") == "" {
		t.Fatal("not saved under its base name")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.PDFDir), "evil.pdf")); err == nil {
		t.Fatal("escaped the PDF folder")
	}
	if w := post([][2]string{{`..\..\win.pdf`, "%PDF-1.4 b"}}); w.Code != 204 || read("win.pdf") == "" {
		t.Fatalf("backslash traversal: %d", w.Code)
	}

	// a bad second part saves nothing, not even the good first part
	if w := post([][2]string{{"good.pdf", "%PDF-1.4 c"}, {"fake.pdf", "<html>not a pdf"}}); w.Code != 400 {
		t.Fatalf("non-PDF part: %d", w.Code)
	}
	if read("good.pdf") != "" {
		t.Fatal("a rejected upload was partly saved")
	}
	if w := post([][2]string{{"notes.txt", "%PDF-1.4"}}); w.Code != 400 {
		t.Fatalf("wrong extension: %d", w.Code)
	}
	if w := post(nil); w.Code != 400 {
		t.Fatalf("empty upload: %d", w.Code)
	}

	// a same-named upload replaces the file (updating a document), atomically and with no temp files left
	post([][2]string{{"doc.pdf", "%PDF-1.4 one"}})
	post([][2]string{{"doc.pdf", "%PDF-1.4 two"}})
	if read("doc.pdf") != "%PDF-1.4 two" {
		t.Fatalf("not replaced: %q", read("doc.pdf"))
	}
	left, _ := filepath.Glob(filepath.Join(s.PDFDir, ".upload-*"))
	if len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestIngestRequestedWhileRunningRunsAgain(t *testing.T) {
	var cfg config.Config
	emb := &countEmb{block: make(chan struct{}), entered: make(chan struct{})}
	dir := t.TempDir()
	lib, _ := rag.Open(filepath.Join(t.TempDir(), "i.gob"), emb, "e", 400, 50)
	s := &Server{Team: agent.NewTeam(cfg, final("x"), lib, web.New("", false)), Lib: lib, PDFDir: dir,
		UI: fstest.MapFS{"index.html": {Data: []byte("x")}}}
	s.Handler()
	writeTestPDF(t, filepath.Join(dir, "a.pdf"), "alpha text")
	if !s.StartIngest() {
		t.Fatal("first run refused")
	}
	<-emb.entered // inside the first run's embedding call
	writeTestPDF(t, filepath.Join(dir, "b.pdf"), "beta text")
	if s.StartIngest() {
		t.Fatal("a second run started while one was going")
	}
	close(emb.block)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(lib.Sources()) == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the file saved during the run was never indexed: %v", lib.Sources())
}

type countEmb struct {
	block   chan struct{}
	entered chan struct{}
	once    sync.Once
}

func (c *countEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	c.once.Do(func() {
		if c.entered != nil {
			close(c.entered)
		}
	})
	<-c.block
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1}
	}
	return out, nil
}

func writeTestPDF(t *testing.T, path, text string) {
	t.Helper()
	b, err := export.PDF{}.Export(export.FromDocument(docfile.Document{Content: []docfile.Item{{Chars: &docfile.Chars{Text: text}}}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUploadIsAllOrNothing(t *testing.T) {
	h, s := hostServer(t)
	post := func(parts [][2]string) *httptest.ResponseRecorder {
		ct, body := multipartBody(parts)
		return req(h, "POST", "/api/upload", "x", nil, ct, body)
	}
	exists := func(n string) bool { _, err := os.Stat(filepath.Join(s.PDFDir, n)); return err == nil }
	if w := post([][2]string{{"ok.pdf", "%PDF-1.4 a"}, {strings.Repeat("n", 300) + ".pdf", "%PDF-1.4 b"}}); w.Code != 400 || exists("ok.pdf") {
		t.Fatalf("over-long name: %d, ok.pdf saved=%v", w.Code, exists("ok.pdf"))
	}
	if w := post([][2]string{{"ok.pdf", "%PDF-1.4 a"}, {"bad\x01name.pdf", "%PDF-1.4 b"}}); w.Code != 400 || exists("ok.pdf") {
		t.Fatalf("control character in name: %d", w.Code)
	}
	// a name that is already a directory cannot be written: nothing of the upload may remain
	os.Mkdir(filepath.Join(s.PDFDir, "dir.pdf"), 0o755)
	if w := post([][2]string{{"ok.pdf", "%PDF-1.4 a"}, {"dir.pdf", "%PDF-1.4 b"}}); w.Code != 500 {
		t.Fatalf("unwritable name: %d", w.Code)
	}
	for i := 0; i < 20; i++ { // map order is random, so repeat: ok.pdf must never survive
		post([][2]string{{"ok.pdf", "%PDF-1.4 a"}, {"dir.pdf", "%PDF-1.4 b"}})
	}
	if exists("ok.pdf") {
		t.Fatal("part of a failed upload was kept")
	}
	if left, _ := filepath.Glob(filepath.Join(s.PDFDir, ".upload-*")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

func TestOversizeUploadIs413(t *testing.T) {
	h, _ := hostServer(t)
	ct, body := multipartBody([][2]string{{"big.pdf", "%PDF-1.4 " + strings.Repeat("x", maxUpload)}})
	if w := req(h, "POST", "/api/upload", "x", nil, ct, body); w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d", w.Code)
	}
}

// recorder keeps what the model was sent.
type recorder struct {
	mu  sync.Mutex
	got [][]llm.Message
}

func (r *recorder) Chat(_ context.Context, _ string, m []llm.Message) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, append([]llm.Message(nil), m...))
	return `{"final":"ok"}`, nil
}

func (r *recorder) first() []llm.Message {
	for i := 0; i < 500; i++ {
		r.mu.Lock()
		if len(r.got) > 0 {
			defer r.mu.Unlock()
			return r.got[0]
		}
		r.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func TestHistoryIsCappedByCharacters(t *testing.T) {
	var cfg config.Config
	rec := &recorder{}
	s := &Server{Team: agent.NewTeam(cfg, rec, &rag.Library{}, web.New("", false)), Lib: &rag.Library{}, PDFDir: t.TempDir(),
		UI: fstest.MapFS{"index.html": {Data: []byte("x")}}}
	h := s.Handler()
	var hist []llm.Message
	for i := 0; i < 10; i++ {
		hist = append(hist, llm.Message{Role: "user", Content: strings.Repeat("u", maxMsgLen)})
	}
	b, _ := json.Marshal(map[string]any{"question": "q", "history": hist})
	if w := req(h, "POST", "/api/ask", "x", nil, "application/json", string(b)); w.Code != 202 {
		t.Fatalf("%d", w.Code)
	}
	sent := rec.first()
	if sent == nil {
		t.Fatal("no chat call")
	}
	n := 0
	for _, m := range sent {
		n += len(m.Content)
	}
	if n > maxHistChars+8<<10 { // history budget plus the system prompt and question
		t.Fatalf("%d characters sent", n)
	}
}

// gateEmb embeds the first file at once and blocks on the next until the context ends.
type gateEmb struct{ calls int }

func (g *gateEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	g.calls++
	if g.calls > 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1}
	}
	return out, nil
}

func TestShutdownKeepsWhatIndexingFinished(t *testing.T) {
	var cfg config.Config
	dir, idx := t.TempDir(), filepath.Join(t.TempDir(), "i.gob")
	writeTestPDF(t, filepath.Join(dir, "a.pdf"), "alpha text")
	writeTestPDF(t, filepath.Join(dir, "b.pdf"), "beta text")
	lib, _ := rag.Open(idx, &gateEmb{}, "e", 400, 50)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{Team: agent.NewTeam(cfg, final("x"), lib, web.New("", false)), Lib: lib, PDFDir: dir, Ctx: ctx,
		UI: fstest.MapFS{"index.html": {Data: []byte("x")}}}
	s.Handler()
	s.StartIngest()
	deadline := time.Now().Add(5 * time.Second)
	for len(lib.Sources()) == 0 && time.Now().Before(deadline) { // a.pdf done, b.pdf blocked in Embed
		time.Sleep(10 * time.Millisecond)
	}
	cancel() // SIGTERM
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the context ended")
	}
	again, err := rag.Open(idx, &gateEmb{}, "e", 400, 50)
	if err != nil || again.Sources()["a.pdf"] == 0 {
		t.Fatalf("progress was lost on shutdown: %v %v", again.Sources(), err)
	}
}

func TestStaleIndexErrorIsClearedByTheNextRun(t *testing.T) {
	var cfg config.Config
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "bad.pdf"), []byte("%PDF-1.4 broken"), 0o644)
	lib, _ := rag.Open(filepath.Join(t.TempDir(), "i.gob"), &countEmb{block: func() chan struct{} { c := make(chan struct{}); close(c); return c }()}, "e", 400, 50)
	s := &Server{Team: agent.NewTeam(cfg, final("x"), lib, web.New("", false)), Lib: lib, PDFDir: dir,
		UI: fstest.MapFS{"index.html": {Data: []byte("x")}}}
	h := s.Handler()
	status := func() (st struct {
		Indexing bool
		Error    string
	}) {
		json.Unmarshal(req(h, "GET", "/api/library", "x", nil, "", "").Body.Bytes(), &st)
		return
	}
	wait := func() {
		for i := 0; i < 500 && status().Indexing; i++ {
			time.Sleep(10 * time.Millisecond)
		}
	}
	s.StartIngest()
	wait()
	if !strings.Contains(status().Error, "bad.pdf") {
		t.Fatalf("error not shown: %q", status().Error)
	}
	os.Remove(filepath.Join(dir, "bad.pdf"))
	s.StartIngest()
	wait()
	if e := status().Error; e != "" {
		t.Fatalf("stale error still shown: %q", e)
	}
}
