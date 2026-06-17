#!/bin/bash
# msgvault installer
# Usage: curl -fsSL https://msgvault.io/install.sh | bash

set -e

REPO="kenn-io/msgvault"
BINARY_NAME="msgvault"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info() { echo -e "${GREEN}$1${NC}"; }
warn() { echo -e "${YELLOW}$1${NC}"; }
error() { echo -e "${RED}$1${NC}" >&2; exit 1; }

# Detect OS
detect_os() {
    case "$(uname -s)" in
        Darwin) echo "darwin" ;;
        Linux) echo "linux" ;;
        *) error "Unsupported OS: $(uname -s). msgvault supports macOS and Linux." ;;
    esac
}

# Detect architecture
detect_arch() {
    case "$(uname -m)" in
        x86_64|amd64) echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *) error "Unsupported architecture: $(uname -m)" ;;
    esac
}

# Find install directory
find_install_dir() {
    if [ -w "/usr/local/bin" ]; then
        echo "/usr/local/bin"
    else
        mkdir -p "$HOME/.local/bin"
        echo "$HOME/.local/bin"
    fi
}

# Download with curl or wget
download() {
    local url="$1"
    local output="$2"
    if command -v curl &>/dev/null; then
        curl -fsSL "$url" -o "$output"
    elif command -v wget &>/dev/null; then
        wget -q "$url" -O "$output"
    else
        error "Neither curl nor wget found"
    fi
}

# Get latest release version
get_latest_version() {
    # Use the HTML /releases/latest endpoint, which 302-redirects to
    # /releases/tag/<version>. Unlike api.github.com it is not rate-limited
    # at 60 req/hr per IP, so users behind shared NAT / VPN don't get 403.
    local url="https://github.com/${REPO}/releases/latest"
    local final_url=""
    if command -v curl &>/dev/null; then
        final_url=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url") || return 1
    elif command -v wget &>/dev/null; then
        final_url=$(wget --spider -S "$url" 2>&1 \
            | awk 'tolower($1)=="location:" {print $2}' \
            | tail -1 \
            | tr -d '\r\n') || return 1
    else
        return 1
    fi
    case "$final_url" in
        */releases/tag/*) echo "${final_url##*/releases/tag/}" ;;
        *) return 1 ;;
    esac
}

# Verify checksum
verify_checksum() {
    local file="$1"
    local checksums_file="$2"
    local filename="$3"

    if [ ! -f "$checksums_file" ]; then
        warn "Checksum file not available, skipping verification"
        return 0
    fi

    local expected=$(awk -v f="$filename" '{gsub(/^\*/, "", $2); if ($2==f) {print $1; exit}}' "$checksums_file")
    if [ -z "$expected" ]; then
        warn "No checksum found for $filename, skipping verification"
        return 0
    fi

    local actual
    if command -v sha256sum &>/dev/null; then
        actual=$(sha256sum "$file" | cut -d' ' -f1)
    elif command -v shasum &>/dev/null; then
        actual=$(shasum -a 256 "$file" | cut -d' ' -f1)
    else
        warn "No sha256 tool available, skipping verification"
        return 0
    fi

    if [ "$expected" != "$actual" ]; then
        error "Checksum verification failed!\n  Expected: $expected\n  Actual:   $actual"
    fi

    info "Checksum verified"
}

# Install from GitHub releases
install_from_release() {
    local os="$1"
    local arch="$2"
    local install_dir="$3"

    info "Fetching latest release..."
    local version=$(get_latest_version)

    if [ -z "$version" ]; then
        return 1
    fi

    info "Found version: $version"

    local platform="${os}_${arch}"
    local filename="${BINARY_NAME}_${version#v}_${platform}.tar.gz"
    local base_url="https://github.com/${REPO}/releases/download/${version}"

    local tmpdir=$(mktemp -d)
    trap "rm -rf $tmpdir" EXIT

    info "Downloading ${filename}..."
    if ! download "${base_url}/${filename}" "$tmpdir/release.tar.gz"; then
        return 1
    fi

    # Download and verify checksum
    if download "${base_url}/SHA256SUMS" "$tmpdir/SHA256SUMS" 2>/dev/null; then
        verify_checksum "$tmpdir/release.tar.gz" "$tmpdir/SHA256SUMS" "$filename"
    else
        warn "WARNING: Could not download SHA256SUMS — integrity not verified"
    fi

    info "Extracting..."
    tar -xzf "$tmpdir/release.tar.gz" -C "$tmpdir"

    # Install binary
    if [ -f "$tmpdir/${BINARY_NAME}" ]; then
        if [ -w "$install_dir" ]; then
            mv "$tmpdir/${BINARY_NAME}" "$install_dir/"
        else
            sudo mv "$tmpdir/${BINARY_NAME}" "$install_dir/"
        fi
        chmod +x "$install_dir/${BINARY_NAME}"
    else
        error "Binary not found in archive"
    fi

    # macOS code signing
    if [ "$os" = "darwin" ] && [ -f "$install_dir/${BINARY_NAME}" ]; then
        codesign -s - "$install_dir/${BINARY_NAME}" 2>/dev/null || true
    fi

    return 0
}

# Main
main() {
    info "Installing msgvault..."
    echo

    local os=$(detect_os)
    local arch=$(detect_arch)
    local install_dir=$(find_install_dir)

    info "Platform: ${os}/${arch}"
    info "Install directory: ${install_dir}"
    echo

    if install_from_release "$os" "$arch" "$install_dir"; then
        info "Installed from GitHub release"
    else
        error "Installation failed. Please check https://github.com/${REPO}/releases for available builds."
    fi

    echo
    info "Installation complete!"
    echo

    # Check PATH
    if ! echo "$PATH" | grep -q "$install_dir"; then
        warn "Add this to your shell profile:"
        echo "  export PATH=\"\$PATH:$install_dir\""
        echo
    fi

    echo "Get started:"
    echo "  msgvault init-db"
    echo "  msgvault add-account you@gmail.com"
    echo "  msgvault sync-full you@gmail.com --limit 100"
    echo "  msgvault tui"
}

main "$@"
