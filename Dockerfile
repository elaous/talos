# syntax=docker/dockerfile:1
#
# gojiberry is a named build context: see docker-compose.yml.

ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS build
WORKDIR /src
# go.mod replace needs gojiberry at ../gojiberry.
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
    # Generate UI sources, build WASM, then a static server.
    (cd /src/gojiberry && go run ./cmd/xgc /src/talos/ui); \
    mkdir -p /out/ui /out/data /out/pdfs; \
    GOOS=js GOARCH=wasm go build -trimpath -o /out/ui/main.wasm ./ui; \
    cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" ui/index.html /out/ui/; \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/talos ./cmd/talos

# distroless: no shell; includes CA certificates for HTTPS.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/talos /app/talos
COPY --from=build /out/ui /app/ui
COPY docker/talos.json /etc/talos/talos.json
# Library folders owned by the image user; volumes inherit ownership.
COPY --from=build --chown=65532:65532 /out/data /data
COPY --from=build --chown=65532:65532 /out/pdfs /pdfs
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD ["/app/talos", "-c", "/etc/talos/talos.json", "healthcheck"]
ENTRYPOINT ["/app/talos", "-c", "/etc/talos/talos.json"]
CMD ["serve"]
