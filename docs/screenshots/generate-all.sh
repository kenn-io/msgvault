#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
IMAGE_NAME="msgvault-vhs"
DEMO_DATA_DIR="$SCRIPT_DIR/demo-data"
OUTPUT_DIR="$SCRIPT_DIR/../assets/generated"

usage() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Generate msgvault SVG screenshots using freeze + tmux.

Options:
  --repo PATH     Path to msgvault source repo (default: integrated repo root)
  --skip-data     Skip demo data generation
  --skip-build    Skip Docker image build
  -h, --help      Show this help
EOF
}

REPO="${MSGVAULT_REPO:-$SCRIPT_DIR/../..}"
SKIP_DATA=false
SKIP_BUILD=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --repo)       REPO="$2"; shift 2 ;;
        --skip-data)  SKIP_DATA=true; shift ;;
        --skip-build) SKIP_BUILD=true; shift ;;
        -h|--help)    usage; exit 0 ;;
        *)            echo "Unknown option: $1"; usage; exit 1 ;;
    esac
done

REPO_RESOLVED="$(cd "$REPO" 2>/dev/null && pwd)" || {
    echo "Error: msgvault repo not found at $REPO"
    echo "Pass --repo PATH or set MSGVAULT_REPO."
    exit 1
}
REPO="$REPO_RESOLVED"
export MSGVAULT_REPO="$REPO"

# --- Step 1: Generate demo data ---
if [[ "$SKIP_DATA" == false ]]; then
    echo "==> Generating demo data..."
    (cd "$SCRIPT_DIR" && uv run generate_demo_data.py)
    echo ""
fi

# --- Step 2: Build Docker image ---
if [[ "$SKIP_BUILD" == false ]]; then
    echo "==> Building Docker image: $IMAGE_NAME"
    if ! docker build --help 2>&1 | grep -q -- '--ignorefile'; then
        echo "Error: docker build must support --ignorefile for the docs screenshot context." >&2
        echo "Upgrade Docker/Buildx rather than sending the full repository context." >&2
        exit 1
    fi
    DOCKER_BUILDKIT=1 docker build --ignorefile "$SCRIPT_DIR/.dockerignore" -t "$IMAGE_NAME" -f "$SCRIPT_DIR/Dockerfile" "$REPO"
    echo ""
fi

# --- Step 3: Build analytics cache inside container ---
echo "==> Building analytics cache..."
docker run --rm \
    -v "$DEMO_DATA_DIR:/data" \
    -e "MSGVAULT_HOME=/data" \
    --entrypoint msgvault \
    "$IMAGE_NAME" \
    build-cache --full-rebuild
echo ""

# --- Step 4: Generate screenshots ---
mkdir -p "$OUTPUT_DIR"

echo "==> Generating SVG screenshots..."
docker run --rm \
    -v "$SCRIPT_DIR:/tapes" \
    -v "$DEMO_DATA_DIR:/data" \
    -v "$OUTPUT_DIR:/output" \
    -e "MSGVAULT_HOME=/data" \
    "$IMAGE_NAME" \
    /tapes/generate-screenshots.sh /output

echo ""
echo "Done! Output files are in $OUTPUT_DIR"
