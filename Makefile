# talos dev workflow. gojiberry (the UI framework) is a sibling checkout; see go.mod's replace.
GOJIBERRY ?= ../gojiberry

.PHONY: all
all: ui build

.PHONY: generate
generate: ## .xg -> .xg.go
	cd $(GOJIBERRY) && go run ./cmd/xgc $(CURDIR)/ui

.PHONY: ui
ui: generate ## build the WASM app into ui/dist
	mkdir -p ui/dist
	GOOS=js GOARCH=wasm go build -o ui/dist/main.wasm ./ui
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" ui/index.html ui/dist/

.PHONY: build
build:
	go build -o bin/talos ./cmd/talos

.PHONY: test
test:
	go vet ./... && go test ./...
	GOOS=js GOARCH=wasm go vet ./ui

.PHONY: run
run: ui build
	./bin/talos serve
