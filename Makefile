# gojiberry is a sibling checkout; override with GOJIBERRY.
GOJIBERRY ?= ../gojiberry

.PHONY: all
all: ui build

.PHONY: generate
generate:
	cd $(GOJIBERRY) && go run ./cmd/xgc $(CURDIR)/ui

.PHONY: ui
ui: generate
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

.PHONY: docker-build
docker-build:
	docker build --build-context gojiberry=$(GOJIBERRY) $(DOCKER_BUILD_FLAGS) -t talos:local .

.PHONY: docker-up
docker-up: docker-build
	GOJIBERRY_DIR=$(GOJIBERRY) docker compose up -d --no-build

.PHONY: docker-up-host
docker-up-host: docker-build
	mkdir -p pdfs data
	TALOS_UID=$$(id -u) TALOS_GID=$$(id -g) GOJIBERRY_DIR=$(GOJIBERRY) docker compose -f docker-compose.yml -f docker-compose.hostdirs.yml up -d --no-build

.PHONY: docker-down
docker-down:
	docker compose down
