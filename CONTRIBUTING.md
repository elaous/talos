# Contributing to talos

Thanks for helping. Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). Contributions are
accepted under the [MIT licence](LICENSE).

## Before you start

- Open an issue before a large change so the direction can be agreed first.
- Vulnerabilities MUST be reported privately, see [SECURITY.md](SECURITY.md).

## Development setup

talos needs Go 1.26+ and a sibling checkout of the private `gojiberry` module at `../gojiberry` (see the
README, "Dependency on gojiberry"). Contributors without access to `gojiberry` cannot build or test talos.

```bash
make test                       # go vet + go test for all packages, and a WASM vet of ./ui
scripts/targeted-tests.sh       # vet and test only the packages affected by your diff against origin/main
scripts/targeted-tests.sh --list web/web.go   # show what a change to a file would run
```

`scripts/targeted-tests.sh` runs the full `make test` when `go.mod`, `go.sum`, `Makefile`, `Dockerfile` or
anything under `.github/workflows/` changes.

## Pull requests

- A pull request MUST pass `make test` locally.
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

Workflows check out `gojiberry` with the repository secret `GOJIBERRY_READ_TOKEN`. Pull requests from forks
cannot read that secret, so their workflows are skipped; a maintainer runs `make test` locally before merging them.
