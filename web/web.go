// Package web provides search and page fetching.
package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/http/httpproxy"
	"golang.org/x/net/idna"
)

type Result struct{ Title, URL, Snippet string }

type Client struct {
	SearxngURL string
	http       *http.Client // pages: refuses private addresses unless allowed
	searx      *http.Client // operator's SearXNG, may be on localhost
}

// Non-public ranges that dial checks often miss.
var blockedNets = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8",        // this network
		"168.63.129.16/32", // Azure WireServer metadata address
		"100.64.0.0/10",    // carrier-grade NAT; Alibaba metadata at 100.100.100.200
		"198.18.0.0/15",    // benchmarking
		"192.0.0.0/24",     // IETF protocol assignments
		"240.0.0.0/4",      // reserved, including 255.255.255.255
		"64:ff9b:1::/48",   // local-use NAT64
		"fd00:ec2::/32",    // AWS IPv6 metadata
		"fe80::/10",        // link-local
		"fec0::/10",        // deprecated site-local
		"2001:db8::/32",    // documentation
		"2001::/32",        // Teredo
		"::ffff:0:0/96",    // IPv4-mapped; caller unmaps first
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// BlockedIP reports whether ip is not public, unwrapping NAT64 and 6to4.
func BlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap().WithZone("") // a zone suffix would defeat the prefix tests below
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	for _, p := range blockedNets {
		if p.Contains(ip) {
			return true
		}
	}
	if ip.Is6() {
		b := ip.As16()
		switch {
		case b[0] == 0x00 && b[1] == 0x64 && b[2] == 0xff && b[3] == 0x9b && b[4] == 0 && b[5] == 0 && b[6] == 0 && b[7] == 0 && b[8] == 0 && b[9] == 0 && b[10] == 0 && b[11] == 0: // NAT64 64:ff9b::/96
			return BlockedIP(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
		case b[0] == 0x20 && b[1] == 0x02: // 6to4
			return BlockedIP(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
		}
	}
	return false
}

// New builds a client. Unless allowPrivate, page fetches refuse non-public addresses.
// Proxied hosts are checked best-effort: the proxy resolves DNS again.
func New(searxngURL string, allowPrivate bool) *Client {
	plain := &net.Dialer{Timeout: 10 * time.Second}
	pf := httpproxy.FromEnvironment().ProxyFunc()
	var page http.RoundTripper
	if allowPrivate {
		page = boundIdle(&http.Transport{DialContext: plain.DialContext, Proxy: func(r *http.Request) (*url.URL, error) { return pf(r.URL) }})
	} else {
		page = newPageTransport(pf)
	}
	return &Client{SearxngURL: searxngURL,
		http:  &http.Client{Timeout: 25 * time.Second, Transport: page},
		searx: &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{DialContext: plain.DialContext, Proxy: func(r *http.Request) (*url.URL, error) { return pf(r.URL) }, IdleConnTimeout: 60 * time.Second, MaxIdleConns: 20, MaxIdleConnsPerHost: 2}}}
}

// pageTransport dials directly (checked) or via a validated proxy.
type pageTransport struct {
	pf      func(*url.URL) (*url.URL, error)
	direct  *http.Transport
	mu      sync.Mutex
	proxied map[string]*http.Transport // per proxy URL; dials only that proxy
	plain   *net.Dialer
}

func newPageTransport(pf func(*url.URL) (*url.URL, error)) *pageTransport {
	checked := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil {
			return err
		}
		if BlockedIP(ip) {
			return fmt.Errorf("web: refusing to connect to private address %s", ip.Unmap())
		}
		return nil
	}}
	return &pageTransport{pf: pf, direct: boundIdle(&http.Transport{DialContext: checked.DialContext}),
		proxied: map[string]*http.Transport{}, plain: &net.Dialer{Timeout: 10 * time.Second}}
}

// proxyTransport returns the lazily built transport for pu.
func (t *pageTransport) proxyTransport(pu *url.URL) *http.Transport {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := pu.String()
	if t.proxied[key] == nil {
		t.proxied[key] = boundIdle(&http.Transport{Proxy: http.ProxyURL(pu), DialContext: t.plain.DialContext})
	}
	return t.proxied[key]
}

func (t *pageTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	pu, err := t.pf(r.URL)
	if err != nil {
		return nil, err
	}
	if pu == nil { // no proxy for this request: checked direct dial
		return t.direct.RoundTrip(r)
	}
	if err := validateProxiedTarget(r.Context(), r.URL.Hostname()); err != nil {
		return nil, err
	}
	// Send the validated name: http and https spell hosts differently.
	h, _ := normalizeHost(r.URL.Hostname()) // validateProxiedTarget has accepted it
	r2 := r.Clone(r.Context())
	r2.URL.Host = hostWithPort(h, r.URL.Port())
	r2.Host = r2.URL.Host
	return t.proxyTransport(pu).RoundTrip(r2)
}

func hostWithPort(host, port string) string {
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// Normalize as net/http sends it: IDNA, lower-case, no trailing dots.
func normalizeHost(h string) (string, error) {
	ascii := true
	for i := 0; i < len(h); i++ {
		if h[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if !ascii {
		a, err := idna.Lookup.ToASCII(h)
		if err != nil {
			return "", fmt.Errorf("web: refusing host %q: %w", h, err)
		}
		h = a
	}
	return strings.TrimRight(strings.ToLower(h), "."), nil
}

// Accepts inet_aton forms such as "127.1" and "0x7f.1".
func parseLegacyIPv4(s string) (netip.Addr, bool) {
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return netip.Addr{}, false
	}
	var vals []uint64
	for _, p := range parts {
		if p == "" {
			return netip.Addr{}, false
		}
		base, digits := 10, p
		switch {
		case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
			base, digits = 16, p[2:]
		case len(p) > 1 && p[0] == '0':
			base, digits = 8, p[1:]
		}
		v, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return netip.Addr{}, false
		}
		vals = append(vals, v)
	}
	var n uint64
	for i, v := range vals {
		if i < len(vals)-1 {
			if v > 255 {
				return netip.Addr{}, false
			}
			n |= v << (8 * (3 - uint(i)))
		} else {
			if v >= 1<<(8*(4-uint(i))) {
				return netip.Addr{}, false
			}
			n |= v
		}
	}
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}), true
}

// Every label numeric: the name is an address spelling, not DNS.
func numericLabels(h string) bool {
	for _, p := range strings.Split(h, ".") {
		if p == "" {
			return false
		}
		d := p
		if strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X") {
			d = p[2:]
			if d == "" {
				continue // "0x" alone is numeric; "0x.org" is not an address
			}
			for _, c := range d {
				if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
					return false
				}
			}
			continue
		}
		for _, c := range d {
			if c < '0' || c > '9' {
				return false
			}
		}
	}
	return true
}

// Refuses localhost and non-public addresses; unresolvable names pass.
func validateProxiedTarget(ctx context.Context, host string) error {
	h, err := normalizeHost(host)
	if err != nil {
		return err
	}
	refuse := func(why string) error { return fmt.Errorf("web: refusing to connect to %s: %s", host, why) }
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return refuse("localhost")
	}
	if ip, ok := parseLegacyIPv4(h); ok {
		if BlockedIP(ip) {
			return refuse("private address " + ip.String())
		}
		return nil
	}
	if numericLabels(h) {
		return refuse("an ambiguous numeric address")
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		if BlockedIP(ip) {
			return refuse("private address " + ip.Unmap().String())
		}
		return nil
	}
	if strings.Contains(h, ":") { // a colon never appears in a DNS name
		return refuse("not a valid address")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", h)
	if err != nil {
		return nil
	}
	for _, ip := range ips {
		if BlockedIP(ip) {
			return refuse("it resolves to private address " + ip.Unmap().String())
		}
	}
	return nil
}

// Caps idle connections in total and per host.
func boundIdle(t *http.Transport) *http.Transport {
	t.IdleConnTimeout = 60 * time.Second
	t.MaxIdleConns = 20
	t.MaxIdleConnsPerHost = 2
	return t
}

func (c *Client) get(ctx context.Context, hc *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; talos)")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func (c *Client) Search(ctx context.Context, q string, n int) ([]Result, error) {
	if n <= 0 {
		n = 5
	}
	var rs []Result
	if c.SearxngURL != "" {
		b, err := c.get(ctx, c.searx, strings.TrimRight(c.SearxngURL, "/")+"/search?format=json&q="+url.QueryEscape(q))
		if err != nil {
			return nil, err
		}
		var r struct {
			Results []struct{ Title, URL, Content string }
		}
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		for _, x := range r.Results {
			rs = append(rs, Result{x.Title, x.URL, x.Content})
		}
	} else {
		b, err := c.get(ctx, c.http, "https://html.duckduckgo.com/html/?q="+url.QueryEscape(q))
		if err != nil {
			return nil, err
		}
		rs = parseDDG(b)
	}
	if len(rs) > n {
		rs = rs[:n]
	}
	return rs, nil
}

func attr(n *html.Node, k string) string {
	for _, a := range n.Attr {
		if a.Key == k {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, c string) bool {
	for _, f := range strings.Fields(attr(n, "class")) {
		if f == c {
			return true
		}
	}
	return false
}

func text(n *html.Node) string {
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(sb.String()), " ")
}

func parseDDG(b []byte) []Result {
	doc, err := html.Parse(strings.NewReader(string(b)))
	if err != nil {
		return nil
	}
	var rs []Result
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "a" && hasClass(n, "result__a") {
			rs = append(rs, Result{Title: text(n), URL: ddgTarget(attr(n, "href"))})
		}
		if n.Type == html.ElementNode && hasClass(n, "result__snippet") && len(rs) > 0 && rs[len(rs)-1].Snippet == "" {
			rs[len(rs)-1].Snippet = text(n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return rs
}

// Unwraps DuckDuckGo's uddg redirect links.
func ddgTarget(h string) string {
	if strings.HasPrefix(h, "//") {
		h = "https:" + h
	}
	if u, err := url.Parse(h); err == nil {
		if t := u.Query().Get("uddg"); t != "" {
			return t
		}
	}
	return h
}

func (c *Client) Fetch(ctx context.Context, raw string, maxChars int) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("web: %q is not an http(s) URL", raw)
	}
	b, err := c.get(ctx, c.http, raw)
	if err != nil {
		return "", err
	}
	return Readable(b, maxChars), nil
}

// Extracts visible text, skipping scripts, styles and page furniture.
func Readable(b []byte, maxChars int) string {
	doc, err := html.Parse(strings.NewReader(string(b)))
	if err != nil {
		return ""
	}
	var sb strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "nav", "footer", "header", "svg", "form":
				return
			case "p", "div", "li", "br", "h1", "h2", "h3", "h4", "tr":
				sb.WriteByte('\n')
			}
		}
		if n.Type == html.TextNode {
			sb.WriteString(n.Data)
			sb.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	var lines []string
	for _, l := range strings.Split(sb.String(), "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			lines = append(lines, l)
		}
	}
	out := strings.Join(lines, "\n")
	if maxChars > 0 && len(out) > maxChars {
		out = strings.ToValidUTF8(out[:maxChars], "") + " …[truncated]"
	}
	return out
}
