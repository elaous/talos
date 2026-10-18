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
