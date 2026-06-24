---
title: Vector Search
description: Semantic and hybrid search over your archive using a configured embedding endpoint.
---


Semantic search finds messages by meaning, not just keyword overlap:
a query like "planning offsite agenda" can surface a message titled
"Q2 team kickoff" if the bodies discuss the same topic, even when
none of the query words appear in the result. msgvault builds that
capability on top of the default keyword search by sending message
text to an embedding endpoint you configure, then storing the
vectors locally. SQLite archives store vectors in `vectors.db`.
PostgreSQL archives store them in pgvector tables inside the same
database as the message archive.

When vector search is enabled, the `search` command, the HTTP
`/api/v1/search` endpoint, and the MCP `search_messages` tool all
accept `mode=vector` (pure semantic) and `mode=hybrid` (BM25 +
vector fused with Reciprocal Rank Fusion). A separate MCP tool,
`find_similar_messages`, returns nearest-neighbor messages for a
given seed. The vectors and archive stay local, but embedding work is
performed by the endpoint in your config. If that endpoint is hosted
by a third party, message text and semantic query text are sent there;
use a local or self-hosted endpoint when you need the workflow to stay
on your own machine or network.

!!! note
    Vector indexing operates over msgvault's shared `messages` table. A
    full rebuild embeds every non-deleted message row, including imported
    chat messages. Chat import commands do not auto-enqueue new rows into
    an existing vector generation, so run
    `msgvault embeddings build --full-rebuild --yes` after importing
    local files or chat/text data if you want those messages in
    the vector index. Chat-specific preprocessing and ranking are not
    separate yet.

## Prerequisites

1. **A running OpenAI-compatible embedding endpoint.** msgvault does
   not host a model. Point it at a local, self-hosted, or hosted
   endpoint that you trust. Common local options include [Ollama](https://ollama.com),
   [llama.cpp's `server`](https://github.com/ggerganov/llama.cpp/tree/master/examples/server),
   and [LM Studio](https://lmstudio.ai). On an Apple Silicon Mac,
   [`afm`](https://github.com/scouzi1966/maclocal-api) is the fastest path
   (see the tip below). The endpoint must accept
   `POST /embeddings` with an OpenAI-style JSON body and return
   indexed data rows such as
   `{"data": [{"index": 0, "embedding": [...]}]}`.

2. **A build with a vector backend.** The standard `make build`
   target already passes `-tags "fts5 sqlite_vec"`. If you see errors
   mentioning "binary was built without -tags sqlite_vec", rebuild
   via `make build` (or `go build -tags "fts5 sqlite_vec"` if you are
   invoking `go build` directly). PostgreSQL vector search additionally
   requires the `pgvector` build tag, for example
   `go build -tags "fts5 sqlite_vec pgvector" ./cmd/msgvault`.

!!! tip "Fastest path to using embeddings on Mac"
    On a Mac, the quickest endpoint is [`afm`](https://github.com/scouzi1966/maclocal-api):
    it serves OpenAI-compatible embeddings from Apple's on-device NaturalLanguage model
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

Add a `[vector]` block to `~/.msgvault/config.toml`:

```toml
[vector]
enabled = true
backend = "sqlite-vec"
# db_path defaults to <data_dir>/vectors.db when empty.
# db_path = "/path/to/vectors.db"

[vector.embeddings]
endpoint = "http://tailnet-host:11434/v1"
api_key_env = "OLLAMA_API_KEY"           # optional; omit for anonymous endpoints
model = "nomic-embed-text"
dimension = 768
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
```

The `[vector]` section only takes effect when `enabled = true` **and**
the binary was built with the needed vector backend. If either is
missing, msgvault behaves as before. Disabled vector search returns
`vector_not_enabled` from server surfaces; a binary built without the
needed backend reports a rebuild-with-vector-backend error when vector
features are requested.

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
```

pgvector embeddings live in the PostgreSQL database. `db_path` and
`vectors.db` apply only to the SQLite sqlite-vec backend. See
[PostgreSQL Backend](/architecture/postgresql/) for database setup.

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

This creates a new **building generation**, seeds the pending queue
with every non-deleted message in your archive, drains the queue in
batches through your configured embedder, and atomically activates
the generation once every pending row has been embedded. During the
first build, when no active generation exists yet, HTTP and MCP
vector/hybrid search return `index_building`; use `mode=fts` for the
interim.

!!! tip
    You can interrupt and resume. Each invocation of `msgvault embeddings build`
    processes whatever is currently pending and activates the generation
    when the queue reaches zero. `Ctrl+C` is safe; run `msgvault embeddings build`
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
`sync-incremental`) by hand, new Gmail and IMAP messages are
**auto-enqueued** into every
non-retired generation during the sync. In steady state that means the
active generation; during a rebuild it means both the old active
generation and the new building generation. Run
`msgvault embeddings build` (no `--full-rebuild`) to drain the queue:

```bash
# Sync new messages (auto-enqueues them for embedding)
msgvault sync you@gmail.com

# Drain the embedding queue into the active generation
msgvault embeddings build
```

`msgvault embeddings build` without `--full-rebuild` is a short, incremental
operation: it picks up the configured active generation, drains any
pending rows, and exits. `msgvault embeddings resume` is a synonym for this
drain that never starts a full rebuild. You can schedule either via cron,
run it after every sync, or chain it (`msgvault sync && msgvault embeddings build`).

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
triggers an immediate embed pass against the queue it just
populated. The standalone cron ensures the queue drains even when
syncs are quiet (e.g. overnight). An empty `cron = ""` disables the
standalone schedule (useful if you only want the post-sync
trigger).

### What auto-enqueues

| Ingest path | Auto-enqueues? |
|---|---|
| `sync-full` / `sync` (Gmail, IMAP) | Yes |
| Scheduled syncs in `msgvault serve` | Yes |
| `import-pst`, `import-emlx`, `import-mbox` | No. Re-run `--full-rebuild` after large imports |
| Chat/text imports (iMessage, WhatsApp, Google Voice, Messenger, SyncTech SMS) | No. Run a full rebuild after importing if you want chats included |

For ingest paths that do not auto-enqueue, running
`msgvault embeddings build --full-rebuild --yes` rebuilds the index over the
full archive including the newly-imported messages. A same-model full
rebuild is atomic from the searcher's perspective: vector and hybrid
queries keep answering from the previous active generation until the
new one is ready. If the rebuild changes the configured model or
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

## Search

**CLI:**

```bash
msgvault search "planning offsite agenda" --mode hybrid
msgvault search "planning offsite agenda" --mode vector --explain
msgvault search "..." --json --mode hybrid    # JSON output with scores
```

CLI vector and hybrid modes run against the local archive. If
`[remote].url` is configured, `msgvault search --mode vector|hybrid`
is rejected; call the remote server's HTTP `/api/v1/search` endpoint
directly for remote vector search.

**HTTP:**

```bash
curl "http://localhost:8080/api/v1/search?q=planning+offsite&mode=hybrid"
curl "http://localhost:8080/api/v1/search?q=planning+offsite&mode=vector&explain=1"
```

Response shape differs from the FTS path; see the
[Web Server](/api-server/#get-apiv1search) reference for details.
HTTP vector/hybrid responses support only the first page; bump
`page_size` (capped at `max_page_size_hybrid`) to retrieve a larger
candidate page.

`mode=vector` and `mode=hybrid` require at least one free-text term:
the free text is what gets embedded as the query vector. A query
that is purely operators (e.g. `from:alice label:IMPORTANT`) is
rejected; HTTP and MCP return `missing_free_text`. Use `mode=fts` for
those.

**MCP tools:**

- `search_messages` accepts `mode` (`fts`/`vector`/`hybrid`) and
  `explain` arguments. It paginates with `offset` and `limit`; for
  vector/hybrid modes, pagination is limited to the configured
  `[vector.search].max_page_size_hybrid` ranking window when that cap
  is positive. Requests whose `offset` is at or beyond that window
  return `pagination_limit`.
- `find_similar_messages` takes a seed `message_id` and returns
  nearest neighbors (excluding the seed itself). Optional `account`,
  `after`, `before`, `has_attachment` filters.

## Model Rotation

To switch models, dimensions, preprocessing settings, or
`max_input_chars`, update your config, then run:

```bash
msgvault embeddings build --full-rebuild --yes
```

This builds a new generation with the new fingerprint and activates
it atomically when the build completes. The fingerprint includes the
model, dimension, preprocessing policy, `max_input_chars`, and
embedding output policy. While the rebuild is in flight,
`mode=vector` and `mode=hybrid` return `index_stale` (the
previously-active generation no longer matches the configured
fingerprint, so search refuses to serve potentially-mismatched
results). Use `mode=fts` until the new generation activates; it does
not depend on the vector index. Once `msgvault embeddings build`
reports the new generation activated, vector and hybrid modes resume.

## Troubleshooting

Common HTTP/MCP error codes and fixes. The CLI reports equivalent
conditions as command errors rather than structured codes.

| Error | Meaning | Recovery |
|---|---|---|
| `vector_not_enabled` | The server or MCP process did not wire a vector backend, usually because `[vector] enabled = false`. | Set `enabled = true`, configure `[vector.embeddings]`, and start with a build that includes the needed backend (`sqlite_vec` or `pgvector`). |
| `index_stale` | Active generation's fingerprint does not match the current model, dimension, preprocessing policy, `max_input_chars`, or embedding output policy. | Run `msgvault embeddings build --full-rebuild --yes`. |
| `index_building` | No active generation yet; one is being built. | Finish running `msgvault embeddings build` or wait for the scheduler. Use `mode=fts` for the interim. |
| `missing_free_text` | `mode=vector` or `mode=hybrid` used with a filter-only query (no free text to embed). | Add free-text terms to `q`, or switch to `mode=fts`. |
| `pagination_unsupported` | HTTP request asked for `page>1` with `mode=vector|hybrid`. | Use `page=1` with a larger `page_size` instead. |
| `pagination_limit` | MCP `search_messages` asked for an `offset` at or beyond a positive `[vector.search].max_page_size_hybrid` cap in `mode=vector|hybrid`. | Use `mode=fts` for deeper pagination, request an earlier page, or raise/disable the hybrid page-size cap. |
| `invalid_mode` | `mode=` value other than `fts`, `vector`, `hybrid`. | Pick one of those. |
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

The `active_generation.message_count` should roughly match
`total_messages`. `missing_embeddings_total` shows how many live
messages still need embedding under the active (and building, if any)
generation — either because a rebuild is in flight or because recent
syncs have not yet been drained.

## What Gets Embedded

The embedder processes one or more vectors per message. Per-message
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

## See Also

- [Web Server](/api-server/): HTTP API reference (search, stats).
- [Searching](/usage/searching/): Full-text search syntax.
- [Search Ranking Across Backends](/architecture/search-ranking/): Ranking differences between SQLite, PostgreSQL, sqlite-vec, and pgvector.
