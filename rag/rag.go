// Package rag is the PDF library: extraction, chunking, embeddings and nearest-neighbour search.
// The index is a gob file and search is a linear cosine scan, which is instant for the tens of thousands of
// chunks a personal library holds and needs no database.
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/elaous/gojiberry/pdftext"
	"github.com/elaous/talos/llm"
)

type Chunk struct {
	Source string
	Page   int
	Text   string
	Vec    []float32 // unit length
}

type Hit struct {
	Chunk
	Score float32
}

type Library struct {
	mu      sync.RWMutex // guards chunks and files; never held across a network call
	ingest  sync.Mutex   // serializes IngestDir runs
	path    string
	emb     llm.Embedder
	size    int
	overlap int
	key     string // embedding model + chunk settings: an index built under another key is not reused
	chunks  []Chunk
	files   map[string]string  // file name -> content hash, so unchanged PDFs are skipped
	bad     map[string]badFile // PDFs that cannot be read (scanned, corrupt): not retried until they change
}

// badFile remembers why a PDF was skipped, for the content hash it had then.
type badFile struct{ Hash, Msg string }

// diskIndex is what is saved: Library's own fields are unexported so callers cannot mutate them unlocked.
type diskIndex struct {
	Key    string
	Chunks []Chunk
	Files  map[string]string
	Bad    map[string]badFile
}

// Open loads the index at path, or starts an empty one. embModel names the embedding model: vectors from
// another model, or chunks cut with other settings, are not comparable, so a mismatch starts a fresh index.
func Open(path string, emb llm.Embedder, embModel string, chunkChars, overlap int) (*Library, error) {
	l := &Library{path: path, emb: emb, size: chunkChars, overlap: overlap, files: map[string]string{}, bad: map[string]badFile{},
		key: fmt.Sprintf("%s|%d|%d", embModel, chunkChars, overlap)}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var saved diskIndex
	if err := gob.NewDecoder(f).Decode(&saved); err != nil {
		return nil, fmt.Errorf("rag: %s is unreadable (delete it to re-index): %w", path, err)
	}
	if saved.Key != l.key {
		return l, nil
	}
	l.chunks = saved.Chunks
	if saved.Files != nil {
		l.files = saved.Files
	}
	if saved.Bad != nil {
		l.bad = saved.Bad
	}
	return l, nil
}

// save writes the index atomically (temp file in the same directory, fsync, rename). Caller holds l.mu.
func (l *Library) save() error {
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(l.path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // a no-op once renamed
	if err := gob.NewEncoder(f).Encode(diskIndex{l.key, l.chunks, l.files, l.bad}); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), l.path)
}

// Split cuts text into overlapping windows of about size bytes, breaking at a space where it can.
func Split(text string, size, overlap int) []string {
	text = strings.Join(strings.Fields(text), " ")
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 || overlap > size/2 { // a larger overlap makes the number of chunks explode
		overlap = size / 6
	}
	var out []string
	for start := 0; start < len(text); {
		end := min(start+size, len(text))
		if end < len(text) {
			if i := strings.LastIndexByte(text[start:end], ' '); i > size/2 {
				end = start + i
			}
			for end > start && !utf8.RuneStart(text[end]) { // never cut inside a UTF-8 sequence
				end--
			}
			for end == start || (end < len(text) && !utf8.RuneStart(text[end])) { // a rune wider than size: take it whole
				end++
			}
		}
		if s := strings.TrimSpace(text[start:end]); s != "" {
			out = append(out, s)
		}
		if end == len(text) {
			break
		}
		start = max(end-overlap, start+1)
		for start < len(text) && text[start]&0xC0 == 0x80 { // do not start inside a UTF-8 sequence
			start++
		}
	}
	return out
}

// IngestDir indexes every PDF under dir that is new or changed and drops those that are gone. It returns how
// many chunks it added. A file that cannot be indexed (scanned, encrypted, corrupt) is reported in the
// returned error, which joins one error per file, but never stops the others. Searches keep working while it
// runs: the lock is taken only to swap in a finished file, never across the (slow) embedding calls.
func (l *Library) IngestDir(ctx context.Context, dir string) (int, error) {
	l.ingest.Lock()
	defer l.ingest.Unlock()
	l.mu.RLock()
	known := make(map[string]string, len(l.files))
	for k, v := range l.files {
		known[k] = v
	}
	knownBad := make(map[string]badFile, len(l.bad))
	for k, v := range l.bad {
		knownBad[k] = v
	}
	l.mu.RUnlock()

	seen := map[string]bool{}
	var errs []error
	added := 0
	lastSave := time.Now()
	walkErr := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".pdf") {
			return nil
		}
		name, _ := filepath.Rel(dir, p)
		seen[name] = true
		data, err := os.ReadFile(p)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			return nil
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		if known[name] == h {
			return nil
		}
		if b, ok := knownBad[name]; ok && b.Hash == h { // failed before and has not changed: report, do not redo
			errs = append(errs, fmt.Errorf("%s: %s", name, b.Msg))
			return nil
		}
		fresh, permanent, err := l.embedPDF(ctx, name, data)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			if permanent { // an unreachable embedding server is not a verdict on the file, so only this is kept
				l.mu.Lock()
				l.bad[name] = badFile{h, err.Error()}
				l.mu.Unlock()
			}
			return nil
		}
		l.mu.Lock()
		delete(l.bad, name)
		l.drop(name)
		l.chunks = append(l.chunks, fresh...)
		l.files[name] = h
		if time.Since(lastSave) > 15*time.Second { // a long run keeps its progress if the process is stopped
			if err := l.save(); err != nil {
				errs = append(errs, err)
			}
			lastSave = time.Now()
		}
		l.mu.Unlock()
		added += len(fresh)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if walkErr == nil { // only a complete walk shows which files are gone
		for name := range l.files {
			if !seen[name] {
				l.drop(name)
				delete(l.files, name)
			}
		}
		for name := range l.bad {
			if !seen[name] {
				delete(l.bad, name)
			}
		}
	}
	if err := l.save(); err != nil {
		errs = append(errs, err)
	}
	return added, errors.Join(errs...)
}

// drop removes a file's chunks. Caller holds l.mu.
func (l *Library) drop(name string) {
	kept := make([]Chunk, 0, len(l.chunks))
	for _, c := range l.chunks {
		if c.Source != name {
			kept = append(kept, c)
		}
	}
	l.chunks = kept
}

// embedPDF returns the file's chunks. permanent says a failure is about the file itself (it will fail the
// same way next time), as opposed to the embedding server being down.
func (l *Library) embedPDF(ctx context.Context, name string, data []byte) (chunks []Chunk, permanent bool, err error) {
	pages, err := pdftext.ExtractPagesBytes(data)
	if err != nil {
		return nil, true, err
	}
	var fresh []Chunk
	var texts []string
	for i, pg := range pages {
		for _, s := range Split(pg, l.size, l.overlap) {
			fresh = append(fresh, Chunk{Source: name, Page: i + 1, Text: s})
			texts = append(texts, s)
		}
	}
	if len(fresh) == 0 {
		return nil, true, fmt.Errorf("no text found (a scanned PDF needs OCR first)")
	}
	const batch = 32
	for i := 0; i < len(texts); i += batch {
		vecs, err := l.emb.Embed(ctx, texts[i:min(i+batch, len(texts))])
		if err != nil {
			return nil, false, err
		}
		for j, v := range vecs {
			fresh[i+j].Vec = normalize(v)
		}
	}
	return fresh, false, nil
}

func normalize(v []float32) []float32 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	if s == 0 {
		return v
	}
	n := float32(1 / math.Sqrt(s))
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = x * n
	}
	return out
}

// Search returns the k chunks nearest the query, optionally limited to one source file.
func (l *Library) Search(ctx context.Context, query string, k int, source string) ([]Hit, error) {
	vecs, err := l.emb.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	q := normalize(vecs[0])
	l.mu.RLock()
	defer l.mu.RUnlock()
	var hits []Hit
	for _, c := range l.chunks {
		if source != "" && c.Source != source || len(c.Vec) != len(q) {
			continue
		}
		var dot float32
		for i, x := range c.Vec {
			dot += x * q[i]
		}
		hits = append(hits, Hit{c, dot})
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if k <= 0 {
		k = 5
	}
	if len(hits) > k {
		hits = hits[:k]
	}
	return hits, nil
}

// Sources lists the indexed files with their chunk counts.
func (l *Library) Sources() map[string]int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m := map[string]int{}
	for _, c := range l.chunks {
		m[c.Source]++
	}
	return m
}
