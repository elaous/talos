package rag

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/elaous/gojiberry/docfile"
	"github.com/elaous/gojiberry/export"
)

// fakeEmb maps text to a bag-of-keywords vector, so nearest-neighbour search is checkable without a model.
type fakeEmb struct{ calls atomic.Int32 }

var vocab = []string{"revenue", "dolphin", "kernel"}

func (f *fakeEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls.Add(1)
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

	calls := emb.calls.Load()
	l2, err := Open(idx, emb, "fake", 400, 50) // reload from disk
	if err != nil {
		t.Fatal(err)
	}
	if again, err := l2.IngestDir(context.Background(), dir); err != nil || again != 0 || emb.calls.Load() != calls {
		t.Fatalf("unchanged PDFs were re-embedded: added %d, calls %d->%d, %v", again, calls, emb.calls.Load(), err)
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
	if l3, _ := Open(idx, emb, "other-model", 400, 50); len(l3.chunks) != 0 {
		t.Fatal("vectors from another embedding model were reused")
	}
}

func TestSplitIsAlwaysValidUTF8AndTerminates(t *testing.T) {
	cjk := strings.Repeat("漢字かな交じり文", 400) // no spaces: nowhere to break but inside a rune
	for _, tc := range []struct {
		text          string
		size, overlap int
	}{
		{cjk, 100, 20}, {cjk, 7, 3}, {cjk, 1, 0}, {"é" + strings.Repeat("ü", 500), 33, 16}, {cjk, 100, 99}, {cjk, 100, -5},
	} {
		parts := Split(tc.text, tc.size, tc.overlap)
		if len(parts) == 0 {
			t.Fatalf("%d/%d: no parts", tc.size, tc.overlap)
		}
		if n := len(tc.text) / max(tc.size/2, 1); len(parts) > n+2 {
			t.Fatalf("%d/%d: %d parts for %d bytes: chunks multiplied", tc.size, tc.overlap, len(parts), len(tc.text))
		}
		for _, p := range parts {
			if !utf8.ValidString(p) || p == "" {
				t.Fatalf("%d/%d: bad part %q", tc.size, tc.overlap, p)
			}
		}
	}
}

func TestOneBadPDFDoesNotBlockTheOthers(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a-corrupt.pdf"), []byte("%PDF-1.4 not really"), 0o644)
	writePDF(t, filepath.Join(dir, "b-good.pdf"), "Revenue grew.")
	l, _ := Open(filepath.Join(t.TempDir(), "i.gob"), &fakeEmb{}, "fake", 400, 50)
	n, err := l.IngestDir(context.Background(), dir)
	if n == 0 || l.Sources()["b-good.pdf"] == 0 {
		t.Fatalf("good PDF not indexed: added %d, sources %v", n, l.Sources())
	}
	if err == nil || !strings.Contains(err.Error(), "a-corrupt.pdf") {
		t.Fatalf("the bad file is not reported: %v", err)
	}
	// a PDF that was indexed and is now gone is still dropped despite the bad file
	os.Remove(filepath.Join(dir, "b-good.pdf"))
	if _, err := l.IngestDir(context.Background(), dir); err == nil || len(l.Sources()) != 0 {
		t.Fatalf("sweep skipped: %v %v", err, l.Sources())
	}
}

// slowEmb blocks inside Embed until released, to prove readers are not blocked by an ingest in progress.
type slowEmb struct{ in, release chan struct{} }

func (s *slowEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	select {
	case s.in <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

func TestSearchAndSourcesDoNotWaitForIngest(t *testing.T) {
	dir := t.TempDir()
	writePDF(t, filepath.Join(dir, "a.pdf"), "Revenue grew.")
	emb := &slowEmb{in: make(chan struct{}, 1), release: make(chan struct{})}
	l, _ := Open(filepath.Join(t.TempDir(), "i.gob"), emb, "slow", 400, 50)
	done := make(chan error, 1)
	go func() { _, err := l.IngestDir(context.Background(), dir); done <- err }()
	<-emb.in // the ingest is now inside a network call
	got := make(chan struct{})
	go func() { l.Sources(); close(got) }()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("Sources blocked while an ingest was embedding")
	}
	close(emb.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if l.Sources()["a.pdf"] == 0 {
		t.Fatal("not indexed")
	}
}

func TestIngestHonoursCancellation(t *testing.T) {
	dir := t.TempDir()
	writePDF(t, filepath.Join(dir, "a.pdf"), "Revenue grew.")
	emb := &slowEmb{in: make(chan struct{}, 1), release: make(chan struct{})}
	l, _ := Open(filepath.Join(t.TempDir(), "i.gob"), emb, "slow", 400, 50)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := l.IngestDir(ctx, dir); done <- err }()
	<-emb.in
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled ingest reported success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ingest ignored cancellation")
	}
}

func TestCorruptIndexAndChangedSettingsAreHandled(t *testing.T) {
	idx := filepath.Join(t.TempDir(), "i.gob")
	os.WriteFile(idx, []byte("garbage"), 0o644)
	if _, err := Open(idx, &fakeEmb{}, "m", 400, 50); err == nil || !strings.Contains(err.Error(), "delete it") {
		t.Fatalf("corrupt index: %v", err)
	}
	os.Remove(idx)
	dir := t.TempDir()
	writePDF(t, filepath.Join(dir, "a.pdf"), "Revenue grew.")
	l, _ := Open(idx, &fakeEmb{}, "m", 400, 50)
	if _, err := l.IngestDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if same, _ := Open(idx, &fakeEmb{}, "m", 400, 50); len(same.chunks) == 0 {
		t.Fatal("index with the same settings was not reused")
	}
	if other, _ := Open(idx, &fakeEmb{}, "m", 800, 50); len(other.chunks) != 0 {
		t.Fatal("index built with other chunk settings was reused")
	}
	if left, _ := filepath.Glob(idx + ".tmp*"); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}

func TestSearchDoesNotWaitForIngestEither(t *testing.T) {
	dir := t.TempDir()
	writePDF(t, filepath.Join(dir, "a.pdf"), "Revenue grew.")
	l, _ := Open(filepath.Join(t.TempDir(), "i.gob"), &fakeEmb{}, "fake", 400, 50)
	if _, err := l.IngestDir(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	// a second, slow ingest of a new file is in flight while the first library state is searched
	writePDF(t, filepath.Join(dir, "b.pdf"), "Dolphin facts.")
	slow := &slowEmb{in: make(chan struct{}, 1), release: make(chan struct{})}
	l.emb = slow
	done := make(chan error, 1)
	go func() { _, err := l.IngestDir(context.Background(), dir); done <- err }()
	<-slow.in
	l.emb = &fakeEmb{} // the query embedder; the in-flight ingest already holds the slow one
	got := make(chan []Hit, 1)
	go func() { h, _ := l.Search(context.Background(), "revenue", 3, ""); got <- h }()
	select {
	case h := <-got:
		if len(h) == 0 || h[0].Source != "a.pdf" {
			t.Fatalf("hits %+v", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Search blocked while an ingest was embedding")
	}
	close(slow.release)
	<-done
}

// failEmb fails every call, standing in for a model server that is down.
type failEmb struct{}

func (failEmb) Embed(context.Context, []string) ([][]float32, error) {
	return nil, errors.New("connection refused")
}

func TestFailedPDFsAreRememberedOnlyWhenTheFileIsAtFault(t *testing.T) {
	dir := t.TempDir()
	idx := filepath.Join(t.TempDir(), "i.gob")
	bad := filepath.Join(dir, "bad.pdf")
	os.WriteFile(bad, []byte("%PDF-1.4 broken"), 0o644)
	writePDF(t, filepath.Join(dir, "good.pdf"), "Revenue grew.")

	l, _ := Open(idx, &fakeEmb{}, "e", 400, 50)
	_, err := l.IngestDir(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "bad.pdf") {
		t.Fatalf("first run: %v", err)
	}
	if _, ok := l.bad["bad.pdf"]; !ok {
		t.Fatal("a corrupt file was not remembered")
	}
	// still reported on the next run (from memory, unchanged), and across a restart
	l2, _ := Open(idx, &fakeEmb{}, "e", 400, 50)
	if _, ok := l2.bad["bad.pdf"]; !ok {
		t.Fatal("not persisted")
	}
	if _, err := l2.IngestDir(context.Background(), dir); err == nil || !strings.Contains(err.Error(), "bad.pdf") {
		t.Fatalf("second run lost the error: %v", err)
	}
	// replaced by a valid PDF: indexed, and no longer bad
	writePDF(t, bad, "Dolphin facts.")
	if _, err := l2.IngestDir(context.Background(), dir); err != nil || l2.Sources()["bad.pdf"] == 0 || len(l2.bad) != 0 {
		t.Fatalf("repaired file: %v %v %v", err, l2.Sources(), l2.bad)
	}
	// removed: forgotten
	os.WriteFile(bad, []byte("%PDF-1.4 broken again"), 0o644)
	l2.IngestDir(context.Background(), dir)
	os.Remove(bad)
	l2.IngestDir(context.Background(), dir)
	if len(l2.bad) != 0 {
		t.Fatalf("bad entry for a deleted file kept: %v", l2.bad)
	}

	// a down model server is not the file's fault: nothing is remembered, and the next run retries
	dir2 := t.TempDir()
	writePDF(t, filepath.Join(dir2, "ok.pdf"), "Revenue grew.")
	l3, _ := Open(filepath.Join(t.TempDir(), "i.gob"), failEmb{}, "e", 400, 50)
	if _, err := l3.IngestDir(context.Background(), dir2); err == nil {
		t.Fatal("embedding failure not reported")
	}
	if len(l3.bad) != 0 {
		t.Fatalf("an embedding outage marked the PDF bad: %v", l3.bad)
	}
	l3.emb = &fakeEmb{}
	if _, err := l3.IngestDir(context.Background(), dir2); err != nil || l3.Sources()["ok.pdf"] == 0 {
		t.Fatalf("not retried after the outage: %v %v", err, l3.Sources())
	}
}

func TestSnapshotIsIndependentOfLaterChanges(t *testing.T) {
	l, _ := Open(filepath.Join(t.TempDir(), "i.gob"), &fakeEmb{}, "e", 400, 50)
	l.chunks = []Chunk{{Source: "a.pdf", Text: "x"}}
	l.files["a.pdf"] = "h"
	snap := l.snapshot()
	l.chunks = append(l.chunks, Chunk{Source: "b.pdf"})
	l.files["b.pdf"] = "h2"
	l.drop("a.pdf")
	if len(snap.Chunks) != 1 || snap.Chunks[0].Source != "a.pdf" || len(snap.Files) != 1 {
		t.Fatalf("snapshot changed: %+v", snap)
	}
	if err := l.save(snap); err != nil {
		t.Fatal(err)
	}
}

func TestSearchDuringSaveAndIngestIsRaceFree(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writePDF(t, filepath.Join(dir, n+".pdf"), "Revenue "+n)
	}
	l, _ := Open(filepath.Join(t.TempDir(), "i.gob"), &fakeEmb{}, "e", 400, 50)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					l.Search(context.Background(), "revenue", 3, "")
					l.Sources()
				}
			}
		}()
	}
	for i := 0; i < 5; i++ {
		os.Remove(filepath.Join(dir, "c.pdf"))
		l.IngestDir(context.Background(), dir)
		writePDF(t, filepath.Join(dir, "c.pdf"), "Revenue c again "+strings.Repeat("x", i))
		l.IngestDir(context.Background(), dir)
	}
	close(stop)
	wg.Wait()
}

// gateAfter embeds the first n calls at once and blocks later ones until the context ends.
type gateAfter struct {
	n     int
	calls atomic.Int32
}

func (g *gateAfter) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if int(g.calls.Add(1)) > g.n {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	out := make([][]float32, len(texts))
	for i := range out {
		out[i] = []float32{1}
	}
	return out, nil
}

func TestLongIngestSavesProgressWhileRunning(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writePDF(t, filepath.Join(dir, n+".pdf"), "Revenue "+n)
	}
	idx := filepath.Join(t.TempDir(), "i.gob")
	emb := &gateAfter{n: 2}
	l, _ := Open(idx, emb, "e", 400, 50)
	l.saveEvery = time.Nanosecond // save after every finished file
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { l.IngestDir(ctx, dir); close(done) }()
	// c.pdf is blocked in Embed: the run is still going, yet a and b must already be on disk
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if again, err := Open(idx, emb, "e", 400, 50); err == nil && len(again.Sources()) == 2 {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatal("progress was not saved while the ingest was still running")
}
