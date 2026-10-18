package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
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

func TestBlockedIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "::1": true, "10.1.2.3": true, "192.168.0.9": true, "172.16.5.5": true,
		"169.254.169.254": true, "100.100.100.200": true, "100.64.1.1": true, "198.18.0.1": true,
		"192.0.0.8": true, "255.255.255.255": true, "0.0.0.0": true, "224.0.0.1": true,
		"fd00:ec2::254": true, "fe80::1": true, "fc00::1": true, "::ffff:127.0.0.1": true, "::ffff:10.0.0.1": true,
		"64:ff9b::7f00:1": true, "64:ff9b::a9fe:a9fe": true, "2002:7f00:1::": true, "2002:c0a8:101::": true,
		"8.8.8.8": false, "1.1.1.1": false, "93.184.216.34": false, "2606:4700:4700::1111": false,
		"64:ff9b::808:808": false, // NAT64 to a public IPv4
	} {
		if got := BlockedIP(netip.MustParseAddr(ip)); got != want {
			t.Errorf("BlockedIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestPageFetchesNeverUseAProxy(t *testing.T) {
	// A proxy would be dialled instead of the target, so the private-address check would see only the proxy.
	if New("", false).http.Transport.(*http.Transport).Proxy != nil {
		t.Fatal("page fetches use a proxy")
	}
	if New("", true).http.Transport.(*http.Transport).Proxy == nil {
		t.Fatal("with allow_private the environment proxy should apply")
	}
}

func TestLocalSearxngWorksWithoutOpeningFetchURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			w.Write([]byte(`{"results":[{"title":"T","url":"https://go.dev","content":"C"}]}`))
			return
		}
		w.Write([]byte("<p>private</p>"))
	}))
	defer srv.Close()
	c := New(srv.URL, false)
	rs, err := c.Search(context.Background(), "go", 5)
	if err != nil || len(rs) != 1 || rs[0].URL != "https://go.dev" {
		t.Fatalf("searxng on loopback: %+v %v", rs, err)
	}
	if _, err := c.Fetch(context.Background(), srv.URL+"/page", 100); err == nil {
		t.Fatal("fetch_url reached the same loopback server")
	}
}
