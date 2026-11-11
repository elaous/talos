// Package config loads talos.json.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
)

// Endpoint is one model server. Backend is "ollama" or "openai".
type Endpoint struct {
	Backend string `json:"backend"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
	Model   string `json:"model,omitempty"`
}

type Config struct {
	Listen string `json:"listen"`
	// Non-loopback Listen requires AllowedHosts; there is no login.
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
		Disabled     bool   `json:"disabled"`      // no web researcher; nothing leaves the machine
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

// Missing file yields defaults without error.
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

func LoadEnv(path string, getenv func(string) string) (Config, error) {
	c, err := Load(path)
	if err != nil {
		return c, err
	}
	c.ApplyEnv(getenv)
	if err := c.Validate(); err != nil {
		return c, fmt.Errorf("invalid settings (the config file with the TALOS_* environment variables applied): %w", err)
	}
	return c, nil
}

// Empty variables are ignored; set ones override the file.
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

// Validate rejects settings that would misbehave.
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
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("listen %q must be host:port (for example 127.0.0.1:8080 or :8080): %w", c.Listen, err)
	}
	for name, e := range map[string]Endpoint{"llm": c.LLM.Endpoint, "embed": c.Embed} {
		u, err := url.Parse(e.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s base_url %q must be a URL such as http://localhost:11434", name, e.BaseURL)
		}
	}
	for _, h := range c.AllowedHosts {
		if h == "" || strings.ContainsAny(h, "/ \t") {
			return fmt.Errorf("allowed_hosts entry %q must be a bare host name (no scheme, port or path)", h)
		}
		if _, err := netip.ParseAddr(strings.Trim(h, "[]")); err != nil && strings.Contains(h, ":") {
			return fmt.Errorf("allowed_hosts entry %q must be a bare host name without a port: the Host header's port is ignored", h)
		}
	}
	return nil
}

// ModelFor returns the agent's model, else the default.
func (c Config) ModelFor(agent string) string {
	if m := c.LLM.Models[agent]; m != "" {
		return m
	}
	return c.LLM.Model
}
