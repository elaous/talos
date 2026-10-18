package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestReadableSkipsScriptsAndTruncates(t *testing.T) {
	got := Readable([]byte(`<html><head><style>x{}</style></head><body><nav>menu</nav><script>evil()</script>
		<h1>Title</h1><p>Hello   <b>world</b></p><footer>f</footer></body></html>`), 0)
	if got != "Title\nHello world" {
		t.Fatalf("%q", got)
	}
	if out := Readable([]byte("<p>"+strings.Repeat("é", 100)+"</p>"), 21); !strings.HasSuffix(out, "[truncated]") {
		t.Fatalf("%q", out)
	}
}

func TestParseDDG(t *testing.T) {
	rs := parseDDG([]byte(`<div><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2F&rut=x">Go</a>
		<a class="result__snippet">The Go language</a></div>`))
	if len(rs) != 1 || rs[0].URL != "https://go.dev/" || rs[0].Title != "Go" || rs[0].Snippet != "The Go language" {
		t.Fatalf("%+v", rs)
	}
}

func TestPrivateAddressesRefusedUnlessAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<p>secret</p>")) }))
	defer srv.Close()
	if _, err := New("", false).Fetch(context.Background(), srv.URL, 100); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Fatalf("loopback fetch not refused: %v", err)
	}
	if got, err := New("", true).Fetch(context.Background(), srv.URL, 100); err != nil || got != "secret" {
		t.Fatalf("allowed fetch: %q %v", got, err)
	}
	if _, err := New("", true).Fetch(context.Background(), "file:///etc/passwd", 100); err == nil {
		t.Fatal("file: URL accepted")
	}
}
