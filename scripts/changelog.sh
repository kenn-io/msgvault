#!/bin/bash
# Generate a changelog since the last release using an AI agent
# Usage: ./scripts/changelog.sh [version] [start_tag] [extra_instructions]
# Set CHANGELOG_AGENT=claude to use claude instead of codex (default)
# If version is not provided, uses "NEXT" as placeholder
# If start_tag is "-" or empty, auto-detects the previous tag

set -e

VERSION="${1:-NEXT}"
START_TAG="$2"
EXTRA_INSTRUCTIONS="$3"
AGENT="${CHANGELOG_AGENT:-codex}"
REPO_ROOT=$(git rev-parse --show-toplevel)
CHANGELOG_PATHS=(
    .
    ':(exclude)docs/**'
    ':(exclude)README.md'
    ':(exclude)CLAUDE.md'
    ':(exclude)AGENTS.md'
)

# Determine the starting point
if [ -n "$START_TAG" ] && [ "$START_TAG" != "-" ]; then
    # Use provided start tag
    RANGE="$START_TAG..HEAD"
    RANGE_LABEL="$START_TAG"
    echo "Generating changelog from $START_TAG to HEAD..." >&2
else
    # Auto-detect previous tag
    PREV_TAG=$(git -C "$REPO_ROOT" describe --tags --abbrev=0 2>/dev/null || echo "")
    if [ -z "$PREV_TAG" ]; then
        # No previous tag, use first commit
        FIRST_COMMIT=$(git -C "$REPO_ROOT" rev-list --max-parents=0 HEAD)
        RANGE="$FIRST_COMMIT..HEAD"
        RANGE_LABEL="$FIRST_COMMIT"
        echo "No previous release found. Generating changelog for all commits..." >&2
    else
        RANGE="$PREV_TAG..HEAD"
        RANGE_LABEL="$PREV_TAG"
        echo "Generating changelog from $PREV_TAG to HEAD..." >&2
    fi
fi

# Get commit log for changelog generation. Documentation-only changes are
# intentionally excluded from release-note material.
COMMITS=$(git -C "$REPO_ROOT" log "$RANGE" --pretty=format:"- %s (%h)" --no-merges -- "${CHANGELOG_PATHS[@]}")
DIFF_STAT=$(git -C "$REPO_ROOT" diff --stat "$RANGE" -- "${CHANGELOG_PATHS[@]}")

if [ -z "$COMMITS" ]; then
    echo "No non-documentation commits since $RANGE_LABEL" >&2
    exit 0
fi

# Use AI agent to generate the changelog
echo "Using $AGENT to generate changelog..." >&2

TMPFILE=$(mktemp)
PROMPTFILE=$(mktemp)
trap 'rm -f "$TMPFILE" "$PROMPTFILE"' EXIT

cat > "$PROMPTFILE" <<EOF
You are generating a changelog for msgvault version $VERSION.

msgvault is an offline Gmail archive tool that exports and stores email data locally
with full-text search capabilities and a terminal UI for browsing archived messages.

IMPORTANT: Do NOT use any tools. Do NOT run any shell commands. Do NOT search or read any files.
All the information you need is provided below. Simply analyze the commit messages and output the changelog.

Here are the commits since the last release:
$COMMITS

Here is the diff summary:
$DIFF_STAT

Please generate a concise, user-focused changelog. Group changes into sections like:
- New Features
- Improvements
- Bug Fixes

Focus on user-visible changes. Skip internal refactoring unless it affects users.
Skip documentation-only changes and do not add documentation-update bullets.
Keep descriptions brief (one line each). Use present tense.
Do NOT mention bugs that were introduced and fixed within this same release cycle.
${EXTRA_INSTRUCTIONS:+

When writing the changelog, look for these features or improvements in the commit log above: $EXTRA_INSTRUCTIONS
Do NOT search files, read code, or do any analysis outside of the commit log provided above.}
Output ONLY the changelog content, no preamble.
EOF

case "$AGENT" in
    codex)
        codex exec --skip-git-repo-check --sandbox read-only -c reasoning_effort=high -o "$TMPFILE" - >/dev/null < "$PROMPTFILE"
        ;;
    claude)
        claude --print < "$PROMPTFILE" > "$TMPFILE"
        ;;
    *)
        echo "Error: unknown CHANGELOG_AGENT '$AGENT' (expected 'codex' or 'claude')" >&2
        exit 1
        ;;
esac

cat "$TMPFILE"
