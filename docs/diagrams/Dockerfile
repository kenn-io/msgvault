# Renders the concept diagrams with build.sh on hosts without Chrome or ImageMagick.
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
    chromium \
    fonts-liberation \
    fonts-noto-color-emoji \
    imagemagick \
    && rm -rf /var/lib/apt/lists/*
