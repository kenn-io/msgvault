---
last_edited: "2026-10-08"
title: Vector Search
description: Find messages by meaning and set up separate people and visual search indexes.
---

Semantic search lets you find messages by meaning rather than exact keywords.
For example, a query like "planning offsite agenda" can surface a message
titled "Q2 team kickoff" if the bodies discuss the same topic, even when
none of the query words appear in the result. msgvault builds this
capability on top of keyword search. An embedding is a numeric representation
of text that lets msgvault rank related content. Msgvault
sends your text to an embedding endpoint you configure and stores the
resulting vectors in your archive. SQLite archives store vectors in
`vectors.db`, while PostgreSQL archives store them in pgvector tables inside
the same database as the message archive.

When you enable vector search, the `search` command and HTTP `/api/v1/search`
endpoint accept `mode=vector` for pure semantic matching and `mode=hybrid` to
combine BM25 and vector results with Reciprocal Rank Fusion. In MCP, you can
use `semantic_search_messages` for the same modes, or `find_similar_messages`
to get nearest-neighbor messages for a given seed. Your vectors and archive
stay local, while the endpoint in your config handles the embedding work. If
a third party hosts that endpoint, your message text and semantic query text
go there. When you need the workflow to stay on your own machine or network,
configure a local or self-hosted endpoint instead.

To check message coverage or diagnose slow embedding runs, run
`msgvault embeddings status --watch`. Its recent batch timings separate
provider calls and retry waits from database writes. You can also fetch the
same snapshot from `GET /api/v1/embeddings/status` or through the MCP
`get_embeddings_status` tool. For rate, ETA, and source-filter details, see
[embedding status](../cli-reference.md#embeddings-status).

Pick the index that matches what you want to search:

| Content | Search command | Setup |
|---|---|---|
| Messages, chats, and meeting transcripts | `search --mode vector` or `--mode hybrid` | This page |
| Curated people | `person search` | [People](/docs/usage/people/#find-a-person-by-what-you-remember) |
| Image and video content | `multimodal search` | [Visual attachment search](#visual-attachment-search) |
| Mirrored document text | `media search <query>` | [Docbank configuration](https://msgvault.io/docs/configuration/#search-document-text-through-docbank) |

Message embeddings do not include attachment pixels or extracted document
text. Visual indexing has its own build and consent flow, and Docbank manages
document processing. If you want a guided setup based on the provider keys
you already have, start with
[Recommended Configuration](/docs/usage/recommended-configuration/).

## Turn on search by meaning

To enable semantic search from the Web UI, open **Settings**, go to
**Overview**, and click **Turn on** beside **Search by meaning**. MsgVault
downloads the shared local server if needed, builds the message index, and
turns on semantic search in the running daemon. You can watch download and
indexing progress in that row, or close the page and keep using keyword
search while it runs.

If you turned on built-in search in another app, the shared model files are
installed, and no provider is configured, MsgVault starts indexing a SQLite
archive at launch. It does not download missing files at startup, and a
configured provider takes precedence. PostgreSQL archives require an explicit
setup.

If Ollama already serves `nomic-embed-text`, **Turn on** uses it, and an
existing Ollama configuration stays on Ollama. When you open the Web UI from
a remote address, the download line names the daemon host. You can also click
the **Turn on** hint below full-text search results to open **Settings**.

## Prerequisites

1. **An embedding provider.** Run `msgvault setup providers --local-embeddings`
   to install and start the shared local server after confirming any missing
   downloads. That option works even if you do not have a running endpoint yet.
   You can also point msgvault at a running local, self-hosted, or hosted
   endpoint that you trust. Common local options include
   [Ollama](https://ollama.com),
   [llama.cpp's `server`](https://github.com/ggerganov/llama.cpp/tree/master/examples/server),
   and [LM Studio](https://lmstudio.ai). On an Apple Silicon Mac,
   [`afm`](https://github.com/scouzi1966/maclocal-api) is another option (see
   the tip below). For `api_format = "openai"`, your endpoint must accept
   `POST /embeddings` with an OpenAI-style JSON body and return indexed data
   rows such as `{"data": [{"index": 0, "embedding": [...]}]}`.

2. **A build with a vector backend.** The standard `make build` target already
   passes `-tags "fts5 sqlite_vec"`. If you see an error mentioning
   "binary was built without -tags sqlite_vec", rebuild with `make build` (or
   run `go build -tags "fts5 sqlite_vec"` if you invoke `go build` directly).
   The official Docker image also includes `sqlite_vec`, so SQLite-backed
   container deployments do not need a custom image for vector search.
   PostgreSQL vector search also requires the `pgvector` build tag, for example
   `go build -tags "fts5 sqlite_vec pgvector" ./cmd/msgvault`.

!!! tip "Local embeddings on Apple Silicon"
    [`afm`](https://github.com/scouzi1966/maclocal-api) serves
    OpenAI-compatible embeddings from Apple's on-device NaturalLanguage model
    over Metal, with no Python, no cloud, and no API key. It requires macOS 26
    (Tahoe) or later on an Apple Silicon Mac with Apple Intelligence enabled.

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

On Windows, the sqlite-vec CGo binding needs `sqlite3.h` at compile time, and
the MinGW 15 toolchain needs two extra flags to link arrow-go/v18's helpers.
To set up everything automatically, run `powershell -File scripts/build.ps1`.
If you prefer to invoke `go build` yourself from PowerShell, run:

```powershell
C:\msys64\usr\bin\pacman.exe -S --noconfirm --needed mingw-w64-x86_64-sqlite3
$env:CGO_ENABLED = "1"
$env:CGO_CFLAGS = "-IC:/msys64/mingw64/include -fgnu89-inline"
$env:CGO_LDFLAGS = "-Wl,--allow-multiple-definition"
go build -tags "fts5 sqlite_vec" -o msgvault.exe ./cmd/msgvault
```

## Enable

Run `msgvault setup providers` to pick default embedding settings. When you do
not have a hosted key, setup uses Ollama with `nomic-embed-text` or offers the
shared local server, asking once before downloading any missing model and
runtime files.

If Ollama is running and you want to switch to the shared local server, run
`msgvault setup providers --local-embeddings`. Switching an Ollama-built Nomic
message index that uses the same recipe keeps its generation, and the next
build embeds only new messages. Switching from another model requires
`msgvault embeddings build --full-rebuild --yes`. This CLI switch changes
semantic-person consent identities once, so renew those consents afterward.
Later port changes on the local server preserve the local generation and
consent identities. Configured providers remain tied to their endpoints.

To configure vector search by hand, add a `[vector]` block to
`~/.msgvault/config.toml`.

Guided Ollama setup sets a conservative `max_input_chars = 2000` to leave
extra room for token-dense content. To raise it to `6000` as shown below,
first check representative content against the
[sizing guidance](#matching-max_input_chars-to-your-embedders-context-window),
then update `max_input_chars` under `[vector.embeddings]` in
`~/.msgvault/config.toml`.

This manual example uses an OpenAI-compatible endpoint:

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
max_input_chars = 6000                   # per-chunk cap; see sizing guidance below
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
sqlite_accelerator = "auto"              # use a ready SQLite ANN index
ann_nprobe = 8                            # partitions searched per query
ann_oversample = 8                        # candidates per result before exact reranking
ann_threads = 8                           # native threads used by embeddings optimize

[vector.embed.schedule]
cron = "*/5 * * * *"                     # embed worker cron (5-field); empty disables cron
run_after_sync = true                    # run a pass after every successful scheduled sync

[vector.embed.scope]
# Optional: leave empty for the full archive, or restrict new generations.
# message_types = ["teams"]
```

The `[vector]` section takes effect only when `enabled = true` **and** you
built the binary with the needed vector backend. If either is missing,
msgvault behaves as before. When vector search is disabled, the server returns
`vector_not_enabled`. When you request vector features from a binary built
without the needed backend, it reports a rebuild-with-vector-backend error.

### Contextual chat and meeting embeddings

With `api_format = "voyage-contextual"`, the embedder embeds Beeper messages
alongside nearby messages from the same conversation, which helps retrieve
short replies whose meaning depends on what came before. It splits meetings
along speaker turns while keeping their meeting context. Other message types,
including email and chat imports outside Beeper, stay individual documents
with overlapping chunks when needed.

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
rebuild. Search results still point to the original messages and matching
text, and the worker tracks edits, late-arriving messages, and deletions so
it can refresh affected conversation windows.

### PostgreSQL and pgvector

When `[data].database_url` is a PostgreSQL DSN, msgvault selects the pgvector
backend at runtime. Use `backend = "pgvector"` as the config marker and build
with the `pgvector` tag:

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

pgvector embeddings live inside the PostgreSQL database. `db_path` and
`vectors.db` apply only to the SQLite sqlite-vec backend. For database setup,
see [PostgreSQL Backend](/docs/architecture/postgresql/).

### Model task prefixes

Some embedding models require different task instructions for indexed documents
and search queries. The `nomic-embed-text` examples above use its required
retrieval prefixes. Set `document_prefix` and `query_prefix` to the values
your model requires, or leave them empty if your model does not use
instructions.

msgvault prepends `document_prefix` to every chunk after chunking and
`query_prefix` to every vector-search query. Prefix characters do not count
against the `max_input_chars` content budget. If you change either prefix,
msgvault marks the existing vector generation stale so prefixed queries are
not mixed with an index built from unprefixed documents.

### Optional EmbeddingGemma 2 text endpoint

You can use EmbeddingGemma 2 through the existing OpenAI-compatible text path
with 768-dimensional output and literal retrieval prefixes. Msgvault does not
load or serve the model itself. Before updating your config, set up a trusted
endpoint to accept the fully formatted text below with automatic prompt
insertion disabled.

```toml
[vector]
enabled = true
backend = "sqlite-vec"

[vector.embeddings]
api_format = "openai"
endpoint = "http://127.0.0.1:8080/v1"
# Example serving alias; the operator must bind it to this revision and recipe.
model = "embeddinggemma-2-914f7f89142e33e77833254d9c9b90c3cef7303b-text-fp32-768-v1"
dimension = 768
document_prefix = "title: none | text: "
query_prefix = "task: search result | query: "
batch_size = 4
timeout = "120s"
max_input_chars = 6000
```

Keep the trailing spaces inside both prefix strings. Msgvault adds each prefix
once after preparing the content. `title: none` is a literal fallback and does
not interpolate the message subject into the prompt. A batch size of 4 and a
120-second timeout are starting values for local CPU serving rather than a
tested performance guarantee, and they do not change the defaults.

The endpoint operator must bind that example alias to
[`google/embeddinggemma-2` revision
`914f7f89142e33e77833254d9c9b90c3cef7303b`](https://huggingface.co/google/embeddinggemma-2/tree/914f7f89142e33e77833254d9c9b90c3cef7303b)
along with a fixed tokenizer, pooling, precision, and output recipe. An alias
is an operator convention, and msgvault does not verify the loaded checkpoint
over HTTP. Give any changed serving recipe a new alias. Changing `model`,
`dimension`, or either prefix separates message generations and requires a
full rebuild. Docbank owns document processing and embeddings; see
[Docbank configuration](https://msgvault.io/docs/configuration/#search-document-text-through-docbank).

The [model card](https://ai.google.dev/gemma/docs/embeddinggemma/model_card_2)
specifies mean pooling including prompts, L2-normalized output, and
`bfloat16` or `float32` activations rather than `float16`. Make sure your
server enforces the shared **8192-token input limit including formatting**.
Treat `max_input_chars = 6000` as a conservative starting point rather than a
token count or admission check, and verify your server's tokenizer and
oversized-input policy before embedding an archive.

Msgvault rejects wrong-width, nonfinite, and zero-norm vectors. It preserves
valid provider values, so the server must return unit-length vectors for this
recipe. `dimension` checks response width; msgvault neither sends an OpenAI
`dimensions` request nor slices vectors. If you want reduced
512/256/128-dimensional output, configure a serving recipe that truncates and
L2-normalizes again under a separate alias with matching query and document
width. The example above covers native 768 only.

Msgvault has not been tested against a live EmbeddingGemma 2 server. This
setup covers text only and adds no image, video, or audio transport.

### Matching `max_input_chars` to your embedder's context window

`max_input_chars` sets an upper limit in characters for each embedding chunk,
and your embedder converts that text to tokens on its own. Keep it below the
embedder's token limit after tokenization, leaving room for any
`document_prefix`. Oversized chunks can be rejected or silently truncated
during `msgvault embeddings build`.

After preprocessing, long messages are split into overlapping chunks instead
of being cut off into a single input. Chunk boundaries prefer paragraph
breaks, then sentence breaks, then word boundaries, falling back to a hard
rune boundary only when needed.

Size `max_input_chars` to match your model's context window instead of setting
it defensively low. It controls both the correctness ceiling and how many chunks
each message produces, and every chunk is a separate embedding input. Halving
`max_input_chars` roughly doubles the chunk count for long messages, which
adds per-input overhead and can slow `embeddings build` depending on your
model, batching, and message lengths. Confirm your server's real context
window before starting a full rebuild.

Practical guidance:

- **Characters are not tokens.** The values below assume 3 characters per
  token. That ratio varies by tokenizer and content, and code, markup, or
  non-Latin scripts may need a lower cap. Treat these values as starting
  points and test representative content.
- **2k-token embedding models.** Start around `max_input_chars = 6000`.
- **8k-token embedding models.** Start around `max_input_chars = 24000`.
- **Self-hosted models.** Match the actual context window exposed by your
  server, not only the upstream model card.

To test a candidate cap, take `max_input_chars` characters of representative
preprocessed content, prepend your configured `document_prefix`, and send the
combined input to your embedder. The prefix does not consume the chunking
budget. Ollama's [native API](https://docs.ollama.com/api/embed) reports the
token count. Set `truncate: false` so an oversized input returns an error
instead of a count for truncated text:

```bash
curl -sS http://127.0.0.1:11434/api/embed \
  -d '{"model":"nomic-embed-text","input":"search_document: <chunk text>","truncate":false}' \
  | jq '.error // .prompt_eval_count'
```

If this returns a context-length error, lower the cap and repeat with
representative content before starting a full rebuild.

!!! warning "Ollama truncates instead of rejecting"
    Through its OpenAI-compatible `/v1/embeddings` endpoint, which msgvault
    uses, Ollama truncates over-limit inputs by default. The ceiling is the
    smaller of the model's trained context length and `num_ctx`, with room
    needed for special tokens. Ollama embeds only the beginning of an
    oversized chunk and returns a normal response, which can silently degrade
    recall. The trained length is the hard ceiling: Ollama's
    `nomic-embed-text` ships with `num_ctx = 8192` but a trained context of
    2048 tokens, so raising `num_ctx` does not help. Check both values with
    `ollama show <model>` before raising `max_input_chars`.

If `msgvault embeddings build` reports `HTTP 400`, check your embedder's own
logs for the reason. The OpenAI-compatible client reports the HTTP status
without echoing the provider's response body. A server log such as
`the input length exceeds the context length` confirms you need to lower
`max_input_chars`. Do not rely on this error to catch oversized inputs through
Ollama's OpenAI-compatible endpoint; see the warning above.

## Initial Embedding

Once vector search is enabled and your archive has synced or imported
messages, build the initial index:

```bash
msgvault embeddings build --full-rebuild --yes
```

For message embeddings, enabling `[vector]` is your opt-in. The command
does not create a separate stored provider-consent record, and `--yes` only
skips the full rebuild confirmation. If your endpoint is hosted by a third
party, both indexed message text and semantic query text leave the archive as
described above. People and visual vectors use separate consent gates, and
Docbank manages document processing consent.

This command creates a new **building generation**, scans every non-deleted
message in your configured scope, embeds missing rows in batches through your
configured embedder, and atomically activates the generation once missing
coverage reaches zero. During the first build, before any active generation
exists, HTTP and MCP vector and hybrid search return `index_building`. For
keyword search in the meantime, use `mode=fts` on the CLI or HTTP API, or in
MCP omit `mode` for metadata search or call `search_message_bodies` for body
keywords.

!!! tip
    You can interrupt and resume a build. Each run of
    `msgvault embeddings build` scans for messages still missing coverage and
    activates the generation once coverage reaches zero. Pressing `Ctrl+C` is
    safe; run `msgvault embeddings build` again to pick up where you left off.

The initial embed is the largest and longest run, with runtime roughly
proportional to archive size divided by embedding throughput. Progress output
reports completed and total messages, a recent-window throughput rate,
milliseconds per message, microseconds per character, and an ETA once enough
samples are available. If a failing batch downshifts to smaller requests, the
ETA window keeps those single-message retries from dominating the displayed
rate.

## Optimize SQLite Search

For large SQLite archives, build the local search accelerator once your
embedding generation is active:

```bash
msgvault embeddings optimize
```

The optimizer reads stored vectors only and does not call your embedding
provider. It commits copy progress in batches, runs native training in a
disposable child process, and publishes the accelerator only after integrity
checks pass. Between committed copy batches, the daemon lets imports and other
archive writes run before resuming preparation. If stored vectors changed
during that pause, it restarts the copy against the new revision. Native
training and publication remain exclusive and pause other archive writes,
while searches stay available.

Configure SQL tracing on the daemon instead of passing `--log-sql` or
`--log-sql-slow-ms` to `optimize`. If the command is interrupted, run it
again. Search stays exact until the accelerator is ready, at which point
`sqlite_accelerator = "auto"` switches semantic and hybrid retrieval to
bounded ANN candidates with exact reranking. New embedding writes update a
ready accelerator in the same transaction.

Run `msgvault embeddings list` to inspect the accelerator state (`exact`,
`building`, `ready`, or `stale`), indexed row count, timestamps, and the last
bounded error. If you need an immediate rollback to exhaustive search without
deleting the accelerator, set `sqlite_accelerator = "exact"`. You can check
the active retrieval path with `search --explain`, and JSON responses include
it as `accelerator`. When an accelerator error occurs, search logs a warning
and retries with exact vectors.

To remove an old or unwanted accelerator, including one for a retired
generation, run `msgvault embeddings optimize --drop <generation-id>`. Exact
vectors remain available. SQLite can reuse the freed pages, though dropping
the accelerator does not shrink the database file on disk.

Run `msgvault embeddings optimize` again after substantial archive growth.
Because the trained bucket count stays fixed until the next optimization, a
much larger archive can reduce retrieval speed and recall.

## Keeping the Index Up to Date

After the initial rebuild, new messages arriving from email sync also need to
be embedded. msgvault handles this in two ways depending on how you run it.

### CLI workflow (manual syncs)

When you run `msgvault sync-full` or `msgvault sync` (alias:
`sync-incremental`) by hand, new Gmail and IMAP messages are saved with
`embed_gen = NULL`. In steady state, `msgvault embeddings build` scans those
rows and tops up the active generation. During a rebuild, the worker targets
the building generation first so it can activate. The old active generation
keeps serving vector and hybrid search, but it is frozen and does not receive
top-ups until the new build activates. Run `msgvault embeddings build` (without
`--full-rebuild`) to continue the scan:

```bash
# Sync new messages (marks them as needing embedding)
msgvault sync you@gmail.com

# Scan and embed missing rows for the active or building generation
msgvault embeddings build
```

Without `--full-rebuild`, `msgvault embeddings build` is a short incremental
pass. It resumes a matching building generation if one exists, or tops up the
configured active generation, and then exits. `msgvault embeddings resume` is
a synonym for this drain that never starts a full rebuild. You can schedule
either command with cron, run it after every sync, or chain them together
(`msgvault sync && msgvault embeddings build`).

### Daemon workflow (`msgvault serve`)

In daemon mode, the scheduler can run syncing and embedding automatically. The
`[vector.embed.schedule]` section controls the embed worker independently from
the sync scheduler:

```toml
[vector.embed.schedule]
cron = "*/5 * * * *"      # run every 5 minutes
run_after_sync = true     # and opportunistically after every scheduled sync
```

With `run_after_sync = true`, every successful scheduled sync triggers an
immediate embed pass over messages still missing coverage. The standalone cron
keeps embedding caught up even when syncs are quiet (for example, overnight).
If you only want the post-sync trigger, set `cron = ""` to disable the
standalone schedule.

### What Triggers Embedding

| Ingest path | Runs the embed worker? |
|---|---|
| Manual `sync-full` / `sync` (Gmail, IMAP) | No. Run `msgvault embeddings build` afterward |
| Manual `sync-calendar` / `sync-teams` / `sync-discord` | No. Run `msgvault embeddings build` afterward |
| Manual `sync-slack` / `sync-beeper` / `sync-omi` / `sync-granola` / `sync-plaud` / `sync-circleback` / `sync-pocket` / `sync-notion-meetings` / `sync-muesli` / `sync-twilio` / `sync-bland` / `sync-twenty` / `sync-chatwoot` | No. Run `msgvault embeddings build` afterward |
| Scheduled account syncs in `msgvault serve` (Gmail, IMAP, Microsoft 365 Graph mail, Teams, Discord) | Yes, when `[vector.embed.schedule].run_after_sync = true` |
| Scheduled calendar, Slack, Beeper, Omi, Granola, Plaud, Circleback, Pocket, Notion, Muesli, Twilio, Bland, Twenty, and Chatwoot syncs in `msgvault serve` | No immediate post-sync run. Picked up by the embed worker's `[vector.embed.schedule].cron` schedule |
| Local email imports (`import-pst`, `import-emlx`, `import-eml`, `import-mbox`) | No. Run `msgvault embeddings build` afterward |
| Local chat/text imports | No. Run `msgvault embeddings build` afterward |

For ingest paths that do not immediately schedule embedding work, run
`msgvault embeddings build` to fill missing coverage in the current index. To
recover older coverage gaps that a normal incremental pass skips, run
`msgvault embeddings resume --backstop`. You need a full rebuild when the
model or index policy changes, or when you want to replace the whole
generation.

From a searcher's perspective, a same-model full rebuild is atomic. Vector and
hybrid queries keep answering from the previous active generation until the
new one is ready. That previous active generation is intentionally frozen
during the rebuild, so messages synced after the rebuild starts may not appear
in vector or hybrid results until the building generation activates. If the
rebuild changes the configured model or dimension, vector and hybrid queries
return `index_stale` until the new generation activates.

### CAS resolution (accepted single-user residual)

If a message's text changes while an embed run is in flight (for example, when
`msgvault repair-encoding` rewrites a body), the embed worker uses an
optimistic compare-and-set on the message's `last_modified` timestamp to avoid
stamping an embedding built from stale text. If `last_modified` moved between
the worker reading the content and writing the coverage stamp, the worker
skips the stamp and re-embeds the message on a later run.

`last_modified` has **1-second resolution** (it is a `CURRENT_TIMESTAMP`
default or trigger). A concurrent edit that lands in the *same whole second*
as the worker's content read leaves `last_modified` unchanged, so the CAS can
mark an embedding current even though it was built from the now-stale text.
This sub-second window is an accepted residual for this single-user tool. An
edit and an embed of the *same* message within the *same second* is rare. It
self-recovers: the next edit to that message bumps `last_modified` (and
`repair-encoding` clears its coverage stamp outright), and either a full
rebuild (`embeddings build --full-rebuild`) or the periodic full-scan backstop
re-embeds it regardless.

## Upgrading an existing archive

Archives that already had embeddings before generation-based coverage tracking
landed are migrated in place on the first writable open, with no expected data
loss and with existing active vectors preserved. Those vectors keep serving
vector and hybrid search only if their fingerprint matches the current
embedding policy and configuration. For example, generations built by v0.14
use an older fingerprint and require a full rebuild even with unchanged
configuration; see
[When a full rebuild is required](#when-a-full-rebuild-is-required).

The migration runs the first time you start a writable daemon or run an
embeddings command against an older database. During migration, msgvault:

- Adds and stamps `messages.embed_gen`, then backfills coverage from the
  active vector generation so messages that already have active embeddings are
  marked covered. This backfill does not re-queue the active generation's
  corpus.
- Leaves messages that had a pending re-embed against the active generation
  uncovered rather than marking them covered, so they stay missing and the
  scan-based worker can find and re-embed them, including below-watermark rows
  on a backstop pass. The migration consults the legacy `pending_embeddings`
  table for this, then drops it once migration completes.

Only the active generation is backfilled. A rebuild that was already in flight
at upgrade time receives no coverage stamps for its existing vectors and
re-embeds those messages when you resume it.

After the upgrade, coverage is tracked entirely through `messages.embed_gen`.
A new or changed message becomes "missing" when its `embed_gen` is cleared
rather than by being queued in a separate table, and the scan-and-fill worker
finds those rows to top up the active generation.

For a generation whose fingerprint still matches, `msgvault serve` with an
embed schedule runs a backstop automatically on its first embed pass for each
generation, and then on the first pass after each backstop interval (24 hours
by default, unless disabled). To finish coverage manually for stragglers left
after the migration, run:

```bash
msgvault embeddings resume --backstop
```

`--backstop` runs a full-scan pass that ignores the per-generation watermark,
so it catches below-watermark rows that a normal incremental resume skips. It
tops up an already-active generation, or activates a building generation in
flight once missing coverage reaches zero.

### When a full rebuild is required

If the active generation's fingerprint no longer matches the current embedding
policy or configuration, including after a policy change shipped in an
upgrade, vector and hybrid search report `index_stale`. Run
`msgvault embeddings build --full-rebuild --yes` to build a matching
generation. If the daemon started before you changed the settings, run
`msgvault daemon restart` after the rebuild. See
[Model Rotation](#model-rotation) for fingerprint inputs and search behavior
while the rebuild runs.

## Scoped Generations

For large mixed archives, you can build a vector index for only selected
message types:

```toml
[vector.embed.scope]
message_types = ["teams"]
```

The scope is part of the generation fingerprint. Changing it requires
`msgvault embeddings build --full-rebuild --yes`, the same as changing the
model or preprocessing policy. Scoped generations are useful when you want
semantic search for a newer corpus such as Teams or SMS without embedding
decades of email immediately.

You can also scope generations to selected accounts, either durably in your
config (so the daemon's scheduled embeds obey it too):

```toml
[vector.embed.scope]
accounts = ["you@gmail.com"]
```

or for a single run from the CLI (overriding the configured accounts for that
run):

```bash
msgvault embeddings build --full-rebuild --account you@gmail.com
msgvault embeddings build --account you@gmail.com --account you@work.com
msgvault embeddings build --collection family   # every account in the collection
```

!!! warning
    `--account` and `--collection` are one-run overrides. After they activate
    a scoped generation, add the equivalent stable account identifiers under
    `[vector.embed.scope].accounts` and restart the daemon before searching.
    Otherwise, the daemon expects a different generation fingerprint and
    returns `index_stale`. Use the config form whenever you want the scoped
    index to persist.

The `--account` flag accepts an identifier or display name as it does
elsewhere, but the durable `[vector.embed.scope] accounts` list requires
canonical account identifiers. Display names are rejected because they are not
stable identities for a privacy boundary. Unknown identifiers fail the run
instead of silently embedding more than requested. Account scoping acts as a
privacy boundary where message text from accounts *outside* the scope is never
sent to the embedding endpoint. It is also the cheapest way to pilot semantic
search on one mailbox before paying to embed the whole archive.

Keep these account-scope caveats in mind:

- The fingerprint records the scope as source IDs, which are archive-local.
  Removing and re-adding an account (or restoring a backup) can renumber its
  source ID, producing a new fingerprint and requiring a full rebuild.
- A scoped account resolves to *all* of its sources, including linked ones
  such as a Google Calendar synced for the same account. Adding such a source
  after a scoped generation activates changes the resolved source set and
  therefore the fingerprint, so vector search reports `index_stale` until a
  full rebuild.
- Unlike a message-type scope, an account scope does NOT gate search:
  unfiltered vector and hybrid queries keep working, and accounts outside the
  scope have no vector matches (hybrid search ranks them on the BM25 signal
  alone). The web UI's semantic-coverage readout counts only in-scope accounts
  as eligible.

A scoped index is intentionally partial, so vector and hybrid search require
an explicit compatible message-type filter:

```bash
msgvault search "release planning" --mode hybrid --message-type teams
msgvault search "message_type:teams release planning" --mode vector
```

If the active generation is scoped to `teams`, an unscoped vector or hybrid
query, or a query scoped to `email`, returns `index_scope_mismatch`. For
unscoped keyword search, use `mode=fts` on the CLI or HTTP API; in MCP, omit
`mode` for metadata or use `search_message_bodies` for body keywords. You can
also add the matching message-type filter, or rebuild an unscoped generation
by clearing `[vector.embed.scope].message_types` and running a full rebuild.

## Search

**CLI:**

```bash
msgvault search "planning offsite agenda" --mode hybrid
msgvault search "planning offsite agenda" --mode vector --explain
msgvault search "..." --json --mode hybrid    # JSON output with scores
```

CLI vector and hybrid modes talk to your configured remote server when
`[remote].url` is set, and use the local daemon otherwise. Whichever server
you target needs vector search configured.

**HTTP:**

```bash
curl "http://localhost:8080/api/v1/search?q=planning+offsite&mode=hybrid"
curl "http://localhost:8080/api/v1/search?q=planning+offsite&mode=vector&explain=1"
```

The response shape differs from the FTS path; see the
[Web UI & API Server](/docs/api-server/#get-apiv1search) reference for
details. HTTP vector and hybrid responses return only the first page. Raise
`page_size` (up to `max_page_size_hybrid`) if you want a larger candidate
page.

Both `mode=vector` and `mode=hybrid` require at least one free-text term. That
free text is what gets embedded as the query vector. Queries made entirely of
operators (for example, `from:alice label:IMPORTANT`) are rejected, and HTTP
and MCP return `missing_free_text`. For filter-only CLI or HTTP queries, use
`mode=fts`. In MCP, omit `mode` for metadata-only filtering.

**MCP tools:**

- `search_metadata` searches subject, sender and recipient, label, date, and
  other metadata fields.
- `semantic_search_messages` accepts explicit `vector` or `hybrid` modes along
  with `explain` and `min_score` arguments. It paginates with `offset` and
  `limit`. For vector and hybrid modes, pagination stays within the configured
  `[vector.search].max_page_size_hybrid` ranking window when that cap is
  positive, and requests whose `offset` is at or beyond that window return
  `pagination_limit`.
- `search_message_bodies` runs body-only full-text search and returns bounded
  context around each keyword match. It requires a free-text term and does not
  depend on the vector index.
- `search_in_message` performs literal keyword matching inside a single
  message and does not expose a vector mode through `msgvault mcp`.
- `find_similar_messages` takes a seed `message_id` and returns nearest
  neighbors, excluding the seed itself. It accepts optional `account`,
  `message_type`, `after`, `before`, and `has_attachment` filters.
- `search_messages` is a deprecated compatibility wrapper that routes to
  `search_metadata` when `mode` is omitted and to `semantic_search_messages`
  for vector and hybrid modes.

## Model Rotation

When you want to change models, dimensions, task prefixes, preprocessing
settings, or `max_input_chars`, update your config and run:

```bash
msgvault embeddings build --full-rebuild --yes
```

This builds a new generation with the updated fingerprint and activates it
atomically when the build finishes. The fingerprint covers the API format,
model, dimension, task prefixes, preprocessing policy, `max_input_chars`,
embedding output policy, and [scope](#scoped-generations). While the rebuild
is in flight, `mode=vector` and `mode=hybrid` return `index_stale` because the
previously active generation no longer matches your configured fingerprint, so
search refuses to serve mismatched results. Until the new generation
activates, use `mode=fts` on the CLI or HTTP API (it does not depend on the
vector index), or in MCP omit `mode` for metadata search or call
`search_message_bodies`. Once `msgvault embeddings build` reports that the new
generation is active, run `msgvault daemon restart` if the daemon started
before you changed the settings. Vector and hybrid modes then resume.

## Troubleshooting

The table below lists common HTTP and MCP error codes and how to recover from
them. On the CLI, equivalent conditions appear as command errors rather than
structured codes.

In the table, a non-vector fallback means `mode=fts` for the CLI or HTTP,
omitted `mode` for MCP metadata search, or `search_message_bodies` for MCP
body keywords.

| Error | Meaning | Recovery |
|---|---|---|
| `vector_not_enabled` | The server or MCP process did not wire a vector backend, usually because `[vector] enabled = false`. | Set `enabled = true`, configure `[vector.embeddings]`, and start with a build that includes the needed backend (`sqlite_vec` or `pgvector`). |
| `index_stale` | Fingerprint mismatch: model, dimension, task prefixes, preprocessing policy, `max_input_chars`, output policy, or scope. | If a rebuild activated settings changed since startup, run `msgvault daemon restart`. For a CLI account-scoped index, set matching `[vector.embed.scope].accounts` and restart. Otherwise run `msgvault embeddings build --full-rebuild --yes`. If the daemon can't finish checking restart versus rebuild, it says to retry in a moment and checks again a few seconds later. |
| `index_building` | No active generation yet; one is being built. | Finish running `msgvault embeddings build`, wait for the scheduler, or use the appropriate non-vector fallback. |
| `missing_free_text` | `mode=vector` or `mode=hybrid` used with a filter-only query (no free text to embed). | Add free-text terms to `q`, or use the appropriate non-vector fallback. |
| `index_scope_mismatch` | The active vector generation was built for selected message types and the query is unscoped or asks for a type outside that scope. | Add a compatible `message_type` filter, use the appropriate non-vector fallback, or rebuild an unscoped generation. |
| `pagination_unsupported` | HTTP request asked for `page>1` with `mode=vector|hybrid`. | Use `page=1` with a larger `page_size` instead. |
| `pagination_limit` | MCP `semantic_search_messages` asked for an `offset` at or beyond a positive `[vector.search].max_page_size_hybrid` cap. | Use `search_metadata` for metadata search, use `search_message_bodies` for body keywords, request an earlier vector page, or raise/disable the hybrid page-size cap. |
| `invalid_mode` | The requested mode is not supported by that surface. | Use `fts`, `vector`, or `hybrid` on the CLI or HTTP; use `vector`, `hybrid`, or omitted `mode` in MCP. |
| `embedding_timeout` | The embedding endpoint did not respond before the request deadline (transient: slow/cold model, network blip). | Retry; if persistent, raise `[vector.embeddings].timeout` or use a faster endpoint. |

The OpenAI-compatible client retries HTTP 408 and 429, while its other HTTP
4xx errors are permanent. The Voyage contextual client retries HTTP 429, and
its other HTTP 4xx errors are permanent. The OpenAI-compatible client reports the HTTP status and a classified reason.
Check your embedder's own logs for the response body. The Voyage contextual
client includes up to the first few KiB of the response body directly in the
error. If a batch mixes good and bad
rows, the worker downshifts to smaller batches and then single-message
requests so valid messages still get embedded while the failing row is dropped
or reported. If your server log or error body says
`the input length exceeds the context length` (Ollama) or an equivalent
token-limit error, lower `max_input_chars` to match your model's context
window as described in the sizing guidance above.

To check whether your binary was built with vector support, run:

```bash
msgvault search "probe" --mode vector
```

A rebuild-with-vector-backend error tells you the build tag is missing. A
different error (`vector_not_enabled`, `index_stale`, etc.) means the command
got past the build-tag check and is waiting on configuration or backfill.

To check index health from the stats endpoint, run:

```bash
curl -H "X-API-Key: ..." http://localhost:8080/api/v1/stats | jq .vector_search
```

Compare coverage against eligible active messages in your configured scope;
source-deleted and out-of-scope messages are excluded. During a rebuild,
`active_generation.message_count` reports the frozen serving index and
`building_generation.progress` reports the replacement index.
`missing_embeddings_total` shows how many live messages still need embedding
for the generation the worker targets next: the building generation during a
rebuild, or the active generation otherwise.

## What Gets Embedded

The OpenAI-compatible embedder creates one or more vectors per message,
building the input from `subject` and `body_text` (or falling back to
`body_html` converted to text for HTML-only messages). After preprocessing,
long messages are split into overlapping chunks, and each chunk becomes one
vector tied back to the same message.

Preprocessing includes these steps:

- Optionally strip quoted-reply blocks (`> ...` lines and common
  reply-preamble markers).
- Optionally strip trailing signatures (lines after `-- `).
- Optionally remove base64 and data blobs before HTML cleanup.
- Optionally remove HTML markup and script or style blocks.
- Optionally remove common URL tracking parameters such as `utm_*`,
  `fbclid`, and `gclid`.
- Optionally clean up whitespace.
- Always split long messages into overlapping chunks at `max_input_chars`.

Messages deleted at the source (`deleted_from_source_at IS NOT NULL`) are
skipped entirely. If a message is empty or contains only whitespace and
invisible characters after preprocessing, it is marked complete and never sent
to the embedding endpoint. Chunks containing only those characters are
dropped.

The [contextual API format](#contextual-chat-and-meeting-embeddings) uses
conversation windows for Beeper and meeting-aware chunks for transcripts,
applying your configured account and message-type scope before assembling
content. Curated people are an optional, separately consented input to the
same generation; see
[semantic person search](/docs/usage/people/#find-a-person-by-what-you-remember).

## Visual attachment search

Visual search finds archived images and videos from a text description or a similar image, returning the attachment and its containing message. It uses a separate index, so enabling message search does not enable visual uploads.

Choose Voyage, Vertex, or a service that implements Kit's embedding API in `[vector.multimodal]`. Kit and Vertex accept JPEG, PNG, WebP, and MP4 up to 120 seconds, with a 20 MiB file limit. Vertex does not extract audio. Voyage uses a capability manifest to determine which formats it can send, and supports animated GIFs as a separate opt-in. Voyage sends bounded containing-message text with eligible attachments; Kit and Vertex send the media alone. All providers receive the text or image used as a query.

### Enable and build

For Kit or Vertex, configure the provider, endpoint, and model using the [configuration reference](/docs/configuration/#vectormultimodal). Vertex uses Google Application Default Credentials. Kit services can use an API key or accept anonymous requests. Then run:

```bash
msgvault setup providers --yes
msgvault daemon restart
msgvault multimodal build --yes
msgvault multimodal status --json
```

For Voyage, set `VOYAGE_API_KEY` and prepare a private directory containing synthetic files named `image_webp.webp`, `image_webp_alt.webp`, `video_mp4.mp4`, and `video_mp4_alt.mp4`, with visibly different content in each alternate file. The probe generates the remaining synthetic fixtures.

```bash
msgvault multimodal probe \
  --seeds /private/msgvault/visual-seeds \
  --out ~/.msgvault/voyage-capabilities.json --yes
msgvault setup providers
msgvault daemon restart
msgvault multimodal build --yes
msgvault multimodal status --json
```

Review the Voyage manifest before enabling visual search. The manifest must be accessible on the daemon host. To configure the lane manually, set `enabled = true` and `capabilities_file` under `[vector.multimodal]`.

Running `multimodal build --yes` records consent for the current generation and builds the index. If interrupted, run `multimodal resume` to continue. Changes to the generation or Voyage capability evidence require consent again. `multimodal status` reports coverage, stale items, retryable work, and pending changes.

### Search and maintain

```bash
msgvault multimodal search "a whiteboard with a project timeline"
msgvault multimodal search --image ./reference.png
msgvault multimodal search "a receipt" --person 123 --direction from_person
msgvault multimodal search "a diagram" --source 4 --after 2026-01-01 --json
```

Pass either a single text query or `--image`, not both. Query images must be
JPEG, PNG, or WebP, at most 20 MiB, and, for Voyage, allowed by your capability manifest. For paging, use `--limit` (1–100, default 20) together with the
returned `--cursor`. You can also filter with `--message`, `--filename`,
`--mime-prefix`, and `--before`.

`--person` and `--participant` are mutually exclusive. `--direction` requires
one of them and accepts `from_person`, `to_person`, or `group`, while the
narrower `--sender-person` filter cannot be combined with those three flags.
Person filters rely on archive identity bindings, not a similarity guess about
who appears in an image.

To retry a single failed attachment, run
`multimodal retry --message <id> --hash <sha256>`, and run `multimodal resume`
to continue processing. To retire a generation and delete its vectors, run
`multimodal retire <generation-id> --yes`; your original attachments stay in
the archive.

In MCP, use `search_visual_attachments` for visual queries, `search_media` for
document text, and `search_person_files` for attachment metadata. On the CLI,
`person files --lane all` combines metadata and visual results for one person.
In the Web UI, [Past runs](/docs/web-ui/#background-jobs-and-past-runs) shows
visual coverage along with the build or resume actions the daemon currently
allows.

## See Also

- [Web UI & API Server](/docs/api-server/): Browser interface and HTTP API reference.
- [Searching](/docs/usage/searching/): Full-text search syntax.
- [Search Ranking Across Backends](/docs/architecture/search-ranking/): Ranking differences between SQLite, PostgreSQL, sqlite-vec, and pgvector.
