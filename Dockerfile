# Build stage
# Pin by digest for reproducibility; update periodically
FROM golang:1.26-bookworm@sha256:5d2b868674b57c9e48cdd39e891acce4196b6926ca6d11e9c270a8f85106203d AS builder

# Install build dependencies for CGO (SQLite, DuckDB)
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    gcc \
    g++ \
    make \
    git \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Download dependencies first (layer caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

# Note: Module path must match go.mod (go.kenn.io/msgvault)
RUN CGO_ENABLED=1 go build \
    -tags fts5 \
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
