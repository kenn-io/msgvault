#!/usr/bin/env python3
"""Shared deterministic selection, manifest, and fixture validation helpers."""

from __future__ import annotations

import hashlib
import gzip
import json
import mailbox
import os
import re
import tarfile
from collections import Counter, defaultdict
from datetime import UTC, datetime
from email import policy
from email.parser import BytesParser
from email.utils import getaddresses, parsedate_to_datetime
from pathlib import Path
from typing import Iterable, Iterator

SOURCE_RELEASE = "2015-05-07"
SOURCE_URL = "https://www.cs.cmu.edu/~enron/enron_mail_20150507.tar.gz"
SOURCE_SHA256 = "b3da1b3fe0369ec3140bb4fbce94702c33b7da810ec15d718b3fadf5cd748ca7"
MANIFEST_SCHEMA_VERSION = 1
SELECTION_ALGORITHM = "correspondent-density-v3-review-prefilter-diverse"
CONTENT_PREFILTER = "obvious-sensitive-markers-v1"
DEDUPLICATE_BY = "message-id-v1"
ATTRIBUTION_PROVIDER = "Carnegie Mellon University / CALO Project"
MANIFEST_NAME = "manifest.json"
FIXTURE_NAME = "enron-web-fixture.mbox.gz"
README_NAME = "README.md"
EXPECTED_FILES = frozenset({FIXTURE_NAME, MANIFEST_NAME, README_NAME})
_MESSAGE_SEPARATOR = re.compile(br"(?m)^From [^\n]*\n")
_HEADER_EMAIL = re.compile(rb"[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
_PHONE_NUMBER = re.compile(
    r"(?<!\d)(?:\+?1[\s.-]?)?(?:\(\d{3}\)|\d{3})[\s.-]\d{3}[\s.-]\d{4}(?!\d)"
)
_SSN = re.compile(r"(?<!\d)\d{3}-\d{2}-\d{4}(?!\d)")
_CREDENTIAL_MARKERS = re.compile(r"\b(?:password|passwd|passcode|username|user name|credentials?)\b")
_PERSONAL_MARKERS = re.compile(
    r"\b(?:medical|surgery|divorc\w*|pregnan\w*|family|wife|husband|daughter|son|children|child|"
    r"mother|father|wedding|honeymoon|birthday|vacation|home address|my whereabouts|personal time)\b"
)
_FINANCIAL_MARKERS = re.compile(
    r"\b(?:bank account|routing number|credit card|social security|ssn|payroll|salary|compensation|"
    r"accounting information|budget)\b"
)
_ATTACHMENT_REFERENCE = re.compile(
    r"(?:^|\n)\s*(?:-|<<\s*File:?)\s*[^\n]+\.(?:docx?|xlsx?|pdf|jpg|jpeg|png|gif|mp3|zip|dat|vcf)\b"
    r"|\S+\.(?:docx?|xlsx?|pdf|jpg|jpeg|png|gif|mp3|zip|dat|vcf)\b",
    flags=re.IGNORECASE,
)
_CONFIDENTIAL_MARKERS = re.compile(
    r"\b(?:personal and confidential|privileged|confidentiality notice|confidential information)\b",
    flags=re.IGNORECASE,
)
REVIEWABLE_CONTENT_TYPES = frozenset({"text/plain", "text/html"})
FALLBACK_THREAD_WINDOW_HOURS = 6
MIN_SELECTED_SENDERS = 3
MIN_SELECTED_SUBJECTS = 5
MIN_MESSAGE_CAP = 20
MIN_NEIGHBOR_LIMIT = 2
MIN_RELATIONSHIP_RESULTS = 3
MAX_SMOKE_ROWS = 500
_SHA256 = re.compile(r"^[0-9a-f]{64}$")


def canonical_json(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode("utf-8")


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_source_archive(path: Path) -> None:
    actual_digest = sha256_file(path)
    if actual_digest != SOURCE_SHA256:
        raise ValueError(
            f"source archive SHA-256 {actual_digest} does not match the pinned source SHA-256 {SOURCE_SHA256}"
        )


def review_attestation_digest(manifest: dict, fixture_digest: str) -> str:
    """Digest the fixture and every non-attestation manifest input."""
    attested_manifest = {
        key: value
        for key, value in manifest.items()
        if key != "manual_review"
    }
    return sha256_bytes(canonical_json({
        "fixture_sha256": fixture_digest,
        "manifest": attested_manifest,
    }))


def canonical_exclusions(path: Path | None) -> tuple[dict[str, list[str]], str]:
    if path is None:
        value = {"message_ids": [], "addresses": []}
    else:
        value = json.loads(path.read_text(encoding="utf-8"))
        value = {
            "message_ids": sorted(set(str(item).strip() for item in value.get("message_ids", []) if str(item).strip())),
            "addresses": sorted(set(normalize_address(str(item)) for item in value.get("addresses", []) if normalize_address(str(item)))),
        }
    return value, sha256_bytes(canonical_json(value))


def normalize_address(value: str) -> str:
    parsed = getaddresses([value])
    address = parsed[0][1] if parsed else value
    address = address.strip().casefold()
    return address if "@" in address and " " not in address else ""


def fallback_thread_key(subject: str, participants: list[str], occurred_at: datetime) -> str:
    window_start = occurred_at.replace(
        hour=(occurred_at.hour // FALLBACK_THREAD_WINDOW_HOURS) * FALLBACK_THREAD_WINDOW_HOURS,
        minute=0,
        second=0,
        microsecond=0,
    )
    participant_key = ",".join(participants) or "unknown"
    return f"subject:{subject}|participants:{participant_key}|window:{window_start.isoformat()}"


def parse_message(raw: bytes) -> dict:
    header_end = re.search(br"\r?\n\r?\n", raw)
    header_bytes = raw if header_end is None else raw[: header_end.start()]
    fields: dict[bytes, list[bytes]] = defaultdict(list)
    current: bytes | None = None
    for line in header_bytes.splitlines():
        if line[:1] in {b" ", b"\t"} and current is not None:
            fields[current][-1] += b" " + line.strip()
            continue
        name, separator, value = line.partition(b":")
        if separator:
            current = name.strip().lower()
            fields[current].append(value.strip())
    def header_addresses(name: bytes) -> list[str]:
        matches = _HEADER_EMAIL.findall(b" ".join(fields.get(name, [])))
        return sorted(
            {
                normalized
                for normalized in (normalize_address(value.decode("ascii", "ignore")) for value in matches)
                if normalized
            }
        )

    sender_matches = _HEADER_EMAIL.findall(b" ".join(fields.get(b"from", [])))
    sender = normalize_address(sender_matches[0].decode("ascii", "ignore")) if sender_matches else ""
    recipient_fields = {
        "from": sender,
        "to": header_addresses(b"to"),
        "cc": header_addresses(b"cc"),
        "bcc": header_addresses(b"bcc"),
    }
    recipients = sorted({value for field in ("to", "cc", "bcc") for value in recipient_fields[field]})
    message_id_match = re.search(rb"<[^>\r\n]+>", b" ".join(fields.get(b"message-id", [])))
    message_id = message_id_match.group(0).decode("ascii", "replace") if message_id_match else f"sha256:{sha256_bytes(raw)}"
    date = b" ".join(fields.get(b"date", [])).decode("latin1", "replace").strip()
    try:
        parsed = parsedate_to_datetime(date)
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=UTC)
        occurred_at = parsed.astimezone(UTC)
    except (TypeError, ValueError, OverflowError):
        occurred_at = datetime.min.replace(tzinfo=UTC)
    references = re.findall(r"<[^>\s]+>", b" ".join(fields.get(b"references", [])).decode("latin1", "replace"))
    in_reply_to = b" ".join(fields.get(b"in-reply-to", [])).decode("latin1", "replace").strip()
    subject = re.sub(r"^(re|fw|fwd):\s*", "", b" ".join(fields.get(b"subject", [])).decode("latin1", "replace"), flags=re.IGNORECASE).strip().casefold()
    participants = sorted(set([sender, *recipients]) - {""})
    thread_key = references[0] if references else in_reply_to or fallback_thread_key(subject, participants, occurred_at)
    return {
        "raw": raw,
        "message": None,
        "message_id": message_id,
        "sender": sender,
        "recipients": recipients,
        "recipient_fields": recipient_fields,
        "participants": participants,
        "subject": subject,
        "occurred_at": occurred_at,
        "thread_key": thread_key,
    }


def review_filter_reason(record: dict) -> str | None:
    """Return a conservative reason to omit an obvious review-gate candidate."""
    if contains_mime_attachment(record["raw"]):
        return "attachment"
    text = record["raw"].decode("latin1", "replace").casefold()
    if (
        "filename=" in text
        or _ATTACHMENT_REFERENCE.search(text)
    ):
        return "attachment"
    if _CONFIDENTIAL_MARKERS.search(text):
        return "confidential-marker"
    if _CREDENTIAL_MARKERS.search(text):
        return "credential-marker"
    if _SSN.search(text) or _PHONE_NUMBER.search(text) or re.search(r"\b(?:phone|mobile|cell|pager|fax|telephone)\b", text):
        return "phone-number"
    if _FINANCIAL_MARKERS.search(text) or re.search(r"\$\s?\d", text):
        return "financial-marker"
    if _PERSONAL_MARKERS.search(text):
        return "personal-marker"
    return None


def select_owner_identifier(records: list[dict]) -> str:
    sender_counts = Counter(record["sender"] for record in records if record["sender"])
    if not sender_counts:
        raise ValueError("selected fixture has no sender identifier")
    return sorted(sender_counts, key=lambda address: (-sender_counts[address], address))[0]


def participant_identifiers(records: Iterable[dict]) -> list[str]:
    return sorted({participant for record in records for participant in record["participants"]})


def message_recipient_edges(records: Iterable[dict]) -> list[dict]:
    return [
        {
            "sequence": sequence,
            "message_id": record["message_id"],
            "from": record["recipient_fields"]["from"],
            "to": record["recipient_fields"]["to"],
            "cc": record["recipient_fields"]["cc"],
            "bcc": record["recipient_fields"]["bcc"],
        }
        for sequence, record in enumerate(records, 1)
    ]


def _record_sort_key(record: dict) -> tuple[datetime, str, int, bytes]:
    return (
        record["occurred_at"],
        record["message_id"],
        record.get("source_position", 0),
        record["raw"],
    )


def _deduplicate_records(records: Iterable[dict]) -> list[dict]:
    unique: dict[str, dict] = {}
    for record in records:
        existing = unique.get(record["message_id"])
        if existing is None or _record_sort_key(record) < _record_sort_key(existing):
            unique[record["message_id"]] = record
    return sorted(unique.values(), key=_record_sort_key)


def addresses(values: Iterable[str]) -> Iterator[str]:
    for _, address in getaddresses(values):
        normalized = normalize_address(address)
        if normalized:
            yield normalized


def iter_source_records(source_archive: Path) -> Iterator[dict]:
    with tarfile.open(source_archive, "r:gz") as archive:
        # Read sequentially: sorting compressed-tar members before extracting
        # them forces a seek/reinflate for every message. The aggregate graph
        # and final selection ordering below use explicit keys, so traversal
        # order does not affect the result.
        for member in archive:
            if not member.isfile() or member.size == 0:
                continue
            extracted = archive.extractfile(member)
            if extracted is None:
                continue
            raw = extracted.read()
            try:
                record = parse_message(raw)
            except (ValueError, UnicodeError):
                continue
            record["source_path"] = member.name
            record["source_position"] = member.offset_data
            yield record


def mbox_messages(path: Path) -> list[bytes]:
    with path.open("rb") as stream:
        box = mailbox.mbox(stream.name, create=False)
        try:
            return [message.as_bytes(policy=policy.default) for message in box]
        finally:
            box.close()


def count_mbox_messages(path: Path) -> int:
    count = 0
    opener = gzip.open if path.suffix == ".gz" else open
    with opener(path, "rb") as stream:
        for line in stream:
            if line.startswith(b"From "):
                count += 1
    return count


def iter_mbox_raw_messages(path: Path) -> Iterator[bytes]:
    opener = gzip.open if path.suffix == ".gz" else open
    with opener(path, "rb") as stream:
        data = stream.read()
    lines = data.splitlines(keepends=True)
    separators = [index for index, line in enumerate(lines) if line.startswith(b"From ")]
    for position, start in enumerate(separators):
        end = separators[position + 1] if position + 1 < len(separators) else len(lines)
        yield b"".join(lines[start + 1 : end])


def contains_mime_attachment(raw: bytes) -> bool:
    message = BytesParser(policy=policy.default).parsebytes(raw)
    return next(iter_non_body_mime_parts(message), None) is not None


def iter_non_body_mime_parts(message: object) -> Iterator[object]:
    for part in message.walk():
        disposition = part.get_content_disposition()
        if disposition in {"attachment", "inline"}:
            yield part
            continue
        if part.is_multipart():
            if not part.get_content_type().startswith("multipart/"):
                yield part
            continue
        if part.get_content_type() not in REVIEWABLE_CONTENT_TYPES:
            yield part


def write_mbox(records: list[dict]) -> bytes:
    chunks: list[bytes] = []
    for record in records:
        stamp = record["occurred_at"].strftime("%a %b %d %H:%M:%S %Y")
        chunks.append(f"From msgvault-fixture@localhost {stamp}\n".encode("ascii"))
        raw = record["raw"].replace(b"\nFrom ", b"\n>From ").rstrip(b"\n")
        chunks.append(raw + b"\n\n")
    return b"".join(chunks)


def _require_mapping(value: object, label: str) -> dict:
    if not isinstance(value, dict):
        raise ValueError(f"manifest {label} must be an object")
    return value


def _require_fields(mapping: dict, fields: tuple[str, ...], label: str) -> None:
    for field in fields:
        if field not in mapping:
            raise ValueError(f"manifest {label} is missing {field}")


def _require_string(mapping: dict, field: str, label: str, *, nonempty: bool = True) -> str:
    value = mapping.get(field)
    if not isinstance(value, str) or (nonempty and not value.strip()):
        raise ValueError(f"manifest {label}.{field} must be a non-empty string")
    return value


def _require_int(mapping: dict, field: str, label: str, *, minimum: int | None = None) -> int:
    value = mapping.get(field)
    if type(value) is not int or (minimum is not None and value < minimum):
        suffix = f" >= {minimum}" if minimum is not None else ""
        raise ValueError(f"manifest {label}.{field} must be an integer{suffix}")
    return value


def _require_digest(mapping: dict, field: str, label: str, *, allow_none: bool = False) -> str | None:
    value = mapping.get(field)
    if allow_none and value is None:
        return None
    if not isinstance(value, str) or _SHA256.fullmatch(value) is None:
        raise ValueError(f"manifest {label}.{field} must be a lowercase SHA-256 digest")
    return value


def _require_sorted_strings(mapping: dict, field: str, label: str) -> list[str]:
    value = mapping.get(field)
    if not isinstance(value, list) or any(not isinstance(item, str) or not item.strip() for item in value):
        raise ValueError(f"manifest {label}.{field} must be a list of non-empty strings")
    if value != sorted(set(value)):
        raise ValueError(f"{field} must be sorted and unique")
    return value


def _require_addresses(mapping: dict, field: str, label: str) -> list[str]:
    values = _require_sorted_strings(mapping, field, label)
    if any(normalize_address(value) != value for value in values):
        raise ValueError(f"manifest {label}.{field} must contain normalized email addresses")
    return values


def _require_message_recipient_edges(value: object, label: str) -> list[dict]:
    if not isinstance(value, list):
        raise ValueError(f"manifest {label} must be a list")
    edges: list[dict] = []
    for expected_sequence, item in enumerate(value, 1):
        edge = _require_mapping(item, f"{label}[{expected_sequence - 1}]")
        _require_fields(edge, ("sequence", "message_id", "from", "to", "cc", "bcc"), label)
        if _require_int(edge, "sequence", label, minimum=1) != expected_sequence:
            raise ValueError(f"manifest {label} sequences must be contiguous and start at one")
        _require_string(edge, "message_id", label)
        sender = _require_string(edge, "from", label)
        if normalize_address(sender) != sender:
            raise ValueError(f"manifest {label}.from must contain a normalized email address")
        normalized = {"sequence": expected_sequence, "message_id": edge["message_id"], "from": sender}
        for field in ("to", "cc", "bcc"):
            normalized[field] = _require_addresses(edge, field, label)
        edges.append(normalized)
    return edges


def _require_timestamp(mapping: dict, field: str, label: str) -> str:
    value = _require_string(mapping, field, label)
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise ValueError(f"manifest {label}.{field} must be an ISO-8601 timestamp") from error
    if parsed.tzinfo is None:
        raise ValueError(f"manifest {label}.{field} must include a timezone")
    return value


def validate_fixture_directory(directory: Path, *, require_review: bool = True) -> dict:
    entries = list(directory.iterdir())
    if any(path.is_symlink() or not path.is_file() for path in entries):
        raise ValueError("fixture directory must contain only root-level regular files")
    actual_files = frozenset(path.name for path in entries)
    if actual_files != EXPECTED_FILES:
        raise ValueError(f"fixture directory must contain exactly {sorted(EXPECTED_FILES)}, found {sorted(actual_files)}")
    manifest_path = directory / MANIFEST_NAME
    fixture_path = directory / FIXTURE_NAME
    manifest = _require_mapping(json.loads(manifest_path.read_text(encoding="utf-8")), "root")
    _require_fields(manifest, ("schema_version", "source", "selection", "fixture", "attribution", "manual_review"), "root")
    if type(manifest["schema_version"]) is not int or manifest["schema_version"] != MANIFEST_SCHEMA_VERSION:
        raise ValueError("manifest schema_version is not the supported version")

    source = _require_mapping(manifest["source"], "source")
    _require_fields(source, ("url", "release", "sha256", "message_count_scanned"), "source")
    if _require_string(source, "url", "source") != SOURCE_URL:
        raise ValueError("manifest source URL is not the pinned CMU CALO source")
    if _require_string(source, "release", "source") != SOURCE_RELEASE:
        raise ValueError("manifest source release is not the pinned CMU CALO release")
    if _require_digest(source, "sha256", "source") != SOURCE_SHA256:
        raise ValueError("manifest source SHA-256 does not match the pinned source archive")
    _require_int(source, "message_count_scanned", "source", minimum=1)

    selection = _require_mapping(manifest["selection"], "selection")
    _require_fields(
        selection,
        (
            "algorithm",
            "content_prefilter",
            "deduplicate_by",
            "fallback_thread_window_hours",
            "minimum_distinct_senders",
            "minimum_distinct_subjects",
            "message_cap",
            "neighbor_limit",
            "seed_mailbox",
            "selected_participants",
            "message_recipient_edges",
            "excluded_message_ids",
            "excluded_addresses",
            "exclusions_sha256",
        ),
        "selection",
    )
    if _require_string(selection, "algorithm", "selection") != SELECTION_ALGORITHM:
        raise ValueError("manifest selection uses an unsupported algorithm")
    if _require_string(selection, "content_prefilter", "selection") != CONTENT_PREFILTER:
        raise ValueError("manifest selection uses an unsupported content prefilter")
    if _require_string(selection, "deduplicate_by", "selection") != DEDUPLICATE_BY:
        raise ValueError("manifest selection uses an unsupported deduplication rule")
    if _require_int(selection, "fallback_thread_window_hours", "selection", minimum=1) != FALLBACK_THREAD_WINDOW_HOURS:
        raise ValueError("manifest fallback thread window does not match the selector")
    if _require_int(selection, "minimum_distinct_senders", "selection", minimum=1) != MIN_SELECTED_SENDERS:
        raise ValueError("manifest sender diversity minimum does not match the selector")
    if _require_int(selection, "minimum_distinct_subjects", "selection", minimum=1) != MIN_SELECTED_SUBJECTS:
        raise ValueError("manifest subject diversity minimum does not match the selector")
    _require_int(selection, "message_cap", "selection", minimum=MIN_MESSAGE_CAP)
    _require_int(selection, "neighbor_limit", "selection", minimum=MIN_NEIGHBOR_LIMIT)
    seed_mailbox = _require_string(selection, "seed_mailbox", "selection")
    if normalize_address(seed_mailbox) != seed_mailbox:
        raise ValueError("manifest selection.seed_mailbox must be a normalized email address")
    selected_participants = _require_addresses(selection, "selected_participants", "selection")
    if len(selected_participants) < 4:
        raise ValueError("manifest selection.selected_participants must contain at least four addresses")
    excluded_message_ids = _require_sorted_strings(selection, "excluded_message_ids", "selection")
    excluded_addresses = _require_addresses(selection, "excluded_addresses", "selection")
    _require_digest(selection, "exclusions_sha256", "selection")
    if seed_mailbox not in selected_participants:
        raise ValueError("manifest selection seed mailbox must be a selected participant")
    message_recipient_edges_value = _require_message_recipient_edges(
        selection["message_recipient_edges"], "selection.message_recipient_edges"
    )

    fixture = _require_mapping(manifest["fixture"], "fixture")
    _require_fields(
        fixture,
        (
            "path",
            "sha256",
            "message_count",
            "participant_count",
            "owner_identifier",
            "attachment_count",
            "sender_count",
            "subject_count",
            "minimum_relationship_results",
        ),
        "fixture",
    )
    if _require_string(fixture, "path", "fixture") != FIXTURE_NAME:
        raise ValueError("manifest fixture path does not match the published fixture")
    _require_digest(fixture, "sha256", "fixture")
    _require_int(fixture, "message_count", "fixture", minimum=1)
    _require_int(fixture, "participant_count", "fixture", minimum=1)
    owner_identifier = _require_string(fixture, "owner_identifier", "fixture")
    if normalize_address(owner_identifier) != owner_identifier or owner_identifier not in selected_participants:
        raise ValueError("fixture owner identifier must be a selected participant")
    _require_int(fixture, "attachment_count", "fixture", minimum=0)
    _require_int(fixture, "sender_count", "fixture", minimum=1)
    _require_int(fixture, "subject_count", "fixture", minimum=1)
    if _require_int(fixture, "minimum_relationship_results", "fixture", minimum=1) != MIN_RELATIONSHIP_RESULTS:
        raise ValueError("manifest relationship result minimum does not match the smoke test")

    attribution = _require_mapping(manifest["attribution"], "attribution")
    _require_fields(attribution, ("provider", "license_note"), "attribution")
    if _require_string(attribution, "provider", "attribution") != ATTRIBUTION_PROVIDER:
        raise ValueError("manifest attribution provider is not the pinned source provider")
    _require_string(attribution, "license_note", "attribution")

    review = _require_mapping(manifest["manual_review"], "manual_review")
    _require_fields(review, ("status", "reviewed_message_count", "reviewed_fixture_sha256"), "manual_review")
    status = _require_string(review, "status", "manual_review")
    if status not in {"pending", "complete"}:
        raise ValueError("manual_review.status must be pending or complete")
    _require_int(review, "reviewed_message_count", "manual_review", minimum=0)
    _require_digest(review, "reviewed_fixture_sha256", "manual_review", allow_none=True)
    if "review_attestation_sha256" in review:
        _require_digest(review, "review_attestation_sha256", "manual_review")
    if "reviewer" in review and review["reviewer"] is not None:
        _require_string(review, "reviewer", "manual_review")
    if "reviewed_at" in review and review["reviewed_at"] is not None:
        _require_timestamp(review, "reviewed_at", "manual_review")

    exclusion_value = {
        "message_ids": excluded_message_ids,
        "addresses": excluded_addresses,
    }
    if selection["exclusions_sha256"] != sha256_bytes(canonical_json(exclusion_value)):
        raise ValueError("exclusion digest does not match canonical exclusion inputs")
    actual_digest = sha256_file(fixture_path)
    if fixture["sha256"] != actual_digest:
        raise ValueError("fixture SHA-256 does not match manifest")
    actual_count = count_mbox_messages(fixture_path)
    if fixture["message_count"] != actual_count:
        raise ValueError("fixture message count does not match manifest")
    if actual_count > MAX_SMOKE_ROWS:
        raise ValueError("fixture message count exceeds the smoke test retrieval limit")
    if actual_count > selection["message_cap"]:
        raise ValueError("fixture message count exceeds the selected message cap")
    if source["message_count_scanned"] < actual_count:
        raise ValueError("source message count is smaller than the published fixture")
    fixture_records = [parse_message(raw) for raw in iter_mbox_raw_messages(fixture_path)]
    message_ids = [record["message_id"] for record in fixture_records]
    if len(message_ids) != len(set(message_ids)):
        raise ValueError("fixture contains duplicate message IDs")
    if set(excluded_message_ids) & set(message_ids):
        raise ValueError("fixture contains excluded message IDs")
    actual_participants = participant_identifiers(fixture_records)
    if set(excluded_addresses) & set(actual_participants):
        raise ValueError("fixture contains excluded addresses")
    if selected_participants != actual_participants:
        raise ValueError("selected participants do not match fixture messages")
    actual_message_recipient_edges = message_recipient_edges(fixture_records)
    if message_recipient_edges_value != actual_message_recipient_edges:
        raise ValueError("message recipient edges do not match fixture messages")
    if fixture["participant_count"] != len(actual_participants):
        raise ValueError("fixture participant count does not match manifest")
    actual_owner = select_owner_identifier(fixture_records)
    if owner_identifier != actual_owner:
        raise ValueError("fixture owner identifier does not match the deterministic selected sender")
    attachment_count = sum(contains_mime_attachment(record["raw"]) for record in fixture_records)
    if fixture["attachment_count"] != attachment_count:
        raise ValueError("fixture attachment count does not match manifest")
    if attachment_count:
        raise ValueError("fixture contains MIME attachments that are outside the reviewable text-only scope")
    sender_count = len({record["sender"] for record in fixture_records if record["sender"]})
    subject_count = len({record["subject"] for record in fixture_records})
    if fixture["sender_count"] != sender_count:
        raise ValueError("fixture sender count does not match manifest")
    if fixture["subject_count"] != subject_count:
        raise ValueError("fixture subject count does not match manifest")
    if sender_count < selection["minimum_distinct_senders"]:
        raise ValueError("fixture does not meet the sender diversity minimum")
    if subject_count < selection["minimum_distinct_subjects"]:
        raise ValueError("fixture does not meet the subject diversity minimum")
    if require_review:
        if status != "complete":
            raise ValueError("fixture has not completed message-by-message review")
        if review.get("reviewed_message_count") != actual_count:
            raise ValueError("reviewed message count does not match fixture")
        if review.get("reviewed_fixture_sha256") != actual_digest:
            raise ValueError("reviewed fixture digest does not match fixture")
        if review.get("review_attestation_sha256") != review_attestation_digest(manifest, actual_digest):
            raise ValueError("review attestation digest does not match fixture and manifest")
        if not review.get("reviewer") or not review.get("reviewed_at"):
            raise ValueError("completed review is missing reviewer or timestamp")
    return manifest


def select_records(records: list[dict], exclusions: dict[str, list[str]], message_cap: int, neighbor_limit: int) -> tuple[list[dict], str, list[str]]:
    excluded_ids = set(exclusions["message_ids"])
    excluded_addresses = set(exclusions["addresses"])
    usable = [
        record
        for record in _deduplicate_records(records)
        if record["message_id"] not in excluded_ids
        and not (set(record["participants"]) & excluded_addresses)
        and review_filter_reason(record) is None
    ]
    graph: dict[str, Counter[str]] = defaultdict(Counter)
    message_counts: Counter[str] = Counter()
    for record in usable:
        participants = record["participants"]
        for participant in participants:
            message_counts[participant] += 1
            for correspondent in participants:
                if participant != correspondent:
                    graph[participant][correspondent] += 1
    if not graph:
        raise ValueError("source archive produced no address graph")
    seed = sorted(graph, key=lambda address: (-len(graph[address]), -message_counts[address], address))[0]
    participants = [seed]
    frontier = [seed]
    while frontier and len(participants) < neighbor_limit + 1:
        current = frontier.pop(0)
        candidates = sorted(graph[current], key=lambda address: (-graph[current][address], -message_counts[address], address))
        for candidate in candidates:
            if candidate not in participants:
                participants.append(candidate)
                frontier.append(candidate)
                if len(participants) >= neighbor_limit + 1:
                    break
    selected_candidates = [record for record in usable if set(record["participants"]) & set(participants)]
    grouped: dict[str, list[dict]] = defaultdict(list)
    for record in selected_candidates:
        grouped[record["thread_key"]].append(record)
    groups = sorted(
        grouped.values(),
        key=lambda group: (
            -len(set().union(*(set(item["participants"]) for item in group)) & set(participants)),
            -len(group),
            min(item["occurred_at"] for item in group),
            min(item["message_id"] for item in group),
        ),
    )
    ordered_groups = [sorted(group, key=_record_sort_key) for group in groups]
    available_senders = {record["sender"] for record in selected_candidates if record["sender"]}
    available_subjects = {record["subject"] for record in selected_candidates}
    required_senders = min(MIN_SELECTED_SENDERS, len(available_senders), message_cap)
    required_subjects = min(MIN_SELECTED_SUBJECTS, len(available_subjects), message_cap)
    selected: list[dict] = []
    selected_ids: set[str] = set()
    selected_senders: set[str] = set()
    selected_subjects: set[str] = set()

    while len(selected) < message_cap and (
        len(selected_senders) < required_senders or len(selected_subjects) < required_subjects
    ):
        progressed = False
        for ordered_group in ordered_groups:
            needs_sender_diversity = len(selected_senders) < required_senders
            candidate = next(
                (
                    item
                    for item in ordered_group
                    if item["message_id"] not in selected_ids
                    and (
                        (needs_sender_diversity and item["sender"] and item["sender"] not in selected_senders)
                        or (not needs_sender_diversity and item["subject"] not in selected_subjects)
                    )
                ),
                None,
            )
            if candidate is None:
                continue
            selected.append(candidate)
            selected_ids.add(candidate["message_id"])
            selected_senders.add(candidate["sender"])
            selected_subjects.add(candidate["subject"])
            progressed = True
            if len(selected) >= message_cap or (
                len(selected_senders) >= required_senders and len(selected_subjects) >= required_subjects
            ):
                break
        if not progressed:
            break

    for ordered_group in ordered_groups:
        for candidate in ordered_group:
            if len(selected) >= message_cap:
                break
            if candidate["message_id"] in selected_ids:
                continue
            selected.append(candidate)
            selected_ids.add(candidate["message_id"])

    selected = sorted(selected, key=_record_sort_key)[:message_cap]
    if len({record["sender"] for record in selected if record["sender"]}) < required_senders:
        raise ValueError("selected fixture lacks required sender diversity")
    if len({record["subject"] for record in selected}) < required_subjects:
        raise ValueError("selected fixture lacks required subject diversity")
    return selected, seed, participant_identifiers(selected)
