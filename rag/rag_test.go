package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elaous/gojiberry/docfile"
	"github.com/elaous/gojiberry/export"
)

// fakeEmb maps text to a bag-of-keywords vector, so nearest-neighbour search is checkable without a model.
type fakeEmb struct{ calls int }

var vocab = []string{"revenue", "dolphin", "kernel"}

func (f *fakeEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, len(vocab))
		for j, w := range vocab {
			v[j] = float32(strings.Count(strings.ToLower(t), w))
		}
		out[i] = v
	}
	return out, nil
}

func writePDF(t *testing.T, path, text string) {
	t.Helper()
	b, err := export.PDF{}.Export(export.FromDocument(docfile.Document{Content: []docfile.Item{{Chars: &docfile.Chars{Text: text}}}}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSplit(t *testing.T) {
	text := strings.Repeat("alpha beta gamma delta ", 100)
	parts := Split(text, 200, 40)
	if len(parts) < 10 {
		t.Fatalf("only %d parts", len(parts))
	}
	for _, p := range parts {
		if len(p) > 200 || strings.HasPrefix(p, " ") {
			t.Fatalf("bad part %q", p)
		}
	}
	if got := Split("héllo wörld ünïcode "+strings.Repeat("é", 300), 50, 10); len(got) == 0 {
		t.Fatal("no parts")
	}
	if len(Split("   ", 100, 10)) != 0 {
		t.Fatal("blank text made chunks")
	}
}

func TestIngestSearchPersistAndSkipUnchanged(t *testing.T) {
	dir := t.TempDir()
	writePDF(t, filepath.Join(dir, "finance.pdf"), "Annual revenue grew strongly. Revenue per customer rose.")
	writePDF(t, filepath.Join(dir, "nature.pdf"), "The dolphin is a marine mammal. A dolphin pod hunts together.")
	idx := filepath.Join(t.TempDir(), "index.gob")
	emb := &fakeEmb{}

	l, err := Open(idx, emb, "fake", 400, 50)
	if err != nil {
		t.Fatal(err)
	}
	n, err := l.IngestDir(context.Background(), dir)
	if err != nil || n < 2 {
		t.Fatalf("ingest %d %v", n, err)
	}
	hits, err := l.Search(context.Background(), "dolphin facts", 3, "")
	if err != nil || len(hits) == 0 || hits[0].Source != "nature.pdf" || hits[0].Page != 1 {
		t.Fatalf("hits %+v %v", hits, err)
	}
	if only, _ := l.Search(context.Background(), "dolphin", 3, "finance.pdf"); len(only) == 0 || only[0].Source != "finance.pdf" {
		t.Fatalf("source filter: %+v", only)
	}

	calls := emb.calls
	l2, err := Open(idx, emb, "fake", 400, 50) // reload from disk
	if err != nil {
		t.Fatal(err)
	}
	if again, err := l2.IngestDir(context.Background(), dir); err != nil || again != 0 || emb.calls != calls {
		t.Fatalf("unchanged PDFs were re-embedded: added %d, calls %d->%d, %v", again, calls, emb.calls, err)
	}
	if hits, _ := l2.Search(context.Background(), "revenue", 1, ""); len(hits) != 1 || hits[0].Source != "finance.pdf" {
		t.Fatalf("after reload: %+v", hits)
	}

	os.Remove(filepath.Join(dir, "nature.pdf"))
	if _, err := l2.IngestDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if got := l2.Sources(); len(got) != 1 || got["finance.pdf"] == 0 {
		t.Fatalf("removed PDF still indexed: %v", got)
	}
	if l3, _ := Open(idx, emb, "other-model", 400, 50); len(l3.Chunks) != 0 {
		t.Fatal("vectors from another embedding model were reused")
	}
}
