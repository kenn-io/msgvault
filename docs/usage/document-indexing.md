---
last_edited: "2026-09-08"
title: Document Attachment Indexing
description: Find words and topics inside archived documents, with explicit control over provider uploads.
---

Document indexing lets you search inside archived attachments and return to
the message that contained them. Msgvault sends eligible files to Mistral OCR
to extract their text, stores that text locally, and builds a keyword index.
Optional document embeddings add search by meaning.

What works today:

- Keyword search over extracted text, with headings and containing-message
  details in each result.
- Semantic and hybrid search after separate document-vector setup and consent.
- Filters for a person, source, message, attachment, message type, and date.
- Optional local CSV-to-PDF conversion so CSV tables can enter the extraction
  pipeline.

Limits:

- Only formats authorized by the authenticated capability manifest can be
  uploaded. A manifest records the formats and processing bounds the probe
  verified for your provider configuration.
- Extraction uploads require an explicit `documents build` or `documents
  resume` command. Enabling the feature does not upload attachments.

For a combined provider setup, start with
[Recommended Configuration](/docs/usage/recommended-configuration/). The steps
below explain the document-specific probe, consent, and build workflow.

## What leaves your archive

The provider receives complete original bytes for each directly authorized format.
Msgvault keeps the containing-message links, normalized text, text chunks,
indexes, consent records, and backups locally. It does not retain the full raw
provider JSON or Markdown response.

Standalone CSV attachments use a local conversion step when enabled. Msgvault
retains the CSV source hash and `text/csv` occurrence identity, then sends only
the generated `application/pdf` bytes to Mistral. The conversion receipt stores
the generated PDF hash, byte count, page count, converter version, policy
fingerprint, and one based page, record, and cell spans. The generated PDF never
becomes an attachment.

## Safety gates

Three independent gates must pass before production extraction:

1. An authenticated probe must show that the configured endpoint and model can
   process the synthetic fixture set.
2. The manifest must prove an enforceable pre-upload unit bound for the detected
   format.
3. Msgvault must hold consent for the exact provider, region, model, privacy
   posture, processing limits, normalization policy, and capability evidence.

A changed policy or manifest produces a different profile identity and requires
new consent. OCR credentials are read only for the explicit probe and extraction
commands. Local fixture validation, status, keyword search, retirement, and
purging make no OCR request. Semantic and hybrid search have separate consent
to send queries to the embedding provider.

!!! note

    The authenticated capability manifest determines the directly authorized
    formats and their upload bounds. Raw CSV has no enforceable Mistral unit bound,
    so the resolver excludes it while conversion is disabled. Enabling CSV
    conversion adds a source route that sends generated PDF bytes after local
    conversion.

Provider uploads are manual-only. `msgvault serve` performs weekly local
reconciliation and derivative cleanup when document indexing is enabled, but it
never starts extraction on its own. Start every upload batch explicitly with
`documents build` or `documents resume`; when the daemon owns the archive, it
runs that requested batch so the command does not contend for the writer lock.

## Configure the policy

Add an `[attachments.documents]` section and explicitly state the provider
privacy posture you have verified. The example below uses zero data retention
and opted-out training; use values that match your provider account.

```toml
[attachments.documents]
enabled = true
provider = "mistral"
region = "eu"
api_key_env = "MISTRAL_API_KEY"
model = "mistral-ocr-4-0"
retention_posture = "zdr"
training_posture = "opted-out"
max_file_bytes = 52428800
max_pages_per_document = 500
max_response_bytes = 67108864
max_normalized_chars = 25000000
max_spool_bytes = 536870912
min_free_space_bytes = 1073741824
request_timeout = "5m"
max_retries = 3
max_pages_per_run = 10000
max_estimated_cost_usd_per_run = 50

[attachments.documents.scope]
message_types = ["email"]

[attachments.documents.index]
lexical = true
store_chunk_text = true

[attachments.documents.conversion.csv]
enabled = true
```

Set the key in the named environment variable only when running an
authenticated operation:

```bash
export MISTRAL_API_KEY="..."
```

Direct formats authorized by the capability manifest retain their original bytes
and media type for upload. CSV conversion uses Docbank's defaults,
tightened by `max_file_bytes`, `max_response_bytes`, and `max_pages_per_document`.
Docbank accepts UTF-8 CSV
cells containing Latin, Greek, Cyrillic, and common Unicode characters that its
embedded monospaced font supports. Control characters, combining marks,
formatting characters, unsupported scripts, and characters absent from that
font fail locally as `invalid_local_source`. Enabling CSV or changing an
effective bound changes the exact profile fingerprint and requires fresh consent.

See the [configuration reference](/docs/configuration/#attachmentsdocuments) for
the complete policy and run limits.

## Build and validate the synthetic fixtures

The repository fixture builder creates 21 formats deterministically. Five
legacy native containers need private seed files named `doc`, `ppt`, `xls`,
`numbers`, and `msg`. Seeds are copied byte-for-byte and checked by format; they
are never committed by Msgvault.

```bash
go run ./scripts/mistral-probe-fixtures \
  --output /private/msgvault/mistral-fixtures \
  --seed-dir /private/msgvault/mistral-seeds

msgvault documents probe-mistral \
  --fixtures /private/msgvault/mistral-fixtures \
  --validate-only
```

Fixture creation is all-or-nothing. The output directory and files are private,
and local validation makes no provider request.

## Produce the capability manifest

Run the authenticated probe and redirect its JSON output to a private file:

```bash
msgvault documents probe-mistral \
  --fixtures /private/msgvault/mistral-fixtures \
  > /private/msgvault/mistral-capabilities.json
```

Review the manifest before supplying it to Msgvault. It records the pinned
target, fixture digests, request fingerprints, extraction results, and observed
unit-bound evidence. It contains no credentials or fixture contents, but it is
upload authority for the exact policy it supports and should be controlled as
deployment configuration.

## Record consent and build

First run the consent command without `--yes` to read the exact disclosure,
then repeat it after review:

The manifest path is resolved on the daemon host. When `[remote].url` is
configured, run manifest-backed mutation commands on that host with `--local`;
the CLI rejects forwarding a client-local manifest path to a remote daemon.

```bash
msgvault documents consent-mistral \
  --capabilities /private/msgvault/mistral-capabilities.json

msgvault documents consent-mistral \
  --capabilities /private/msgvault/mistral-capabilities.json \
  --yes
```

Build the incremental index in bounded batches:

```bash
msgvault documents build \
  --capabilities /private/msgvault/mistral-capabilities.json \
  --limit 100 \
  --yes
```

The command claims a candidate before local inspection, so an oversized or
invalid attachment reaches a durable terminal state without starving later
documents. Transient provider and staging-capacity failures remain retryable.
Every retry reopens and verifies the private staged copy.

Use `--full-rebuild` to begin a replacement generation. If the bounded run does
not finish it, continue with `documents resume` and the same manifest:

```bash
msgvault documents build \
  --capabilities /private/msgvault/mistral-capabilities.json \
  --full-rebuild --yes

msgvault documents resume \
  --capabilities /private/msgvault/mistral-capabilities.json \
  --yes
```

## Search and inspect status

```bash
msgvault documents search "shipping damage"
msgvault documents search "shipping damage" --message-type email --limit 50
msgvault documents search "shipping damage" --person 123 --direction from_person
msgvault documents status \
  --capabilities /private/msgvault/mistral-capabilities.json
```

Search results include the containing message and attachment provenance,
heading path, normalized text, checksum, score, and an opaque stable cursor.
The same search is available at `GET /api/v1/documents/search`; status is at
`GET /api/v1/documents/status`.

Use `--source-id`, `--message-id`, or `--attachment-id` for exact archive
objects. `--person` selects a durable person; `--participant` selects an
observed participant and uses their durable person when one is bound. They
are mutually exclusive. With either person filter, `--direction` accepts
`from_person`, `to_person`, or `group`. `--after` includes its boundary and
`--before` excludes it; both accept `YYYY-MM-DD` or RFC3339 timestamps.

Add `--json` for scores and structured provenance. To fetch the next page,
repeat the same query and filters with `--cursor <next-cursor>`. If the index
changes and the cursor becomes stale, start again without the cursor.

### Semantic and hybrid document search

Keyword search is local and is the default (`--mode lexical`; `auto` also
means lexical). `--mode semantic` finds related meaning in document chunks.
`--mode hybrid` combines keyword and semantic rankings. Both explicitly send
the query text to the configured embedding provider.

First configure [message embeddings](/docs/usage/vector-search/#enable) and
enable document vectors:

```toml
[attachments.documents.index.embeddings]
enabled = true
```

Document vectors use the configured text embedding provider and a separate
document generation. Extraction consent does not authorize that provider to
receive document text or search queries. Review each disclosure without
`--yes`, then record both consents:

```bash
msgvault documents vectors consent --yes
msgvault documents vectors consent --purpose queries --yes
msgvault daemon restart
msgvault documents vectors build
msgvault documents vectors status
```

`build` processes a bounded batch of extracted chunks. Read the generation ID
from its output or `status`, then use `documents vectors resume
--generation-id <id>` to continue. Restarting after consent enables semantic
document queries and scheduled document-vector work. The schedule comes from
`[vector.embed.schedule]` and embeds already-extracted text; OCR uploads still
require an explicit extraction command.

```bash
msgvault documents search "evidence of transit damage" --mode semantic
msgvault documents search "shipping damage" --mode hybrid --person 123 --json
```

Semantic and hybrid search require a matching index and query consent. They
report an error when those prerequisites are missing; they do not silently
fall back to keyword search. The default candidate limit is 100, with a
maximum of 1,000 via `--candidate-limit`; lexical search allows up to 10,000.
Pagination stays within that candidate set.

In MCP, use `search_document_attachments` for document text search, including
semantic/hybrid mode and optional `person_id` scope. `search_person_files`
searches attachment metadata only. The CLI `person files --lane all` provides
the broader combined search. See [MCP](/docs/usage/chat/).

## Recovery and removal

Retry one terminal document by its canonical attachment SHA-256:

```bash
msgvault documents retry \
  --capabilities /private/msgvault/mistral-capabilities.json \
  --hash <sha256>
```

Retiring a profile stops it from being current but retains derived data for
recovery. Purging removes local normalized derivatives for one exact attachment
hash and does not delete the original attachment or its containing message.

```bash
msgvault documents retire <profile-id> --yes
msgvault documents purge-derived --hash <sha256> --yes
```

Document derivatives participate in full backups. Rebuilds are deterministic
under the same source bytes and profile policy.

For document vectors, use `documents vectors status --json` to inspect
generations, consent, usage, and failures. These operations are separate from
rebuilding or retiring the OCR extraction profile:

| Task | Command |
|---|---|
| Reset failed chunks for another attempt | `msgvault documents vectors retry --generation-id <id>` |
| Continue a build or retired-generation cleanup | `msgvault documents vectors resume --generation-id <id>` |
| Replace an active generation while retaining it during the build | `msgvault documents vectors rebuild --generation-id <active-id> --yes` |
| Retire a generation | `msgvault documents vectors retire --generation-id <id> --yes` |

Once a generation is active, use `rebuild` to replace it for coverage changes;
`build` reports that it is already active. Retirement keeps the backend
ledger; later vector operations finish the cleanup.
