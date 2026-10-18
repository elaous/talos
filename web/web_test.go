package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
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

// proxyServer records the requests that reach it, like an HTTP proxy would see them.
func proxyServer(t *testing.T) (*httptest.Server, *[]string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		w.Write([]byte("<p>via proxy</p>"))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestProxyIsUsedButTargetsAreStillChecked(t *testing.T) {
	proxy, seen := proxyServer(t)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	c := New("", false)

	// a public address goes through the proxy, which itself is on loopback and so exempt from the check
	got, err := c.Fetch(context.Background(), "http://203.0.113.9/page", 100)
	if err != nil || got != "via proxy" || len(*seen) != 1 || (*seen)[0] != "http://203.0.113.9/page" {
		t.Fatalf("public target via proxy: %q %v %v", got, err, *seen)
	}
	// private targets are refused before the proxy is asked, so it cannot be used to reach the LAN
	for _, u := range []string{"http://10.1.2.3/", "http://192.168.0.1/admin", "http://169.254.169.254/latest/meta-data/", "http://[::1]:9/", "http://100.100.100.200/"} {
		if _, err := c.Fetch(context.Background(), u, 100); err == nil {
			t.Errorf("%s was fetched through the proxy", u)
		}
	}
	if len(*seen) != 1 {
		t.Fatalf("the proxy saw requests for private targets: %v", *seen)
	}
}

func TestProxyEnvironmentAppliesWhenPrivateAllowed(t *testing.T) {
	proxy, seen := proxyServer(t)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	if got, err := New("", true).Fetch(context.Background(), "http://10.1.2.3/x", 100); err != nil || got != "via proxy" || len(*seen) != 1 {
		t.Fatalf("%q %v %v", got, err, *seen)
	}
}
