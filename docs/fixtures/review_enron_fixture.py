#!/usr/bin/env python3
"""Render and attest the required human review of every selected message."""

from __future__ import annotations

import argparse
import gzip
import json
import sys
from datetime import UTC, datetime
from email import policy
from email.parser import BytesParser
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fixture_lib import (  # noqa: E402
    FIXTURE_NAME,
    MANIFEST_NAME,
    REVIEWABLE_CONTENT_TYPES,
    canonical_json,
    count_mbox_messages,
    iter_non_body_mime_parts,
    review_attestation_digest,
    sha256_file,
    validate_fixture_directory,
)


def render_report(
    fixture_path: Path,
    report_path: Path,
    attestation_digest: str | None = None,
) -> tuple[int, str]:
    digest = sha256_file(fixture_path)
    with gzip.open(fixture_path, "rb") as stream:
        data = stream.read()
    separators = [index for index, line in enumerate(data.splitlines(keepends=True)) if line.startswith(b"From ")]
    messages: list[bytes] = []
    lines = data.splitlines(keepends=True)
    for position, start in enumerate(separators):
        end = separators[position + 1] if position + 1 < len(separators) else len(lines)
        messages.append(b"".join(lines[start + 1 : end]))
    with report_path.open("w", encoding="utf-8") as output:
        output.write(f"MSGVAULT FIXTURE REVIEW\nfixture_sha256={digest}\nmessage_count={len(messages)}\n\n")
        if attestation_digest is not None:
            output.write(f"review_attestation_sha256={attestation_digest}\n\n")
        for index, raw in enumerate(messages, 1):
            message = BytesParser(policy=policy.default).parsebytes(raw.replace(b"\n>From ", b"\nFrom "))
            output.write(f"===== MESSAGE {index}/{len(messages)} =====\n")
            output.write("MESSAGE HEADERS:\n")
            for name, value in message.items():
                output.write(f"{name}: {value}\n")
            output.write("\n")
            if message.is_multipart():
                if message.preamble is not None:
                    output.write(f"MESSAGE PREAMBLE:\n{message.preamble}\n\n")
                if message.epilogue is not None:
                    output.write(f"MESSAGE EPILOGUE:\n{message.epilogue}\n\n")
            nested_parts = list(message.walk())[1:]
            for part_index, part in enumerate(nested_parts, 1):
                output.write(f"MIME PART {part_index}/{len(nested_parts)} HEADERS:\n")
                for name, value in part.items():
                    output.write(f"{name}: {value}\n")
                output.write("\n")
                if part.is_multipart():
                    if part.preamble is not None:
                        output.write(f"MIME PART {part_index}/{len(nested_parts)} PREAMBLE:\n{part.preamble}\n\n")
                    if part.epilogue is not None:
                        output.write(f"MIME PART {part_index}/{len(nested_parts)} EPILOGUE:\n{part.epilogue}\n\n")
            non_body_parts = list(iter_non_body_mime_parts(message))
            if non_body_parts:
                output.write("NON-BODY MIME PARTS (publication rejects these):\n")
                for part in non_body_parts:
                    output.write(
                        f"- content-type={part.get_content_type()} "
                        f"content-disposition={part.get_content_disposition() or ''}\n"
                    )
                output.write("\n")
            text_parts = [
                part for part in message.walk()
                if not part.is_multipart() and part.get_content_type() in REVIEWABLE_CONTENT_TYPES
            ]
            for part_index, part in enumerate(text_parts, 1):
                output.write(
                    f"TEXT MIME PART {part_index}/{len(text_parts)}: "
                    f"content-type={part.get_content_type()} "
                    f"content-disposition={part.get_content_disposition() or ''}\n\n"
                )
                try:
                    body = part.get_content()
                except (LookupError, UnicodeError):
                    payload = part.get_payload(decode=True)
                    body = payload.decode("utf-8", "replace") if isinstance(payload, bytes) else str(payload)
                output.write(body if isinstance(body, str) else str(body))
                output.write("\n\n")
            if not text_parts:
                output.write("(no reviewable text MIME leaves)\n\n")
    return len(messages), digest


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--fixture-dir", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    parser.add_argument("--complete", action="store_true")
    parser.add_argument("--reviewer")
    args = parser.parse_args()
    manifest = validate_fixture_directory(args.fixture_dir, require_review=False)
    fixture_path = args.fixture_dir / FIXTURE_NAME
    digest = sha256_file(fixture_path)
    attestation_digest = review_attestation_digest(manifest, digest)
    count, digest = render_report(fixture_path, args.report, attestation_digest)
    if not args.complete:
        print(f"review report written: {args.report} ({count} messages, {digest})")
        return 0
    if not args.reviewer:
        parser.error("--reviewer is required with --complete")
    expected = f"I_HAVE_READ_EVERY_MESSAGE {digest}"
    confirmation = input(f"Read every message in {args.report}; type '{expected}' to attest: ")
    if confirmation.strip() != expected:
        raise SystemExit("review not attested")
    if count != manifest["fixture"]["message_count"] or count_mbox_messages(fixture_path) != count:
        raise SystemExit("review report message count is stale")
    manifest["manual_review"] = {
        "status": "complete",
        "reviewed_message_count": count,
        "reviewed_fixture_sha256": digest,
        "review_attestation_sha256": attestation_digest,
        "reviewer": args.reviewer,
        "reviewed_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
    }
    manifest_path = args.fixture_dir / MANIFEST_NAME
    temporary = manifest_path.with_suffix(".json.tmp")
    temporary.write_bytes(canonical_json(manifest) + b"\n")
    temporary.replace(manifest_path)
    print(f"review complete: {count} messages, {digest}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
