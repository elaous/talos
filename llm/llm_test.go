package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elaous/talos/config"
)

func TestOllamaChatAndEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		switch r.URL.Path {
		case "/api/chat":
			if in["model"] != "m" || in["stream"] != false {
				t.Errorf("chat request %v", in)
			}
			w.Write([]byte(`{"message":{"role":"assistant","content":"hi"}}`))
		case "/api/embed":
			w.Write([]byte(`{"embeddings":[[1,0],[0,1]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := config.Endpoint{Backend: "ollama", BaseURL: srv.URL, Model: "emb"}
	c, _ := NewChat(e, 0.1)
	if got, err := c.Chat(context.Background(), "m", []Message{{"user", "yo"}}); err != nil || got != "hi" {
		t.Fatalf("%q %v", got, err)
	}
	em, _ := NewEmbedder(e)
	if v, err := em.Embed(context.Background(), []string{"a", "b"}); err != nil || len(v) != 2 || v[1][1] != 1 {
		t.Fatalf("%v %v", v, err)
	}
}

func TestOpenAICompatibleOrdersEmbeddingsByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
		case "/v1/embeddings":
			w.Write([]byte(`{"data":[{"index":1,"embedding":[2]},{"index":0,"embedding":[1]}]}`))
		}
	}))
	defer srv.Close()
	e := config.Endpoint{Backend: "openai", BaseURL: srv.URL, APIKey: "k", Model: "e"}
	c, _ := NewChat(e, 0)
	if got, err := c.Chat(context.Background(), "m", nil); err != nil || got != "ok" {
		t.Fatalf("%q %v", got, err)
	}
	em, _ := NewEmbedder(e)
	if v, err := em.Embed(context.Background(), []string{"a", "b"}); err != nil || v[0][0] != 1 || v[1][0] != 2 {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := NewChat(config.Endpoint{Backend: "x"}, 0); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestUnreachableServerSaysSo(t *testing.T) {
	c, _ := NewChat(config.Endpoint{Backend: "ollama", BaseURL: "http://127.0.0.1:1"}, 0)
	if _, err := c.Chat(context.Background(), "m", nil); err == nil {
		t.Fatal("no error")
	}
}
