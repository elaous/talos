// Package web is the agents' window on the internet: search and page fetch.
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
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
)

type Result struct{ Title, URL, Snippet string }

type Client struct {
	SearxngURL string
	http       *http.Client // pages: refuses private addresses unless allowed
	searx      *http.Client // the operator's own SearXNG, which may well be on localhost
}

// Address ranges that are not the public internet and that dial checks tend to forget.
var blockedNets = func() []netip.Prefix {
	var ps []netip.Prefix
	for _, s := range []string{
		"100.64.0.0/10",  // carrier-grade NAT: Tailscale, and Alibaba's metadata service 100.100.100.200
		"198.18.0.0/15",  // benchmarking
		"192.0.0.0/24",   // IETF protocol assignments
		"240.0.0.0/4",    // reserved, including 255.255.255.255
		"64:ff9b:1::/48", // local-use NAT64
		"fd00:ec2::/32",  // AWS IPv6 metadata
		"fe80::/10",      // link-local
		"fec0::/10",      // deprecated site-local
		"2001:db8::/32",  // documentation
		"2001::/32",      // Teredo
		"::ffff:0:0/96",  // IPv4-mapped (also unmapped by the caller)
	} {
		ps = append(ps, netip.MustParsePrefix(s))
	}
	return ps
}()

// BlockedIP reports whether fetching ip would reach a loopback, LAN, link-local, metadata or otherwise
// non-public address, including IPv4 addresses embedded in NAT64 and 6to4 IPv6 addresses.
func BlockedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
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

// New builds a client. Unless allowPrivate, page fetches refuse non-public addresses, checked at connect time
// (so redirects and DNS tricks cannot get around it) with no proxy in between (a proxy would be dialled
// instead of the target, hiding it from the check): a model that was fed a hostile page must not be able to
// reach services on this machine or its network.
func New(searxngURL string, allowPrivate bool) *Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{DialContext: d.DialContext}
	if allowPrivate {
		tr.Proxy = http.ProxyFromEnvironment
	} else {
		dc := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
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
		tr.DialContext = dc.DialContext
	}
	return &Client{SearxngURL: searxngURL,
		http:  &http.Client{Timeout: 25 * time.Second, Transport: tr},
		searx: &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{DialContext: d.DialContext, Proxy: http.ProxyFromEnvironment}}}
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

// Search uses SearXNG when configured and DuckDuckGo's HTML endpoint otherwise.
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

// ddgTarget unwraps DuckDuckGo's //duckduckgo.com/l/?uddg=<url> redirect links.
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

// Fetch returns a page's readable text, cut to maxChars.
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

// Readable extracts the visible text of an HTML page, skipping scripts, styles and page furniture.
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
