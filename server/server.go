// Package server is talos's HTTP face: a small JSON API and the WASM UI.
//
// There is no login, so it listens on loopback by default. Every state-changing request must come from the
// same origin, which stops a web page you happen to visit from driving the agents through localhost.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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

	mu       sync.Mutex
	jobs     map[string]*job
	indexing bool
	lastErr  string
	lastAdd  int
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
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/ask", s.ask)
	mux.HandleFunc("GET /api/jobs/{id}", s.poll)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancel)
	mux.HandleFunc("GET /api/library", s.library)
	mux.HandleFunc("POST /api/ingest", s.ingest)
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.Handle("/", wasmserve.Handler(s.UI, wasmserve.Options{Fallback: "index.html"}))
	return sameOrigin(mux)
}

func sameOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	for i := range in.History { // the client supplies history, so it may not smuggle in system prompts
		if in.History[i].Role != "assistant" {
			in.History[i].Role = "user"
		}
	}
	var idb [8]byte
	_, _ = rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	j := &job{cancel: cancel, start: time.Now()}
	s.mu.Lock()
	s.gc()
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

// gc forgets finished jobs older than an hour. Caller holds s.mu.
func (s *Server) gc() {
	for id, j := range s.jobs {
		j.mu.Lock()
		old := j.done && time.Since(j.start) > time.Hour
		j.mu.Unlock()
		if old {
			delete(s.jobs, id)
		}
	}
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
		for _, c := range v {
			if c < '0' || c > '9' {
				since = 0
				break
			}
			since = since*10 + int(c-'0')
		}
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

// StartIngest indexes the PDF folder in the background; false means a run is already going.
func (s *Server) StartIngest() bool {
	s.mu.Lock()
	if s.indexing {
		s.mu.Unlock()
		return false
	}
	s.indexing, s.lastErr = true, ""
	s.mu.Unlock()
	go func() {
		n, err := s.Lib.IngestDir(context.Background(), s.PDFDir)
		s.mu.Lock()
		s.indexing, s.lastAdd = false, n
		if err != nil {
			s.lastErr = err.Error()
		}
		s.mu.Unlock()
	}()
	return true
}

func (s *Server) ingest(w http.ResponseWriter, _ *http.Request) {
	s.StartIngest()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad upload", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(s.PDFDir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "bad upload", http.StatusBadRequest)
			return
		}
		name := filepath.Base(p.FileName())
		if p.FileName() == "" || !strings.EqualFold(filepath.Ext(name), ".pdf") || name == "." || name == string(filepath.Separator) {
			continue
		}
		data, err := io.ReadAll(p)
		if err != nil || !strings.HasPrefix(string(data), "%PDF-") {
			http.Error(w, "not a PDF: "+name, http.StatusBadRequest)
			return
		}
		if err := os.WriteFile(filepath.Join(s.PDFDir, name), data, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	s.StartIngest()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
