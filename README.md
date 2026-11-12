# talos

A local multi-agent RAG assistant, written in Go. A **supervisor** agent delegates to specialists — a
**PDF researcher** (semantic search over your PDF library), a **web researcher** (search + page reading) and a
**writer** — all running on models you host yourself. The UI is a WASM app built with gojiberry and xango (see
[Dependencies on gojiberry and xango](#dependencies-on-gojiberry-and-xango)).

No Python: the model servers speak HTTP, vectors live in a gob file, PDFs are read with gojiberry's `pdftext`.

```
browser (xango WASM) ──HTTP──> talos server ──> supervisor ─┬─> pdf_researcher ──> PDF index (embeddings)
                                                            ├─> web_researcher ──> DuckDuckGo / SearXNG, fetch_url
                                                            └─> writer
                                       every agent ──> Ollama  or  any OpenAI-compatible local server
```

## Requirements

- Go 1.26 or newer.
- A model server: [Ollama](https://ollama.com) or any OpenAI-compatible server. Models are not bundled.
- Docker 23+ with BuildKit and Compose, only for the container setup.

## Dependencies on gojiberry and xango

talos imports the public modules [`github.com/elaous/gojiberry`](https://github.com/elaous/gojiberry) and
[`github.com/elaous/xango`](https://github.com/elaous/xango) as normal Go modules, so a fresh clone builds with no
sibling checkout. `go.mod` pins gojiberry to commit `e1989ac1491c`
(`v0.0.0-20261112103843-e1989ac1491c`) and xango to `v0.1.0`.

- The UI generator and runtime come from xango: `go tool xgc ui` (`make generate`) runs the `xgc` declared as a `tool`
  directive in `go.mod`, and `ui/main.xg` imports `github.com/elaous/xango` and `github.com/elaous/xango/domrt`.
- To bump xango, run `go get github.com/elaous/xango@<tag>`, then `go mod tidy`, `make generate` and `make test`;
  commit `go.mod`, `go.sum` and the regenerated `ui/main.xg.go`. Run `make ui` only to rebuild the distribution.
- To bump gojiberry, run `go get github.com/elaous/gojiberry@<commit-or-tag>` and the same follow-up steps.
  Dependabot proposes updates weekly.
- gojiberry and xango are licensed under AGPL-3.0 (see [Licence](#licence)).

## Run

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
git clone https://github.com/elaous/talos.git
cd talos
make docker-up                                # builds, then http://127.0.0.1:8080
```

It works on a fresh clone with no setup: the PDF library and the index live in named volumes (`talos-pdfs`,
`talos-data`) that the container's user owns, and you add PDFs with the UI's upload button. To keep them in
folders on the host instead, `make docker-up-host` (creates `./pdfs` and `./data`, and runs the container as you).
The image is small (~33 MB, distroless, non-root), runs with a read-only root filesystem and no capabilities, and
has a health check. If the data folders are not writable, talos says so at start-up instead of at the first upload.
Without make: `docker build -t talos:local .` then `docker compose up -d`.

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
- **Behind a TLS-intercepting proxy** (corporate): the build downloads Go modules, so it needs your proxy CA and
  address. Both are build-time only and optional. The CA is passed as the BuildKit secret `cacert` (a PEM file); the
  Dockerfile points `SSL_CERT_FILE` at it for `go mod download` and the build when the secret is present, and
  the secret is not stored in the image. The proxy is the usual `HTTPS_PROXY` build argument:

  ```bash
  docker build \
    --secret id=cacert,src=/path/to/ca.pem \
    --build-arg HTTPS_PROXY=http://proxy:3128 -t talos:local .
  ```

  With make, `docker-up` builds first, so the same flags apply to it:
  `make docker-up DOCKER_BUILD_FLAGS="--secret id=cacert,src=ca.pem --build-arg HTTPS_PROXY=http://proxy:3128"`.
  The running container does not use this CA: for web access through a proxy at run time, see "Web access" below.
- **Pinned versions.** The Go build image is the `golang:1.26` series; the Ollama image in the compose file is
  pinned to a release. Update them deliberately.

## Models: Ollama and Hugging Face

Copy `talos.example.json` to `talos.json`. Each agent can use its own model (`llm.models`).

- **Ollama**: any tag, including Hugging Face GGUFs: `hf.co/bartowski/Qwen2.5-7B-Instruct-GGUF:Q4_K_M`.
- **Any other local server** (llama.cpp `llama-server`, vLLM, TGI, LM Studio): set `"backend": "openai"` and its
  `base_url` — that serves any Hugging Face model. Same for `embed` (e.g. `BAAI/bge-m3` behind such a server).

Changing the embedding model or the chunk settings re-indexes the library automatically.
Use a 7B+ model for the agents: the tool protocol is plain JSON in the reply, which very small models get wrong.

## Configuration

Copy `talos.example.json` to `talos.json` (git-ignored) and edit it; pass another path with `-c`. In the container,
`docker/talos.json` is used, and the `TALOS_*` variables listed under [Docker](#docker) override the file.

## Tests

```bash
make test                      # go vet + go test for all packages, plus a GOOS=js GOARCH=wasm vet of ./ui
scripts/targeted-tests.sh      # only the packages affected by your diff against origin/main
make lint                      # needs a local golangci-lint v2.15.0 (config in .golangci.yml)
```

The LLM is scripted in tests, so no model is needed.

## Continuous integration

Workflows in `.github/workflows/`:

- `pr.yml`: on pull requests, runs `golangci-lint` v2.15.0 through `golangci-lint-action` (the same checks as `make lint`) and `scripts/targeted-tests.sh`, which vets and tests the packages touched by the
  change plus the packages that import them. It runs the full `make test` instead when `go.mod`, `go.sum`,
  `Makefile`, `Dockerfile` or a workflow file changes.
- `nightly.yml`: runs the full `make test` every night and on demand.
- `security.yml`: CodeQL (Go), `govulncheck` and `gosec` (report-only, uploaded as code scanning alerts) on pull
  requests, pushes to `main` and weekly.
- `dependabot.yml`: weekly updates for Go modules, Docker and GitHub Actions.

Third-party actions are referenced by major version tag, not by commit SHA.

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

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md). Report vulnerabilities
privately as described in [SECURITY.md](SECURITY.md).

## Licence

talos's own source is released under the [MIT licence](LICENSE). It links the
[`gojiberry`](https://github.com/elaous/gojiberry) and [`xango`](https://github.com/elaous/xango) modules, which are
each licensed under the GNU AGPL-3.0. This is a summary, not legal advice.

- A binary or container image that includes gojiberry or xango (every build of talos does) is a combined work. It MUST
  be conveyed under the AGPL-3.0, including its Corresponding Source.
- Running a modified version as a network service MUST offer that source to its users (AGPL-3.0 section 13).
- The NOTICE files of gojiberry and xango and the licence notices MUST be kept. The credit "Made with the Gojiberry
  project, github.com/elaous/gojiberry" MUST be honoured wherever the work shows its own notices to its users
  (gojiberry's NOTICE, additional terms under AGPL-3.0 section 7(b)). The credit "Made with the Xangô project,
  github.com/elaous/xango" MUST be honoured in the same places (xango's NOTICE, additional terms under AGPL-3.0
  section 7(b)).
- For use that does not fit the AGPL, a commercial licence is available for each project: see gojiberry's
  [COMMERCIAL.md](https://github.com/elaous/gojiberry/blob/main/COMMERCIAL.md) and xango's
  [COMMERCIAL.md](https://github.com/elaous/xango/blob/main/COMMERCIAL.md).

## Layout

`Dockerfile`, `docker-compose.yml`, `docker/` container setup · `config/` settings · `llm/` Ollama + OpenAI-compatible clients · `rag/` PDF chunking, embeddings, search ·
`web/` search & fetch · `agent/` tool loop and the team · `server/` API + static UI · `ui/` the xango app (`main.xg`).
