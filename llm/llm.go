// Package llm talks to local model servers over HTTP. Nothing here needs more than net/http.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/elaous/talos/config"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Chat generates text; Embedder turns text into vectors. Agents and the library depend only on these.
type Chat interface {
	Chat(ctx context.Context, model string, msgs []Message) (string, error)
}
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// NewChat and NewEmbedder pick the client for an endpoint's backend.
func NewChat(e config.Endpoint, temperature float64) (Chat, error) {
	switch e.Backend {
	case "ollama":
		return &client{e: e, temp: temperature, ollama: true}, nil
	case "openai":
		return &client{e: e, temp: temperature}, nil
	}
	return nil, fmt.Errorf("llm: unknown backend %q (want ollama or openai)", e.Backend)
}

func NewEmbedder(e config.Endpoint) (Embedder, error) {
	c, err := NewChat(e, 0)
	if err != nil {
		return nil, err
	}
	return c.(*client), nil
}

type client struct {
	e      config.Endpoint
	temp   float64
	ollama bool
}

var httpc = &http.Client{Timeout: 10 * time.Minute} // a big model's first token can be slow

func (c *client) post(ctx context.Context, path string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.e.BaseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.e.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.e.APIKey)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable (is the model server running?): %w", c.e.BaseURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s%s: %s: %s", c.e.BaseURL, path, resp.Status, strings.TrimSpace(string(data)))
	}
	return json.Unmarshal(data, out)
}

func (c *client) Chat(ctx context.Context, model string, msgs []Message) (string, error) {
	if c.ollama {
		var r struct {
			Message Message `json:"message"`
		}
		err := c.post(ctx, "/api/chat", map[string]any{
			"model": model, "messages": msgs, "stream": false,
			"options": map[string]any{"temperature": c.temp},
		}, &r)
		return r.Message.Content, err
	}
	var r struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	err := c.post(ctx, "/v1/chat/completions", map[string]any{
		"model": model, "messages": msgs, "temperature": c.temp,
	}, &r)
	if err == nil && len(r.Choices) == 0 {
		err = fmt.Errorf("llm: empty response")
	}
	if err != nil {
		return "", err
	}
	return r.Choices[0].Message.Content, nil
}

func (c *client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if c.ollama {
		var r struct {
			Embeddings [][]float32 `json:"embeddings"`
		}
		if err := c.post(ctx, "/api/embed", map[string]any{"model": c.e.Model, "input": texts}, &r); err != nil {
			return nil, err
		}
		if len(r.Embeddings) != len(texts) {
			return nil, fmt.Errorf("llm: asked for %d embeddings, got %d", len(texts), len(r.Embeddings))
		}
		return r.Embeddings, nil
	}
	var r struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/v1/embeddings", map[string]any{"model": c.e.Model, "input": texts}, &r); err != nil {
		return nil, err
	}
	if len(r.Data) != len(texts) {
		return nil, fmt.Errorf("llm: asked for %d embeddings, got %d", len(texts), len(r.Data))
	}
	out := make([][]float32, len(texts))
	for _, d := range r.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("llm: embedding index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}
