---
last_edited: "2026-08-17"
title: Document Attachment Indexing
description: Safely extract, index, search, rebuild, and remove standalone document attachments.
---

Msgvault can extract text from standalone document attachments with Mistral
OCR, store deterministic normalized chunks locally, and expose them through
full-text search. The feature is opt-in and fail-closed: configuration alone
cannot upload a document.

The provider receives the complete original document bytes and media type.
Message provenance, normalized text, chunks, indexes, orchestration, consent, and
backups remain owned by Msgvault. Raw provider JSON and full provider Markdown
are transient.

## Safety gates

Three independent gates must pass before production extraction:

1. An authenticated probe must show that the configured endpoint and model can
   process the synthetic fixture set.
2. The manifest must prove an enforceable pre-upload unit bound for the detected
   format.
3. Msgvault must hold consent for the exact provider, region, model, privacy
   posture, processing limits, normalization policy, and capability evidence.

A changed policy or manifest produces a different profile identity and requires
new consent. Credentials are read only for the explicit probe and extraction
commands. Local fixture validation, status, search, retirement, and purging do
not contact Mistral.

!!! note

    The first capability contract authorizes at most PDF for production upload.
    Other formats are still probed for extraction support, but remain blocked
    until Msgvault can enforce their provider-unit limits before upload.

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
```

Set the key in the named environment variable only when running an
authenticated operation:

```bash
export MISTRAL_API_KEY="..."
```

See the [configuration reference](/docs/configuration/#attachmentsdocuments) for
all policy and run limits.

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
msgvault documents status \
  --capabilities /private/msgvault/mistral-capabilities.json
```

Search results include the containing message and attachment provenance,
heading path, normalized text, checksum, score, and an opaque stable cursor.
The same search is available at `GET /api/v1/documents/search`; status is at
`GET /api/v1/documents/status`.

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
