// Command talos is a local multi-agent RAG assistant: a supervisor and specialist agents over your PDF
// library and the web, running on models you host (Ollama, or any OpenAI-compatible local server).
//
//	talos [-c talos.json] serve            web UI + API (default)
//	talos [-c talos.json] ingest           index the PDFs
//	talos [-c talos.json] ask "question"   one-shot, steps on stderr
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/elaous/talos/agent"
	"github.com/elaous/talos/config"
	"github.com/elaous/talos/llm"
	"github.com/elaous/talos/rag"
	"github.com/elaous/talos/server"
	"github.com/elaous/talos/web"
)

func main() {
	cfgPath := flag.String("c", "talos.json", "config file (optional; defaults run Ollama on localhost)")
	flag.Parse()
	if err := run(*cfgPath, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "talos:", err)
		os.Exit(1)
	}
}

func run(cfgPath string, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	chat, err := llm.NewChat(cfg.LLM.Endpoint, cfg.LLM.Temperature)
	if err != nil {
		return err
	}
	emb, err := llm.NewEmbedder(cfg.Embed)
	if err != nil {
		return err
	}
	lib, err := rag.Open(filepath.Join(cfg.DataDir, "index.gob"), emb, cfg.Embed.Backend+":"+cfg.Embed.Model, cfg.ChunkChars, cfg.ChunkOverlap)
	if err != nil {
		return err
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	switch cmd {
	case "ingest":
		n, err := lib.IngestDir(ctx, cfg.PDFDir)
		fmt.Printf("indexed %d new passages from %d files\n", n, len(lib.Sources()))
		return err
	case "ask":
		if len(args) < 2 {
			return fmt.Errorf(`usage: talos ask "question"`)
		}
		team := agent.NewTeam(cfg, chat, lib, web.New(cfg.Web.SearxngURL, cfg.Web.AllowPrivate))
		ans, err := team.Ask(ctx, args[1], nil, func(e agent.Event) {
			fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", e.Agent, e.Kind, e.Text)
		})
		fmt.Println(ans)
		return err
	case "serve":
		ui := os.DirFS(cfg.UIDir)
		if _, err := fs.Stat(ui, "main.wasm"); err != nil {
			return fmt.Errorf("the UI is not built (%s/main.wasm missing): run `make ui`", cfg.UIDir)
		}
		s := &server.Server{
			Team:   agent.NewTeam(cfg, chat, lib, web.New(cfg.Web.SearxngURL, cfg.Web.AllowPrivate)),
			Lib:    lib,
			PDFDir: cfg.PDFDir,
			UI:     ui,
		}
		srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
		go func() { <-ctx.Done(); _ = srv.Shutdown(context.Background()) }()
		log.Printf("talos on http://%s  (%d PDFs indexed)", cfg.Listen, len(lib.Sources()))
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		return nil
	}
	return fmt.Errorf("unknown command %q (serve, ingest, ask)", cmd)
}
