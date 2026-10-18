package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// healthURL is where the server's own front page can be fetched from inside the same container or host: the
// listen address, with a wildcard host (":8080", "0.0.0.0", "::") replaced by loopback.
func healthURL(listen string) (string, error) {
	h, p, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen %q: %w", listen, err)
	}
	switch h {
	case "", "0.0.0.0", "::":
		h = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(h, p) + "/", nil
}

// checkHealth fetches url and wants a 200.
func checkHealth(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, strings.TrimSpace(resp.Status))
	}
	return nil
}
