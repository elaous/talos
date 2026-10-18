package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

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
