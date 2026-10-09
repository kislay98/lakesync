# syntax=docker/dockerfile:1.7
# Multi-stage build: Node builds the UI, Go builds the binary (CGO for DuckDB),
# a slim Debian image runs it as a non-root user with the DuckDB extensions bundled.

FROM node:22-bookworm-slim AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.27-bookworm AS build
ARG VERSION=dev
ARG COMMIT=unknown
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=bind,source=go.mod,target=go.mod \
    --mount=type=bind,source=go.sum,target=go.sum \
    go mod download
COPY . .
COPY --from=ui /src/web/dist/ internal/api/static/
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 go build -trimpath \
      -ldflags "-s -w -X lakesync/internal/config.Version=${VERSION} -X lakesync/internal/config.Commit=${COMMIT}" \
      -o /out/lakesync ./cmd/lakesync
# Bundle the DuckDB extensions so nothing is downloaded at runtime.
RUN /out/lakesync duckdb-install --dir /out/duckdb-extensions

FROM debian:bookworm-slim AS runtime
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -r -g 10001 lakesync && useradd -r -u 10001 -g lakesync -d /home/lakesync -m lakesync
COPY --from=build /out/lakesync /usr/local/bin/lakesync
COPY --from=build --chown=lakesync:lakesync /out/duckdb-extensions /opt/duckdb/extensions
ENV DUCKDB_EXTENSION_DIR=/opt/duckdb/extensions \
    HOME=/home/lakesync \
    LISTEN_ADDR=:8080 \
    LOG_FORMAT=json
USER lakesync
WORKDIR /home/lakesync
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=5 CMD curl -sf http://localhost:8080/healthz || exit 1
ENTRYPOINT ["lakesync"]
CMD ["serve"]
