// Package config loads talos.json.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Endpoint is one model server. Backend "ollama" speaks Ollama's native API; "openai" speaks the
// OpenAI-compatible API that llama.cpp (llama-server), vLLM, TGI, LM Studio and others expose, which is how
// any Hugging Face model runs locally outside Ollama.
type Endpoint struct {
	Backend string `json:"backend"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

type Config struct {
	Listen string `json:"listen"`
	// A non-loopback Listen exposes the library and the agents to the network, with no login. Requests whose
	// Host header is not the listen host, a loopback name or in AllowedHosts are refused (DNS rebinding).
	AllowedHosts []string `json:"allowed_hosts"`
	DataDir      string   `json:"data_dir"`
	PDFDir       string   `json:"pdf_dir"`

	LLM struct {
		Endpoint
		Temperature float64           `json:"temperature"`
		NumCtx      int               `json:"num_ctx"` // Ollama context window; its own default is small
		Models      map[string]string `json:"models"`  // per agent: supervisor, pdf_researcher, web_researcher, writer
	} `json:"llm"`
	Embed Endpoint `json:"embed"`

	ChunkChars   int `json:"chunk_chars"`
	ChunkOverlap int `json:"chunk_overlap"`
	MaxSteps     int `json:"max_steps"`

	Web struct {
		SearxngURL   string `json:"searxng_url"`   // empty: DuckDuckGo
		Disabled     bool   `json:"disabled"`      // no web researcher: nothing leaves the machine, not even search queries
		AllowPrivate bool   `json:"allow_private"` // let fetch_url reach localhost/LAN addresses (and use HTTP(S)_PROXY)
	} `json:"web"`
	UIDir string `json:"ui_dir"`
}

func Default() Config {
	var c Config
	c.Listen = "127.0.0.1:8080"
	c.DataDir, c.PDFDir, c.UIDir = "data", "pdfs", "ui/dist"
	c.LLM.Backend, c.LLM.BaseURL = "ollama", "http://localhost:11434"
	c.LLM.Model, c.LLM.Temperature, c.LLM.NumCtx = "qwen2.5:7b-instruct", 0.2, 16384
	c.Embed = Endpoint{Backend: "ollama", BaseURL: "http://localhost:11434", Model: "nomic-embed-text"}
	c.ChunkChars, c.ChunkOverlap, c.MaxSteps = 1200, 200, 6
	return c
}

// Load reads path over the defaults. A missing file is not an error: the defaults run.
func Load(path string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, err
	}
	return c, c.Validate()
}

// LoadEnv is Load followed by the TALOS_* environment overrides (see ApplyEnv).
func LoadEnv(path string, getenv func(string) string) (Config, error) {
	c, err := Load(path)
	if err != nil {
		return c, err
	}
	c.ApplyEnv(getenv)
	return c, c.Validate()
}

// ApplyEnv overlays the few settings that differ between machines, so a container needs no edited JSON:
//
//	TALOS_LISTEN           listen address, e.g. ":8080"
//	TALOS_ALLOWED_HOSTS    comma-separated Host names, in addition to the loopback ones
//	TALOS_LLM_BASE_URL     chat model server, e.g. "http://ollama:11434"
//	TALOS_LLM_MODEL        default chat model
//	TALOS_EMBED_BASE_URL   embedding model server
//	TALOS_EMBED_MODEL      embedding model
//
// An unset or empty variable changes nothing.
func (c *Config) ApplyEnv(getenv func(string) string) {
	if v := getenv("TALOS_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := getenv("TALOS_ALLOWED_HOSTS"); v != "" {
		for _, h := range strings.Split(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				c.AllowedHosts = append(c.AllowedHosts, h)
			}
		}
	}
	if v := getenv("TALOS_LLM_BASE_URL"); v != "" {
		c.LLM.BaseURL = v
	}
	if v := getenv("TALOS_LLM_MODEL"); v != "" {
		c.LLM.Model = v
	}
	if v := getenv("TALOS_EMBED_BASE_URL"); v != "" {
		c.Embed.BaseURL = v
	}
	if v := getenv("TALOS_EMBED_MODEL"); v != "" {
		c.Embed.Model = v
	}
}

// Validate rejects settings that would misbehave rather than fail clearly.
func (c Config) Validate() error {
	if c.ChunkChars < 200 {
		return fmt.Errorf("chunk_chars must be at least 200, got %d", c.ChunkChars)
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap > c.ChunkChars/2 {
		return fmt.Errorf("chunk_overlap must be between 0 and half of chunk_chars (%d), got %d", c.ChunkChars/2, c.ChunkOverlap)
	}
	if c.MaxSteps < 1 || c.MaxSteps > 20 {
		return fmt.Errorf("max_steps must be between 1 and 20, got %d", c.MaxSteps)
	}
	return nil
}

// ModelFor is the model an agent runs on: its own entry, else the default.
func (c Config) ModelFor(agent string) string {
	if m := c.LLM.Models[agent]; m != "" {
		return m
	}
	return c.LLM.Model
}
