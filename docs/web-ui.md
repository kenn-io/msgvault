---
title: Web UI
description: Use and securely deploy msgvault's daemon-served analytical interface.
---

# Web UI

Msgvault's first-party web UI is embedded in every release binary and served by
`msgvault serve`. It needs no Node/Bun process, hosted service, or external asset
directory at runtime. The transactional archive remains authoritative while the
Parquet/DuckDB analytical cache supplies the interactive tables.

## Start and discover the URL

```bash
msgvault build-cache
msgvault serve
```

Foreground startup prints `API server: http://HOST:PORT`. The default
`server.api_port = 0` chooses a free port. Local CLI commands discover that
address through the private daemon runtime record under the configured msgvault
home; browsers should use the printed URL. Configure a fixed port for a stable
remote bookmark:

```toml
[server]
bind_addr = "127.0.0.1"
api_port = 8080
```

The default loopback deployment is trusted. If an API key is active and the
request is not loopback-trusted, `/` still loads the public shell and the UI asks
for the key. A successful login creates an expiring, in-memory browser session.
Daemon restarts, logout, expiry, and API-key activation invalidate sessions.
Existing bearer-key API clients are unchanged.

## Remote access and HTTPS

For remote access, set a strong API key and bind deliberately:

```toml
[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = "replace-with-a-long-random-key"
```

HTTPS at a reverse proxy is recommended. Forwarded scheme and host headers are
accepted only from explicitly trusted proxy addresses or CIDRs:

```toml
[server]
trusted_proxies = ["127.0.0.1", "192.0.2.8/32"]
```

Do not add a whole client network merely to silence proxy warnings. When HTTPS
is known through a trusted proxy, the browser cookie uses `Secure`. Plain HTTP
on an encrypted private network is supported as an explicit tradeoff, but the UI
warns that its session cookie travels without TLS. `HttpOnly` and
`SameSite=Strict` do not encrypt that traffic.

## Explore and search

Everything opens as a compact, sortable table of logical entries: one row per
email, calendar event, meeting note, other durable item, or chat conversation.
Raw chat fragments appear only after drilling into a conversation. Filter,
Group by, Show as, and Search compose into one URL-backed context, so browser
Back and Forward restore the analytical slice and focused item.

Search mode is always explicit:

- **Full text** searches the complete lexical index.
- **Semantic** ranks only content covered by the current embedding generation.
- **Hybrid** combines complete lexical matching with semantic ranking where it
  is available.

The context strip reports semantic coverage. Disabled, building, stale,
incomplete, unavailable, and ready are different states; msgvault never silently
changes the requested mode. Semantic-only results cannot include unembedded
content. Hybrid retains full-text coverage and labels the semantic contribution.

Search execution also has explicit terminal states. **Timed out** means the
selected search backend did not finish within the request budget; it is an
error, not an empty result, and the query and filters remain available to retry.
**Incompatible mode** means the daemon, browser contract, or current index
cannot safely honor the selected search mode. Update or rebuild the named
component, or deliberately select a supported mode. Msgvault does not quietly
substitute full-text search for either state.

## Cache states

The web tables use one modality-neutral analytical cache. When it is missing,
building, stale, or unavailable, the UI names that state instead of quietly
switching selected modalities to a different read path. Run `msgvault
build-cache` for an explicit rebuild, or leave `analytics.auto_build_cache =
true` for daemon startup to build a stale cache. With `analytics.engine =
"duckdb"`, startup fails if no usable cache can be produced.

## Files and containing context

Files is a searchable table of attachment date, filename, type, size, person or
domain, source, containing item, and content availability. Archived images and
PDFs open in application-controlled viewers. Metadata-only, missing,
unsupported, and previewable content remain distinct. From a file, navigate to
its containing item and then its email or chat conversation.

## People and domains

People combines identifiers backed by explicit archive identity evidence; it
does not merge records merely because their display names match. Select a
person to inspect contextual activity across email, chat, calendar events, and
meeting notes, plus the files associated with that person. The active search
and filters continue to scope both the timeline and file table.

Domains provides the same activity-and-files analysis for an exact domain
fact. A domain is not treated as an inferred organization identity. Selecting
a grouped person or domain in Everything opens its inspector in the current
context, including chronologically ordered related files.

## Saved Views

Saved Views persist useful analytical contexts in the daemon, so the same
library is available from every authenticated browser connected to this
single-user archive. A view records its query, explicit search mode, filters,
grouping, presentation, sort, visible columns, and inspector preference.
Selection is intentionally not saved.

Each record carries a schema version. An incompatible record remains visible,
but cannot be opened or edited: automatic migration is not attempted. Remove it
after confirmation and save the current context again. Updates and deletion use
the record revision as an optimistic-concurrency guard. If another browser
changes the view first, msgvault reports a conflict and requires you to reload
and review the latest revision instead of overwriting it.

## Sources and sync status

Sources is a status workspace. For each source it shows schedule information,
an active run's processed, added, and error counts, the latest terminal result,
and the last successful sync separately. A failed status request remains an
error rather than becoming an empty source list. Failed runs expose their
run-level and item-level errors, and a terminal result older than 24 hours is
marked `stale_last_result`.

`Sync now` is available only when that source reports the capability. A `202
Accepted` response means the daemon accepted the request, not that work has
finished. While the page is visible, the UI polls source status with bounded
backoff to show the run and live progress; it opens no streaming connection. If
the accepted run never appears, the UI reports `sync_start_not_observed` rather
than claiming success. Conflicting runs and unavailable capabilities retain
their explicit errors or reasons. Full resync, pause/resume, schedule editing,
and source add/remove are outside this workspace's initial scope.

## Deletions

Everything supports explicit row selection and select-all-matching for the
current canonical filter. `d` and `D` open the Deletions workspace, where the
daemon first preflights the selection and reports any unavailable action before
the UI offers a separate staging confirmation. The workspace lists, inspects,
and cancels manifests; it cannot execute deletion against a provider. Use the
explicit `msgvault delete-staged` CLI workflow for that final operation.

## Keyboard controls

Tab keeps its normal browser meaning. Outside inputs and content viewers:

| Key | Action |
|---|---|
| `j` / `k`, arrows | Move row focus |
| `Home` / `End`, `PgUp` / `PgDn` | Navigate large tables |
| `Enter` | Open or drill into the focused row |
| `Esc` | Close the current shell layer or restore prior context |
| `/` | Focus search |
| `Space` | Toggle the focused row |
| `A` / `x` | Select visible rows / clear selection |
| `d` / `D` | Review deletion staging |
| `f`, `g`, `s`, `r` | Filter, group, sort, reverse sort |
| `?` | Searchable shortcut help |
| `Cmd/Ctrl+K` | Command palette |

Destructive keys open a review; they never execute deletion immediately.
Shortcuts are suspended while typing and inside message/file content.

## Settings and restart behavior

Settings exposes the supported browser, server, search, source, and optional
integration keys. It performs targeted, comment-preserving edits to
`config.toml`, rejects a stale browser edit after a concurrent hand edit, and
never displays secret values—only whether they are configured.

Most settings are restart-required by design. After saving, the UI shows a
pending-restart state until the daemon restarts. An API-key change requires an
extra confirmation. The current process keeps the active key until restart;
after restart, old browser sessions are gone and the login screen appears.

## Optional integration states

The optional task integration is server-side and provider-neutral. Msgvault
shows disabled, discovering, authentication required, reachable but
incompatible, partial, stale, unavailable, or ready instead of presenting a
failed lookup as “no links.” Credentials never enter browser types, URLs, or
error messages. The archive remains fully usable while the integration is
absent or unhealthy.
