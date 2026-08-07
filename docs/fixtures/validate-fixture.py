#!/usr/bin/env python3
"""Validate a hydrated or publish-ready documentation fixture directory."""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fixture_lib import validate_fixture_directory  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--fixture-dir", type=Path, required=True)
    parser.add_argument("--allow-pending-review", action="store_true")
    args = parser.parse_args()
    manifest = validate_fixture_directory(args.fixture_dir, require_review=not args.allow_pending_review)
    print(f"fixture valid: {manifest['fixture']['message_count']} messages, {manifest['fixture']['participant_count']} participants")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
