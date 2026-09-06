# Frontend build stage. Bun is present only here; the release image contains
# neither a JavaScript runtime nor a filesystem web distribution.
FROM oven/bun:1.3.14@sha256:e10577f0db68676a7024391c6e5cb4b879ebd17188ab750cf10024a6d700e5c4 AS web-builder

WORKDIR /src
COPY web/package.json web/bun.lock ./web/
RUN cd web && bun install --frozen-lockfile
COPY api/openapi.yaml ./api/openapi.yaml
COPY web/ ./web/
RUN cd web && bun run generate && bun run build

# Go build stage.
# Pin by digest for reproducibility; update periodically.
FROM golang:1.27.0-bookworm@sha256:484ef6066fa69acb059fdfeda7ba2b8f7391f2ef6abc6f9b8411e669ebd56466 AS builder

# Install build dependencies for CGO (SQLite, DuckDB).
# libsqlite3-dev provides sqlite3.h, required to compile the sqlite-vec
# extension (asg017/sqlite-vec-go-bindings) under -tags sqlite_vec.
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    make \
    git \
    libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Download dependencies first (layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
# Never trust an ambient or host-staged frontend bundle. The pinned frontend
# stage is the sole source of release assets for this image build.
RUN find internal/web/dist -mindepth 1 -maxdepth 1 ! -name stub.html -exec rm -rf {} +
COPY --from=web-builder /src/web/dist/ ./internal/web/dist/

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Note: Module path must match go.mod (go.kenn.io/msgvault)
RUN CGO_ENABLED=1 go build \
    -tags "fts5 sqlite_vec" \
    -trimpath \
    -ldflags="-s -w \
        -X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=${VERSION} \
        -X go.kenn.io/msgvault/cmd/msgvault/cmd.Commit=${COMMIT} \
        -X go.kenn.io/msgvault/cmd/msgvault/cmd.BuildDate=${BUILD_DATE}" \
    -o /msgvault \
    ./cmd/msgvault

# Runtime stage - Debian provides current glibc for CGO/DuckDB bindings
FROM debian:bookworm-slim@sha256:96e378d7e6531ac9a15ad505478fcc2e69f371b10f5cdf87857c4b8188404716

# Install runtime dependencies (libstdc++ required for CGO/DuckDB)
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    ca-certificates \
    tzdata \
    wget \
    libstdc++6 \
    && rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd --gid 1000 msgvault \
    && useradd --uid 1000 --gid msgvault --home-dir /home/msgvault --create-home --shell /bin/sh msgvault

# Copy binary from builder
COPY --from=builder /msgvault /usr/local/bin/msgvault

# Set up data directory with correct ownership
ENV MSGVAULT_HOME=/data
RUN mkdir -p /data && chown msgvault:msgvault /data
VOLUME /data

# Switch to non-root user
USER msgvault
WORKDIR /data

# Health check using wget (curl not included to keep image small)
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO/dev/null http://localhost:8080/health || exit 1

# Default port for HTTP API
EXPOSE 8080

# Use entrypoint so users can run any msgvault command
ENTRYPOINT ["msgvault"]

# Default to serve mode
CMD ["serve"]
