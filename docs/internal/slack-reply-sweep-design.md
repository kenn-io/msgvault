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
- **Cursor: a UTC watermark per source plus a certification stamp per
  conversation.** The watermark (`replies swept before T`) covers the
  current target set as a whole; each conversation's `SweptThrough`
  normally tracks it and lags when the conversation missed sweeps
  (excluded, unreadable, or filtered while the watermark advanced). A
  conversation re-entering behind the watermark first recovers its gap
  with a channel-scoped sweep (`in:<#C…>` over just the missed days) —
  without this, the workspace sweep's floor is already past the gap and
  those replies are lost permanently. DMs/group DMs (non-`C` IDs, where
  the `in:` scope form is unprobed) recover through the blunter thread
  catch-up walk (`ThreadsPending`) instead.
- **Certification derives only from fully-searched intervals — never
  from fetched content.** A canonical fetch returns the whole thread,
  including replies newer than `now − lag margin` that the search index
  may not serve yet; letting those advance the watermark would skip
  not-yet-indexed replies in *other* threads forever. So: each cleanly
  completed day certifies to its end (capped at the lag horizon, and
  checkpointed per day so multi-day catch-ups drain across runs); a
  failed canonical fetch parks certification just before the failed
  thread's first hit; the search itself still runs through *now*, so
  in-lag-window replies are archived early and simply re-swept next run
  (idempotent upserts). Both boundaries advance only behind persisted
  work.
- Date modifiers are evaluated in the searching user's *current profile
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
  tripwire. A single-day, single-scope result set beyond the ~10,000
  reachable ceiling **parks and fails**: ascending order means the
  reachable results are the day's earliest, so certification advances to
  the last processed hit, the run fails loudly, and the sweep halts —
  it never certifies past unreachable replies, and it cannot drain the
  day across runs either (re-querying serves the same first 10k; `on:`
  has no intra-day lower bound). Recovery today is `sync-slack --full`,
  whose backfill-style inline thread fetches need no search; `in:`-batch
  narrowing (a fresh 10k ceiling per channel scope) is the specified
  sweep-native escape hatch, unbuilt until the tripwire ever fires —
  `threads:replies` filtering makes it Enterprise-scale rare.
  Cursormark pagination is not supported on this method (parameter
  silently ignored).
- **`--limit` bounds committed work via a resumable thread drain.** The
  backfill records each discovered root as durable debt on a
  per-conversation pending list — `(root ts, drained-to ts, remaining
  reply_count forecast)` — before the page cursor advances, charging the
  forecast against the run budget so root progress stays loosely aligned
  with thread progress. The drain pays the list head-first, resuming each
  thread with `oldest=drained-to` (a self-validating ts bound; the
  archived root is not refetched) on budget-sized pages, converting
  forecast to actuals as replies land. Invariants: the walk never fetches
  a new history page while debt is outstanding (bounding the list at one
  page's roots), and the drain runs before anything else touches the
  conversation — so a standing `--limit` schedule converges to a complete
  archive by itself, draining arbitrarily large threads across runs with
  durable progress every run. The list merges like the backfill cursor
  (non-empty wins; emptiness never clears — stale debt re-drains into
  idempotent upserts, dropped debt would lose replies). Sweeps and
  catch-up walks only run unlimited, so their thread fetches stay
  whole-thread.
- **The thread catch-up walk pins its upper bound at its own start time**,
  not the original backfill pin: conversation-level debt (`--no-threads`
  runs, non-channel sweep-gap recovery) can include replies to roots
  created after the backfill, which a pin-bounded walk would never
  anchor. Roots newer than the fresh pin need no walk (their replies
  postdate the watermark by creation time), and the pin keeps the
  newest-first pagination window stable during the walk.
- **Duplicate file content keeps one row per Slack file ID** via the
  Discord alias pattern: the schema's `(message_id, content_hash)`
  uniqueness keeps the real hash on the first row; same-content siblings
  become hashless alias rows sharing the trusted CAS path, re-derived as
  downloaded on read — so repairs never re-download and no file ID's
  metadata is silently dropped.
- **Backfill owes threads as recorded debt**: the history walker records
  each discovered root on the pending-drain list before the containing
  page's cursor advances, so "cursor past page" means "page durable and
  its thread debt recorded" (the debt and the cursor persist in the same
  checkpointed blob). Unlimited runs drain a page's debt immediately
  after the page, which reduces to the old inline behavior; limited runs
  carry the remainder across runs. Incremental needs no root fetching at
  all: any reply is created after some watermark and the sweep finds it.

## Sweep algorithm

Per source, after the per-conversation walks complete:

```
offset = current user tz_offset (from the users cache)

# Stamp adoption (targets without a SweptThrough):
#   max(own backfill pin, watermark) — the pin is exact for a freshly
#   completed backfill (inline thread fetches covered everything up to
#   it, and the pin always postdates the watermark at completion time);
#   the watermark is correct for legacy pre-stamp state.

# Gap recovery, per target certified behind the watermark:
for conv C with SweptThrough < SweepWatermark:
    if C is not a channel ID: set ThreadsPending; stamp = watermark
    else: sweepRange(scope=C, floor=SweptThrough, searchEnd=watermark,
                     ceiling=watermark) → stamp advances per day

# Workspace sweep:
floor = SweepWatermark, or min(SweptThrough of targets) on first sweep
sweepRange(scope=none, floor, searchEnd=now, ceiling=now − lagMargin)
    → watermark advances per day; targets certified through the floor
      are stamped forward with it (a conversation parked behind by a
      failed gap sweep keeps its stamp and retries next run)

sweepRange(scope, floor, searchEnd, ceiling):
    for day D = day(floor, offset) … day(searchEnd, offset):   // ascending
        for page = 1 … min(pages, 100):
            q = `[in:<#scope>] threads:replies on:D -"<nonce>"`
            stop if echoed page ≠ requested page               // clamp tell
            collect hits: (channel_id, ts, permalink)
        hits ∩ sweep targets, above floor, ascending by ts
        group by permalink thread_ts (fallback: per hit)
        for each group (ascending by min hit ts):
            conversations.replies(channel, ts=hit, oldest=minHit−1µs)
            persist via the standard upsert path
            on fetch failure: certify min(minHit−1µs, ceiling); halt
        if day total > 10k ceiling:
            certify min(last processed hit, ceiling); FAIL RUN; halt
        certify min(end of D, ceiling); checkpoint             // per-day drain
```

Certification never passes unpersisted work and never derives from
fetched content (fetches return whole threads, including replies newer
than the index horizon); searchEnd exceeding the ceiling means fresh
replies are archived early and re-swept next run. An aborted sweep
resumes exactly where it stopped; everything downstream of discovery is
the existing idempotent upsert machinery.

## State

```go
type SyncState struct {
    Conversations  map[string]*ConvState
    SweepWatermark string // UTC ts: the current target set's certification boundary
    SweepOffset    int    // tz_offset in effect when the watermark was written (audit)
}

type ConvState struct {
    // …cursors…
    SweptThrough   string          // UTC ts: this conversation's own reply certification
    PendingThreads []PendingThread // backfill's outstanding thread-drain debt (≤ one page's roots)
}

type PendingThread struct {
    RootTS    string // thread root (already archived with its page)
    DrainedTo string // newest reply fetched; drain resumes oldest=DrainedTo
    Forecast  int    // remaining reply_count estimate (budget pacing only)
}
```

`ConvState` loses `Threads` (old checkpoints carrying a `threads` key
load cleanly; the field is simply gone) and gains `SweptThrough` plus the
bounded, transient `PendingThreads` work queue. Merge
rules: the further-advanced watermark wins, carrying its offset with it;
`SweptThrough` merges further-advanced-wins per conversation;
`PendingThreads` merges like the backfill cursor (non-empty wins,
emptiness never clears).

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
  not-done conversations skipped; multi-page sweep days; certification
  stays behind the lag horizon even when fetches return fresher content;
  a truncated day fails the run without certifying past it; an
  excluded-then-re-included channel recovers its gap via the scoped
  sweep; `--limit` bounds thread replies and leaves the page resumable;
  tombstoned/omitted files keep their archived attachment rows;
  `add-slack` rejects tokens without `search:read`.
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
