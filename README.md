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

## Docker

```bash
git clone … talos && git clone … gojiberry   # gojiberry (private) next to talos
cd talos
make docker-up                                # builds, then http://127.0.0.1:8080
```

It works on a fresh clone with no setup: the PDF library and the index live in named volumes (`talos-pdfs`,
`talos-data`) that the container's user owns, and you add PDFs with the UI's upload button. To keep them in
folders on the host instead, `make docker-up-host` (creates `./pdfs` and `./data`, and runs the container as you).
The image is small (~33 MB, distroless, non-root), runs with a read-only root filesystem and no capabilities, and
has a health check. If the data folders are not writable, talos says so at start-up instead of at the first upload.
Without make: `docker build --build-context gojiberry=../gojiberry -t talos:local .` then `docker compose up -d`.

- **Your data.** The library and the index live in the named volumes `talos_talos-pdfs` and `talos_talos-data`
  (compose project `talos`). `docker compose down` keeps them; **`docker compose down -v` deletes them**, including
  every uploaded PDF. Back up a volume with
  `docker run --rm -v talos_talos-pdfs:/v -v "$PWD":/b alpine tar czf /b/pdfs.tgz -C /v .`, and look inside with
  `docker run --rm -v talos_talos-pdfs:/v alpine ls -l /v`. Folders `./pdfs` and `./data` from an earlier
  bind-mount setup are not used by `make docker-up`: use `make docker-up-host` to keep using them (or copy their
  contents into the volumes). A PDF folder mounted read-only (`-v ./library:/pdfs:ro`) works for searching;
  uploads are then refused with a clear message.
- **Models.** By default it talks to an Ollama on the host: that Ollama must listen beyond loopback
  (`OLLAMA_HOST=0.0.0.0`). To run Ollama in a container as well:
  `OLLAMA_URL=http://ollama:11434 docker compose --profile ollama up -d --build`, then
  `docker compose exec ollama ollama pull qwen2.5:7b-instruct` (and `nomic-embed-text`). Any OpenAI-compatible
  server works too: mount your own config over `/etc/talos/talos.json` (start from `docker/talos.json`).
- **Settings without editing JSON** (they override the file; bad values are rejected at start-up):
  `TALOS_LLM_BASE_URL`, `TALOS_LLM_MODEL`, `TALOS_EMBED_BASE_URL`, `TALOS_EMBED_MODEL`, `TALOS_ALLOWED_HOSTS`
  (bare host names, no port), `TALOS_LISTEN` (leave it as `:8080` in a container: a loopback address inside the
  container makes the published port unreachable).
- **Who can reach it. This matters.** talos has no login. The compose file publishes the port on `127.0.0.1`
  only, and **that is the only thing protecting it**: `allowed_hosts` stops a web page in your browser from
  reaching it through DNS rebinding, but not a client that sets its own `Host` header. So
  `docker run -p 8080:8080 talos` (or editing the port to `8080:8080` / `0.0.0.0:8080:8080`) exposes an
  unauthenticated agent that can read your library, upload files and fetch URLs, to everyone who can reach the
  machine. Put an authenticating reverse proxy in front before publishing it anywhere else, and add the name
  you use to `TALOS_ALLOWED_HOSTS`.
- **Behind a TLS-intercepting proxy** (corporate): pass the proxy CA and address at build time only; `make
  docker-up` builds first, so the flags apply to it as well:
  `make docker-up DOCKER_BUILD_FLAGS="--secret id=cacert,src=ca.pem --build-arg HTTPS_PROXY=http://proxy:3128"`.
- **Pinned versions.** The Go build image is the `golang:1.26` series; the Ollama image in the compose file is
  pinned to a release. Update them deliberately.

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
- **Web access.** `fetch_url` refuses loopback, LAN, link-local, CGNAT and cloud-metadata addresses.
  Direct requests use a dialler that checks the address it actually connects to, with no exemptions, so
  redirects, DNS tricks and odd spellings of a host (`127.1`, `0x7f.1`, `１２７…`, `LOCALHOST.`) do not help.
  With `HTTP(S)_PROXY` set (and `NO_PROXY` applied per request), a proxied request goes through a transport
  that can dial only the proxy, after its target is validated: not `localhost`, not a non-public address in any
  spelling, and not a name that resolves here to one. That validation is **best-effort**: the proxy resolves
  the name again, a hostile DNS server can answer the two lookups differently, and a name only the proxy can
  resolve is passed on, so with a proxy what the LAN exposes depends on the proxy. `fetch_url` opens only URLs
  from your message or from search results, so a hostile page cannot make the model request an attacker URL
  carrying private text. `web.allow_private` lifts the address check; a SearXNG on localhost works without it.
  (A split-DNS or fake-IP resolver that answers with `198.18.0.0/15` or other private addresses for public
  names makes proxied fetches fail by design; set `allow_private` if that is your network.)
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
- Files in the PDF folder named `.upload-*` or `*.replaced~` are deleted when the server starts (they are an
  interrupted upload's leftovers); `*.recovered~` (and `*.recovered~2`, …) is never touched: it appears next to a PDF when
  an upload failed and putting the old file back failed too, and is the only copy of that file.
- A PDF with no text layer (a scan) or one that is corrupt is reported and skipped, and not retried until it
  changes; the rest are still indexed. An embedding-server outage is not held against the PDF: it is retried.

## Layout

`Dockerfile`, `docker-compose.yml`, `docker/` container setup · `config/` settings · `llm/` Ollama + OpenAI-compatible clients · `rag/` PDF chunking, embeddings, search ·
`web/` search & fetch · `agent/` tool loop and the team · `server/` API + static UI · `ui/` the xango app (`main.xg`).
