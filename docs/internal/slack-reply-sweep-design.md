# Slack Reply Sweep — Design

Date: 2026-07-20
Status: Probed and decided; supersedes the thread-lookback polling design
in `slack-ingestion-design.md` (its LB-3 mitigation section). Every
load-bearing behavior below was verified live against a Slack Developer
Program sandbox and, where noted, a production workspace.

## Goal

Replace lookback-window thread polling with search-driven reply discovery
for incremental sync. Consequences:

- The 30-day thread blind spot disappears: a reply to a years-old thread
  is discovered by the next sweep regardless of root age.
- Per-conversation state shrinks: no tracked-root map, no reply cursors,
  no pruning rules; one UTC watermark per source replaces all of it.
- Edits/reactions maintenance becomes explicit-only (`--maintenance`):
  the periodic rescan loses its thread-discovery role and reverts to a
  pure repair pass the user runs deliberately.

## Decisions

- **Discovery via `search.messages`** (user scope `search:read`) with the
  `threads:replies` modifier, which returns thread replies exclusively —
  verified: not even root messages are included. Day-granular `on:`/
  `after:` bounds; `sort=timestamp, sort_dir=asc`.
- `search.messages` is marked legacy (its replacement is
  `assistant.search.context`), but is preferred here: ~100 results/page
  at ~20 req/min versus 20/page at ~10 req/min with daily-restriction
  language. The provider is a small interface so the alternate API can
  be slotted in if the legacy method is removed; verified: the new API
  works with a plain user token (no AI-features app flag in practice).
- **Search is discovery only; `conversations.replies` is the archival
  fetch.** Search result objects are not native message JSON; archiving
  them would fork `raw_format`. Instead each hit is a pointer
  (channel id, reply ts): `conversations.replies` accepts *any* message
  ts in a thread and returns the thread with its true parent first, so
  root resolution is authoritative from the API. The permalink's
  `thread_ts` parameter is used only to group hits into one fetch per
  thread; a parse failure degrades to per-hit fetches, never data loss.
- **Membership boundary is enforced by our filter, not the API.**
  Verified: search returns hits from public channels the user never
  joined (both scoped and unscoped queries). Hits are matched against
  the known conversation set and only `done` conversations are fetched;
  everything else is dropped. Channel scoping, when needed, must use the
  immutable `in:<#C…>` ID form (names are mutable/recyclable and were
  observed unreliable as scopes).
- **Cursor: one UTC watermark per source** (`replies swept before T`),
  advanced only behind persisted work, never past `now − lag margin`.
  Date modifiers are evaluated in the searching user's *current profile
  timezone at query time* (verified, including retroactive re-filing
  after a tz change), so sweep-day arithmetic reads `tz_offset` fresh
  each run and derives days from the watermark under the current offset
  — gap-free across tz changes and DST with no standing overlap.
- **Every query string is unique** (an inert negated nonce term):
  verified that search responses are cached by query string and can
  serve stale results — including pre-tz-change day filings — for
  repeated queries.
- **Pager rules** (verified): `page` beyond 100 is silently *clamped to
  page 1* — the pager must stop when the echoed `paging.page` differs
  from the requested page, and bound itself by `min(paging.pages, 100)`.
  `pagination.total_count` is accurate and serves as the truncation
  tripwire; a single-day, single-scope result set beyond the ~10,000
  reachable ceiling is logged (`in:`-batch narrowing is the specified
  escape hatch, unbuilt until the tripwire ever fires — `threads:replies`
  filtering makes it Enterprise-scale rare). Cursormark pagination is
  not supported on this method (parameter silently ignored).
- **Backfill is unchanged in shape**: the history walker fetches each
  discovered root's replies inline, before the containing page's cursor
  advances, so "cursor past page" continues to mean "page and its
  threads durable". Incremental needs no root fetching at all: any reply
  is created after some watermark and the sweep finds it.

## Sweep algorithm

Per source, after the per-conversation walks complete:

```
offset    = current user tz_offset (from the users cache)
floor     = SweepWatermark, or min(BackfillLatest of done convs) on first sweep
for day D = day(floor, offset) … day(now, offset):        // ascending
    for page = 1 … min(pages, 100):
        q = `threads:replies on:D -"<nonce>"`             // nonce unique per query
        stop if echoed page ≠ requested page               // clamp tell
        collect hits: (channel_id, ts, permalink)
    hits ∩ known done conversations, ascending by ts
    group by permalink thread_ts (fallback: per hit)
    for each group (ascending by min hit ts):
        conversations.replies(channel, ts=hit, oldest=minHit−1µs)
        persist via the standard upsert path
        on fetch failure: watermark = minHit − 1µs; abort sweep   // resume here next run
advance watermark to min(now − lagMargin) on clean completion
```

Failure semantics mirror the rest of the importer: the watermark never
passes unpersisted work; an aborted sweep resumes exactly where it
stopped; everything downstream of discovery is the existing idempotent
upsert machinery.

## State

```go
type SyncState struct {
    Conversations  map[string]*ConvState
    SweepWatermark string // UTC ts: all replies created before this are archived
    SweepOffset    int    // tz_offset in effect when the watermark was written (audit)
}
```

`ConvState` loses `Threads` (old checkpoints carrying a `threads` key
load cleanly; the field is simply gone). Merge rule: the further-advanced
watermark wins, carrying its offset with it.

## Probe ledger

| Behavior | Verdict | Where |
|---|---|---|
| `threads:replies` returns replies only (no roots) | verified | sandbox + production workspace |
| Search completeness for replies after a date | verified | production workspace |
| New search API works without AI-features flag | verified | production workspace |
| Unjoined public channels appear in results | verified — filter is load-bearing | sandbox (`#msgvault-probe-unjoined`) |
| `in:<#ID>` scoping reliable; name form observed unreliable | verified / observed | production + sandbox |
| Date modifiers use current profile tz at query time | verified (tz changed mid-test; messages re-filed) | sandbox |
| Results cached by query string (stale across tz change) | verified — nonce required | sandbox |
| `page=101` silently clamps to page 1 | verified — echo check required | sandbox |
| Cursormark pagination unsupported (ignored) | verified — 10k/query ceiling stands | sandbox |
| `sort=timestamp asc` stable across full 11-page walk, no dups | verified | sandbox (1,099-message corpus) |
| `pagination.total_count` accurate | verified at 1k scale | sandbox |
| >10k single-day behavior | unprobed (needs 3h seeded corpus); clamp+tripwire characterize it | — |

## Testing

- Fake server gains `/search.messages` with faithful semantics: reply
  flattening, day filtering, `in:<#ID>` scoping, ascending sort,
  pagination **including the page-clamp behavior**, and accurate totals.
- e2e: late reply to an ancient thread discovered by sweep (the old
  blind spot); watermark holds on canonical-fetch failure and resumes;
  not-done conversations skipped; multi-page sweep days.
- Maintenance gating: edits invisible to plain incremental runs, caught
  by `--maintenance`.
- Live (env-gated, sandbox): `threads:replies` pin-test — asserts
  replies-only results with a no-modifier control query, so a silent
  change in Slack's modifier grammar fails a test instead of degrading
  the sweep into noise.

## Out of scope

- `assistant.search.context` provider (interface accommodates it).
- `in:`-batch narrowing implementation (specified; built if the
  truncation tripwire ever fires in practice).
- Concurrent fetch/persist pipeline — orthogonal performance work,
  sequenced separately.
