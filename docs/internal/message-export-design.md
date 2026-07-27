# Generic Message Export

Design for a provider-neutral, machine-readable archive export. Status: draft
for review.

## Summary

Msgvault needs a stable way for external tools to consume complete message
history without reading its database, depending on its analytics cache, or
knowing provider-specific metadata layouts.

The public command will be:

```text
msgvault export-messages \
  --start <RFC3339> \
  --end <RFC3339> \
  [--message-type <type> ...] \
  [--source <source-type>:<identifier> ...] \
  [--format jsonl]
```

It emits a normalized JSON Lines stream with schema
`msgvault-message-export/1`. The command is provider-neutral: Discord, Gmail,
IMAP, Teams, text imports, meeting transcripts, calendar events, and future
message types use the same record model. Message-type and source filters are
optional. Time bounds are required and form a half-open interval.

The stream begins with a manifest, emits source and conversation records before
message records, and ends with a completion record. The completion record and
record counts let consumers reject a truncated stream.

Although `export-discord` and `msgvault-discord-export/1` have not shipped
from msgvault's main branch, an active downstream integration already consumes
them. The generic contract therefore rolls out additively: `export-messages`
ships first, the downstream migrates and verifies parity, and only then is the
provider-specific command removed.

## Goals

- Export complete normalized message text and identity through a stable public
  contract.
- Support every archived message type without adding provider-specific export
  commands.
- Allow optional, repeatable source and message-type filters.
- Preserve deterministic output for fixtures, hashing, diffs, and reproducible
  downstream artifacts.
- Stream large bounded exports without accumulating the full result in memory.
- Work through msgvault's configured local or remote daemon path.
- Detect interrupted or partially written streams reliably.
- Keep provider metadata, credentials, database IDs, and storage schemas out of
  the public contract.
- Support both SQLite and PostgreSQL archives.

## Non-goals

- Exporting raw provider metadata JSON.
- Exporting raw MIME messages, HTML bodies, attachments, reactions, labels, or
  provider credentials.
- Replacing `export-eml`, `export-attachment`, or `export-attachments`.
- Providing an archive restore format. Backup and restore retain their existing
  stronger physical and integrity contracts.
- Making an unbounded whole-archive export the default.
- Guaranteeing that every source has a successful sync run. Imported sources
  may not have one.
- Adding a new dedicated HTTP endpoint. The command uses the existing
  daemon-routed CLI transport.

## Command contract

### Required bounds

`--start` is inclusive and `--end` is exclusive. Both accept RFC3339
timestamps, are normalized to UTC in the manifest, and must satisfy
`start < end`.

Required bounds prevent accidental multi-decade exports and make retries and
downstream provenance explicit. A caller that wants the full archive can pass
bounds covering the archive's known lifetime.

Messages use their effective archive timestamp:

1. `sent_at`;
2. `received_at` when `sent_at` is absent; then
3. `internal_date` when both are absent.

A message without any effective timestamp cannot satisfy a bounded export and
is omitted. Date-repair tooling remains responsible for correcting such rows.

### Message-type filters

`--message-type` is optional and repeatable. Multiple values are combined with
OR semantics. Omitting the flag includes every message type.

The filter accepts any non-empty exact message-type value rather than relying
on a hard-coded allowlist. This keeps export forward-compatible with newly
added importers. A filter that matches no messages produces a valid empty
export.

### Source filters

`--source` is optional and repeatable. Each value is a stable typed selector:

```text
<source-type>:<identifier>
```

Examples include `discord:123456789012345678`,
`gmail:user@example.com`, and `teams:user@example.com`. The first colon
separates the source type from the identifier; the remainder belongs to the
identifier unchanged.

Typed selectors avoid ambiguity when Gmail, IMAP, and Teams sources share an
identifier. Multiple selectors are combined with OR semantics. Omitting the
flag permits matching messages from every source. A supplied selector that
does not resolve is an error rather than a silent empty export.

When explicit source filters resolve successfully, their source records are
emitted even if the bounded window contains no matching messages. This allows a
consumer to distinguish an empty window from a misspelled or unavailable
source. Without source filters, only sources represented by matching messages
are emitted.

### Format and output

`jsonl` is the default and only v1 format. `--format jsonl` is accepted
explicitly so future formats can be added without changing command structure.
Stdout contains only compact JSON records, one object per line. Diagnostics and
progress belong on stderr.

The command reads the archive only. It does not contact a provider, load a
provider credential, or implicitly synchronize a source.

## JSONL stream contract

Every record contains `record_type`. The first record is exactly one
`manifest`; the final record is exactly one `complete`. Records between them
occur in three phases: all `source` records, all `conversation` records, then
all `message` records.

### Manifest

```json
{
  "record_type": "manifest",
  "schema": "msgvault-message-export/1",
  "msgvault_version": "v0.19.0",
  "window": {
    "start": "2026-07-20T00:00:00Z",
    "end": "2026-07-27T00:00:00Z"
  },
  "filters": {
    "message_types": ["discord"],
    "sources": [
      {
        "source_type": "discord",
        "identifier": "123456789012345678"
      }
    ]
  }
}
```

Filter arrays contain the normalized effective filters in stable order.
Omitted CLI filters appear as empty arrays, meaning no restriction.

### Source

```json
{
  "record_type": "source",
  "source_type": "discord",
  "identifier": "123456789012345678",
  "display_name": "Example Guild",
  "last_successful_sync_at": "2026-07-27T14:30:00Z"
}
```

`source_type` and `identifier` form the stable source key. Internal integer
source IDs are not exported. `display_name` is an empty string when absent.
`last_successful_sync_at` is nullable because manual and file-based imports do
not necessarily create sync runs.

The timestamp is provenance, not a completeness guarantee. Export does not
reject a source whose sync is absent or old; a downstream consumer may enforce
its own freshness policy.

### Conversation

```json
{
  "record_type": "conversation",
  "source_type": "discord",
  "source_identifier": "123456789012345678",
  "id": "987654321098765432",
  "title": "release-thread",
  "conversation_type": "thread",
  "parent_id": "876543210987654321"
}
```

`id` is the provider or importer conversation identity, not msgvault's integer
primary key. The source key plus `id` uniquely identifies the conversation in
the stream. `title` is an empty string when absent. `parent_id` is nullable and
contains the normalized source conversation ID of a parent when the provider
has hierarchical conversations.

`conversation_type` is a closed v1 vocabulary:

- `email_thread`
- `channel`
- `thread`
- `direct_chat`
- `group_chat`
- `meeting`
- `calendar`
- `other`

The export mapping normalizes existing storage values into this vocabulary.
Discord channel types 10, 11, and 12 become `thread`; other Discord containers
become `channel`. Email threads, provider channels, direct and group chats,
meetings, and calendar conversations use their corresponding values. A
conversation that cannot be classified without exposing provider-specific
semantics becomes `other`. Adding another public value requires a schema
version change, so strict consumers never need to guess whether a new raw
storage value is valid.

Only conversations referenced by exported messages are emitted. They are
emitted before messages so a streaming consumer can resolve each message
without buffering future context.

Provider-specific fields such as the raw Discord channel type and thread
archive state are not exported. Thread versus channel is normalized through
`conversation_type`, and hierarchy through `parent_id`. Archive state is
deliberately omitted: it is not a cross-provider conversation lifecycle, and
the existing downstream integration validates but does not use it. A provider
importer may normalize a generally useful relationship into the fields above,
but its raw metadata object is not part of this schema.

### Message

```json
{
  "record_type": "message",
  "source_type": "discord",
  "source_identifier": "123456789012345678",
  "id": "112233445566778899",
  "conversation_id": "987654321098765432",
  "message_type": "discord",
  "subject": "",
  "text": "Complete normalized message text",
  "author": {
    "display_name": "Example User",
    "address": ""
  },
  "occurred_at": "2026-07-24T18:20:31Z",
  "deleted_from_source": false
}
```

The source key plus `id` uniquely identifies a message in the stream.
`conversation_id` refers to a preceding conversation record. `subject` and
`text` are normalized strings and use an empty string when absent.

`author` is nullable when the archive has no normalized sender. Its
`display_name` and `address` fields use empty strings when only one form is
available. Provider mappings may derive these normalized values internally,
but the source metadata from which they were derived is not exported.

`occurred_at` is the effective timestamp used for the bounded filter. It is
named for its contract meaning because the stored value may come from
`sent_at`, `received_at`, or `internal_date`.
`deleted_from_source` preserves archived content while identifying a row that a
later provider scan found deleted. Locally hidden or dedup-suppressed rows are
not exported.

URLs are not a separate field. Consumers can extract them from `text` according
to their own parsing requirements.

### Completion

```json
{
  "record_type": "complete",
  "counts": {
    "sources": 1,
    "conversations": 8,
    "messages": 247
  }
}
```

The completion record is written only after every preceding record has been
encoded successfully. Counts must equal the observed records of each type. A
consumer must reject a stream with no completion record, records after
`complete`, duplicate manifest or completion records, phase-order violations,
or mismatched counts.

The first version does not add a stream checksum. Transport security, storage
checksums, and ordinary file hashing can provide byte integrity when needed;
the completion marker addresses the more immediate partial-stream failure
without adding a second integrity protocol.

## Ordering and identity

Output is byte-for-byte deterministic for a fixed archive state, command
version, window, and filters. Records use these orders:

1. sources by `source_type`, then `identifier`;
2. conversations by source key, then conversation ID; and
3. messages by source key, conversation ID, effective timestamp, then message
   ID.

Message and conversation identities are scoped by the stable source key.
Duplicate scoped identities are an export error. The command does not expose
database primary keys because they can change across restore, migration, or
independent archives.

## Normalization boundary

The store-level exporter owns selection, ordering, normalized joins, and
backend compatibility. It returns or iterates provider-neutral source,
conversation, and message records.

Provider-specific normalization remains behind that boundary:

- Discord author display names and parent channels are decoded internally.
- Email and chat senders use normalized participant relationships.
- Sources without hierarchical conversations emit `parent_id: null`.
- Missing display names, subjects, or bodies become the documented empty
  values.

The public types do not embed raw `messages.metadata` or
`conversations.metadata`. Adding a normalized field in a future schema requires
an explicit schema version change rather than silently exposing new metadata
keys.

The exporter first selects bounded message metadata and identities without
joining `message_bodies`. It then loads each selected body through a direct
primary-key lookup. It must not add body joins to existing list, aggregate,
search, or export-selection queries. SQLite and PostgreSQL implementations use
their normal bound parameters and backend dialect support.

## Data flow and streaming

1. The CLI validates bounds, format, message types, and source-selector syntax.
1. It opens the configured local or remote daemon path.
1. The daemon resolves explicit source selectors and rejects missing ones.
1. The exporter emits the manifest.
1. It emits selected source records and their nullable sync provenance.
1. It emits conversations referenced by matching messages.
1. It streams matching messages and loads each selected full body through the
   bounded export path.
1. It emits the completion record with observed counts.

The implementation may execute separate deterministic queries for the record
phases. It does not need a single database snapshot in v1. Although export is
logically read-only, `export-messages` deliberately remains under the daemon's
operation gate so archive-mutating work cannot run between phases. Direct local
execution uses the same daemon command-routing policy rather than defining a
weaker consistency mode. This favors a coherent stream over concurrent sync
during the bounded export; a future implementation may replace the exclusive
gate with an equivalent cross-backend read snapshot.

The existing daemon CLI transport may wrap subprocess output in its own NDJSON
events internally. The daemon client reconstructs command stdout, so callers
still observe only the message-export JSONL stream.

## Failure handling

- Invalid or non-increasing bounds fail before writing a manifest.
- Invalid source-selector syntax or an unresolved explicit source fails before
  writing a manifest.
- An unsupported format fails before writing a manifest.
- Database, decoding, normalization, context-cancellation, or output errors
  return a nonzero exit status.
- If a failure occurs after streaming begins, the command does not write
  `complete`. Any bytes already written are an invalid partial export.
- Stdout never contains log or progress text.
- Error messages and structured logs must not include message bodies,
  credentials, or raw provider metadata.
- An empty matching window is successful and produces a manifest, any
  explicitly selected source records, zero conversations and messages, and a
  zero-count completion record.

The exporter does not fall back to analytics-cache queries, direct consumer
database access, search endpoints, or provider API calls.

## Security and privacy

The export contains archived message content and is therefore as sensitive as
the vault itself. It uses existing daemon authentication, loopback, host
validation, and operation-gate rules. The feature adds no unauthenticated HTTP
surface.

The command never exports provider tokens, OAuth material, raw provider
metadata, attachment bytes, raw MIME, or internal database IDs. It does not
write files itself; persistence is an explicit caller decision through shell
redirection or another process.

Documentation and fixtures use synthetic identities and content. Public code,
tests, documentation, commits, and pull-request text must pass the repository's
private-data scrub.

## Testing

Store tests cover both supported database backends and verify:

- half-open timestamp bounds and effective timestamp fallback;
- unfiltered, message-type-filtered, source-filtered, and combined selection;
- multiple source types and identifiers;
- explicit source records for empty windows;
- unresolved source errors;
- stable source, conversation, and message ordering;
- scoped identity uniqueness;
- full normalized text and subject;
- normalized authors for email/chat and provider-derived display names;
- parent-conversation normalization;
- source-deleted inclusion and locally hidden exclusion; and
- nullable last-successful-sync provenance.

CLI contract tests decode real command output line by line and verify:

- manifest-first and completion-last framing;
- schema, version, UTC bounds, and normalized filters;
- phase ordering and completion counts;
- compact one-object-per-line JSON;
- no completion record after an injected mid-stream failure;
- no stdout output for preflight errors; and
- daemon routing through the existing CLI command path.

The implementation runs the repository's standard formatting, vet, SQLite,
PostgreSQL, lint, documentation, generated-reference, and private-data checks
required for publication.

## Documentation and compatibility

The public exporting guide documents the generic command, record types,
conversation-type vocabulary, filtering, half-open bounds, deletion semantics,
and partial-stream validation. The CLI reference is regenerated from the
command.

The source repository alone does not define the compatibility surface. An
active downstream already invokes `export-discord`, validates
`msgvault-discord-export/1`, and consumes its URL and container fields. The
cutover is therefore explicitly additive:

1. ship `export-messages` and `msgvault-message-export/1` while retaining the
   existing Discord command and schema;
1. migrate the downstream adapter to the JSONL stream, including consumer-side
   URL extraction, deliberate removal of unused thread archive state, and
   strict `channel`/`thread` validation for Discord sources;
1. verify a representative live window against both contracts; then
1. remove `export-discord` and `msgvault-discord-export/1` in a coordinated
   follow-up.

The compatibility command need not become a permanent alias. It remains only
long enough to prevent a delete-first break while the known consumer migrates.

Future additive record fields require consumers to tolerate unknown fields
within a known schema. Changes to identity, ordering, record phases, field
meaning, required validation, or the closed `conversation_type` vocabulary
advance the schema version.

## Alternatives considered

### Provider-specific export commands

A Discord-only command can expose exactly the fields needed by one consumer,
but it makes msgvault's public surface grow once per provider and encourages
provider metadata to leak into downstream contracts. The normalized archive
model already provides the correct abstraction boundary.

### One monolithic JSON document

A single envelope is convenient for small fixtures but requires msgvault and
consumers to buffer large exports. JSONL retains normalized typed records while
supporting incremental encoding, bounded memory, ordinary Unix pipelines, and
explicit truncation detection.

### Message-only JSONL

Repeating all source and conversation context on every message makes each line
self-contained but duplicates data and obscures relational identity. Omitting
that context requires consumers to infer titles and source provenance. Typed
source and conversation records preserve the normalized model without
repetition.

### SQL query or analytics views

The analytics cache deliberately excludes full message bodies and provider
conversation metadata. Treating SQL views as an interchange contract would
also couple consumers to cache rebuild behavior and table-shaped schema
evolution. Export reads the authoritative archive through a purpose-built
stable model instead.

### Raw provider metadata

Passing through metadata JSON would make the first implementation smaller but
would expose storage details, freeze undocumented importer representations, and
force every consumer to understand every provider. Provider decoding remains
an internal normalization concern.

### Dedicated export HTTP endpoint

A separate HTTP surface could stream JSONL directly, but msgvault already has
an authenticated daemon-aware CLI transport. Reusing it keeps local and remote
behavior aligned and avoids another authorization and routing contract. A
dedicated endpoint can be added later if non-CLI clients establish a concrete
need.
