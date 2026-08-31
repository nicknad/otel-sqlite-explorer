# ------------------------------------------------------------
# Log Explorer image: read-only web UI over the otel-sqlite sink
# database on the shared /data volume.
#
#   docker build -t log-explorer:latest .
#
# Used by otel-sqlite's docker/compose.stack-e2e.yml as the
# `explorer` service; runs as uid 10001 to match the sink's
# file ownership on the shared volume.
# ------------------------------------------------------------

FROM golang:1.27-trixie AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/log-explorer ./cmd/server

# ------------------------------------------------------------
# Runtime image
# ------------------------------------------------------------

FROM debian:trixie-slim AS runtime

ARG REVISION="unknown"

LABEL org.opencontainers.image.title="log-explorer" \
      org.opencontainers.image.description="Read-only web UI for OTel logs stored in SQLite" \
      org.opencontainers.image.revision="${REVISION}" \
      org.opencontainers.image.licenses="MIT"

RUN useradd \
        --system \
        --uid 10001 \
        --create-home \
        --shell /usr/sbin/nologin \
        log-explorer \
    && mkdir -p /data \
    && chown -R log-explorer:log-explorer /data

COPY --from=builder /out/log-explorer /usr/local/bin/log-explorer

USER log-explorer

# Default: point at the sink database mounted at /data.
ENTRYPOINT ["/usr/local/bin/log-explorer"]
CMD ["-db", "/data/otel-logs.db", "-addr", ":8080"]
