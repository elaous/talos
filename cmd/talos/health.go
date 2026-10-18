package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
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

// checkWritable makes sure each folder exists and this process can write in it, and says what to do when it
// cannot. In a container the usual causes are a bind-mounted folder that belongs to another user, a named volume
// left over from a run as another user, and no mount at all under a read-only root filesystem; without this
// they would show up only as "permission denied" at the first upload or ingest, with a green health check.
func checkWritable(dirs ...string) error {
	for _, d := range dirs {
		if err := probeWritable(d); err != nil {
			return err
		}
	}
	return nil
}

// createTemp is os.CreateTemp; tests replace it, since a test run as root can write anywhere.
var createTemp = os.CreateTemp

func probeWritable(d string) error {
	err := os.MkdirAll(d, 0o755)
	if err == nil {
		var f *os.File
		if f, err = createTemp(d, ".writable-*"); err == nil {
			f.Close()
			os.Remove(f.Name())
			return nil
		}
	}
	uid, gid := os.Getuid(), os.Getgid()
	switch {
	case errors.Is(err, syscall.EROFS):
		return fmt.Errorf("%s is on a read-only filesystem: %w\n  nothing writable is mounted there; mount a volume or folder at %s (the compose file does)", d, err, d)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s is not writable by uid %d: %w\n  a bind-mounted folder must belong to that user (chown -R %d:%d on the host, or `make docker-up-host`);\n  a named volume left from a run as another user needs its owner fixed: docker run --rm -v <volume>:/v alpine chown -R %d:%d /v",
			d, uid, err, uid, gid, uid, gid)
	}
	return fmt.Errorf("%s: %w", d, err)
}
