package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDefaultsAndValidation(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil || c.Listen != "127.0.0.1:8080" || c.LLM.NumCtx == 0 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	write := func(s string) string {
		p := filepath.Join(t.TempDir(), "t.json")
		os.WriteFile(p, []byte(s), 0o644)
		return p
	}
	if c, err := Load(write(`{"llm":{"model":"x","models":{"writer":"w"}}}`)); err != nil || c.ModelFor("writer") != "w" || c.ModelFor("pdf_researcher") != "x" {
		t.Fatalf("overlay: %v", err)
	}
	for in, want := range map[string]string{
		`{"chunk_chars":1200,"chunk_overlap":1100}`: "chunk_overlap",
		`{"chunk_chars":50}`:                        "chunk_chars",
		`{"max_steps":0}`:                           "max_steps",
		`{"max_steps":500}`:                         "max_steps",
		`{not json`:                                 "",
	} {
		if _, err := Load(write(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", in, err)
		}
	}
}

func TestApplyEnv(t *testing.T) {
	env := map[string]string{
		"TALOS_LISTEN": ":8080", "TALOS_ALLOWED_HOSTS": " talos.lan , ,box.local", "TALOS_LLM_BASE_URL": "http://ollama:11434",
		"TALOS_LLM_MODEL": "m1", "TALOS_EMBED_BASE_URL": "http://emb:1", "TALOS_EMBED_MODEL": "e1",
	}
	c, err := LoadEnv(filepath.Join(t.TempDir(), "missing.json"), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.LLM.BaseURL != "http://ollama:11434" || c.LLM.Model != "m1" || c.Embed.BaseURL != "http://emb:1" || c.Embed.Model != "e1" {
		t.Fatalf("%+v", c)
	}
	if strings.Join(c.AllowedHosts, ",") != "talos.lan,box.local" {
		t.Fatalf("allowed hosts: %q", c.AllowedHosts)
	}
	// unset and empty variables change nothing, and the file's values are the base
	p := filepath.Join(t.TempDir(), "t.json")
	os.WriteFile(p, []byte(`{"llm":{"model":"from-file","base_url":"http://file:1"}}`), 0o644)
	c, _ = LoadEnv(p, func(k string) string { return map[string]string{"TALOS_LLM_MODEL": ""}[k] })
	if c.LLM.Model != "from-file" || c.LLM.BaseURL != "http://file:1" {
		t.Fatalf("%+v", c.LLM)
	}
	c, _ = LoadEnv(p, func(k string) string { return map[string]string{"TALOS_LLM_MODEL": "env"}[k] })
	if c.LLM.Model != "env" || c.LLM.BaseURL != "http://file:1" {
		t.Fatalf("the environment did not override only what it names: %+v", c.LLM)
	}
}

func TestValidateListenBaseURLsAndAllowedHosts(t *testing.T) {
	ok := func(mod func(*Config)) error {
		c := Default()
		mod(&c)
		return c.Validate()
	}
	if err := ok(func(c *Config) {}); err != nil {
		t.Fatalf("the defaults must be valid: %v", err)
	}
	for name, mod := range map[string]func(*Config){
		"listen without a port":        func(c *Config) { c.Listen = "localhost" },
		"listen nonsense":              func(c *Config) { c.Listen = "nonsense" },
		"llm url without a scheme":     func(c *Config) { c.LLM.BaseURL = "ollama:11434" },
		"llm url empty":                func(c *Config) { c.LLM.BaseURL = "" },
		"embed url ftp":                func(c *Config) { c.Embed.BaseURL = "ftp://x" },
		"embed url without a host":     func(c *Config) { c.Embed.BaseURL = "http://" },
		"allowed host with a port":     func(c *Config) { c.AllowedHosts = []string{"talos.lan:8080"} },
		"allowed host with a scheme":   func(c *Config) { c.AllowedHosts = []string{"http://talos.lan"} },
		"allowed host with a path":     func(c *Config) { c.AllowedHosts = []string{"talos.lan/x"} },
		"allowed host empty":           func(c *Config) { c.AllowedHosts = []string{""} },
		"allowed host with whitespace": func(c *Config) { c.AllowedHosts = []string{"talos lan"} },
	} {
		if err := ok(mod); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	for name, mod := range map[string]func(*Config){
		"wildcard listen": func(c *Config) { c.Listen = ":8080" },
		"ipv6 listen":     func(c *Config) { c.Listen = "[::1]:8080" },
		"https base url":  func(c *Config) { c.LLM.BaseURL = "https://models.lan/v1" },
		"host names":      func(c *Config) { c.AllowedHosts = []string{"talos.lan", "localhost", "192.168.1.5", "::1", "[::1]"} },
	} {
		if err := ok(mod); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
}

// An environment value that makes the configuration invalid is an error at start-up, not a surprise later.
func TestLoadEnvRejectsBadValues(t *testing.T) {
	for k, v := range map[string]string{
		"TALOS_LISTEN": "nonsense", "TALOS_LLM_BASE_URL": "ollama:11434", "TALOS_EMBED_BASE_URL": "x", "TALOS_ALLOWED_HOSTS": "talos.lan:8080",
	} {
		_, err := LoadEnv(filepath.Join(t.TempDir(), "missing.json"), func(name string) string {
			if name == k {
				return v
			}
			return ""
		})
		if err == nil {
			t.Errorf("%s=%q was accepted", k, v)
		}
	}
}
