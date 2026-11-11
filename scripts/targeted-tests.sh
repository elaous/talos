#!/usr/bin/env bash
# Vet and test only the packages affected by changed files; full `make test` for build-level changes.
# Usage: scripts/targeted-tests.sh [--list] [changed-file ...]
# Without files, uses `git diff --name-only $BASE...HEAD` (BASE defaults to origin/main).
set -euo pipefail

list_only=0
if [ "${1:-}" = "--list" ]; then
  list_only=1
  shift
fi

cd "$(git rev-parse --show-toplevel)"

if [ "$#" -gt 0 ]; then
  changed=("$@")
else
  mapfile -t changed < <(git diff --name-only "${BASE:-origin/main}...HEAD")
fi

if [ "${#changed[@]}" -eq 0 ]; then
  echo "no changed files; nothing to test"
  exit 0
fi

full_run() {
  echo "full run: $1"
  if [ "$list_only" -eq 1 ]; then
    echo "make test"
  else
    make test
  fi
  exit 0
}

module=$(go list -m)
direct=()
for f in "${changed[@]}"; do
  case "$f" in
    go.mod | go.sum | Makefile | Dockerfile | scripts/targeted-tests.sh | .github/workflows/* | .github/dependabot.yml)
      full_run "$f changed"
      ;;
    *.md | LICENSE | .gitignore | .dockerignore | .github/* | docker-compose*.yml | docker/* | talos.example.json)
      continue
      ;;
  esac
  dir=$(dirname "$f")
  if [ ! -d "$dir" ]; then
    full_run "$f was deleted"
  fi
  if [ "$dir" = "." ]; then
    pkg="$module"
  else
    pkg="$module/$dir"
  fi
  direct+=("$pkg")
done

if [ "${#direct[@]}" -eq 0 ]; then
  echo "no Go-relevant changes; nothing to test"
  exit 0
fi

affected_in_graph() {
  # stdin: "pkg imports..." lines from go list; $@: directly changed packages
  awk -v seeds="$*" '
    BEGIN { n = split(seeds, s, " "); for (i = 1; i <= n; i++) hit[s[i]] = 1 }
    { pkgs[NR] = $1; for (i = 2; i <= NF; i++) imp[NR, i] = $i; nf[NR] = NF; names[$1] = NR }
    END {
      do {
        changed = 0
        for (r = 1; r <= NR; r++) {
          if (hit[pkgs[r]]) continue
          for (i = 2; i <= nf[r]; i++) if (hit[imp[r, i]]) { hit[pkgs[r]] = 1; changed = 1; break }
        }
      } while (changed)
      for (p in hit) if (hit[p] && (p in names)) print p
    }' | sort
}

graph_fmt='{{.ImportPath}} {{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}'

mapfile -t native_pkgs < <(go list -f "$graph_fmt" ./... | affected_in_graph "${direct[@]}")
mapfile -t wasm_pkgs < <(GOOS=js GOARCH=wasm go list -f "$graph_fmt" ./... | affected_in_graph "${direct[@]}")

wasm_ui=0
for p in "${wasm_pkgs[@]}"; do
  [ "$p" = "$module/ui" ] && wasm_ui=1
done

echo "changed packages:  ${direct[*]}"
echo "native affected:   ${native_pkgs[*]:-none}"
echo "wasm ui affected:  $wasm_ui"

if [ "$list_only" -eq 1 ]; then
  exit 0
fi

if [ "${#native_pkgs[@]}" -gt 0 ]; then
  go vet "${native_pkgs[@]}"
  go test "${native_pkgs[@]}"
fi
if [ "$wasm_ui" -eq 1 ]; then
  GOOS=js GOARCH=wasm go vet ./ui
fi
