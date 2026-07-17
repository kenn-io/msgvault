#!/bin/bash
set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

VERSION="$1"
EXTRA_INSTRUCTIONS="$2"

if [ -z "$VERSION" ]; then
    echo "Usage: $0 <version> [extra_instructions]"
    echo "Example: $0 0.1.0"
    echo "Example: $0 0.1.0 \"Focus on TUI improvements\""
    exit 1
fi

# Validate version format
if ! [[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    echo "Error: Version must be in format X.Y.Z (e.g., 0.1.0)"
    exit 1
fi

TAG="v$VERSION"

# Check if tag already exists
if git rev-parse "$TAG" >/dev/null 2>&1; then
    echo "Error: Tag $TAG already exists"
    exit 1
fi

# Check for uncommitted changes
if ! git diff-index --quiet HEAD --; then
    echo "Error: You have uncommitted changes. Please commit or stash them first."
    exit 1
fi

# Check if gh CLI is available
if ! command -v gh &> /dev/null; then
    echo "Error: gh CLI is required. Install from https://cli.github.com/"
    exit 1
fi

# Create a temp file for the changelog
CHANGELOG_FILE=$(mktemp)
trap 'rm -f "$CHANGELOG_FILE"' EXIT

# Use changelog.sh to generate the changelog
"$SCRIPT_DIR/changelog.sh" "$VERSION" "-" "$EXTRA_INSTRUCTIONS" > "$CHANGELOG_FILE"

echo ""
echo "=========================================="
echo "PROPOSED CHANGELOG FOR $TAG"
echo "=========================================="
cat "$CHANGELOG_FILE"
echo ""
echo "=========================================="
echo ""

# Ask for confirmation
read -p "Accept this changelog and create release $TAG? [y/N] " -n 1 -r
echo ""

if [[ ! $REPLY =~ ^[Yy]$ ]]; then
    echo "Release cancelled."
    exit 0
fi

# Create the tag with changelog as message
echo "Creating tag $TAG..."
git tag -a "$TAG" -m "Release $VERSION

$(cat $CHANGELOG_FILE)"

echo "Pushing tag to origin..."
git push origin "$TAG"

echo ""
echo "Release $TAG created and pushed successfully!"
echo "GitHub Actions will create the release with the changelog from the tag message."
echo ""
echo "GitHub release URL: https://github.com/kenn-io/msgvault/releases/tag/$TAG"

# Offer to update nix flake
if [ -f "$REPO_ROOT/flake.nix" ] && command -v nix &>/dev/null; then
    echo ""
    read -p "Update nix flake for $TAG? (opens a PR) [y/N] " -n 1 -r
    echo ""
    if [[ $REPLY =~ ^[Yy]$ ]]; then
        "$SCRIPT_DIR/update-nix-flake.sh" "$TAG"
    else
        echo "Skipped. Run manually later: scripts/update-nix-flake.sh $TAG"
    fi
fi
