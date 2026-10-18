package main

import (
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
