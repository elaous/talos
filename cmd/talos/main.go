// Command talos is a local multi-agent RAG assistant: a supervisor and specialist agents over your PDF
// library and the web, running on models you host (Ollama, or any OpenAI-compatible local server).
//
//	talos [-c talos.json] serve            web UI + API (default)
//	talos [-c talos.json] ingest           index the PDFs
//	talos [-c talos.json] ask "question"   one-shot, steps on stderr
//	talos [-c talos.json] healthcheck      exit 0 if the server answers (for container health checks)
//
// TALOS_* environment variables override a few settings; see config.Config.ApplyEnv.
package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
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
	cfg, err := config.LoadEnv(cfgPath, os.Getenv)
	if err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	if len(args) > 0 && args[0] == "healthcheck" { // light: no model clients, no index
		u, err := healthURL(cfg.Listen)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return checkHealth(ctx, u)
	}
	chat, err := llm.NewChat(cfg.LLM.Endpoint, cfg.LLM.Temperature, cfg.LLM.NumCtx)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
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
		hosts, err := allowedHosts(cfg)
		if err != nil {
			return err
		}
		s := &server.Server{
			Team:         agent.NewTeam(cfg, chat, lib, web.New(cfg.Web.SearxngURL, cfg.Web.AllowPrivate)),
			Lib:          lib,
			PDFDir:       cfg.PDFDir,
			UI:           ui,
			AllowedHosts: hosts,
			Ctx:          ctx,
		}
		srv := &http.Server{Addr: cfg.Listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
		shutdown := make(chan struct{})
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(sc)
			close(shutdown)
		}()
		log.Printf("talos on http://%s  (%d PDFs indexed)", cfg.Listen, len(lib.Sources()))
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			return err
		}
		<-shutdown
		s.Wait() // an ingest in progress saves what it finished before the process exits
		return nil
	}
	return fmt.Errorf("unknown command %q (serve, ingest, ask)", cmd)
}

// allowedHosts is the set of Host names the server answers to: the listen host, loopback and any configured
// names. A wildcard listen address (0.0.0.0, ::, or just a port) needs allowed_hosts: without a list the Host
// check would be off, and with no login that is what makes DNS rebinding work.
func allowedHosts(cfg config.Config) ([]string, error) {
	hosts := append([]string(nil), cfg.AllowedHosts...)
	h, _, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("listen %q: %w", cfg.Listen, err)
	}
	switch h {
	case "", "0.0.0.0", "::":
		if len(hosts) == 0 {
			return nil, fmt.Errorf("listen %q is open to the network and talos has no login: set allowed_hosts to the names you reach it by", cfg.Listen)
		}
	default:
		hosts = append(hosts, h)
	}
	return append(hosts, "localhost", "127.0.0.1", "::1"), nil
}
