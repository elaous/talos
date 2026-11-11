// Command talos runs the server, indexer, one-shot ask or healthcheck.
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
	"strings"
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
	if len(args) > 0 && args[0] == "healthcheck" { // no model clients or index
		u, err := healthURL(cfg.Listen)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return checkHealth(ctx, u)
	}
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd { // fail at start-up, not at first upload
	case "serve":
		if err := checkWritable(cfg.DataDir); err != nil { // the index must be written
			return err
		}
		// Read-only PDF folder still serves searches.
		if err := checkWritable(cfg.PDFDir); err != nil {
			log.Printf("warning: uploads will not work: %v", err)
		}
	case "ingest":
		if err := checkWritable(cfg.DataDir); err != nil {
			return err
		}
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
		s.Wait() // in-progress ingest saves finished files on exit
		return nil
	}
	return fmt.Errorf("unknown command %q (serve, ingest, ask, healthcheck)", cmd)
}

// allowedHosts lists Host names the server answers to, besides loopback.
// Wildcard listen needs allowed_hosts: without it, DNS rebinding works.
func allowedHosts(cfg config.Config) ([]string, error) {
	var hosts []string
	for _, h := range cfg.AllowedHosts {
		hosts = append(hosts, strings.Trim(h, "[]")) // Host names are compared without brackets
	}
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
