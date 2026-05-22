# Build stage
# Pin by digest for reproducibility; update periodically
FROM golang:1.26-bookworm@sha256:252599aeb51ad60b83e4d8821802068127c528c707cb7dd7afd93be057c6011c AS builder

# Install build dependencies for CGO (SQLite, DuckDB)
RUN apt-get update && apt-get install -y --no-install-recommends \
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

# Note: Module path must match go.mod (github.com/wesm/msgvault)
RUN CGO_ENABLED=1 go build \
    -tags fts5 \
    -trimpath \
    -ldflags="-s -w \
        -X go.kenn.io/msgvault/cmd/msgvault/cmd.Version=${VERSION} \
        -X go.kenn.io/msgvault/cmd/msgvault/cmd.Commit=${COMMIT} \
        -X go.kenn.io/msgvault/cmd/msgvault/cmd.BuildDate=${BUILD_DATE}" \
    -o /msgvault \
    ./cmd/msgvault

# Runtime stage — wolfi-base provides current glibc for CGO/DuckDB bindings
FROM cgr.dev/chainguard/wolfi-base@sha256:52e71f61c6afd1f8d2625cff4465d8ecee156668ca665f7e9c582d1cc914eb6a

# Install runtime dependencies (libstdc++ required for CGO/DuckDB)
RUN apk update && apk add --no-cache \
    ca-certificates \
    tzdata \
    wget \
    libstdc++

# Create non-root user
RUN adduser -D -h /home/msgvault -u 1000 -s /bin/sh msgvault

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
