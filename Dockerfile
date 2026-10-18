# syntax=docker/dockerfile:1
#
# talos image. gojiberry (the UI framework, a private sibling module) is passed as a named build context:
#
#   docker build --build-context gojiberry=../gojiberry -t talos .
#
# (docker compose does this for you: see docker-compose.yml.)
#
# Behind a proxy that re-signs TLS (a corporate one), pass its CA as an optional secret and the proxy as the usual
# predefined build args; neither ends up in the image:
#
#   docker build --build-context gojiberry=../gojiberry --secret id=cacert,src=/path/to/ca.pem \
#     --build-arg HTTPS_PROXY=http://proxy:3128 -t talos .

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS build
WORKDIR /src
# go.mod has `replace github.com/elaous/gojiberry => ../gojiberry`, so it must sit next to talos.
COPY --from=gojiberry / /src/gojiberry
WORKDIR /src/talos
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=secret,id=cacert,required=false \
    if [ -f /run/secrets/cacert ]; then export SSL_CERT_FILE=/run/secrets/cacert; fi; go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=secret,id=cacert,required=false \
    set -eux; \
    if [ -f /run/secrets/cacert ]; then export SSL_CERT_FILE=/run/secrets/cacert; fi; \
    # .xg -> .xg.go with gojiberry's own compiler, then the WASM UI, then the server (static, no cgo)
    (cd /src/gojiberry && go run ./cmd/xgc /src/talos/ui); \
    mkdir -p /out/ui /out/data /out/pdfs; \
    GOOS=js GOARCH=wasm go build -trimpath -o /out/ui/main.wasm ./ui; \
    cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" ui/index.html /out/ui/; \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/talos ./cmd/talos

# distroless: no shell, no package manager; CA certificates are included (web search and fetch use HTTPS).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/talos /app/talos
COPY --from=build /out/ui /app/ui
COPY docker/talos.json /etc/talos/talos.json
# Directories the compose file bind-mounts; they exist (owned by the default user) for plain `docker run` too.
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/pdfs /pdfs
WORKDIR /app
EXPOSE 8080
VOLUME ["/data", "/pdfs"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/app/talos", "-c", "/etc/talos/talos.json", "healthcheck"]
ENTRYPOINT ["/app/talos", "-c", "/etc/talos/talos.json"]
CMD ["serve"]
