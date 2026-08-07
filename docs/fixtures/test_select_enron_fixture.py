#!/usr/bin/env python3
"""Tests for deterministic Enron fixture selection."""

from __future__ import annotations

import unittest
import gzip
import io
import json
import os
import subprocess
import tarfile
import tempfile
from pathlib import Path
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent))
from fixture_lib import (
    ATTRIBUTION_PROVIDER,
    CONTENT_PREFILTER,
    DEDUPLICATE_BY,
    FALLBACK_THREAD_WINDOW_HOURS,
    MANIFEST_SCHEMA_VERSION,
    MIN_NEIGHBOR_LIMIT,
    MIN_RELATIONSHIP_RESULTS,
    MIN_SELECTED_SENDERS,
    MIN_SELECTED_SUBJECTS,
    SELECTION_ALGORITHM,
    SOURCE_RELEASE,
    SOURCE_SHA256,
    SOURCE_URL,
    canonical_json,
    iter_source_records,
    parse_message,
    review_filter_reason,
    review_attestation_digest,
    select_owner_identifier,
    message_recipient_edges,
    select_records,
    sha256_bytes,
    sha256_file,
    validate_fixture_directory,
    write_mbox,
)
from review_enron_fixture import render_report


def synthetic_record(message_id: str, sender: str, recipient: str, date: str, subject: str, body: str = "Operational update") -> dict:
    raw = (
        f"From: {sender}\nTo: {recipient}\nDate: {date}\n"
        f"Message-ID: {message_id}\nSubject: {subject}\n\n{body}\n"
    ).encode()
    record = parse_message(raw)
    record["source_position"] = 0
    return record


def write_test_fixture(
    fixture_dir: Path,
    records: list[dict],
    exclusions: dict[str, list[str]] | None = None,
    manual_review: dict | None = None,
) -> dict:
    exclusions = exclusions or {"message_ids": [], "addresses": []}
    fixture_path = fixture_dir / "enron-web-fixture.mbox.gz"
    with gzip.open(fixture_path, "wb") as stream:
        stream.write(write_mbox(records))
    participants = sorted({participant for record in records for participant in record["participants"]})
    manifest = {
        "schema_version": MANIFEST_SCHEMA_VERSION,
        "source": {
            "url": SOURCE_URL,
            "release": SOURCE_RELEASE,
            "sha256": SOURCE_SHA256,
            "message_count_scanned": len(records),
        },
        "selection": {
            "algorithm": SELECTION_ALGORITHM,
            "content_prefilter": CONTENT_PREFILTER,
            "deduplicate_by": DEDUPLICATE_BY,
            "excluded_message_ids": exclusions["message_ids"],
            "excluded_addresses": exclusions["addresses"],
            "exclusions_sha256": sha256_bytes(canonical_json(exclusions)),
            "fallback_thread_window_hours": FALLBACK_THREAD_WINDOW_HOURS,
            "minimum_distinct_senders": MIN_SELECTED_SENDERS,
            "minimum_distinct_subjects": MIN_SELECTED_SUBJECTS,
            "message_cap": 20,
            "neighbor_limit": MIN_NEIGHBOR_LIMIT,
            "seed_mailbox": select_owner_identifier(records),
            "selected_participants": participants,
            "message_recipient_edges": message_recipient_edges(records),
        },
        "fixture": {
            "path": fixture_path.name,
            "sha256": sha256_file(fixture_path),
            "message_count": len(records),
            "participant_count": len(participants),
            "owner_identifier": select_owner_identifier(records),
            "attachment_count": 0,
            "sender_count": len({record["sender"] for record in records if record["sender"]}),
            "subject_count": len({record["subject"] for record in records}),
            "minimum_relationship_results": MIN_RELATIONSHIP_RESULTS,
        },
        "attribution": {
            "provider": ATTRIBUTION_PROVIDER,
            "license_note": "See the source release terms and attribution guidance at the source URL.",
        },
        "manual_review": manual_review or {
            "status": "pending",
            "reviewed_message_count": 0,
            "reviewed_fixture_sha256": None,
        },
    }
    (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")
    (fixture_dir / "README.md").write_text("fixture test\n", encoding="utf-8")
    return manifest


class ReviewFilterTest(unittest.TestCase):
    def test_selector_rejects_unpinned_source_archive(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            source_archive = temporary_path / "source.tar.gz"
            with tarfile.open(source_archive, "w:gz") as archive:
                raw = (
                    b"From: sender@example.com\n"
                    b"To: recipient@example.com\n"
                    b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
                    b"Message-ID: <source@example.com>\n"
                    b"Subject: Source\n\nOperational message.\n"
                )
                member = tarfile.TarInfo("maildir/example/inbox/1")
                member.size = len(raw)
                archive.addfile(member, io.BytesIO(raw))

            result = subprocess.run(
                [
                    sys.executable,
                    str(repo_root / "docs/fixtures/select_enron_fixture.py"),
                    "--source-archive",
                    str(source_archive),
                    "--output-dir",
                    str(temporary_path / "output"),
                ],
                cwd=repo_root,
                capture_output=True,
                text=True,
                check=False,
            )

        self.assertNotEqual(0, result.returncode)
        self.assertIn("does not match the pinned source SHA-256", result.stderr)

    def test_bcc_recipient_is_a_participant_and_exclusion_target(self) -> None:
        bcc_record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Bcc: hidden@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <bcc@example.com>\n"
            b"Subject: Bcc\n\n"
            b"Operational message.\n"
        )
        companion = parse_message(
            b"From: recipient@example.com\n"
            b"To: sender@example.com\n"
            b"Date: Tue, 2 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <companion@example.com>\n"
            b"Subject: Reply\n\n"
            b"Operational reply.\n"
        )
        bcc_record["source_position"] = 1
        companion["source_position"] = 2
        records = [bcc_record, companion]

        self.assertIn("hidden@example.com", bcc_record["participants"])
        selected, _, _ = select_records(
            records,
            {"message_ids": [], "addresses": ["hidden@example.com"]},
            20,
            2,
        )

        self.assertNotIn(bcc_record["message_id"], {record["message_id"] for record in selected})

    def test_selection_uses_real_tar_records_and_omits_obvious_sensitive_messages(self) -> None:
        messages = [
            ("clean-a.eml", "<clean-a@example.com>", "a@example.com", "b@example.com", "Operations update"),
            ("clean-b.eml", "<clean-b@example.com>", "b@example.com", "a@example.com", "Operations reply"),
            ("clean-c.eml", "<clean-c@example.com>", "a@example.com", "c@example.com", "Operations note"),
            ("credential.eml", "<credential@example.com>", "a@example.com", "b@example.com", "Password: secret"),
            ("phone.eml", "<phone@example.com>", "a@example.com", "b@example.com", "Call 713-555-0123"),
        ]
        with tempfile.TemporaryDirectory() as temporary:
            archive_path = Path(temporary) / "messages.tar.gz"
            with tarfile.open(archive_path, "w:gz") as archive:
                for name, message_id, sender, recipient, body in messages:
                    raw = (
                        f"From: {sender}\nTo: {recipient}\nDate: Mon, 1 Jan 2001 12:00:00 +0000\n"
                        f"Message-ID: {message_id}\nSubject: Fixture\n\n{body}\n"
                    ).encode()
                    info = tarfile.TarInfo(name)
                    info.size = len(raw)
                    archive.addfile(info, io.BytesIO(raw))

            records = list(iter_source_records(archive_path))
            selected, _, _ = select_records(records, {"message_ids": [], "addresses": []}, 20, 2)

        self.assertEqual(
            {"<clean-a@example.com>", "<clean-b@example.com>", "<clean-c@example.com>"},
            {record["message_id"] for record in selected},
        )

    def test_rejects_plaintext_credentials(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <credentials@example.com>\n"
            b"Subject: Access\n\n"
            b"The username is demo and the password is secret.\n"
        )

        self.assertEqual("credential-marker", review_filter_reason(record))

    def test_rejects_phone_numbers(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <phone@example.com>\n"
            b"Subject: Contact\n\n"
            b"Call me at (713) 555-0123.\n"
        )

        self.assertEqual("phone-number", review_filter_reason(record))

    def test_rejects_unreviewed_attachment_reference(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <attachment@example.com>\n"
            b"Subject: Report\n\n"
            b"Please see the attached report.\n"
            b" - report.doc\n"
        )

        self.assertEqual("attachment", review_filter_reason(record))

    def test_rejects_unnamed_inline_binary_mime_part(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <inline-image@example.com>\n"
            b"Subject: Inline image\n"
            b"Content-Type: multipart/mixed; boundary=fixture\n\n"
            b"--fixture\n"
            b"Content-Type: text/plain\n\n"
            b"The visible message body.\n"
            b"--fixture\n"
            b"Content-Type: image/png\n"
            b"Content-Transfer-Encoding: base64\n\n"
            b"iVBORw0KGgo=\n"
            b"--fixture--\n"
        )

        self.assertEqual("attachment", review_filter_reason(record))

    def test_rejects_attached_message_mime_part(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <attached-message@example.com>\n"
            b"Subject: Attached message\n"
            b"Content-Type: multipart/mixed; boundary=outer\n\n"
            b"--outer\n"
            b"Content-Type: text/plain\n\n"
            b"The visible message body.\n"
            b"--outer\n"
            b"Content-Type: message/rfc822\n\n"
            b"From: nested@example.com\n"
            b"To: recipient@example.com\n"
            b"Subject: Nested\n\n"
            b"Unreviewed nested message.\n"
            b"--outer--\n"
        )

        self.assertEqual("attachment", review_filter_reason(record))

    def test_rejects_multipart_attachment_subtree(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <multipart-attachment@example.com>\n"
            b"Subject: Multipart attachment\n"
            b"Content-Type: multipart/mixed; boundary=outer\n\n"
            b"--outer\n"
            b"Content-Type: multipart/alternative; boundary=inner\n"
            b"Content-Disposition: attachment\n\n"
            b"--inner\n"
            b"Content-Type: text/plain\n\n"
            b"Unreviewed attached text.\n"
            b"--inner--\n"
            b"--outer--\n"
        )

        self.assertEqual("attachment", review_filter_reason(record))

    def test_rejects_headerless_message_in_multipart_digest(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <digest@example.com>\n"
            b"Subject: Digest\n"
            b"Content-Type: multipart/digest; boundary=digest\n\n"
            b"--digest\n\n"
            b"Unreviewed headerless nested message.\n"
            b"--digest--\n"
        )

        self.assertEqual("attachment", review_filter_reason(record))

    def test_review_report_renders_every_text_leaf(self) -> None:
        raw_message = (
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <alternatives@example.com>\n"
            b"Subject: Alternatives\n"
            b"Content-Type: multipart/alternative; boundary=body\n\n"
            b"--body\n"
            b"Content-Type: text/plain\n\n"
            b"Plain alternative that must be reviewed.\n"
            b"--body\n"
            b"Content-Type: text/html\n\n"
            b"<p>HTML alternative that must also be reviewed.</p>\n"
            b"--body--\n"
        )
        mbox = b"From sender@example.com Mon Jan  1 00:00:00 2001\n" + raw_message + b"\n"

        with tempfile.TemporaryDirectory() as temporary:
            fixture_path = Path(temporary) / "fixture.mbox.gz"
            report_path = Path(temporary) / "review.txt"
            with gzip.open(fixture_path, "wb") as stream:
                stream.write(mbox)

            count, _ = render_report(fixture_path, report_path)
            report = report_path.read_text(encoding="utf-8")

        self.assertEqual(1, count)
        self.assertIn("TEXT MIME PART 1/2", report)
        self.assertIn("TEXT MIME PART 2/2", report)
        self.assertIn("Plain alternative that must be reviewed.", report)
        self.assertIn("HTML alternative that must also be reviewed.", report)

    def test_review_report_renders_every_repeated_decoded_header(self) -> None:
        raw_message = (
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Reply-To: reply@example.com\n"
            b"X-Audit: first value\n"
            b"X-Audit: second value\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <headers@example.com>\n"
            b"Subject: Headers\n\n"
            b"Operational message.\n"
        )
        mbox = b"From sender@example.com Mon Jan  1 12:00:00 2001\n" + raw_message + b"\n"

        with tempfile.TemporaryDirectory() as temporary:
            fixture_path = Path(temporary) / "fixture.mbox.gz"
            report_path = Path(temporary) / "review.txt"
            with gzip.open(fixture_path, "wb") as stream:
                stream.write(mbox)

            render_report(fixture_path, report_path)
            report = report_path.read_text(encoding="utf-8")

        self.assertIn("Reply-To: reply@example.com", report)
        self.assertIn("X-Audit: first value", report)
        self.assertIn("X-Audit: second value", report)
        self.assertEqual(2, report.count("X-Audit:"))

    def test_review_report_renders_headers_for_nested_mime_parts(self) -> None:
        raw_message = (
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <nested-headers@example.com>\n"
            b"Subject: Nested headers\n"
            b"Content-Type: multipart/alternative; boundary=body\n\n"
            b"--body\n"
            b"Content-Type: text/plain\n"
            b"Content-Location: https://example.com/plain.txt\n"
            b"X-Nested-Audit: first value\n"
            b"X-Nested-Audit: second value\n\n"
            b"Plain body.\n"
            b"--body\n"
            b"Content-Type: text/html\n"
            b"Content-Location: https://example.com/html.txt\n\n"
            b"<p>HTML body.</p>\n"
            b"--body--\n"
        )
        mbox = b"From sender@example.com Mon Jan  1 12:00:00 2001\n" + raw_message + b"\n"

        with tempfile.TemporaryDirectory() as temporary:
            fixture_path = Path(temporary) / "fixture.mbox.gz"
            report_path = Path(temporary) / "review.txt"
            with gzip.open(fixture_path, "wb") as stream:
                stream.write(mbox)

            render_report(fixture_path, report_path)
            report = report_path.read_text(encoding="utf-8")

        self.assertIn("MIME PART 1/2 HEADERS:", report)
        self.assertIn("Content-Location: https://example.com/plain.txt", report)
        self.assertIn("Content-Location: https://example.com/html.txt", report)
        self.assertIn("X-Nested-Audit: first value", report)
        self.assertIn("X-Nested-Audit: second value", report)
        self.assertEqual(2, report.count("X-Nested-Audit:"))

    def test_review_report_renders_multipart_preambles_and_epilogues(self) -> None:
        raw_message = (
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <multipart-boundaries@example.com>\n"
            b"Subject: Multipart boundaries\n"
            b"Content-Type: multipart/mixed; boundary=outer\n\n"
            b"Outer preamble that must be reviewed.\n"
            b"--outer\n"
            b"Content-Type: multipart/alternative; boundary=inner\n\n"
            b"Inner preamble that must be reviewed.\n"
            b"--inner\n"
            b"Content-Type: text/plain\n\n"
            b"Visible body.\n"
            b"--inner--\n"
            b"Inner epilogue that must be reviewed.\n"
            b"--outer--\n"
            b"Outer epilogue that must be reviewed.\n"
        )
        mbox = b"From sender@example.com Mon Jan  1 12:00:00 2001\n" + raw_message + b"\n"

        with tempfile.TemporaryDirectory() as temporary:
            fixture_path = Path(temporary) / "fixture.mbox.gz"
            report_path = Path(temporary) / "review.txt"
            with gzip.open(fixture_path, "wb") as stream:
                stream.write(mbox)

            render_report(fixture_path, report_path)
            report = report_path.read_text(encoding="utf-8")

        self.assertIn("MESSAGE PREAMBLE:\nOuter preamble that must be reviewed.", report)
        self.assertIn("MESSAGE EPILOGUE:\nOuter epilogue that must be reviewed.", report)
        self.assertIn("MIME PART 1/2 PREAMBLE:\nInner preamble that must be reviewed.", report)
        self.assertIn("MIME PART 1/2 EPILOGUE:\nInner epilogue that must be reviewed.", report)

    def test_fallback_thread_key_separates_participants_and_time_windows(self) -> None:
        first = synthetic_record(
            "<first@example.com>",
            "sender-a@example.com",
            "recipient-a@example.com",
            "Mon, 1 Jan 2001 12:00:00 +0000",
            "Repeated subject",
        )
        second = synthetic_record(
            "<second@example.com>",
            "sender-b@example.com",
            "recipient-b@example.com",
            "Tue, 2 Jan 2001 12:00:00 +0000",
            "Repeated subject",
        )

        self.assertNotEqual(first["thread_key"], second["thread_key"])

    def test_selection_deduplicates_mailbox_copies(self) -> None:
        duplicate = synthetic_record(
            "<duplicate@example.com>",
            "sender-a@example.com",
            "hub@example.com",
            "Mon, 1 Jan 2001 12:00:00 +0000",
            "Alert",
        )
        duplicate_copy = dict(duplicate)
        duplicate["source_position"] = 2
        duplicate_copy["source_position"] = 1
        records = [
            duplicate,
            duplicate_copy,
            synthetic_record("<project@example.com>", "sender-b@example.com", "hub@example.com", "Mon, 1 Jan 2001 12:01:00 +0000", "Project"),
            synthetic_record("<meeting@example.com>", "sender-c@example.com", "hub@example.com", "Mon, 1 Jan 2001 12:02:00 +0000", "Meeting"),
        ]

        selected, _, _ = select_records(records, {"message_ids": [], "addresses": []}, 20, 2)

        selected_ids = [record["message_id"] for record in selected]
        self.assertEqual(len(selected_ids), len(set(selected_ids)))

    def test_selected_participants_are_union_of_selected_messages(self) -> None:
        records = [
            synthetic_record("<hub@example.com>", "hub@example.com", "seed@example.com", "Mon, 1 Jan 2001 12:00:00 +0000", "Hub"),
            synthetic_record("<outside@example.com>", "seed@example.com", "outside@example.com", "Mon, 1 Jan 2001 12:01:00 +0000", "Outside"),
            synthetic_record("<seed-reply@example.com>", "seed@example.com", "hub@example.com", "Mon, 1 Jan 2001 12:02:00 +0000", "Seed reply"),
            synthetic_record("<sender-c@example.com>", "sender-c@example.com", "hub@example.com", "Mon, 1 Jan 2001 12:03:00 +0000", "Sender C"),
            synthetic_record("<sender-d@example.com>", "sender-d@example.com", "hub@example.com", "Mon, 1 Jan 2001 12:04:00 +0000", "Sender D"),
        ]

        selected, _, participants = select_records(records, {"message_ids": [], "addresses": []}, 20, 2)

        expected = sorted({participant for record in selected for participant in record["participants"]})
        self.assertEqual(expected, participants)
        self.assertIn("outside@example.com", participants)

    def test_validator_rejects_incomplete_or_invalid_manifest_metadata(self) -> None:
        records = [
            synthetic_record(
                f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]
        cases = (
            ("schema_version", lambda manifest: manifest.pop("schema_version"), "schema_version"),
            ("source URL", lambda manifest: manifest["source"].pop("url"), "source.*url"),
            ("source checksum", lambda manifest: manifest["source"].pop("sha256"), "source.*sha256"),
            ("source checksum pin", lambda manifest: manifest["source"].update(sha256="f" * 64), "pinned source archive"),
            ("attribution", lambda manifest: manifest.pop("attribution"), "attribution"),
            ("selection algorithm", lambda manifest: manifest["selection"].pop("algorithm"), "selection.*algorithm"),
            ("content prefilter", lambda manifest: manifest["selection"].pop("content_prefilter"), "selection.*content_prefilter"),
            ("fixture path", lambda manifest: manifest["fixture"].pop("path"), "fixture.*path"),
            ("relationship threshold", lambda manifest: manifest["fixture"].pop("minimum_relationship_results"), "fixture.*minimum_relationship_results"),
            ("fixture checksum format", lambda manifest: manifest["fixture"].update(sha256="not-a-digest"), "fixture.*sha256"),
            ("message cap range", lambda manifest: manifest["selection"].update(message_cap=19), "message_cap"),
            ("neighbor limit range", lambda manifest: manifest["selection"].update(neighbor_limit=1), "neighbor_limit"),
        )

        for name, mutate, expected_error in cases:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as temporary:
                fixture_dir = Path(temporary)
                manifest = write_test_fixture(fixture_dir, records)
                mutate(manifest)
                (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")

                with self.assertRaisesRegex(ValueError, expected_error):
                    validate_fixture_directory(fixture_dir, require_review=False)

    def test_validator_rejects_duplicate_fixture_message_ids(self) -> None:
        records = [
            synthetic_record(
                "<duplicate@example.com>" if index < 2 else f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]
        with tempfile.TemporaryDirectory() as temporary:
            fixture_dir = Path(temporary)
            write_test_fixture(fixture_dir, records)

            with self.assertRaisesRegex(ValueError, "duplicate message IDs"):
                validate_fixture_directory(fixture_dir, require_review=False)

    def test_validator_recomputes_selected_participants_and_count(self) -> None:
        records = [
            synthetic_record(
                f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]
        with tempfile.TemporaryDirectory() as temporary:
            fixture_dir = Path(temporary)
            participants = sorted({participant for record in records for participant in record["participants"]})
            manifest = write_test_fixture(fixture_dir, records)
            manifest["selection"]["selected_participants"] = participants[:-1]
            (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")

            with self.assertRaisesRegex(ValueError, "selected participants"):
                validate_fixture_directory(fixture_dir, require_review=False)

            manifest["selection"]["selected_participants"] = participants
            manifest["fixture"]["participant_count"] = len(participants) - 1
            (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")
            with self.assertRaisesRegex(ValueError, "participant count"):
                validate_fixture_directory(fixture_dir, require_review=False)

    def test_validator_rejects_message_recipient_edge_mismatch(self) -> None:
        records = [
            synthetic_record(
                f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]
        with tempfile.TemporaryDirectory() as temporary:
            fixture_dir = Path(temporary)
            manifest = write_test_fixture(fixture_dir, records)
            manifest["selection"]["message_recipient_edges"][0]["to"] = ["other@example.com"]
            (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")

            with self.assertRaisesRegex(ValueError, "message recipient edges"):
                validate_fixture_directory(fixture_dir, require_review=False)

    def test_validator_rejects_declared_message_and_address_exclusions(self) -> None:
        records = [
            synthetic_record(
                f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]

        with tempfile.TemporaryDirectory() as temporary:
            fixture_dir = Path(temporary)
            write_test_fixture(
                fixture_dir,
                records,
                exclusions={"message_ids": ["<message-0@example.com>"], "addresses": []},
            )

            with self.assertRaisesRegex(ValueError, "excluded message IDs"):
                validate_fixture_directory(fixture_dir, require_review=False)

            write_test_fixture(
                fixture_dir,
                records,
                exclusions={"message_ids": [], "addresses": ["hub@example.com"]},
            )
            with self.assertRaisesRegex(ValueError, "excluded addresses"):
                validate_fixture_directory(fixture_dir, require_review=False)

    def test_validator_rejects_complete_review_without_attestation_digest(self) -> None:
        records = [
            synthetic_record(
                f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]

        with tempfile.TemporaryDirectory() as temporary:
            fixture_dir = Path(temporary)
            fixture_path = fixture_dir / "enron-web-fixture.mbox.gz"
            manifest = write_test_fixture(
                fixture_dir,
                records,
            )
            manifest["manual_review"] = {
                "status": "complete",
                "reviewed_message_count": len(records),
                "reviewed_fixture_sha256": sha256_file(fixture_path),
                "reviewer": "Test reviewer",
                "reviewed_at": "2001-01-01T00:00:00Z",
            }
            (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")

            with self.assertRaisesRegex(ValueError, "attestation digest"):
                validate_fixture_directory(fixture_dir)

    def test_validator_rejects_selection_changes_after_review_attestation(self) -> None:
        records = [
            synthetic_record(
                f"<message-{index}@example.com>",
                f"sender-{index}@example.com",
                "hub@example.com",
                f"Mon, {index + 1} Jan 2001 12:00:00 +0000",
                f"Subject {index}",
            )
            for index in range(5)
        ]

        with tempfile.TemporaryDirectory() as temporary:
            fixture_dir = Path(temporary)
            manifest = write_test_fixture(fixture_dir, records)
            fixture_digest = sha256_file(fixture_dir / "enron-web-fixture.mbox.gz")
            manifest["manual_review"] = {
                "status": "complete",
                "reviewed_message_count": len(records),
                "reviewed_fixture_sha256": fixture_digest,
                "review_attestation_sha256": review_attestation_digest(manifest, fixture_digest),
                "reviewer": "Test reviewer",
                "reviewed_at": "2001-01-01T00:00:00Z",
            }
            (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")
            validate_fixture_directory(fixture_dir)

            manifest["selection"]["excluded_message_ids"] = ["<not-selected@example.com>"]
            manifest["selection"]["exclusions_sha256"] = sha256_bytes(canonical_json({
                "message_ids": ["<not-selected@example.com>"],
                "addresses": [],
            }))
            (fixture_dir / "manifest.json").write_bytes(canonical_json(manifest) + b"\n")

            with self.assertRaisesRegex(ValueError, "attestation digest"):
                validate_fixture_directory(fixture_dir)

    def test_hydrator_rejects_lock_for_uncontrolled_branch(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        lock = json.loads((repo_root / "docs/fixtures/fixture.lock.json").read_text(encoding="utf-8"))
        lock["branch"] = "not-docs-fixtures"
        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            lock_path = temporary_path / "fixture.lock.json"
            output_dir = temporary_path / "output"
            lock_path.write_bytes(canonical_json(lock) + b"\n")
            environment = os.environ.copy()
            environment.pop("MSGVAULT_DOCS_FIXTURE_DIR", None)
            result = subprocess.run(
                [
                    "bash",
                    str(repo_root / "docs/fixtures/hydrate-fixture.sh"),
                    "--lock",
                    str(lock_path),
                    "--output-dir",
                    str(output_dir),
                ],
                cwd=repo_root,
                env=environment,
                capture_output=True,
                text=True,
                check=False,
            )

        self.assertNotEqual(0, result.returncode)
        self.assertIn("unsupported fixture branch", result.stderr)

    def test_hydrator_rejects_commit_outside_controlled_branch(self) -> None:
        repo_root = Path(__file__).resolve().parents[2]
        lock = json.loads((repo_root / "docs/fixtures/fixture.lock.json").read_text(encoding="utf-8"))
        lock["commit"] = subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=repo_root, capture_output=True, text=True, check=True
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as temporary:
            temporary_path = Path(temporary)
            lock_path = temporary_path / "fixture.lock.json"
            output_dir = temporary_path / "output"
            lock_path.write_bytes(canonical_json(lock) + b"\n")
            environment = os.environ.copy()
            environment.pop("MSGVAULT_DOCS_FIXTURE_DIR", None)
            result = subprocess.run(
                [
                    "bash",
                    str(repo_root / "docs/fixtures/hydrate-fixture.sh"),
                    "--lock",
                    str(lock_path),
                    "--output-dir",
                    str(output_dir),
                ],
                cwd=repo_root,
                env=environment,
                capture_output=True,
                text=True,
                check=False,
            )

        self.assertNotEqual(0, result.returncode)
        self.assertIn("not an ancestor", result.stderr)

    def test_selection_enforces_sender_and_subject_diversity(self) -> None:
        records = [
            synthetic_record(
                f"<alert-{index}@example.com>",
                "sender-a@example.com",
                "hub@example.com",
                "Mon, 1 Jan 2001 12:00:00 +0000",
                f"Alert {index}",
            )
            for index in range(8)
        ]
        records.extend(
            [
                synthetic_record("<project@example.com>", "sender-b@example.com", "hub@example.com", "Tue, 2 Jan 2001 12:01:00 +0000", "Project"),
                synthetic_record("<meeting@example.com>", "sender-c@example.com", "hub@example.com", "Wed, 3 Jan 2001 12:02:00 +0000", "Meeting"),
            ]
        )

        selected, _, _ = select_records(records, {"message_ids": [], "addresses": []}, 6, 2)

        self.assertGreaterEqual(len({record["sender"] for record in selected}), 3)
        self.assertGreaterEqual(len({record["subject"] for record in selected}), 5)

    def test_rejects_privileged_or_confidential_message(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <privileged@example.com>\n"
            b"Subject: Legal\n\n"
            b"Personal and Confidential\nPrivileged Solicitor and Client Communication\n"
        )

        self.assertEqual("confidential-marker", review_filter_reason(record))

    def test_allows_plain_operational_message(self) -> None:
        record = parse_message(
            b"From: sender@example.com\n"
            b"To: recipient@example.com\n"
            b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
            b"Message-ID: <operations@example.com>\n"
            b"Subject: Deployment\n\n"
            b"The migration workshop begins at 14:00 in the operations room.\n"
        )

        self.assertIsNone(review_filter_reason(record))

    def test_owner_identifier_is_the_most_frequent_selected_sender(self) -> None:
        records = [
            parse_message(
                b"From: owner@example.com\n"
                b"To: recipient@example.com\n"
                b"Date: Mon, 1 Jan 2001 12:00:00 +0000\n"
                b"Message-ID: <owner-1@example.com>\n"
                b"Subject: Operations\n\nUpdate\n"
            ),
            parse_message(
                b"From: owner@example.com\n"
                b"To: recipient@example.com\n"
                b"Date: Tue, 2 Jan 2001 12:00:00 +0000\n"
                b"Message-ID: <owner-2@example.com>\n"
                b"Subject: Operations\n\nUpdate\n"
            ),
            parse_message(
                b"From: other@example.com\n"
                b"To: recipient@example.com\n"
                b"Date: Wed, 3 Jan 2001 12:00:00 +0000\n"
                b"Message-ID: <other@example.com>\n"
                b"Subject: Operations\n\nUpdate\n"
            ),
        ]

        self.assertEqual("owner@example.com", select_owner_identifier(records))


if __name__ == "__main__":
    unittest.main()
