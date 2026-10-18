# talos

A local multi-agent RAG assistant, written in Go. A **supervisor** agent delegates to specialists — a
**PDF researcher** (semantic search over your PDF library), a **web researcher** (search + page reading) and a
**writer** — all running on models you host yourself. The UI is a [gojiberry](../gojiberry) (xango) WASM app.

No Python: the model servers speak HTTP, vectors live in a gob file, PDFs are read with gojiberry's `pdftext`.

```
browser (xango WASM) ──HTTP──> talos server ──> supervisor ─┬─> pdf_researcher ──> PDF index (embeddings)
                                                            ├─> web_researcher ──> DuckDuckGo / SearXNG, fetch_url
                                                            └─> writer
                                       every agent ──> Ollama  or  any OpenAI-compatible local server
```

## Run

Needs Go 1.26+, a sibling `../gojiberry` checkout (private module, see `go.mod`'s `replace`), and Ollama.

```bash
ollama pull qwen2.5:7b-instruct && ollama pull nomic-embed-text
mkdir -p pdfs && cp ~/papers/*.pdf pdfs/
make run                      # builds the WASM UI + server, serves http://127.0.0.1:8080
# or without the UI (stop `serve` first, see "One process per index"):
./bin/talos ingest && ./bin/talos ask "What do my PDFs say about X, and what's new on the web?"
```

PDFs can also be uploaded in the UI. Indexing is incremental (unchanged files are skipped, removed files dropped).

## Models: Ollama and Hugging Face

Copy `talos.example.json` to `talos.json`. Each agent can use its own model (`llm.models`).

- **Ollama**: any tag, including Hugging Face GGUFs: `hf.co/bartowski/Qwen2.5-7B-Instruct-GGUF:Q4_K_M`.
- **Any other local server** (llama.cpp `llama-server`, vLLM, TGI, LM Studio): set `"backend": "openai"` and its
  `base_url` — that serves any Hugging Face model. Same for `embed` (e.g. `BAAI/bge-m3` behind such a server).

Changing the embedding model or the chunk settings re-indexes the library automatically.
Use a 7B+ model for the agents: the tool protocol is plain JSON in the reply, which very small models get wrong.

## Tests

`make test` — all packages (run with `-race` in CI), plus a `GOOS=js GOARCH=wasm` vet of the UI. The LLM is
scripted in tests, so no model is needed.

## Safety notes

- **No login.** The server listens on `127.0.0.1` by default. It refuses requests whose `Host` is not the listen
  host, a loopback name or in `allowed_hosts` (DNS rebinding), cross-site requests (`Sec-Fetch-Site`,
  `Origin`), and non-JSON asks. Listening on all interfaces (`0.0.0.0`, `::`, `:8080`) needs `allowed_hosts`
  (the names you reach it by; talos refuses to start otherwise). It still has no login: put it behind
  something that authenticates if others can reach it.
- **Web access.** `fetch_url` refuses loopback, LAN, link-local, CGNAT and cloud-metadata addresses. Without
  a proxy this is checked on the address actually connected to, so redirects and DNS tricks do not help. With
  `HTTP(S)_PROXY` set the proxy is dialled instead, so the target is resolved and checked first, and that is
  **best-effort only**: the proxy resolves the name again, and a hostile DNS server can answer the two
  lookups differently, so what the LAN exposes through the proxy depends on the proxy. (A name only the
  proxy can resolve is passed on to it.) The proxy's own address is the one exempt connection, and it is
  never a fetch target itself, not directly and not through a redirect. `fetch_url` opens only URLs from
  your message or from search results, so a hostile page cannot make the model request an attacker URL
  carrying private text. `web.allow_private` lifts the address check; a SearXNG on localhost works without it.
- **Search queries leave the machine.** A model steered by hostile text could put private PDF text into a
  `web_search` query. That is inherent to giving an agent both your documents and web search. Use a local
  SearXNG, or set `"web": {"disabled": true}` to drop the web researcher entirely so nothing leaves the machine.
- **Prompt injection.** Tool results are fenced as untrusted data, but small models can still be steered by
  hostile text. The agents have no write tools, so the risk is wrong answers, not changed files.
- **Limits.** 4 questions at once, 100 jobs kept, history and question sizes capped, agent context bounded;
  `llm.num_ctx` (default 16384) sets Ollama's context window, whose own default is small.
- **Shutdown.** Ctrl-C/SIGTERM stops indexing after the current file and saves what was finished; a long
  run also saves about every 15 seconds.
- **One process per index.** Run `ingest` only while `serve` is stopped (or use the UI's re-index): two
  processes would overwrite each other's saved index.
- A PDF with no text layer (a scan) or one that is corrupt is reported and skipped, and not retried until it
  changes; the rest are still indexed. An embedding-server outage is not held against the PDF: it is retried.

## Layout

`config/` settings · `llm/` Ollama + OpenAI-compatible clients · `rag/` PDF chunking, embeddings, search ·
`web/` search & fetch · `agent/` tool loop and the team · `server/` API + static UI · `ui/` the xango app (`main.xg`).
