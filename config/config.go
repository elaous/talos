// Package config loads talos.json.
package config

import (
	"encoding/json"
	"os"
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
	// Allowing a non-loopback Listen exposes the library and the agents to the network, with no login.
	DataDir string `json:"data_dir"`
	PDFDir  string `json:"pdf_dir"`

	LLM struct {
		Endpoint
		Temperature float64           `json:"temperature"`
		Models      map[string]string `json:"models"` // per agent: supervisor, pdf_researcher, web_researcher, writer
	} `json:"llm"`
	Embed Endpoint `json:"embed"`

	ChunkChars   int `json:"chunk_chars"`
	ChunkOverlap int `json:"chunk_overlap"`
	MaxSteps     int `json:"max_steps"`

	Web struct {
		SearxngURL   string `json:"searxng_url"`   // empty: DuckDuckGo
		AllowPrivate bool   `json:"allow_private"` // let fetch_url reach localhost/LAN addresses
	} `json:"web"`
	UIDir string `json:"ui_dir"`
}

func Default() Config {
	var c Config
	c.Listen = "127.0.0.1:8080"
	c.DataDir, c.PDFDir, c.UIDir = "data", "pdfs", "ui/dist"
	c.LLM.Backend, c.LLM.BaseURL = "ollama", "http://localhost:11434"
	c.LLM.Model, c.LLM.Temperature = "qwen2.5:7b-instruct", 0.2
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
	return c, json.Unmarshal(b, &c)
}

// ModelFor is the model an agent runs on: its own entry, else the default.
func (c Config) ModelFor(agent string) string {
	if m := c.LLM.Models[agent]; m != "" {
		return m
	}
	return c.LLM.Model
}
