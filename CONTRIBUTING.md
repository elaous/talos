# Contributing to talos

Thanks for helping. Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). Contributions are
accepted under the [MIT licence](LICENSE).

## Before you start

- Open an issue before a large change so the direction can be agreed first.
- Vulnerabilities MUST be reported privately, see [SECURITY.md](SECURITY.md).

## Development setup

talos needs Go 1.26+. Its dependency on the public `gojiberry` module is fetched by the Go toolchain, so a fresh
clone builds with no extra setup (see the README, "Dependency on gojiberry").

```bash
make test                       # go vet + go test for all packages, and a WASM vet of ./ui
scripts/targeted-tests.sh       # vet and test only the packages affected by your diff against origin/main
scripts/targeted-tests.sh --list web/web.go   # show what a change to a file would run
```

`scripts/targeted-tests.sh` runs the full `make test` when `go.mod`, `go.sum`, `Makefile`, `Dockerfile` or
anything under `.github/workflows/` changes.

## Pull requests

- A pull request MUST pass `make test` and `make lint` locally (`make lint` needs a local golangci-lint v2.15.0, the version CI pins).
- A pull request SHOULD be small and focused on one change, and MUST describe what changed and how it was tested.
- Tests SHOULD accompany behaviour changes. Tests MUST NOT need a running model: script the LLM as the existing
  tests do.
- Commit subjects MUST be imperative ("Add X", not "Added X"). A body SHOULD be added only for a fact the diff
  cannot show.
- Code MUST explain itself through names and structure. A comment MAY be added only for behaviour a careful reader
  would not expect or for a constraint a refactor must keep, in about ten words or fewer. Comments MUST NOT narrate
  the change, the bug or an issue number; that belongs in the pull request description.
- Documentation and pull request descriptions SHOULD state obligations with the RFC 2119 keywords (MUST, SHOULD,
  MAY).

## Continuous integration

Workflows run on every pull request, including those from forks and from Dependabot; none of them needs a repository
secret. A pull request that bumps `gojiberry` MUST also commit the regenerated `ui/main.xg.go`.
