# syntax=docker/dockerfile:1

# --- build stage -----------------------------------------------------------
FROM golang:1.25 AS build
WORKDIR /src

# Resolve modules first so dependency layers are cached independently.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# Static binaries, no VCS metadata stamping (the build context may be a
# detached checkout), determinism via -trimpath.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-mod=readonly \
    go build -buildvcs=false -trimpath -ldflags="-s -w" \
      -o /out/api ./cmd/api && \
    CGO_ENABLED=0 GOFLAGS=-mod=readonly \
    go build -buildvcs=false -trimpath -ldflags="-s -w" \
      -o /out/verify ./cmd/verify

# --- runtime stage ---------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS release
COPY --from=build /out/api /usr/local/bin/api
COPY --from=build /out/verify /usr/local/bin/verify

EXPOSE 8080

# The api binary doubles as its own container health probe.
HEALTHCHECK --interval=5s --timeout=2s --start-period=3s --retries=10 \
    CMD ["/usr/local/bin/api", "-healthcheck"]

ENTRYPOINT ["/usr/local/bin/api"]
