# syntax=docker/dockerfile:1.4
# Stage 1: Build msgvault binary
FROM golang:1.25-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends git make gcc g++ && rm -rf /var/lib/apt/lists/*

COPY . /src
WORKDIR /src
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    make build

# Stage 2: Screenshot runtime with freeze + tmux
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    bash \
    tmux \
    fontconfig \
    ca-certificates \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Install fonts for screenshot rendering (mobile-compatible fallback chain)
RUN apt-get update && apt-get install -y --no-install-recommends \
    fonts-liberation \
    && rm -rf /var/lib/apt/lists/* && \
    fc-cache -f

# Install freeze (arch-aware)
RUN FREEZE_VERSION="0.2.2" && \
    DPKG_ARCH="$(dpkg --print-architecture)" && \
    case "${DPKG_ARCH}" in \
      arm64) FREEZE_ARCH="arm64" ;; \
      *)     FREEZE_ARCH="x86_64" ;; \
    esac && \
    curl -fsSL "https://github.com/charmbracelet/freeze/releases/download/v${FREEZE_VERSION}/freeze_${FREEZE_VERSION}_Linux_${FREEZE_ARCH}.tar.gz" \
      | tar -xz -C /tmp && \
    mv /tmp/freeze_*/freeze /usr/local/bin/freeze && \
    rm -rf /tmp/freeze_* && \
    freeze --version

# Copy msgvault binary from builder
COPY --from=builder /src/msgvault /usr/local/bin/msgvault

# Verify binary runs
RUN msgvault version

WORKDIR /tapes
ENTRYPOINT ["bash"]
