package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
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

// The config baked into the image must stay loadable and servable: a wildcard listen needs allowed_hosts, and the
// paths must be the ones the Dockerfile creates.
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
	df, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/app/ui", "/data", "/pdfs", "EXPOSE 8080", "HEALTHCHECK", `"healthcheck"`} {
		if !strings.Contains(string(df), want) {
			t.Errorf("Dockerfile lacks %q", want)
		}
	}
	// every TALOS_* variable the compose file sets is one the code reads
	dc, _ := os.ReadFile("../../docker-compose.yml")
	for _, v := range []string{"TALOS_LLM_BASE_URL", "TALOS_EMBED_BASE_URL"} {
		if !strings.Contains(string(dc), v) {
			t.Errorf("docker-compose.yml lacks %s", v)
		}
		env := map[string]string{v: "http://x:1"}
		c := config.Default()
		c.ApplyEnv(func(k string) string { return env[k] })
		if c.LLM.BaseURL != "http://x:1" && c.Embed.BaseURL != "http://x:1" {
			t.Errorf("%s is set in compose but not read by ApplyEnv", v)
		}
	}
}
