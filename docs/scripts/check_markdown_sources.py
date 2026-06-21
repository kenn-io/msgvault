#!/usr/bin/env python3
from __future__ import annotations

import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parents[1]
FRONTMATTER_LIMIT = 80
EXCLUDED_DIRS = {
    ".venv",
    ".vercel",
    "assets",
    "diagrams",
    "internal",
    "screenshots",
    "scripts",
    "site",
    "superpowers",
}
ADMONITION_DIRECTIVE_RE = re.compile(
    r'!!!\s+[A-Za-z][\w-]*(?:\s+"(?:[^"\\]|\\.)*")?\s*'
)
PUBLIC_MARKDOWN = {
    "api-server.md",
    "architecture/overview.md",
    "architecture/postgresql.md",
    "architecture/search-ranking.md",
    "architecture/storage.md",
    "changelog.md",
    "cli-reference.md",
    "configuration.md",
    "development.md",
    "faq.md",
    "guides/oauth-setup.md",
    "guides/remote-deployment.md",
    "guides/verification.md",
    "index.md",
    "introduction.md",
    "setup.md",
    "troubleshooting.md",
    "usage/analytics.md",
    "usage/chat.md",
    "usage/deduplication.md",
    "usage/deletion.md",
    "usage/exporting.md",
    "usage/importing.md",
    "usage/multi-account.md",
    "usage/querying.md",
    "usage/searching.md",
    "usage/text-messages.md",
    "usage/tui.md",
    "usage/vector-search.md",
}
FORBIDDEN_MARKERS = (
    "@astrojs/starlight",
    "<Aside",
    "<Card",
    "<CardGrid",
    "<Screenshot",
    "<style>{`",
    ":::",
    "sl-markdown-content",
    "virtual:starlight",
)


def is_excluded(path: pathlib.Path) -> bool:
    rel = path.relative_to(ROOT)
    if path.name == "README.md":
        return True
    for part in rel.parts[:-1]:
        if part in EXCLUDED_DIRS:
            return True
        if part.startswith("zensical-public-docs."):
            return True
    return False


def public_markdown_files() -> list[pathlib.Path]:
    return [ROOT / path for path in sorted(PUBLIC_MARKDOWN)]


def check_frontmatter(path: pathlib.Path, lines: list[str], errors: list[str]) -> None:
    if not lines or lines[0] != "---":
        errors.append(f"{path.relative_to(ROOT)}: missing YAML frontmatter")
        return

    closing = next(
        (
            index
            for index, line in enumerate(lines[1:FRONTMATTER_LIMIT], start=1)
            if line == "---"
        ),
        None,
    )
    if closing is None:
        errors.append(
            f"{path.relative_to(ROOT)}: missing closing YAML frontmatter delimiter"
        )
        return

    frontmatter = "\n".join(lines[1:closing])
    if not re.search(r"^title:\s+\S", frontmatter, flags=re.MULTILINE):
        errors.append(f"{path.relative_to(ROOT)}: missing title in YAML frontmatter")
    if not re.search(r"^description:\s+\S", frontmatter, flags=re.MULTILINE):
        errors.append(
            f"{path.relative_to(ROOT)}: missing description in YAML frontmatter"
        )


def check_admonitions(path: pathlib.Path, lines: list[str], errors: list[str]) -> None:
    for index, line in enumerate(lines):
        stripped = line.strip()
        if not stripped.startswith("!!! "):
            continue
        if ADMONITION_DIRECTIVE_RE.fullmatch(stripped) is None:
            errors.append(
                f"{path.relative_to(ROOT)}:{index + 1}: "
                "malformed or collapsed admonition"
            )
            continue

        following = next(
            (
                candidate
                for candidate in lines[index + 1 :]
                if candidate.strip()
            ),
            None,
        )
        if following is None or not following.startswith("    "):
            errors.append(
                f"{path.relative_to(ROOT)}:{index + 1}: "
                "admonition body must be indented"
            )


def check_forbidden_markers(path: pathlib.Path, text: str, errors: list[str]) -> None:
    if re.search(r"(?m)^import\s", text):
        errors.append(
            f"{path.relative_to(ROOT)}: contains unsupported MDX import statement"
        )
    for marker in FORBIDDEN_MARKERS:
        if marker in text:
            errors.append(
                f"{path.relative_to(ROOT)}: contains unsupported MDX/Starlight marker {marker!r}"
            )


def main() -> None:
    errors: list[str] = []

    for path in public_markdown_files():
        if not path.exists():
            errors.append(f"{path.relative_to(ROOT)}: missing public Markdown file")
            continue
        text = path.read_text(encoding="utf-8")
        lines = text.splitlines()
        check_frontmatter(path, lines, errors)
        check_admonitions(path, lines, errors)
        check_forbidden_markers(path, text, errors)

    if errors:
        print("docs markdown source check failed:", file=sys.stderr)
        for error in errors:
            print(f"  {error}", file=sys.stderr)
        raise SystemExit(1)

    print("docs markdown source checks passed")


if __name__ == "__main__":
    main()
