# syntax=docker/dockerfile:1.28
# Runtime for the Web UI documentation captures. build-generated-assets.sh
# passes the @playwright/test version from web/package.json, so the image's
# Chromium matches it. Go and Bun come from their official images at the
# versions go.mod and package.json pin.
ARG PLAYWRIGHT_VERSION
FROM golang:1.27.4-bookworm AS go
FROM oven/bun:1.4.4 AS bun

FROM mcr.microsoft.com/playwright:v${PLAYWRIGHT_VERSION}-noble

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    g++ \
    gcc \
    git \
    libsqlite3-dev \
    make \
    perl \
    python3 \
    && rm -rf /var/lib/apt/lists/*

COPY --from=go /usr/local/go /usr/local/go
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
RUN ln -s /usr/local/bin/bun /usr/local/bin/bunx

ENV PATH=/usr/local/go/bin:$PATH \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
