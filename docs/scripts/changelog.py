#!/usr/bin/env python3
"""Render changelog fragments or fold them into a release."""

import argparse
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
SECTIONS = ("Upgrade notes", "New features", "Improvements", "Bug fixes")


def fragments():
    notes = {}
    for path in sorted((ROOT / "docs/changelog.d").glob("*.md")):
        text = path.read_text(encoding="utf-8").strip()
        lines = text.splitlines()
        if len(lines) < 2 or not "\n".join(lines[1:]).strip() or lines[0] not in [f"### {s}" for s in SECTIONS] or any(re.match(r"^#{1,3} ", line) for line in lines[1:]):
            raise ValueError(f"{path}: expected one known section heading and a non-empty body")
        notes[path] = (lines[0][4:], "\n".join(lines[1:]).strip())
    return notes


def fold(changelog, notes, paths):
    before, rest = changelog.split("## Unreleased\n", 1)
    unreleased, sep, after = rest.partition("\n## ")
    sections = re.split(r"(?m)^### (.+)\n", unreleased)
    intro = sections[0].strip()
    existing = dict(zip(sections[1::2], sections[2::2]))
    for path in paths:
        section, body = notes[path]
        existing[section] = "\n".join(filter(None, (existing.get(section, "").strip(), body)))
    order = list(SECTIONS) + [name for name in existing if name not in SECTIONS]
    content = "\n\n".join(filter(None, [intro] + [f"### {name}\n\n{existing[name].strip()}" for name in order if name in existing]))
    return before + "## Unreleased\n\n" + (content + "\n" if content else "") + ("\n## " + after if sep else "")


def render():
    changelog = (ROOT / "docs/changelog.md").read_text(encoding="utf-8")
    notes = fragments()
    return fold(changelog, notes, notes)


def release_order(paths):
    try:
        shallow = subprocess.check_output(["git", "rev-parse", "--is-shallow-repository"], cwd=ROOT, stderr=subprocess.DEVNULL).strip()
        if shallow != b"false":
            raise ValueError("release needs full Git history")
        output = subprocess.check_output(["git", "log", "--first-parent", "--reverse", "--no-renames", "--diff-filter=A", "--name-only", "--format=", "-z", "--", "docs/changelog.d"], cwd=ROOT, stderr=subprocess.DEVNULL)
    except (OSError, subprocess.CalledProcessError) as exc:
        raise ValueError("release needs full Git history") from exc
    # Enumerating additions gives a re-added filename its latest position.
    positions = {ROOT / os.fsdecode(item.lstrip(b"\n")): i for i, item in enumerate(output.split(b"\0")) if item.lstrip(b"\n")}
    return sorted(paths, key=lambda path: (positions.get(path, len(output)), path.name))


def main():
    parser = argparse.ArgumentParser()
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("render").add_argument("output", type=Path)
    release = commands.add_parser("release")
    release.add_argument("version")
    release.add_argument("date")
    args = parser.parse_args()
    try:
        if args.command == "release" and (not re.fullmatch(r"\d+\.\d+\.\d+", args.version) or not re.fullmatch(r"\d{4}-\d\d-\d\d", args.date)):
            raise ValueError("release requires a version X.Y.Z and date YYYY-MM-DD")
        if args.command == "render":
            args.output.write_text(render(), encoding="utf-8")
        else:
            path = ROOT / "docs/changelog.md"
            changelog = path.read_text(encoding="utf-8")
            notes = fragments()
            if re.search(rf"(?m)^## {re.escape(args.version)}\s*$", changelog):
                raise ValueError(f"version {args.version} already exists")
            result = fold(changelog, notes, release_order(notes))
            if not result.split("## Unreleased\n", 1)[1].partition("\n## ")[0].strip():
                raise ValueError("no release notes")
            result = result.replace("## Unreleased\n", f"## Unreleased\n\n## {args.version}\n<small>{args.date}</small>\n", 1)
            path.write_text(result, encoding="utf-8")
            for fragment in notes:
                fragment.unlink()
    except (OSError, ValueError) as exc:
        print(exc, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
