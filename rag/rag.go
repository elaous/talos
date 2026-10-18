// Package rag is the PDF library: extraction, chunking, embeddings and nearest-neighbour search.
// The index is a gob file and search is a linear cosine scan, which is instant for the tens of thousands of
// chunks a personal library holds and needs no database.
package rag

import (
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

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
	mu       sync.RWMutex
	path     string
	emb      llm.Embedder
	size     int
	overlap  int
	Chunks   []Chunk
	Files    map[string]string // file name -> content hash, so unchanged PDFs are skipped
	EmbModel string            // vectors from a different embedding model are not comparable
}

// Open loads the index at path, or starts an empty one.
func Open(path string, emb llm.Embedder, embModel string, chunkChars, overlap int) (*Library, error) {
	l := &Library{path: path, emb: emb, size: chunkChars, overlap: overlap, Files: map[string]string{}, EmbModel: embModel}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var saved Library
	if err := gob.NewDecoder(f).Decode(&saved); err != nil {
		return nil, fmt.Errorf("rag: %s is unreadable (delete it to re-index): %w", path, err)
	}
	if saved.EmbModel != embModel {
		return l, nil // different embeddings: start over rather than mix spaces
	}
	l.Chunks, l.Files = saved.Chunks, saved.Files
	if l.Files == nil {
		l.Files = map[string]string{}
	}
	return l, nil
}

func (l *Library) save() error {
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(l); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// Split cuts text into overlapping windows of about size bytes, breaking at a space where it can.
func Split(text string, size, overlap int) []string {
	text = strings.Join(strings.Fields(text), " ")
	if size <= 0 {
		size = 1200
	}
	if overlap < 0 || overlap >= size {
		overlap = size / 6
	}
	var out []string
	for start := 0; start < len(text); {
		end := min(start+size, len(text))
		if end < len(text) {
			if i := strings.LastIndexByte(text[start:end], ' '); i > size/2 {
				end = start + i
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

// IngestDir indexes every PDF under dir that is new or changed and drops those that are gone.
// It returns how many chunks it added.
func (l *Library) IngestDir(ctx context.Context, dir string) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := map[string]bool{}
	added := 0
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".pdf") {
			return err
		}
		name, _ := filepath.Rel(dir, p)
		seen[name] = true
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		h := hex.EncodeToString(sum[:])
		if l.Files[name] == h {
			return nil
		}
		n, err := l.addPDF(ctx, name, data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		l.Files[name] = h
		added += n
		return nil
	})
	if err != nil {
		_ = l.save() // keep what was indexed before the failure
		return added, err
	}
	for name := range l.Files {
		if !seen[name] {
			l.drop(name)
			delete(l.Files, name)
		}
	}
	return added, l.save()
}

func (l *Library) drop(name string) {
	kept := l.Chunks[:0]
	for _, c := range l.Chunks {
		if c.Source != name {
			kept = append(kept, c)
		}
	}
	l.Chunks = kept
}

func (l *Library) addPDF(ctx context.Context, name string, data []byte) (int, error) {
	pages, err := pdftext.ExtractPagesBytes(data)
	if err != nil {
		return 0, err
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
		return 0, fmt.Errorf("no text found (a scanned PDF needs OCR first)")
	}
	const batch = 32
	for i := 0; i < len(texts); i += batch {
		vecs, err := l.emb.Embed(ctx, texts[i:min(i+batch, len(texts))])
		if err != nil {
			return 0, err
		}
		for j, v := range vecs {
			fresh[i+j].Vec = normalize(v)
		}
	}
	l.drop(name)
	l.Chunks = append(l.Chunks, fresh...)
	return len(fresh), nil
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
	for _, c := range l.Chunks {
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
	for _, c := range l.Chunks {
		m[c.Source]++
	}
	return m
}
