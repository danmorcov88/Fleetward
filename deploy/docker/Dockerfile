# Control plane image. Plugins are built into the same image so a single container is a complete,
# self-contained Fleetward install — which is what makes the docker-compose quickstart one command.
FROM golang:1.27-bookworm AS build

WORKDIR /src

# Dependencies first: they change far less often than source, so this layer stays cached across
# almost every rebuild.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

ENV CGO_ENABLED=0
RUN LDFLAGS="-s -w \
      -X github.com/danmorcov88/fleetward/internal/version.Version=${VERSION} \
      -X github.com/danmorcov88/fleetward/internal/version.Commit=${COMMIT} \
      -X github.com/danmorcov88/fleetward/internal/version.Date=${BUILD_DATE}" && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/fleetward ./cmd/fleetward && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/fleetward-cli ./cmd/fleetward-cli && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/plugins/fleetward-plugin-postgresql ./cmd/plugins/postgres && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/plugins/fleetward-plugin-sqlserver  ./cmd/plugins/sqlserver && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/plugins/fleetward-plugin-mysql      ./cmd/plugins/mysql && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/plugins/fleetward-plugin-mongodb    ./cmd/plugins/mongodb && \
    go build -trimpath -ldflags "${LDFLAGS}" -o /out/plugins/fleetward-plugin-redis      ./cmd/plugins/redis

# Debian rather than distroless or Alpine: plugins orchestrate native tooling (pg_dump,
# pg_basebackup, mysqldump, mongodump, redis-cli), and those clients need a normal userland.
# Choosing a smaller base here would only push the problem into a later phase.
#
# trixie rather than bookworm because of one hard rule: pg_dump refuses to dump a server newer than
# itself. bookworm's postgresql-client is 15, which cannot back up the PostgreSQL 16 in the dev
# stack — and could not back up most of the estate this tool is for. trixie ships 17, which covers
# every version the plugin declares support for (>=13 <18).
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates \
      postgresql-client \
      curl \
    && rm -rf /var/lib/apt/lists/*

# Run unprivileged. The control plane needs no root capability, and it holds database credentials.
RUN groupadd --system --gid 10001 fleetward \
    && useradd --system --uid 10001 --gid fleetward --home-dir /app --shell /usr/sbin/nologin fleetward

WORKDIR /app

COPY --from=build /out/fleetward     /usr/local/bin/fleetward
COPY --from=build /out/fleetward-cli /usr/local/bin/fleetward-cli
COPY --from=build /out/plugins/      /app/plugins/

ENV FLEETWARD_PLUGINS_DIR=/app/plugins \
    FLEETWARD_HTTP_ADDR=:8080

USER fleetward
EXPOSE 8080 9090

HEALTHCHECK --interval=15s --timeout=5s --start-period=20s --retries=5 \
  CMD ["/usr/local/bin/fleetward-cli", "health", "--server", "http://127.0.0.1:8080"]

ENTRYPOINT ["/usr/local/bin/fleetward"]
