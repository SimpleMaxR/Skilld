# Packages the same skilld-server binary used by `make build` and systemd.
# Web assets and SQL migrations are embedded at build time (see embed.go).

ARG GO_VERSION=1.27.4

FROM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./
COPY vendor ./vendor
COPY embed.go ./
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
COPY packages/web ./packages/web

# Flags match the Makefile `build` target. CGO is required by mattn/go-sqlite3.
RUN CGO_ENABLED=1 go build -mod=vendor -trimpath \
    -tags "sqlite_omit_load_extension" \
    -ldflags "-s -w" \
    -o /out/skilld-server ./cmd/skilld-server

FROM debian:bookworm-slim AS runtime

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl gosu \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 3737 skilld \
    && useradd --uid 3737 --gid 3737 --home-dir /var/lib/skilld --no-create-home --shell /bin/false skilld \
    && mkdir -p /var/lib/skilld \
    && chown skilld:skilld /var/lib/skilld

COPY --from=build /out/skilld-server /usr/local/bin/skilld-server
COPY deploy/docker/entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh /usr/local/bin/skilld-server

ENV HOST=0.0.0.0 \
    PORT=3737 \
    DATA_DIR=/var/lib/skilld

EXPOSE 3737
VOLUME ["/var/lib/skilld"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/api/health" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
