#!/usr/bin/env python3
from __future__ import annotations

import json
import math
import pathlib
import sys
from typing import Any

ROOT = pathlib.Path(__file__).resolve().parents[1]
VERCEL = ROOT / "vercel.json"

TEMPORARY = {
    "/install.sh": "https://raw.githubusercontent.com/kenn-io/msgvault/main/scripts/install.sh",
    "/install.ps1": "https://raw.githubusercontent.com/kenn-io/msgvault/main/scripts/install.ps1",
}


def fail(message: str) -> None:
    print(f"FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def reject_json_constant(constant: str) -> None:
    raise ValueError(f"non-finite numeric constant {constant}")


def validate_finite_json_numbers(path: str, value: object) -> None:
    if isinstance(value, bool):
        return
    if isinstance(value, float):
        if not math.isfinite(value):
            fail(f"{path} contains non-finite number")
        return
    if isinstance(value, dict):
        for key, item in value.items():
            validate_finite_json_numbers(f"{path}.{key}", item)
    elif isinstance(value, list):
        for index, item in enumerate(value):
            validate_finite_json_numbers(f"{path}[{index}]", item)


def load_vercel() -> dict[str, Any]:
    try:
        data = json.loads(
            VERCEL.read_text(encoding="utf-8"),
            parse_constant=reject_json_constant,
        )
    except FileNotFoundError:
        fail("missing vercel.json")
    except json.JSONDecodeError as error:
        fail(f"invalid vercel.json: {error}")
    except ValueError as error:
        fail(f"invalid vercel.json: {error}")

    if not isinstance(data, dict):
        fail("vercel.json must contain an object")
    validate_finite_json_numbers("vercel.json", data)
    return data


def collect_redirects(data: dict[str, object]) -> dict[str, dict[str, object]]:
    raw_redirects = data.get("redirects", [])
    if not isinstance(raw_redirects, list):
        fail("vercel redirects must be a list")
    if len(raw_redirects) != len(TEMPORARY):
        fail(f"vercel redirects must contain exactly {len(TEMPORARY)} entries")

    redirects: dict[str, dict[str, object]] = {}
    for index, item in enumerate(raw_redirects):
        if not isinstance(item, dict):
            fail(f"redirect entry {index} must be an object")
        if set(item) != {"source", "destination", "permanent"}:
            fail(f"redirect entry {index} must contain source, destination, and permanent only")
        source = item.get("source")
        destination = item.get("destination")
        permanent = item.get("permanent")
        if not isinstance(source, str) or not source:
            fail(f"redirect entry {index} missing source")
        if not isinstance(destination, str) or not destination:
            fail(f"redirect entry {index} missing destination")
        if not isinstance(permanent, bool):
            fail(f"redirect entry {index} permanent must be boolean")
        if source in redirects:
            fail(f"duplicate redirect source {source}")
        redirects[source] = item
    return redirects


def main() -> None:
    data = load_vercel()
    if "framework" not in data or data["framework"] is not None:
        fail("vercel framework must be null")
    if data.get("installCommand") != "uv sync --frozen --no-dev":
        fail("unexpected Vercel installCommand")
    if data.get("buildCommand") != "uv run --frozen bash ./vercel-build.sh":
        fail("unexpected Vercel buildCommand")
    if data.get("outputDirectory") != "site":
        fail("unexpected Vercel outputDirectory")
    if data.get("trailingSlash") is not True:
        fail("vercel.json must set trailingSlash true")

    redirects = collect_redirects(data)
    for source, destination in TEMPORARY.items():
        item = redirects.get(source)
        if not item:
            fail(f"missing temporary redirect {source}")
        if item.get("destination") != destination or item.get("permanent") is not False:
            fail(f"incorrect temporary redirect {source}")

    print("vercel redirect checks passed")


if __name__ == "__main__":
    main()
