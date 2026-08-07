#!/usr/bin/env python3
"""Select a deterministic, connected Enron MBOX slice for documentation evidence."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fixture_lib import (  # noqa: E402
    FIXTURE_NAME,
    ATTRIBUTION_PROVIDER,
    CONTENT_PREFILTER,
    DEDUPLICATE_BY,
    FALLBACK_THREAD_WINDOW_HOURS,
    MIN_RELATIONSHIP_RESULTS,
    MIN_SELECTED_SENDERS,
    MIN_SELECTED_SUBJECTS,
    message_recipient_edges,
    SELECTION_ALGORITHM,
    SOURCE_RELEASE,
    validate_source_archive,
    SOURCE_URL,
    canonical_exclusions,
    canonical_json,
    contains_mime_attachment,
    iter_source_records,
    participant_identifiers,
    select_records,
    select_owner_identifier,
    sha256_bytes,
    sha256_file,
    write_mbox,
)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source-archive", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--exclusions", type=Path)
    parser.add_argument("--message-cap", type=int, default=80)
    parser.add_argument("--neighbor-limit", type=int, default=16)
    args = parser.parse_args()
    if args.message_cap < 20 or args.neighbor_limit < 2:
        parser.error("message cap must be at least 20 and neighbor limit at least 2")
    try:
        validate_source_archive(args.source_archive)
    except ValueError as error:
        parser.error(str(error))
    args.output_dir.mkdir(parents=True, exist_ok=True)
    exclusions, exclusions_sha256 = canonical_exclusions(args.exclusions)
    records = list(iter_source_records(args.source_archive))
    selected, seed, _ = select_records(records, exclusions, args.message_cap, args.neighbor_limit)
    owner_identifier = select_owner_identifier(selected)
    selected_participants = participant_identifiers(selected)
    if len(selected_participants) < 4:
        raise SystemExit("selected fixture has fewer than four participants")
    mbox_bytes = write_mbox(selected)
    fixture_path = args.output_dir / FIXTURE_NAME
    fixture_path.write_bytes(__import__("gzip").compress(mbox_bytes, mtime=0))
    manifest = {
        "schema_version": 1,
        "source": {
            "url": SOURCE_URL,
            "release": SOURCE_RELEASE,
            "sha256": sha256_file(args.source_archive),
            "message_count_scanned": len(records),
        },
        "selection": {
            "algorithm": SELECTION_ALGORITHM,
            "content_prefilter": CONTENT_PREFILTER,
            "deduplicate_by": DEDUPLICATE_BY,
            "fallback_thread_window_hours": FALLBACK_THREAD_WINDOW_HOURS,
            "minimum_distinct_senders": MIN_SELECTED_SENDERS,
            "minimum_distinct_subjects": MIN_SELECTED_SUBJECTS,
            "message_cap": args.message_cap,
            "neighbor_limit": args.neighbor_limit,
            "seed_mailbox": seed,
            "selected_participants": selected_participants,
            "message_recipient_edges": message_recipient_edges(selected),
            "excluded_message_ids": exclusions["message_ids"],
            "excluded_addresses": exclusions["addresses"],
            "exclusions_sha256": exclusions_sha256,
        },
        "fixture": {
            "path": FIXTURE_NAME,
            "sha256": sha256_file(fixture_path),
            "message_count": len(selected),
            "participant_count": len(selected_participants),
            "owner_identifier": owner_identifier,
            "attachment_count": sum(contains_mime_attachment(record["raw"]) for record in selected),
            "sender_count": len({record["sender"] for record in selected if record["sender"]}),
            "subject_count": len({record["subject"] for record in selected}),
            "minimum_relationship_results": MIN_RELATIONSHIP_RESULTS,
        },
        "attribution": {
            "provider": ATTRIBUTION_PROVIDER,
            "license_note": "See the source release terms and attribution guidance at the source URL.",
        },
        "manual_review": {"status": "pending", "reviewed_message_count": 0, "reviewed_fixture_sha256": None},
    }
    (args.output_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")
    template = Path(__file__).with_name("README.template.md")
    (args.output_dir / "README.md").write_text(template.read_text(encoding="utf-8"), encoding="utf-8")
    print(json.dumps({"fixture": str(fixture_path), "messages": len(selected), "participants": manifest["fixture"]["participant_count"], "fixture_sha256": manifest["fixture"]["sha256"]}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
