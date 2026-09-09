---
last_edited: "2026-09-08"
title: Vector Search
description: Find messages by meaning and set up separate people, visual, and document search indexes.
---

Semantic search finds messages by meaning:
a query like "planning offsite agenda" can surface a message titled
"Q2 team kickoff" if the bodies discuss the same topic, even when
none of the query words appear in the result. msgvault builds that
capability on top of keyword search. An embedding is a numeric representation
of text that lets msgvault rank related content. Msgvault sends text to an
embedding endpoint you configure, then stores the vectors in your archive.
SQLite archives store vectors in `vectors.db`.
PostgreSQL archives store them in pgvector tables inside the same
database as the message archive.

When vector search is enabled, the `search` command and HTTP
`/api/v1/search` endpoint accept `mode=vector` (pure semantic) and
`mode=hybrid` (BM25 + vector fused with Reciprocal Rank Fusion). The MCP
equivalent is `semantic_search_messages`. A separate MCP tool,
`find_similar_messages`, returns nearest-neighbor messages for a
given seed. The vectors and archive stay local, but embedding work is
performed by the endpoint in your config. If that endpoint is hosted
by a third party, message text and semantic query text are sent there;
use a local or self-hosted endpoint when you need the workflow to stay
on your own machine or network.

Choose the index for the thing you want to find:

| Content | Search command | Setup |
|---|---|---|
| Messages, chats, and meeting transcripts | `search --mode vector` or `--mode hybrid` | This page |
| Curated people | `person search` | [People](/docs/usage/people/#find-a-person-by-what-you-remember) |
| Image and video content | `multimodal search` | [Visual attachment search](#visual-attachment-search) |
| Extracted document text | `documents search --mode semantic` or `--mode hybrid` | [Document indexing](/docs/usage/document-indexing/#semantic-and-hybrid-document-search) |

Message embeddings do not include attachment pixels or extracted document
text. Those indexes have separate builds and consent. For a guided setup using
the provider keys you have, start with
[Recommended Configuration](/docs/usage/recommended-configuration/).

## Prerequisites

1. **A running embedding endpoint.** msgvault does
   not host a model. Point it at a local, self-hosted, or hosted
   endpoint that you trust. Common local options include [Ollama](https://ollama.com),
   [llama.cpp's `server`](https://github.com/ggerganov/llama.cpp/tree/master/examples/server),
   and [LM Studio](https://lmstudio.ai). On an Apple Silicon Mac,
   [`afm`](https://github.com/scouzi1966/maclocal-api) is another option
   (see the tip below). For `api_format = "openai"`, the endpoint must accept
   `POST /embeddings` with an OpenAI-style JSON body and return
   indexed data rows such as
   `{"data": [{"index": 0, "embedding": [...]}]}`.

2. **A build with a vector backend.** The standard `make build`
   target already passes `-tags "fts5 sqlite_vec"`. If you see errors
   mentioning "binary was built without -tags sqlite_vec", rebuild
   via `make build` (or `go build -tags "fts5 sqlite_vec"` if you are
   invoking `go build` directly). The official Docker image also includes
   `sqlite_vec`, so SQLite-backed container deployments do not need a custom
   image for vector search. PostgreSQL vector search additionally requires the
   `pgvector` build tag, for example
   `go build -tags "fts5 sqlite_vec pgvector" ./cmd/msgvault`.

!!! tip "Local embeddings on Apple Silicon"
    [`afm`](https://github.com/scouzi1966/maclocal-api)
    serves OpenAI-compatible embeddings from Apple's on-device NaturalLanguage model
    over Metal, with no Python, no cloud, and no API key. Requires macOS 26 (Tahoe) or
    later on an Apple Silicon Mac with Apple Intelligence enabled.

    ```bash
    brew tap scouzi1966/afm && brew install afm
    afm embed                      # serves http://127.0.0.1:9998/v1
    ```

    Then point the `[vector.embeddings]` block below at it:

    ```toml
    [vector.embeddings]
    endpoint = "http://127.0.0.1:9998/v1"
    model = "apple-nl-contextual-en"   # or apple-nl-contextual-multi (Latin-script)
    dimension = 512                    # Apple's English contextual model is 512-dim
    batch_size = 50                    # batches are serialized server-side; 50 balances HTTP overhead vs batch-rejection risk
    # afm needs no api_key_env
    ```

### Windows source builds

The sqlite-vec CGo binding needs `sqlite3.h` at compile time, and the
MinGW 15 toolchain needs two extra flags to link arrow-go/v18's
helpers. The easiest path is `powershell -File scripts/build.ps1`,
which wires everything up automatically. To invoke `go build`
yourself from PowerShell:

```powershell
C:\msys64\usr\bin\pacman.exe -S --noconfirm --needed mingw-w64-x86_64-sqlite3
$env:CGO_ENABLED = "1"
$env:CGO_CFLAGS = "-IC:/msys64/mingw64/include -fgnu89-inline"
$env:CGO_LDFLAGS = "-Wl,--allow-multiple-definition"
go build -tags "fts5 sqlite_vec" -o msgvault.exe ./cmd/msgvault
```

## Enable

Use `msgvault setup providers` to select defaults, or add a `[vector]` block
to `~/.msgvault/config.toml`. This manual example uses an OpenAI-compatible
endpoint:

```toml
[vector]
enabled = true
backend = "sqlite-vec"
# db_path defaults to <data_dir>/vectors.db when empty.
# db_path = "/path/to/vectors.db"

[vector.embeddings]
endpoint = "http://127.0.0.1:11434/v1"
api_key_env = "OLLAMA_API_KEY"           # optional; omit for anonymous endpoints
model = "nomic-embed-text"
dimension = 768
document_prefix = "search_document: "  # required by nomic-embed-text
query_prefix = "search_query: "        # required by nomic-embed-text
batch_size = 32                          # embeddings per HTTP call
timeout = "30s"
max_retries = 3
max_input_chars = 2000                   # per-chunk cap; see sizing guidance below
eta_window = 10                          # progress ETA smoothing window

[vector.preprocess]
strip_quotes = true                      # drop quoted reply blocks before embedding
strip_signatures = true                  # drop common `-- ` signature blocks
strip_html = true                        # convert HTML-only bodies to text and remove markup
strip_base64 = true                      # remove base64/data blobs before HTML stripping
strip_url_tracking = true                # remove common tracking query params from URLs
collapse_whitespace = true               # normalize repeated spaces and blank lines

[vector.search]
rrf_k = 60                               # RRF constant; higher flattens score differences
k_per_signal = 100                       # candidate pool size per signal (BM25 or vector)
subject_boost = 2.0                      # score boost when a query term hits the subject
max_page_size_hybrid = 50                # hard cap on vector/hybrid page_size

[vector.embed.schedule]
cron = "*/5 * * * *"                     # embed worker cron (5-field); empty disables cron
run_after_sync = true                    # run a pass after every successful scheduled sync

[vector.embed.scope]
# Optional: leave empty for the full archive, or restrict new generations.
# message_types = ["teams"]
```

The `[vector]` section only takes effect when `enabled = true` **and**
the binary was built with the needed vector backend. If either is
missing, msgvault behaves as before. Disabled vector search returns
`vector_not_enabled` from server surfaces; a binary built without the
needed backend reports a rebuild-with-vector-backend error when vector
features are requested.

### Contextual chat and meeting embeddings

With `api_format = "voyage-contextual"`, Beeper messages are embedded with
nearby messages from the same conversation. This helps retrieve short replies
whose meaning depends on what came before. Meetings are split along speaker
turns with their meeting context. Other message types, including email and
chat imports outside Beeper, remain individual documents with overlapping
chunks when needed.

```toml
[vector.embeddings]
api_format = "voyage-contextual"
endpoint = "https://api.voyageai.com/v1"
api_key_env = "VOYAGE_API_KEY"
model = "voyage-context-4"
dimension = 1024
```

This API format requires `voyage-context-4`. Switching from an
OpenAI-compatible endpoint changes the index policy and requires a full
rebuild. Results still refer to the original messages and matching text.
The worker tracks edits, late-arriving messages, and deletions so affected
conversation windows can be refreshed.

### PostgreSQL and pgvector

When `[data].database_url` is a PostgreSQL DSN, msgvault selects the
pgvector backend at runtime. Use `backend = "pgvector"` as the config
marker and build with the `pgvector` tag:

```toml
[data]
database_url = "postgres://user:pass@host:5432/msgvault?sslmode=require"

[vector]
enabled = true
backend = "pgvector"

[vector.embeddings]
endpoint = "http://localhost:11434/v1"
model = "nomic-embed-text"
dimension = 768
document_prefix = "search_document: "
query_prefix = "search_query: "
```

pgvector embeddings live in the PostgreSQL database. `db_path` and
`vectors.db` apply only to the SQLite sqlite-vec backend. See
[PostgreSQL Backend](/docs/architecture/postgresql/) for database setup.

### Model task prefixes

Some embedding models require different task instructions for indexed
documents and search queries. The `nomic-embed-text` examples above use
its required retrieval prefixes. Set `document_prefix` and `query_prefix`
to the values required by your model, or leave them empty for models that
do not use instructions.

msgvault prepends `document_prefix` to every chunk after chunking and
`query_prefix` to every vector-search query. Prefix characters do not
reduce the `max_input_chars` content budget. Changing either prefix marks
the existing vector generation stale so prefixed queries cannot be mixed
with an index built from unprefixed documents.

### Matching `max_input_chars` to your embedder's context window

`max_input_chars` is an upper bound in characters per embedding
chunk; the embedder converts this to tokens on its own. Set it below
the embedder's maximum context or individual chunks can fail with
HTTP 400 during `msgvault embeddings build`.

Long post-preprocess messages are split into overlapping chunks
instead of being truncated to one embedding input. Chunk boundaries
prefer paragraph breaks, then sentence breaks, then word boundaries,
falling back to a hard rune boundary only when needed.

Practical guidance:

- **2k-token embedding models:** start around `max_input_chars = 2000`
  and raise only after confirming the endpoint accepts longer inputs.
- **8k-token embedding models:** start around `max_input_chars = 24000`.
- **Self-hosted models:** match the actual context window exposed by
  your server, not just the upstream model card.

If `msgvault embeddings build` logs `HTTP 400`, msgvault now includes
the response body from the embedder when available. Check both the
CLI log and the embedder's own logs. `the input length exceeds the
context length` confirms you need to lower `max_input_chars`.

## Initial Embedding

Once vector search is enabled and your archive has synced or imported
messages, embed it:

```bash
msgvault embeddings build --full-rebuild --yes
```

This creates a new **building generation**, scans every non-deleted
message in the configured scope, embeds missing rows in batches through your
configured embedder, and atomically activates the generation once
coverage reaches zero. During the
first build, when no active generation exists yet, HTTP and MCP
vector/hybrid search return `index_building`. For interim keyword search,
use `mode=fts` on the CLI or HTTP API. In MCP, omit `mode` for metadata
search or use `search_message_bodies` for body keywords.

!!! tip
    You can interrupt and resume. Each invocation of `msgvault embeddings build`
    scans for messages still missing coverage and activates the generation
    when coverage reaches zero. `Ctrl+C` is safe; run `msgvault embeddings build`
    again and it picks up from where it left off.

The initial embed is the largest and longest operation. Runtime is
roughly proportional to archive size divided by embedding throughput.
Progress output reports completed/total messages, a recent-window
throughput rate, milliseconds per message, microseconds per character,
and an ETA once enough samples are available. If a failing batch is
downshifted to smaller requests, the ETA window keeps those singleton
retries from dominating the displayed rate.

## Keeping the Index Up to Date

After the initial rebuild, new messages arriving via email sync need
to be embedded as well. msgvault handles this in two ways depending
on how you run it.

### CLI workflow (manual syncs)

If you run `msgvault sync-full` or `msgvault sync` (alias:
`sync-incremental`) by hand, new Gmail and IMAP messages persist with
`embed_gen = NULL`. In steady state, `msgvault embeddings build` scans
those rows and tops up the active generation. During a rebuild, the
worker targets the building generation first so it can activate; the old
active generation keeps serving vector and hybrid search, but is frozen
and will not receive top-ups until the build activates. Run
`msgvault embeddings build` (no `--full-rebuild`) to continue the scan:

```bash
# Sync new messages (marks them as needing embedding)
msgvault sync you@gmail.com

# Scan and embed missing rows for the active or building generation
msgvault embeddings build
```

`msgvault embeddings build` without `--full-rebuild` is a short, incremental
operation: it resumes a matching building generation if one exists,
otherwise it tops up the configured active generation, and exits.
`msgvault embeddings resume` is a synonym for this drain that never starts a
full rebuild. You can schedule either via cron, run it after every sync, or
chain it (`msgvault sync && msgvault embeddings build`).

### Daemon workflow (`msgvault serve`)

In daemon mode the scheduler can run both pieces automatically. The
`[vector.embed.schedule]` section controls the embed worker
independently from the sync scheduler:

```toml
[vector.embed.schedule]
cron = "*/5 * * * *"      # run every 5 minutes
run_after_sync = true     # and opportunistically after every scheduled sync
```

With `run_after_sync = true`, every successful scheduled sync
triggers an immediate embed pass over messages still missing coverage.
The standalone cron ensures embedding catches up even when syncs are
quiet (e.g. overnight). An empty `cron = ""` disables the
standalone schedule (useful if you only want the post-sync
trigger).

### What Triggers Embedding

| Ingest path | Runs the embed worker? |
|---|---|
| Manual `sync-full` / `sync` (Gmail, IMAP) | No. Run `msgvault embeddings build` afterward |
| Manual `sync-calendar` / `sync-teams` / `sync-discord` | No. Run `msgvault embeddings build` afterward |
| Manual `sync-slack` / `sync-beeper` / `sync-granola` / `sync-circleback` / `sync-notion` | No. Run `msgvault embeddings build` afterward |
| Scheduled account syncs in `msgvault serve` (Gmail, IMAP, Teams, Discord) | Yes, when `[vector.embed.schedule].run_after_sync = true` |
| Scheduled calendar, Slack, Beeper, Granola, Circleback, and Notion syncs in `msgvault serve` | No immediate post-sync run. Picked up by the embed worker's `[vector.embed.schedule].cron` schedule |
| Local email imports (`import-pst`, `import-emlx`, `import-eml`, `import-mbox`) | No. Run `msgvault embeddings build` afterward |
| Local chat/text imports | No. Run `msgvault embeddings build` afterward |

For ingest paths that do not immediately schedule embedding work, running
`msgvault embeddings build` fills missing coverage in the current index. Use
`msgvault embeddings resume --backstop` to recover older coverage gaps that a
normal incremental pass skips. A full rebuild is needed when the model or
index policy changes, or when you want to replace the whole generation.

A same-model full
rebuild is atomic from the searcher's perspective: vector and hybrid
queries keep answering from the previous active generation until the
new one is ready. That previous active generation is intentionally frozen
during the rebuild, so messages synced after the rebuild starts may not appear
in vector or hybrid results until the building generation activates. If the
rebuild changes the configured model or
dimension, vector and hybrid queries return `index_stale` until the
new generation activates.

### CAS resolution (accepted single-user residual)

When a message's text changes during embedding (for example
`msgvault repair-encoding` rewriting a body while an embed run is in flight),
the embed worker uses an optimistic compare-and-set on the message's
`last_modified` timestamp to avoid stamping an embedding built from stale
text: if `last_modified` moved between the worker reading the content and
writing the coverage stamp, the stamp is skipped and the message is re-embedded
on a later run.

`last_modified` has **1-second resolution** (it is a `CURRENT_TIMESTAMP`
default/trigger). A concurrent edit that lands in the *same whole second* as
the worker's content read leaves `last_modified` unchanged, so the CAS can
mark an embedding current even though it was built from the now-stale text.
This sub-second window is an accepted residual for this single-user tool — an
edit and an embed of the *same* message within the *same second* is rare. It
self-recovers: the next edit to that message bumps `last_modified` (and
`repair-encoding` clears its coverage stamp outright), and a full rebuild
(`embeddings build --full-rebuild`) or the periodic full-scan backstop
re-embeds it regardless.

## Upgrading an existing archive

Archives that already had embeddings before the generation-based coverage
tracking landed are migrated in place on the first writable open. There is no
expected data loss: existing active vectors are preserved. They keep serving
vector and hybrid search only if their fingerprint matches the current embedding
policy and configuration. For example, generations built by v0.14 use an older
fingerprint and require a full rebuild even with unchanged configuration; see
[When a full rebuild is required](#when-a-full-rebuild-is-required).

The migration runs the first time you start a writable daemon or run an
embeddings command against an older database. It:

- adds and stamps `messages.embed_gen`, then backfills coverage from the
  active vector generation, so messages that already have active embeddings are
  marked covered. This backfill does not re-queue the active generation's corpus.
- leaves messages that had a pending re-embed against the active generation
  uncovered rather than marking them covered, so they stay missing and the
  scan-based worker can find and re-embed them, including below-watermark rows
  on a backstop pass. The legacy `pending_embeddings` table is consulted for this, then
  dropped once the migration completes.

Only the active generation is backfilled. A rebuild already in flight at upgrade
time receives no coverage stamps for its existing vectors and re-embeds those
messages when resumed.

After the upgrade, coverage is tracked entirely through `messages.embed_gen`:
a new or changed message becomes "missing" by clearing its `embed_gen` rather
than by being queued in a separate table. The scan-and-fill worker finds those
rows and tops up the active generation.

For a generation whose fingerprint still matches, `msgvault serve` with an embed
schedule runs a backstop automatically on its first embed pass for each generation
and then at the first pass after each backstop interval (24 hours by default,
unless disabled). To finish coverage manually for stragglers left after the
migration, run:

```bash
msgvault embeddings resume --backstop
```

`--backstop` runs a full-scan pass that ignores the per-generation watermark, so
it catches below-watermark rows a normal incremental resume would skip. It tops
up an already-active generation; if a rebuild is in flight, it activates the
building generation once missing coverage reaches zero.

### When a full rebuild is required

If the active generation's fingerprint no longer matches the current embedding
policy or configuration, including a policy change shipped in an upgrade, vector
and hybrid search report `index_stale`; run
`msgvault embeddings build --full-rebuild --yes` to build a matching generation.
See [Model Rotation](#model-rotation) for fingerprint inputs and search behavior
while the rebuild runs.

## Scoped Generations

Large mixed archives can build a vector index for only selected message types:

```toml
[vector.embed.scope]
message_types = ["teams"]
```

The scope is part of the generation fingerprint. Changing it requires
`msgvault embeddings build --full-rebuild --yes`, just like changing the model
or preprocessing policy. Scoped generations are useful when you want semantic
search for a newer corpus such as Teams or SMS without embedding decades of
email immediately.

Generations can also be scoped to selected accounts, either durably in config
(so the daemon's scheduled embeds obey it too):

```toml
[vector.embed.scope]
accounts = ["you@gmail.com"]
```

or per run from the CLI (overriding the configured accounts for that run):

```bash
msgvault embeddings build --full-rebuild --account you@gmail.com
msgvault embeddings build --account you@gmail.com --account you@work.com
msgvault embeddings build --collection family   # every account in the collection
```

!!! warning
    `--account` and `--collection` are one-run overrides. After they activate
    a scoped generation, add the equivalent stable account identifiers under
    `[vector.embed.scope].accounts` and restart the daemon before searching.
    Otherwise the daemon expects a different generation fingerprint and
    returns `index_stale`. Prefer the config form when the scoped index is
    meant to persist.

The `--account` flag accepts an identifier or display name like elsewhere,
but the durable `[vector.embed.scope] accounts` list requires canonical
account identifiers — display names are rejected because they are not
stable identities for a privacy boundary. Unknown identifiers fail the run
instead of silently embedding more than requested. Account scoping acts as a privacy boundary —
message text from accounts *outside* the scope is never sent to the embedding
endpoint — and is the cheapest way to pilot semantic search on one mailbox
before paying to embed the whole archive.

Account-scope caveats:

- The fingerprint records the scope as source IDs, which are archive-local.
  Removing and re-adding an account (or restoring a backup) can renumber its
  source ID, producing a new fingerprint and requiring a full rebuild.
- A scoped account resolves to *all* of its sources, including linked ones
  such as a Google Calendar synced for the same account. Adding such a
  source after a scoped generation activates changes the resolved source
  set and therefore the fingerprint, so vector search reports `index_stale`
  until a full rebuild.
- Unlike a message-type scope, an account scope does NOT gate search:
  unfiltered vector/hybrid queries keep working, and accounts outside the
  scope simply have no vector matches (hybrid search ranks them on the BM25
  signal alone). The web UI's semantic-coverage readout counts only in-scope
  accounts as eligible.

A scoped index is intentionally partial, so vector and hybrid search require an
explicit compatible message-type filter:

```bash
msgvault search "release planning" --mode hybrid --message-type teams
msgvault search "message_type:teams release planning" --mode vector
```

If the active generation is scoped to `teams`, an unscoped vector/hybrid query
or a query scoped to `email` returns `index_scope_mismatch`. For unscoped
keyword search, use `mode=fts` on the CLI or HTTP API; in MCP, omit `mode` for
metadata or use `search_message_bodies` for body keywords. Alternatively, add
the matching message-type filter or rebuild an unscoped generation by clearing
`[vector.embed.scope].message_types` and running a full rebuild.

## Search

**CLI:**

```bash
msgvault search "planning offsite agenda" --mode hybrid
msgvault search "planning offsite agenda" --mode vector --explain
msgvault search "..." --json --mode hybrid    # JSON output with scores
```

CLI vector and hybrid modes use the configured remote server when
`[remote].url` is set; otherwise they use the local daemon. The selected
server must have vector search configured.

**HTTP:**

```bash
curl "http://localhost:8080/api/v1/search?q=planning+offsite&mode=hybrid"
curl "http://localhost:8080/api/v1/search?q=planning+offsite&mode=vector&explain=1"
```

Response shape differs from the FTS path; see the
[Web UI & API Server](/docs/api-server/#get-apiv1search) reference for details.
HTTP vector/hybrid responses support only the first page; bump
`page_size` (capped at `max_page_size_hybrid`) to retrieve a larger
candidate page.

`mode=vector` and `mode=hybrid` require at least one free-text term:
the free text is what gets embedded as the query vector. A query
that is purely operators (e.g. `from:alice label:IMPORTANT`) is
rejected; HTTP and MCP return `missing_free_text`. Use `mode=fts` for
filter-only CLI or HTTP queries. In MCP, omit `mode` for metadata-only
filtering.

**MCP tools:**

- `search_metadata` searches subject, sender/recipient, label, date, and other
  metadata fields.
- `semantic_search_messages` accepts explicit `vector` or `hybrid` modes plus
  `explain` and `min_score` arguments. It
  paginates with `offset` and `limit`; for vector/hybrid modes, pagination
  is limited to the configured
  `[vector.search].max_page_size_hybrid` ranking window when that cap
  is positive. Requests whose `offset` is at or beyond that window
  return `pagination_limit`.
- `search_message_bodies` performs body-only full-text search and returns
  bounded context around each keyword match. It requires a free-text term
  and does not depend on the vector index.
- `search_in_message` performs literal keyword matching within one message; it
  does not expose a vector mode through `msgvault mcp`.
- `find_similar_messages` takes a seed `message_id` and returns
  nearest neighbors (excluding the seed itself). Optional `account`,
  `after`, `before`, `has_attachment` filters.
- `search_messages` is a deprecated compatibility wrapper that dispatches to
  `search_metadata` when mode is omitted and `semantic_search_messages` for
  vector/hybrid modes.

## Model Rotation

To switch models, dimensions, task prefixes, preprocessing settings, or
`max_input_chars`, update your config, then run:

```bash
msgvault embeddings build --full-rebuild --yes
```

This builds a new generation with the new fingerprint and activates
it atomically when the build completes. The fingerprint includes the
API format, model, dimension, task prefixes, preprocessing policy, `max_input_chars`,
embedding output policy, and [scope](#scoped-generations). While the rebuild is in flight,
`mode=vector` and `mode=hybrid` return `index_stale` (the
previously-active generation no longer matches the configured
fingerprint, so search refuses to serve potentially-mismatched
results). On the CLI or HTTP API, use `mode=fts` until the new generation
activates; it does not depend on the vector index. In MCP, omit `mode` for
metadata search or use `search_message_bodies`. Once `msgvault embeddings
build` reports the new generation activated, vector and hybrid modes resume.

## Troubleshooting

Common HTTP/MCP error codes and fixes. The CLI reports equivalent
conditions as command errors rather than structured codes.

In the table, a non-vector fallback means `mode=fts` for the CLI or HTTP,
omitted `mode` for MCP metadata search, or `search_message_bodies` for MCP
body keywords.

| Error | Meaning | Recovery |
|---|---|---|
| `vector_not_enabled` | The server or MCP process did not wire a vector backend, usually because `[vector] enabled = false`. | Set `enabled = true`, configure `[vector.embeddings]`, and start with a build that includes the needed backend (`sqlite_vec` or `pgvector`). |
| `index_stale` | Active generation's fingerprint does not match the current embedding settings: model, dimension, task prefixes, preprocessing policy, `max_input_chars`, output policy, or scope. | For an existing account-scoped index built with CLI flags, set matching `[vector.embed.scope].accounts` and restart the daemon. Otherwise run `msgvault embeddings build --full-rebuild --yes`. |
| `index_building` | No active generation yet; one is being built. | Finish running `msgvault embeddings build`, wait for the scheduler, or use the appropriate non-vector fallback. |
| `missing_free_text` | `mode=vector` or `mode=hybrid` used with a filter-only query (no free text to embed). | Add free-text terms to `q`, or use the appropriate non-vector fallback. |
| `index_scope_mismatch` | The active vector generation was built for selected message types and the query is unscoped or asks for a type outside that scope. | Add a compatible `message_type` filter, use the appropriate non-vector fallback, or rebuild an unscoped generation. |
| `pagination_unsupported` | HTTP request asked for `page>1` with `mode=vector|hybrid`. | Use `page=1` with a larger `page_size` instead. |
| `pagination_limit` | MCP `semantic_search_messages` asked for an `offset` at or beyond a positive `[vector.search].max_page_size_hybrid` cap. | Use `search_metadata` for metadata search, use `search_message_bodies` for body keywords, request an earlier vector page, or raise/disable the hybrid page-size cap. |
| `invalid_mode` | The requested mode is not supported by that surface. | Use `fts`, `vector`, or `hybrid` on the CLI or HTTP; use `vector`, `hybrid`, or omitted `mode` in MCP. |
| `embedding_timeout` | The embedding endpoint did not respond before the request deadline (transient: slow/cold model, network blip). | Retry; if persistent, raise `[vector.embeddings].timeout` or use a faster endpoint. |

For non-429 HTTP 4xx errors, msgvault treats the response as
permanent and includes up to the first few KiB of the response body in
the error. If a batch contains both good and bad rows, the worker
downshifts to smaller batches and then single-message requests so
valid messages can still be embedded while the failing row is dropped
or reported. If the body says `the input length exceeds the context
length` (Ollama) or an equivalent token-limit error, lower
`max_input_chars` to match the model's context window. See the sizing
guidance above.

To confirm the binary was built with vector support:

```bash
msgvault search "probe" --mode vector
```

A clear rebuild-with-vector-backend error indicates the tag is missing.
A different error (`vector_not_enabled`, `index_stale`, etc.) means
the command moved past the build-tag check and is now waiting on
config or backfill.

Check index health via the stats endpoint:

```bash
curl -H "X-API-Key: ..." http://localhost:8080/api/v1/stats | jq .vector_search
```

Compare coverage with eligible, active messages in the configured scope;
source-deleted and out-of-scope messages are excluded. During a rebuild,
`active_generation.message_count` reports
the frozen serving index, while `building_generation.progress` reports the
replacement index. `missing_embeddings_total` shows how many live messages
still need embedding for the generation the worker will target next: the
building generation during a rebuild, otherwise the active generation.

## What Gets Embedded

The OpenAI-compatible embedder processes one or more vectors per message. Per-message
input is assembled from `subject` and `body_text`. HTML-only messages
fall back to `body_html` converted to text. After preprocessing, long
messages are split into overlapping chunks; each chunk becomes one
vector tied back to the same message.

- Optional stripping of quoted-reply blocks (`> ...` lines and
  common reply-preamble markers).
- Optional stripping of trailing signatures (lines after `-- `).
- Optional removal of base64/data blobs before HTML cleanup.
- Optional removal of HTML markup and script/style blocks.
- Optional removal of common URL tracking parameters such as `utm_*`,
  `fbclid`, and `gclid`.
- Optional whitespace cleanup.
- Chunking at `max_input_chars` with overlap for long messages.

Messages deleted at the source (`deleted_from_source_at IS NOT NULL`)
are skipped entirely. Messages that become empty after preprocessing
are marked complete and not sent to the embedding endpoint.

The [contextual API format](#contextual-chat-and-meeting-embeddings) uses
conversation windows for Beeper and meeting-aware chunks for transcripts.
The configured account and message-type scope applies before their content is
assembled. Curated people are an optional, separately consented input to the
same generation; see [semantic person search](/docs/usage/people/#find-a-person-by-what-you-remember).

## Visual attachment search

Visual search finds archived images and videos from a description such as
"a whiteboard with a project timeline", or from a similar image you supply.
It returns the attachment and its containing message. It uses a separate
Voyage index; enabling message search does not enable visual uploads.

An authenticated capability probe establishes which formats and query types
the configured provider can process. JPEG, PNG, WebP, and direct MP4 are the
configured defaults, subject to the probe results. Animated GIFs are a separate
opt-in. Msgvault sends eligible attachment bytes and bounded containing-message
text to the provider. Search sends your query text or query image there too.

### Enable and build

The guided path is `msgvault setup providers` with `VOYAGE_API_KEY` set. Setup
leaves visual indexing pending until a probe manifest exists. To create one,
prepare a private directory containing synthetic files named
`image_webp.webp`, `image_webp_alt.webp`, `video_mp4.mp4`, and
`video_mp4_alt.mp4`. Each alternate should have visibly different content
from its primary file. The probe generates the remaining synthetic fixtures.

```bash
msgvault multimodal probe \
  --seeds /private/msgvault/visual-seeds \
  --out ~/.msgvault/voyage-capabilities.json --yes
msgvault setup providers
msgvault daemon restart
msgvault multimodal build --yes
msgvault multimodal status --json
```

Review the probe manifest before enabling the lane. For manual configuration,
set `[vector.multimodal].enabled = true` and `capabilities_file` to its path;
the [configuration reference](/docs/configuration/#vectormultimodal) lists the
provider, format, scope, context-size, and schedule settings. The manifest
must be accessible on the daemon host.

`multimodal build --yes` records consent for the exact capability profile and
builds the index. Use `multimodal resume` after an interrupted build. Changes
to configuration or capability evidence require matching consent again.
Status reports coverage, stale items, retryable work, and pending changes.

### Search and maintain

```bash
msgvault multimodal search "a whiteboard with a project timeline"
msgvault multimodal search --image ./reference.png
msgvault multimodal search "a receipt" --person 123 --direction from_person
msgvault multimodal search "a diagram" --source 4 --after 2026-01-01 --json
```

Supply exactly one text query or `--image`. Query images must be JPEG, PNG,
or WebP, at most 20 MiB, and allowed by the configured capability profile.
Use `--limit` (1–100, default 20) and the returned `--cursor` for paging.
Other filters include `--message`, `--filename`, `--mime-prefix`, and
`--before`.

`--person` and `--participant` are mutually exclusive. `--direction` requires
one of them and accepts `from_person`, `to_person`, or `group`. The narrower
`--sender-person` filter cannot be combined with those three flags. Person
filters use archive identity bindings, not a similarity guess about who
appears in an image.

Use `multimodal retry --message <id> --hash <sha256>` for one failed attachment
and `multimodal resume` to continue processing. `multimodal retire
<generation-id> --yes` retires that generation and deletes its vectors.
Original attachments remain in the archive.

MCP clients use `search_visual_attachments` for visual queries,
`search_document_attachments` for document text, and `search_person_files` for
attachment metadata. The CLI `person files --lane all` combines those search
paths for one person. The Web UI's [Operations workspace](/docs/web-ui/#operations) shows
visual coverage and the build or resume actions the daemon currently allows.

## See Also

- [Web UI & API Server](/docs/api-server/): browser interface and HTTP API reference.
- [Searching](/docs/usage/searching/): Full-text search syntax.
- [Search Ranking Across Backends](/docs/architecture/search-ranking/): Ranking differences between SQLite, PostgreSQL, sqlite-vec, and pgvector.
