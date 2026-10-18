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
