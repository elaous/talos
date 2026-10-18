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

# --- Docker (gojiberry is a named build context: see the Dockerfile) ---
.PHONY: docker-build
docker-build: ## build the image as talos:local (DOCKER_BUILD_FLAGS e.g. --secret id=cacert,src=ca.pem --build-arg HTTPS_PROXY=...)
	docker build --build-context gojiberry=$(GOJIBERRY) $(DOCKER_BUILD_FLAGS) -t talos:local .

.PHONY: docker-up
docker-up: docker-build ## start on http://127.0.0.1:8080 (PDFs and index in named volumes)
	GOJIBERRY_DIR=$(GOJIBERRY) docker compose up -d --no-build

.PHONY: docker-up-host
docker-up-host: docker-build ## like docker-up, but PDFs in ./pdfs and the index in ./data on the host
	mkdir -p pdfs data
	TALOS_UID=$$(id -u) TALOS_GID=$$(id -g) GOJIBERRY_DIR=$(GOJIBERRY) docker compose -f docker-compose.yml -f docker-compose.hostdirs.yml up -d --no-build

.PHONY: docker-down
docker-down:
	docker compose down
