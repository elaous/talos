// Package server serves the JSON API and the WASM UI.
package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/elaous/gojiberry/wasmserve"
	"github.com/elaous/talos/agent"
	"github.com/elaous/talos/llm"
	"github.com/elaous/talos/rag"
)

const maxUpload = 64 << 20 // pdftext refuses larger PDFs anyway

type Server struct {
	Team   *agent.Team
	Lib    *rag.Library
	PDFDir string
	UI     fs.FS
	// Host names (without port) allowed besides loopback. Empty allows any.
	AllowedHosts []string
	// Cancels background indexing; nil never cancels.
	Ctx     context.Context
	wg      sync.WaitGroup // background ingests
	up      chan struct{}  // limits concurrent buffered uploads
	writeMu sync.Mutex     // one upload writes into the library at a time

	mu       sync.Mutex
	jobs     map[string]*job
	indexing bool
	rerun    bool // files arrived while indexing: run again
	closed   bool // Wait was called: StartIngest refuses
	lastErr  string
	lastAdd  int
}

const (
	maxRunning   = 4   // concurrent agent runs
	maxJobs      = 100 // jobs kept (running or finished)
	maxHistory   = 10  // history messages sent along
	maxMsgLen    = 16 << 10
	maxHistChars = 24 << 10 // total characters of history sent along
	maxUploads   = 2
)

var uploadTimeout = 2 * time.Minute

// Wait blocks until background indexing stops.
func (s *Server) Wait() {
	s.mu.Lock()
	s.closed = true // wg.Add must not race with Wait
	s.mu.Unlock()
	s.wg.Wait()
}

func (s *Server) ctx() context.Context {
	if s.Ctx != nil {
		return s.Ctx
	}
	return context.Background()
}

type job struct {
	mu     sync.Mutex
	events []agent.Event
	done   bool
	answer string
	err    string
	cancel context.CancelFunc
	start  time.Time
}

func (s *Server) Handler() http.Handler {
	s.jobs = map[string]*job{}
	s.up = make(chan struct{}, maxUploads)
	removeStaleBackups(s.PDFDir)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ask", s.ask)
	mux.HandleFunc("GET /api/jobs/{id}", s.poll)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /api/library", s.library)
	mux.HandleFunc("POST /api/ingest", s.ingest)
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.Handle("/", wasmserve.Handler(s.UI, wasmserve.Options{Fallback: "index.html"}))
	return s.guard(mux)
}

// Origin alone is not enough; also checks Host.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostOK(r.Host) {
			http.Error(w, "unexpected Host header", http.StatusForbidden)
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "cross-origin request refused", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) hostOK(hostport string) bool {
	if len(s.AllowedHosts) == 0 {
		return true
	}
	h := hostport
	if sh, _, err := net.SplitHostPort(hostport); err == nil {
		h = sh
	}
	h = strings.TrimSuffix(strings.Trim(strings.ToLower(h), "[]"), ".") // trailing dot: same name
	switch h {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	for _, a := range s.AllowedHosts {
		if strings.EqualFold(a, h) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		http.Error(w, "want application/json", http.StatusUnsupportedMediaType)
		return
	}
	var in struct {
		Question string        `json:"question"`
		History  []llm.Message `json:"history"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || strings.TrimSpace(in.Question) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(in.Question) > maxMsgLen {
		http.Error(w, "question too long", http.StatusRequestEntityTooLarge)
		return
	}
	if len(in.History) > maxHistory {
		in.History = in.History[len(in.History)-maxHistory:]
	}
	for i := range in.History { // client history may not carry system roles
		if in.History[i].Role != "assistant" {
			in.History[i].Role = "user"
		}
		if len(in.History[i].Content) > maxMsgLen {
			in.History[i].Content = in.History[i].Content[:maxMsgLen]
		}
	}
	for n := 0; ; { // drop oldest messages until history fits budget
		n = 0
		for _, m := range in.History {
			n += len(m.Content)
		}
		if n <= maxHistChars || len(in.History) == 0 {
			break
		}
		in.History = in.History[1:]
	}
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	j := &job{cancel: cancel, start: time.Now()}
	s.mu.Lock()
	s.gc()
	if s.running() >= maxRunning {
		s.mu.Unlock()
		cancel()
		http.Error(w, "too many questions running; wait for one to finish", http.StatusTooManyRequests)
		return
	}
	s.jobs[id] = j
	s.mu.Unlock()
	go func() {
		defer cancel()
		ans, err := s.Team.Ask(ctx, in.Question, in.History, func(e agent.Event) {
			j.mu.Lock()
			j.events = append(j.events, e)
			j.mu.Unlock()
		})
		j.mu.Lock()
		j.done, j.answer = true, ans
		if err != nil {
			j.err = err.Error()
		}
		j.mu.Unlock()
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"id": id})
}

// gc drops old finished jobs. Caller holds s.mu.
func (s *Server) gc() {
	type fin struct {
		id string
		at time.Time
	}
	var done []fin
	for id, j := range s.jobs {
		j.mu.Lock()
		d := j.done
		j.mu.Unlock()
		if !d {
			continue
		}
		if time.Since(j.start) > time.Hour {
			delete(s.jobs, id)
		} else {
			done = append(done, fin{id, j.start})
		}
	}
	sort.Slice(done, func(i, k int) bool { return done[i].at.Before(done[k].at) })
	for len(s.jobs) >= maxJobs && len(done) > 0 {
		delete(s.jobs, done[0].id)
		done = done[1:]
	}
}

// running counts unfinished jobs. Caller holds s.mu.
func (s *Server) running() (n int) {
	for _, j := range s.jobs {
		j.mu.Lock()
		if !j.done {
			n++
		}
		j.mu.Unlock()
	}
	return
}

func (s *Server) job(r *http.Request) *job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[r.PathValue("id")]
}

func (s *Server) poll(w http.ResponseWriter, r *http.Request) {
	j := s.job(r)
	if j == nil {
		http.NotFound(w, r)
		return
	}
	since := 0
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			http.Error(w, "bad since", http.StatusBadRequest)
			return
		}
		since = n
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	ev := []agent.Event{}
	if since < len(j.events) {
		ev = j.events[since:]
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": ev, "next": len(j.events), "done": j.done, "answer": j.answer, "error": j.err})
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	if j := s.job(r); j != nil {
		j.cancel()
	}
	w.WriteHeader(http.StatusNoContent)
}

type libStatus struct {
	Sources  []libSource `json:"sources"`
	Indexing bool        `json:"indexing"`
	Added    int         `json:"added"`
	Error    string      `json:"error"`
}
type libSource struct {
	Name     string `json:"name"`
	Passages int    `json:"passages"`
}

func (s *Server) library(w http.ResponseWriter, _ *http.Request) {
	st := libStatus{Sources: []libSource{}}
	for n, c := range s.Lib.Sources() {
		st.Sources = append(st.Sources, libSource{n, c})
	}
	sort.Slice(st.Sources, func(i, j int) bool { return st.Sources[i].Name < st.Sources[j].Name })
	s.mu.Lock()
	st.Indexing, st.Added, st.Error = s.indexing, s.lastAdd, s.lastErr
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, st)
}

// Indexes PDFs in background; a busy run repeats once more.
func (s *Server) StartIngest() bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	if s.indexing {
		s.rerun = true
		s.mu.Unlock()
		return false
	}
	s.indexing, s.lastErr, s.lastAdd = true, "", 0
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		for {
			n, err := s.Lib.IngestDir(s.ctx(), s.PDFDir)
			s.mu.Lock()
			s.lastAdd += n
			s.lastErr = ""
			if err != nil {
				s.lastErr = err.Error()
			}
			if s.rerun && s.ctx().Err() == nil {
				s.rerun = false
				s.mu.Unlock()
				continue
			}
			s.indexing, s.rerun = false, false
			s.mu.Unlock()
			return
		}
	}()
	return true
}

func (s *Server) ingest(w http.ResponseWriter, _ *http.Request) {
	s.StartIngest()
	w.WriteHeader(http.StatusAccepted)
}

// Saves all uploaded PDFs or none; existing names are replaced.
func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.up <- struct{}{}:
		defer func() { <-s.up }()
	default:
		http.Error(w, "too many uploads in progress", http.StatusTooManyRequests)
		return
	}
	if err := probeDir(s.PDFDir); err != nil { // e.g. the folder is mounted read-only
		http.Error(w, "uploads are disabled: the PDF folder is not writable", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	// Stalled clients must not hold an upload slot.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(uploadTimeout))
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	files := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		if p.FileName() == "" {
			continue
		}
		name := filepath.Base(strings.ReplaceAll(p.FileName(), "\\", "/"))
		if !strings.EqualFold(filepath.Ext(name), ".pdf") || name == ".pdf" || len(name) > 200 || strings.ContainsFunc(name, unicode.IsControl) {
			http.Error(w, "not an acceptable .pdf file name", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(p)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		if !bytes.HasPrefix(data, []byte("%PDF-")) {
			http.Error(w, "not a PDF: "+name, http.StatusBadRequest)
			return
		}
		files[name] = data
	}
	if len(files) == 0 {
		http.Error(w, "no PDF in the upload", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(s.PDFDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.writeMu.Lock()
	err = writeAll(s.PDFDir, files)
	s.writeMu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.StartIngest()
	w.WriteHeader(http.StatusNoContent)
}

// Replaced in tests: root can write anywhere.
var probeTemp = os.CreateTemp

// probeDir reports whether dir accepts new files.
func probeDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := probeTemp(dir, ".probe-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

// Replaced in tests to inject rename failures.
var rename = os.Rename

// Replaced in tests to inject backup failures.
var (
	link      = os.Link
	writeFile = os.WriteFile
)

const backupSuffix = ".replaced~"

// Returns a free kept-copy name; never overwrites an earlier one.
func uniqueKeptName(to string) string {
	name := to + keptSuffix
	for i := 2; ; i++ {
		if _, err := os.Lstat(name); err != nil {
			return name
		}
		name = fmt.Sprintf("%s%s%d", to, keptSuffix, i)
	}
}

// Kept copies are never swept at startup.
const keptSuffix = ".recovered~"

// Saves all files or none, via atomic renames and backups.
// Startup sweep must skip *.recovered~ files.
func writeAll(dir string, files map[string][]byte) error {
	type tmp struct{ from, to, backup string }
	var done []tmp
	cleanup := func() {
		for _, t := range done {
			os.Remove(t.from)
		}
	}
	for name := range files {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err == nil && !fi.Mode().IsRegular() {
			return fmt.Errorf("%s exists and is not a file", name)
		}
	}
	for name, data := range files {
		f, err := os.CreateTemp(dir, ".upload-*")
		if err != nil {
			cleanup()
			return err
		}
		done = append(done, tmp{from: f.Name(), to: filepath.Join(dir, name)})
		_, werr := f.Write(data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			cleanup()
			return werr
		}
	}
	var placed []tmp // destinations already replaced or created
	undo := func() {
		kept := map[string]bool{} // backups whose restore failed; kept, not removed
		for i := len(placed) - 1; i >= 0; i-- {
			if t := placed[i]; t.backup != "" {
				if err := os.Rename(t.backup, t.to); err != nil { // atomically puts the old file back
					// Keep the backup under a name the startup sweep skips.
					if os.Rename(t.backup, uniqueKeptName(t.to)) == nil {
						continue
					}
					kept[t.backup] = true
				}
			} else {
				os.Remove(t.to)
			}
		}
		for _, t := range done {
			if t.backup != "" && !kept[t.backup] {
				os.Remove(t.backup)
			}
		}
		cleanup()
	}
	for i := range done {
		t := &done[i]
		if _, err := os.Lstat(t.to); err == nil {
			t.backup = t.to + backupSuffix
			os.Remove(t.backup)
			if err := link(t.to, t.backup); err != nil { // no hard links here: copy
				old, rerr := os.ReadFile(t.to)
				if rerr == nil {
					rerr = writeFile(t.backup, old, 0o644)
				}
				if rerr != nil {
					os.Remove(t.backup) // partial copy must not look like a backup
					t.backup = ""
					undo()
					return rerr
				}
			}
		}
		if err := rename(t.from, t.to); err != nil {
			if t.backup != "" {
				os.Remove(t.backup)
				t.backup = ""
			}
			undo()
			return err
		}
		placed = append(placed, *t)
	}
	for _, t := range placed {
		if t.backup != "" {
			os.Remove(t.backup)
		}
	}
	return nil
}

// Removes backups and temp files left by a crashed upload.
func removeStaleBackups(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && (strings.HasSuffix(n, backupSuffix) || strings.HasPrefix(n, ".upload-")) {
			os.Remove(filepath.Join(dir, n))
		}
	}
}
