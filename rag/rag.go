// Package rag indexes PDFs and searches chunks by cosine similarity.
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
	mu        sync.RWMutex // guards chunks and files; never held across a network call
	ingest    sync.Mutex   // serializes IngestDir runs
	path      string
	emb       llm.Embedder
	size      int
	overlap   int
	key       string // embedding model and chunk settings; a mismatch invalidates the index
	chunks    []Chunk
	files     map[string]string  // file name -> content hash
	bad       map[string]badFile // failed PDFs; unchanged ones are not retried
	saveMu    sync.Mutex         // one index write at a time
	saveEvery time.Duration      // how often a long ingest saves its progress (0: 15s)
}

// badFile is a failed PDF's content hash and error.
type badFile struct{ Hash, Err string }

// diskIndex is the saved form; Library fields stay unexported.
type diskIndex struct {
	Key    string
	Chunks []Chunk
	Files  map[string]string
	Bad    map[string]badFile
}

// Changed embModel or chunk settings start empty.
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

func (l *Library) saveInterval() time.Duration {
	if l.saveEvery > 0 {
		return l.saveEvery
	}
	return 15 * time.Second
}

// Caller holds l.mu; chunk slices are shared, never mutated.
func (l *Library) snapshot() diskIndex {
	d := diskIndex{Key: l.key, Chunks: l.chunks[:len(l.chunks):len(l.chunks)],
		Files: make(map[string]string, len(l.files)), Bad: make(map[string]badFile, len(l.bad))}
	for k, v := range l.files {
		d.Files[k] = v
	}
	for k, v := range l.bad {
		d.Bad[k] = v
	}
	return d
}

// save writes atomically and takes no lock; pass a snapshot.
func (l *Library) save(d diskIndex) error {
	if l.path == "" { // in-memory library: nowhere to save
		return nil
	}
	l.saveMu.Lock()
	defer l.saveMu.Unlock()
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(l.path)+".tmp*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // a no-op once renamed
	if err := gob.NewEncoder(f).Encode(d); err != nil {
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

// Split cuts text into overlapping windows, preferring spaces.
func Split(text string, size, overlap int) []string {
	text = strings.Join(strings.Fields(text), " ")
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 || overlap > size/2 { // larger overlap explodes the chunk count
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

// errEmbed marks embedding server failures; retried next run.
var errEmbed = errors.New("embedding failed")

// IngestDir indexes changed PDFs under dir and drops removed ones.
// Lock only when swapping files, never across embedding.
func (l *Library) IngestDir(ctx context.Context, dir string) (int, error) {
	l.ingest.Lock()
	defer l.ingest.Unlock()
	l.mu.Lock()
	if l.files == nil { // zero-value Library: initialize maps
		l.files = map[string]string{}
	}
	if l.bad == nil {
		l.bad = map[string]badFile{}
	}
	l.mu.Unlock()
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
		if b, ok := knownBad[name]; ok && b.Hash == h { // failed before, unchanged: do not retry
			errs = append(errs, fmt.Errorf("%s: %s", name, b.Err))
			return nil
		}
		fresh, err := l.embedPDF(ctx, name, data)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			if !errors.Is(err, errEmbed) && !errors.Is(err, pdftext.ErrTimeout) { // file is at fault: remember it
				l.mu.Lock()
				l.bad[name] = badFile{h, err.Error()}
				l.mu.Unlock()
			}
			return nil
		}
		l.mu.Lock()
		l.drop(name)
		l.chunks = append(l.chunks, fresh...)
		l.files[name] = h
		delete(l.bad, name)
		var snap *diskIndex
		if time.Since(lastSave) > l.saveInterval() { // keep progress of long runs
			d := l.snapshot()
			snap, lastSave = &d, time.Now()
		}
		l.mu.Unlock()
		if snap != nil {
			if err := l.save(*snap); err != nil {
				errs = append(errs, err)
			}
		}
		added += len(fresh)
		return nil
	})
	if walkErr != nil {
		errs = append(errs, walkErr)
	}
	l.mu.Lock()
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
	snap := l.snapshot()
	l.mu.Unlock()
	if err := l.save(snap); err != nil {
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

// Replaced in tests to simulate extraction failures.
var extractPages = pdftext.ExtractPagesBytes

func (l *Library) embedPDF(ctx context.Context, name string, data []byte) ([]Chunk, error) {
	pages, err := extractPages(data)
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("no text found (a scanned PDF needs OCR first)")
	}
	const batch = 32
	for i := 0; i < len(texts); i += batch {
		vecs, err := l.emb.Embed(ctx, texts[i:min(i+batch, len(texts))])
		if err != nil {
			return nil, fmt.Errorf("%w: %w", errEmbed, err)
		}
		for j, v := range vecs {
			fresh[i+j].Vec = normalize(v)
		}
	}
	return fresh, nil
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

// Search returns the k nearest chunks, optionally from one source.
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

func (l *Library) Sources() map[string]int {
	l.mu.RLock()
	defer l.mu.RUnlock()
	m := map[string]int{}
	for _, c := range l.chunks {
		m[c.Source]++
	}
	return m
}
