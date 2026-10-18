package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
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

// checkWritable makes sure each folder exists and this process can write in it. In a container the usual cause of
// failure is a bind-mounted folder that belongs to another user, which would otherwise show up only as a
// "permission denied" at the first upload or ingest, while the health check stays green.
// createTemp is os.CreateTemp; tests replace it, since a test run as root can write anywhere.
var createTemp = os.CreateTemp

func checkWritable(dirs ...string) error {
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("%s: %w", d, err)
		}
		f, err := createTemp(d, ".writable-*")
		if err != nil {
			return fmt.Errorf("%s is not writable by uid %d: %w\n  with a bind mount the folder must belong to that user: use a named volume, or run the container as the folder's owner (make docker-up-host)", d, os.Getuid(), err)
		}
		f.Close()
		os.Remove(f.Name())
	}
	return nil
}
