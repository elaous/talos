package main

import (
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/elaous/talos/config"
)

func TestAllowedHosts(t *testing.T) {
	mk := func(listen string, extra ...string) config.Config {
		c := config.Default()
		c.Listen, c.AllowedHosts = listen, extra
		return c
	}
	got, err := allowedHosts(mk("127.0.0.1:8080"))
	if err != nil || !slices.Contains(got, "localhost") || !slices.Contains(got, "127.0.0.1") {
		t.Fatalf("loopback: %v %v", got, err)
	}
	got, err = allowedHosts(mk("192.168.1.5:8080", "talos.lan"))
	if err != nil || !slices.Contains(got, "192.168.1.5") || !slices.Contains(got, "talos.lan") {
		t.Fatalf("lan: %v %v", got, err)
	}
	for _, l := range []string{"0.0.0.0:8080", ":8080", "[::]:8080"} {
		if _, err := allowedHosts(mk(l)); err == nil || !strings.Contains(err.Error(), "allowed_hosts") {
			t.Errorf("%s without allowed_hosts must refuse to start: %v", l, err)
		}
		if got, err := allowedHosts(mk(l, "talos.lan")); err != nil || !slices.Contains(got, "talos.lan") {
			t.Errorf("%s with allowed_hosts: %v %v", l, got, err)
		}
	}
	if _, err := allowedHosts(mk("nonsense")); err == nil {
		t.Error("malformed listen address accepted")
	}
}

func TestHealthURL(t *testing.T) {
	for in, want := range map[string]string{
		"127.0.0.1:8080": "http://127.0.0.1:8080/", ":8080": "http://127.0.0.1:8080/", "0.0.0.0:9": "http://127.0.0.1:9/",
		"[::]:8080": "http://127.0.0.1:8080/", "[::1]:8080": "http://[::1]:8080/", "192.168.1.5:80": "http://192.168.1.5:80/",
	} {
		if got, err := healthURL(in); err != nil || got != want {
			t.Errorf("%s -> %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := healthURL("nonsense"); err == nil {
		t.Error("malformed listen address accepted")
	}
}

func TestCheckHealth(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ui")) }))
	defer ok.Close()
	if err := checkHealth(context.Background(), ok.URL); err != nil {
		t.Fatal(err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 500) }))
	defer bad.Close()
	if err := checkHealth(context.Background(), bad.URL); err == nil {
		t.Error("a 500 passed")
	}
	if err := checkHealth(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Error("a closed port passed")
	}
}

// Image config must load and serve; wildcard listen needs allowed_hosts.
// Paths must match those the Dockerfile creates.
func TestDockerConfigIsValid(t *testing.T) {
	cfg, err := config.Load("../../docker/talos.json")
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := allowedHosts(cfg)
	if err != nil || !slices.Contains(hosts, "localhost") {
		t.Fatalf("%v %v", hosts, err)
	}
	if cfg.DataDir != "/data" || cfg.PDFDir != "/pdfs" || cfg.UIDir != "/app/ui" || cfg.Listen != ":8080" {
		t.Fatalf("paths drifted from the Dockerfile: %+v", cfg)
	}
}

func TestCheckWritable(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "a", "b")
	if err := checkWritable(fresh, dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(fresh); err != nil || !fi.IsDir() {
		t.Fatalf("a missing folder was not created: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".writable-*")); len(left) != 0 {
		t.Fatalf("probe files left behind: %v", left)
	}
	// a path that cannot be a folder
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("x"), 0o644)
	if err := checkWritable(filepath.Join(file, "sub")); err == nil {
		t.Fatal("a folder under a regular file was accepted")
	}
	// a folder this process may not write to (simulated)
	createTemp = func(string, string) (*os.File, error) { return nil, os.ErrPermission }
	defer func() { createTemp = os.CreateTemp }()
	err := checkWritable(dir)
	if err == nil || !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), fmt.Sprintf("uid %d", os.Getuid())) || !strings.Contains(err.Error(), "named volume") {
		t.Fatalf("the message does not say which folder, which user and what to do: %v", err)
	}
}

// The remedy printed must fit the actual cause.
func TestCheckWritableHintsMatchTheCause(t *testing.T) {
	defer func() { createTemp = os.CreateTemp }()
	dir := t.TempDir()
	fail := func(err error) error {
		createTemp = func(string, string) (*os.File, error) { return nil, &fs.PathError{Op: "open", Path: dir, Err: err} }
		return checkWritable(dir)
	}
	ro := fail(syscall.EROFS)
	if ro == nil || !strings.Contains(ro.Error(), "read-only filesystem") || !strings.Contains(ro.Error(), "mount a volume") || strings.Contains(ro.Error(), "chown") {
		t.Errorf("read-only root: %v", ro)
	}
	perm := fail(syscall.EACCES)
	if perm == nil || !strings.Contains(perm.Error(), "chown -R") || !strings.Contains(perm.Error(), "docker run --rm -v <volume>:/v alpine chown") ||
		!strings.Contains(perm.Error(), "make docker-up-host") || !strings.Contains(perm.Error(), fmt.Sprintf("uid %d", os.Getuid())) {
		t.Errorf("permission denied: %v", perm)
	}
	if strings.Contains(perm.Error(), "read-only filesystem") {
		t.Error("a permission problem was described as a read-only filesystem")
	}
	other := fail(syscall.ENOSPC)
	if other == nil || strings.Contains(other.Error(), "chown") || strings.Contains(other.Error(), "mount a volume") || !strings.Contains(other.Error(), dir) {
		t.Errorf("another failure must not get a remedy meant for something else: %v", other)
	}
}

func writeCfg(t *testing.T, data, pdfs string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "talos.json")
	os.WriteFile(p, []byte(fmt.Sprintf(`{"data_dir":%q,"pdf_dir":%q,"ui_dir":%q}`, data, pdfs, filepath.Join(t.TempDir(), "no-ui"))), 0o644)
	return p
}

// run() must perform the start-up check itself.
func TestStartUpChecksAreWired(t *testing.T) {
	defer func() { createTemp = os.CreateTemp }()
	data, pdfs := t.TempDir(), t.TempDir()
	cfg := writeCfg(t, data, pdfs)
	only := func(bad string) { // fail to write in exactly one folder
		createTemp = func(dir, pat string) (*os.File, error) {
			if dir == bad {
				return nil, os.ErrPermission
			}
			return os.CreateTemp(dir, pat)
		}
	}
	for _, cmd := range []string{"serve", "ingest"} {
		only(data)
		if err := run(cfg, []string{cmd}); err == nil || !strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), data) {
			t.Errorf("%s with an unwritable data folder: %v", cmd, err)
		}
	}
	// ask writes nothing: the check must not stop it
	only(data)
	if err := run(cfg, []string{"ask"}); err == nil || strings.Contains(err.Error(), "not writable") {
		t.Errorf("ask was stopped by the writability check: %v", err)
	}
	// unwritable PDF folder only warns; serve continues
	only(pdfs)
	err := run(cfg, []string{"serve"})
	if err == nil || strings.Contains(err.Error(), "not writable") || !strings.Contains(err.Error(), "UI is not built") {
		t.Errorf("serve with a read-only PDF folder: %v", err)
	}
}

func TestAllowedHostsAreBracketFree(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:8080"
	cfg.AllowedHosts = []string{"[fd00::5]", "Talos.LAN", "::1"}
	hosts, err := allowedHosts(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fd00::5", "Talos.LAN", "::1"} {
		if !slices.Contains(hosts, want) {
			t.Errorf("%q missing from %v", want, hosts)
		}
	}
	if slices.Contains(hosts, "[fd00::5]") {
		t.Errorf("a bracketed entry can never match the Host header: %v", hosts)
	}
}
