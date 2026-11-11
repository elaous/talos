package web

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sort"
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

// proxyServer records the requests it receives.
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

	// Loopback proxy is exempt from the check.
	got, err := c.Fetch(context.Background(), "http://203.0.113.9/page", 100)
	if err != nil || got != "via proxy" || len(*seen) != 1 || (*seen)[0] != "http://203.0.113.9/page" {
		t.Fatalf("public target via proxy: %q %v %v", got, err, *seen)
	}
	// private targets are refused before reaching the proxy
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

func setProxy(t *testing.T, url string) {
	t.Helper()
	for _, k := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy"} {
		t.Setenv(k, url)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
}

func TestZonedIPv6AddressesAreStillChecked(t *testing.T) {
	for _, ip := range []string{"64:ff9b:1::1%lo", "2001::1%lo", "fe80::1%eth0", "::1%lo", "::ffff:10.0.0.1%lo"} {
		if !BlockedIP(netip.MustParseAddr(ip)) {
			t.Errorf("%s was not blocked", ip)
		}
	}
	if BlockedIP(netip.MustParseAddr("2606:4700:4700::1111%lo")) {
		t.Error("a public zoned address was blocked")
	}
}

// Proxy exemption must not extend to other fetch targets.
func TestProxyAddressIsNotAFetchTarget(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	var proxy *httptest.Server
	proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.String())
		mu.Unlock()
		if r.URL.String() == "http://203.0.113.9/redirect" { // public page redirecting to the proxy's admin port
			http.Redirect(w, r, proxy.URL+"/admin/secret", http.StatusFound)
			return
		}
		w.Write([]byte("<p>proxy admin page</p>"))
	}))
	defer proxy.Close()
	setProxy(t, proxy.URL)
	c := New("", false)
	addr := strings.TrimPrefix(proxy.URL, "http://")
	port := addr[strings.LastIndex(addr, ":")+1:]

	for _, u := range []string{"http://" + addr + "/admin", "http://localhost:" + port + "/admin", "http://LOCALHOST:" + port + "/admin", "http://127.0.0.1:" + port + "/"} {
		if got, err := c.Fetch(context.Background(), u, 100); err == nil {
			t.Errorf("%s fetched the proxy itself: %q", u, got)
		}
	}
	if _, err := c.Fetch(context.Background(), "http://203.0.113.9/redirect", 100); err == nil {
		t.Error("a redirect to the proxy's own address was followed")
	}
	// only the proxy is exempt; other loopback servers are refused
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<p>other</p>")) }))
	defer other.Close()
	if got, err := c.Fetch(context.Background(), other.URL, 100); err == nil || !strings.Contains(err.Error(), "private address") {
		t.Errorf("another loopback server was reachable: %q %v", got, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, s := range seen {
		if strings.Contains(s, "admin") {
			t.Errorf("the proxy's admin page was requested: %v", seen)
		}
	}
}

// A hostname-named proxy must work despite a private address.
func TestProxyGivenByHostnameWorks(t *testing.T) {
	proxy, seen := proxyServer(t)
	addr := strings.TrimPrefix(proxy.URL, "http://")
	setProxy(t, "http://localhost:"+addr[strings.LastIndex(addr, ":")+1:])
	c := New("", false)
	if got, err := c.Fetch(context.Background(), "http://203.0.113.9/x", 100); err != nil || got != "via proxy" || len(*seen) != 1 {
		t.Fatalf("hostname proxy: %q %v %v", got, err, *seen)
	}
	// private targets are still refused before reaching the proxy
	if _, err := c.Fetch(context.Background(), "http://10.0.0.5/", 100); err == nil || len(*seen) != 1 {
		t.Fatalf("private target through a hostname proxy: %v %v", err, *seen)
	}
}

func TestNameOnlyTheProxyCanResolvePassesThrough(t *testing.T) {
	proxy, seen := proxyServer(t)
	setProxy(t, proxy.URL)
	if got, err := New("", false).Fetch(context.Background(), "http://intranet.corp.invalid/page", 100); err != nil || got != "via proxy" || len(*seen) != 1 {
		t.Fatalf("%q %v %v", got, err, *seen)
	}
}

// A non-ASCII proxy address must not be dialled unchecked.
func TestNonASCIISpellingOfTheProxyAddressIsRefused(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Write([]byte("<p>proxy</p>"))
	}))
	proxy.Config.ConnState = func(_ net.Conn, st http.ConnState) { // a TLS ClientHello never reaches the handler
		if st == http.StateNew {
			mu.Lock()
			hits++
			mu.Unlock()
		}
	}
	proxy.Start()
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	addr := strings.TrimPrefix(proxy.URL, "http://")
	port := addr[strings.LastIndex(addr, ":")+1:]
	c := New("", false)
	for _, u := range []string{
		"https://１２７.0.0.1:" + port + "/", // fullwidth digits, which net/http maps to 127
		"https://127.0.0.1:" + port + "/",
		"https://LOCALHOST.:" + port + "/",
		"https://[::ffff:127.0.0.1]:" + port + "/",
		"http://１２７.0.0.1:" + port + "/", // http goes through the proxy: validated instead
	} {
		if _, err := c.Fetch(context.Background(), u, 100); err == nil {
			t.Errorf("%s was fetched", u)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("the proxy's listener received %d connections or requests meant for a private target", hits)
	}
}

// Spellings a C resolver accepts as loopback must be refused.
func TestLegacyAndLocalhostSpellingsAreRefusedBeforeTheProxy(t *testing.T) {
	proxy, seen := proxyServer(t)
	setProxy(t, proxy.URL)
	c := New("", false)
	for _, u := range []string{
		"http://2130706433/", "http://0x7f.1/", "http://127.1/", "http://0177.0.0.1/", "http://0x7f000001/",
		"http://LOCALHOST./", "http://localhost/", "http://foo.localhost/", "http://１２７.0.0.1/",
		"http://3232235777/", // 192.168.1.1
		"http://0xa.0.0.1/",  // 10.0.0.1
		"http://169.254.169.254/", "http://[::1]/", "http://[::ffff:10.0.0.1]/",
		"http://0x/", "http://1.2.3.4.5/", // not valid addresses, but not names either
	} {
		if _, err := c.Fetch(context.Background(), u, 100); err == nil {
			t.Errorf("%s was fetched", u)
		}
	}
	if len(*seen) != 0 {
		t.Fatalf("the proxy was asked for: %v", *seen)
	}
	// public addresses in those spellings still work
	for _, u := range []string{"http://134744072/", "http://0x8.8.8.8/", "http://203.0.113.9/", "http://intranet.corp.invalid/", "http://xn--bcher-kva.example.invalid/"} {
		if got, err := c.Fetch(context.Background(), u, 100); err != nil || got != "via proxy" {
			t.Errorf("%s: %q %v", u, got, err)
		}
	}
}

func TestParseLegacyIPv4(t *testing.T) {
	for in, want := range map[string]string{
		"2130706433": "127.0.0.1", "0x7f.1": "127.0.0.1", "127.1": "127.0.0.1", "0177.0.0.1": "127.0.0.1",
		"0x7f000001": "127.0.0.1", "1.2.3.4": "1.2.3.4", "134744072": "8.8.8.8", "0": "0.0.0.0", "10.1": "10.0.0.1",
		"0300.0250.0.1": "192.168.0.1", "255.255.255.255": "255.255.255.255", "4294967295": "255.255.255.255",
	} {
		ip, ok := parseLegacyIPv4(in)
		if !ok || ip.String() != want {
			t.Errorf("%s -> %v %v, want %s", in, ip, ok, want)
		}
	}
	for _, in := range []string{"", "1.2.3.4.5", "256.1.1.1", "1.2.3.256", "4294967296", "1..2", "0x", "08", "09.1", "abc", "1.2.3.", "-1", "1.1.65536"} {
		if ip, ok := parseLegacyIPv4(in); ok {
			t.Errorf("%q parsed as %v", in, ip)
		}
	}
}

// Without a proxy, judge each spelling by the dialled address.
func TestDirectFetchesJudgeTheDialledAddress(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<p>secret</p>")) }))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	c := New("", false)
	for _, u := range []string{"http://１２７.0.0.1:" + port, "http://LOCALHOST.:" + port, "http://localhost:" + port, "http://[::1]:" + port} {
		if got, err := c.Fetch(context.Background(), u, 100); err == nil {
			t.Errorf("%s: %q", u, got)
		}
	}
}

// connectProxy records CONNECT targets and request URLs, and never connects.
func connectProxy(t *testing.T) (*httptest.Server, func() []string) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		if r.Method == http.MethodConnect {
			seen = append(seen, "CONNECT "+r.Host)
		} else {
			seen = append(seen, r.Method+" "+r.URL.String())
		}
		mu.Unlock()
		http.Error(w, "no", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), seen...) }
}

// Validated host must match the spelling sent to the proxy.
func TestHTTPSTargetsReachTheProxyOnlyIfPublic(t *testing.T) {
	proxy, seen := connectProxy(t)
	setProxy(t, proxy.URL)
	c := New("", false)
	for _, u := range []string{
		"https://localhost。/", "https://127.0.0.1。/", "https://LOCALHOST．/", "https://0x7f.1。/", "https://2130706433。/",
		"https://localhost../", "https://127.0.0.1../", "https://LOCALHOST./", "https://127.0.0.1./",
		"https://１０.0.0.1/", "https://１９２.168.1.1/", "https://１２７.0.0.1/", "https://0.1.2.3/", "https://168.63.129.16/",
		"https://[::1]/", "https://[fe80::1%25eth0]/", "https://[::ffff:10.0.0.1]/", "https://[64:ff9b::a00:1]/",
		"https://169.254.169.254/", "https://foo.localhost/",
	} {
		c.Fetch(context.Background(), u, 100)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("the proxy was asked to connect to private targets: %v", got)
	}
	for _, u := range []string{
		"https://203.0.113.9/", "https://[2606:4700:4700::1111]/", "https://my_host.example.invalid/", "https://xn--bcher-kva.example.invalid/",
		"https://straße.example.invalid/", "https://1e100.net/", "https://3com.com/", "https://a.b.c.1/",
	} {
		c.Fetch(context.Background(), u, 100)
	}
	want := []string{
		"CONNECT 203.0.113.9:443", "CONNECT [2606:4700:4700::1111]:443", "CONNECT my_host.example.invalid:443",
		"CONNECT xn--bcher-kva.example.invalid:443", "CONNECT xn--strae-oqa.example.invalid:443", "CONNECT 1e100.net:443",
		"CONNECT 3com.com:443", "CONNECT a.b.c.1:443",
	}
	got := seen()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("public targets did not all reach the proxy:\n got  %v\n want %v", got, want)
	}
}

func TestZeroNetworkAndAzureWireServerAreBlocked(t *testing.T) {
	for _, ip := range []string{"0.1.2.3", "0.255.255.255", "168.63.129.16"} {
		if !BlockedIP(netip.MustParseAddr(ip)) {
			t.Errorf("%s not blocked", ip)
		}
	}
	for _, ip := range []string{"1.0.0.1", "168.63.129.17", "8.8.4.4"} {
		if BlockedIP(netip.MustParseAddr(ip)) {
			t.Errorf("%s wrongly blocked", ip)
		}
	}
}

// A NO_PROXY entry must not break proxied requests.
func TestProxyTransportsAreMadeOnDemand(t *testing.T) {
	proxy, seen := proxyServer(t)
	setProxy(t, proxy.URL)
	t.Setenv("NO_PROXY", ".invalid")
	t.Setenv("no_proxy", ".invalid")
	if got, err := New("", false).Fetch(context.Background(), "http://203.0.113.9/x", 100); err != nil || got != "via proxy" || len(*seen) != 1 {
		t.Fatalf("%q %v %v", got, err, *seen)
	}
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"Example.COM": "example.com", "localhost.": "localhost", "localhost..": "localhost", "localhost。": "localhost",
		"LOCALHOST．": "localhost", "１２７.0.0.1": "127.0.0.1", "my_host.example.com": "my_host.example.com",
		"2606:4700::1111": "2606:4700::1111", "straße.de": "xn--strae-oqa.de", "": "",
	} {
		if got, err := normalizeHost(in); err != nil || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, err, want)
		}
	}
}

func TestValidateProxiedTargetDirectly(t *testing.T) {
	ctx := context.Background()
	for _, h := range []string{"::1::2", "2606:4700::zz", "fe80::1", "::1", "::ffff:127.0.0.1", "localhost", "foo.LOCALHOST", "0x7f.1", "1.2.3.4.5", "0x"} {
		if err := validateProxiedTarget(ctx, h); err == nil {
			t.Errorf("%q was accepted", h)
		}
	}
	for _, h := range []string{"2606:4700:4700::1111", "8.8.8.8", "example.invalid", "my_host.example.invalid", "134744072"} {
		if err := validateProxiedTarget(ctx, h); err != nil {
			t.Errorf("%q was refused: %v", h, err)
		}
	}
}

// Plain-http requests must be sent with the validated host spelling.
func TestPlainHTTPTargetsAreSentUnderTheValidatedName(t *testing.T) {
	proxy, seen := connectProxy(t)
	setProxy(t, proxy.URL)
	c := New("", false)
	for _, u := range []string{"http://localhost。/", "http://LOCALHOST．/p", "http://１０.0.0.1/", "http://127.0.0.1。/"} {
		c.Fetch(context.Background(), u, 100)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("private targets reached the proxy over plain http: %v", got)
	}
	for _, u := range []string{"http://EXAMPLE.invalid./x", "http://[2606:4700:4700::1111]:8080/y", "http://Ünï.example.invalid/z", "http://user:pw@203.0.113.9:81/q?a=1"} {
		c.Fetch(context.Background(), u, 100)
	}
	got := seen()
	sort.Strings(got)
	want := []string{
		"GET http://203.0.113.9:81/q?a=1", // userinfo is not part of the host
		"GET http://[2606:4700:4700::1111]:8080/y",
		"GET http://example.invalid/x",
		"GET http://xn--n-nga1b.example.invalid/z", // mapped to lower case, then punycoded
	}
	sort.Strings(want)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("what the proxy was asked for:\n got  %v\n want %v", got, want)
	}
}

func TestNumericLookingFirstLabelsAreStillRealSites(t *testing.T) {
	for _, h := range []string{"0x.org", "0x.example.invalid", "0X.example.invalid", "0xdeadbeef.example.invalid", "1.example.invalid"} {
		if err := validateProxiedTarget(context.Background(), h); err != nil {
			t.Errorf("%q was refused: %v", h, err)
		}
	}
	for _, h := range []string{"0x", "0x.0x", "0x.1", "1.0x"} {
		if err := validateProxiedTarget(context.Background(), h); err == nil {
			t.Errorf("%q was accepted", h)
		}
	}
}

// Refuse IDNA failures; raw UTF-8 would reach CONNECT.
func TestHostsThatFailIDNAAreRefused(t *testing.T) {
	for _, h := range []string{"localhost․", "local‍host", "127.0.0.1﹒", "a　b.example.invalid", "x‌.example.invalid"} {
		if got, err := normalizeHost(h); err == nil {
			t.Errorf("normalizeHost(%q) = %q, want an error", h, got)
		}
		if err := validateProxiedTarget(context.Background(), h); err == nil {
			t.Errorf("validateProxiedTarget(%q) accepted it", h)
		}
	}
	proxy, seen := connectProxy(t)
	setProxy(t, proxy.URL)
	New("", false).Fetch(context.Background(), "https://localhost․/", 100)
	if got := seen(); len(got) != 0 {
		t.Fatalf("the proxy received %v", got)
	}
}

// Concurrent first use of per-proxy transports must be race-free.
func TestConcurrentFirstUseOfAProxyIsSafe(t *testing.T) {
	proxy, seen := proxyServer(t)
	setProxy(t, proxy.URL)
	c := New("", false)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if got, err := c.Fetch(context.Background(), fmt.Sprintf("http://203.0.113.%d/", i+1), 100); err != nil || got != "via proxy" {
				t.Errorf("fetch %d: %q %v", i, got, err)
			}
		}(i)
	}
	wg.Wait()
	if len(*seen) != 40 {
		t.Fatalf("the proxy saw %d requests", len(*seen))
	}
}

func TestIdleConnectionsAreBounded(t *testing.T) {
	proxy, _ := proxyServer(t)
	setProxy(t, proxy.URL)
	c := New("", false)
	pt := c.http.Transport.(*pageTransport)
	for name, tr := range map[string]*http.Transport{"direct": pt.direct, "proxied": pt.proxyTransport(mustURL(proxy.URL)), "searx": c.searx.Transport.(*http.Transport), "allow_private": New("", true).http.Transport.(*http.Transport)} {
		if tr.IdleConnTimeout <= 0 || tr.MaxIdleConns <= 0 || tr.MaxIdleConnsPerHost <= 0 {
			t.Errorf("%s: idle connections are unbounded: %v %d %d", name, tr.IdleConnTimeout, tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
		}
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
