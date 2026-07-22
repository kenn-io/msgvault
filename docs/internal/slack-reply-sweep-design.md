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
- **Boundaries are pins; floors overlap.** Stored boundaries (the
  watermark, the `SweptThrough` stamps, the per-conversation `Cursor`)
  are the run's own start instant — the pin — never a lagged or
  forward-rounded value. A boundary means "covered through boundary −
  margin for sure"; every consumer's floor overlaps back by the lag
  margin, re-covering the trailing interval into idempotent upserts.
  The overlap absorbs the two clock uncertainties in the system: search
  index lag (a reply created just before a sweep may not be indexed yet
  — the next sweep's floor reaches back behind it) and clock skew
  between our pin (local clock) and message ts (Slack's clock) on the
  window walks. Boundaries advance only from fully-searched/walked
  intervals — never from fetched content — capped at the pin,
  checkpointed per day (sweeps) or per page (walks), and never rounded
  forward: a forward-rounded pin claims coverage of instants nothing
  has covered. Sweep hits above the pin are acted on when the index
  serves them early (harmless upserts; the next window re-covers them),
  which is safe because phase order guarantees their roots: the window
  walks run before the sweep, so any acted-on hit's root is already
  archived — and the canonical fetch keeps an existence check as the
  belt for the one leak (a failed window walk in the same run),
  processing the parent only when it is genuinely missing and skipping
  it otherwise (refreshing an archived root's content and reactions is
  --maintenance work).
- Date modifiers are evaluated in the searching user's *current profile
  timezone at query time* (verified, including retroactive re-filing
  after a tz change) **using the IANA zone's historical DST rules**
  (verified against a production corpus spanning DST transitions: a
  January day boundary follows the winter offset even when queried in
  summer, and the spring-forward day is served as a 23-hour span). So
  sweep-day arithmetic reads the user's IANA `tz` fresh each run and
  derives day boundaries with `time.LoadLocation` — a fixed zone at the
  current `tz_offset` (the fallback for unloadable zone names, and exact
  for non-DST zones) would put historical boundaries an hour off across
  a transition, letting an interrupted per-day sweep certify an hour the
  day's query never served. Gap-free across tz changes and DST with no
  standing overlap.
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
- **Guaranteed first unit.** A budget may end a run early, but it must
  never gate a phase's FIRST unit of durable progress — the invariant
  behind every convergence guarantee here. Violations recur as stalls:
  a drain page that could be all-parent (fixed by the floor-2 page), a
  catch-up flag that re-visited its final page forever (fixed by
  clearing at walk end), a sweep day-charge that starved the fetch that
  would advance the boundary (fixed by parking only once the run has
  progressed). Budget-site audit: window walks and catch-up pages are
  ≥1 message and advance a persisted cursor; drain pages are ≥2 (the
  response may lead with the parent); the sweep parks only when
  exhausted AND progressed.
- **`--limit` bounds committed work via a resumable thread drain.** The
  window walks record each discovered root as durable debt on a
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
- **The thread catch-up walk is resumable and budget-aware**, shaped like
  the backfill: each page's roots are recorded as pending-drain debt
  (charging their `reply_count` forecasts), paging holds while debt is
  outstanding, and the page cursor persists (`CatchUpCursor`, with the
  walk's pin in `CatchUpLatest` — page cursors are only valid against the
  bound they were minted with). Scanned pages charge the budget too: the
  re-read is the walk's dominant work. When the walk reaches the end, the
  flag and cursor clear even with drain debt left — the debt lives on the
  pending list, which drain-first pays unconditionally (keeping the flag
  would re-visit the final page forever). A gone conversation clears its
  debt as skipped work instead of wedging every future sync into partial
  failure.
- **The catch-up walk pins its upper bound at its own start time**, not
  the original backfill pin: conversation-level debt (`--no-threads`
  runs, non-channel sweep-gap recovery) can include replies to roots
  created after the backfill, which a pin-bounded walk would never
  anchor. Roots newer than the fresh pin need no walk (their replies
  postdate the watermark by creation time), and the pin keeps the
  newest-first pagination window stable during the walk.
- **Limited runs sweep too**, with a work budget: searched days and
  canonically fetched messages charge it, and exhaustion parks the
  boundary at the last safe point without failing the run. Per-day
  commits are durable, so a standing `--limit` schedule converges on
  reply discovery like every other path — a permanently-capped sync
  must never mean "no replies, ever". (A standing limit below the
  workspace's message rate falls progressively behind — a throughput
  ceiling, never a completeness loss.)
- **`--full` is a repair SESSION, not a one-shot.** It resets the state
  under a bumped generation and sets a repair-pending flag; every
  subsequent run — full, plain, or limited — continues the repair
  through the ordinary resumable walks until everything is Done and all
  debt is paid. Merge treats generations as lineage: a newer generation
  wins wholesale (an interrupted repair's checkpoint supersedes the
  pre-repair success blob; field-wise blending would OR the old Done
  flags over fresh partial cursors and silently abandon the repair),
  and an older one is ignored.
- **Duplicate file content keeps one row per Slack file ID** via the
  Discord alias pattern: the schema's `(message_id, content_hash)`
  uniqueness keeps the real hash on the first row; same-content siblings
  become hashless alias rows sharing the trusted CAS path, re-derived as
  downloaded on read (store read paths AND the message-detail API) — so
  repairs never re-download and no file ID's metadata is silently
  dropped. The re-derivation is provider-gated to `discord:`/`slack:`
  deliberately: a Beeper hashless local path means pending/untrusted and
  must stay hashless.
- **One walk, pinned windows**: the initial backfill and every
  incremental fetch are the SAME pinned window walk — backfill covers
  `("", pin]`, every later walk covers `(Cursor − margin, pin]` — with
  identical pagination, drain, budget, and resume machinery (`Cursor`
  is a covered-through pin, advanced only when its window completes;
  `BackfillCursor`/`BackfillLatest` are the in-flight walk's page
  cursor and pin). The walker records each discovered root on the
  pending-drain list before the containing page's cursor advances, so
  "cursor past page" means "page durable and its thread debt recorded"
  (the debt and the cursor persist in the same checkpointed blob).
  Unlimited runs drain a page's debt immediately after the page;
  limited runs carry the remainder across runs. Replies to window
  roots therefore archive immediately with no search dependency; the
  sweep's remaining job is late replies to roots below the previous
  boundary.

## Sweep algorithm

Per source, after the per-conversation walks complete:

```
zone = current user IANA tz (users cache; historical DST rules — probed);
       fall back to FixedZone(tz_offset) when the name will not load

# Stamp adoption (targets without a SweptThrough):
#   max(own backfill pin, watermark) — the pin is exact for a freshly
#   completed backfill (inline thread fetches covered everything up to
#   it, and the pin always postdates the watermark at completion time);
#   the watermark is correct for legacy pre-stamp state.

pin = tsFormat(now)                    # this sweep's boundary

# Stamp adoption (targets without a SweptThrough):
#   max(own walk pin, watermark) — the pin is exact for a freshly
#   completed backfill (inline drains covered everything up to it, and
#   the pin always postdates the watermark at completion time); the
#   watermark is correct for legacy pre-stamp state.

# Gap recovery, per target stamped behind the watermark:
for conv C with SweptThrough < SweepWatermark:
    if C is not a channel ID: set ThreadsPending; stamp = watermark
    else: sweepRange(scope=C, floor=SweptThrough, searchEnd=watermark,
                     ceiling=watermark) → stamp advances per day

# Workspace sweep:
floor = SweepWatermark, or min(SweptThrough of targets) on first sweep
sweepRange(scope=none, floor, searchEnd=now, ceiling=pin)
    → watermark advances per day; targets stamped at or past the floor
      are stamped forward with it (a conversation parked behind by a
      failed gap sweep keeps its stamp and retries next run)

sweepRange(scope, floor, searchEnd, ceiling):
    queryFloor = floor − lagMargin                 # the OVERLAP
    budget: one charge per day; parks only when exhausted AND the run
            has already fetched something (guaranteed first unit)
    for day D = day(queryFloor, zone) … day(searchEnd, zone):  // ascending
        for page = 1 … min(pages, 100):
            q = `[in:<#scope>] threads:replies on:D -"<nonce>"`
            stop if echoed page ≠ requested page               // clamp tell
            collect hits: (channel_id, ts, permalink)
        hits ∩ sweep targets, above queryFloor, ascending by ts
        group by permalink thread_ts (fallback: per hit)
        for each group (ascending by min hit ts):
            conversations.replies(channel, ts=hit, oldest=minHit−1µs)
            persist via the standard upsert path (archived parents
            skipped; missing parents processed)
            on fetch failure or blocked budget:
                advance to min(minHit−1µs, ceiling) if > floor; halt
        if day total > 10k ceiling:
            advance to min(last hit, ceiling) if > floor; FAIL RUN; halt
        advance to min(end of D, ceiling) if > floor; checkpoint
```

The boundary never passes unpersisted work, never derives from fetched
content, and never regresses (overlap-region parks sit below the stored
floor and are dropped). An aborted sweep resumes exactly where it
stopped; everything downstream of discovery is the existing idempotent
upsert machinery.

## State

```go
type SyncState struct {
    Conversations  map[string]*ConvState
    SweepWatermark string // pin of the last workspace sweep; covered through this − margin
    SweepOffset    int    // tz_offset in effect when the watermark was written (audit)
    Generation     int    // state lineage; --full bumps it (newer supersedes wholesale)
    RepairPending  bool   // an in-flight --full repair session
}

type ConvState struct {
    Cursor         string          // covered-through pin (window walks resume from this − margin)
    BackfillCursor string          // in-flight window walk's page cursor
    BackfillLatest string          // in-flight window walk's pin
    Done           bool            // initial walk reached the beginning of history
    SweptThrough   string          // pin of the last sweep covering this conversation
    PendingThreads []PendingThread // outstanding thread-drain debt (≤ one page's roots)
    ThreadsPending bool            // a catch-up walk is owed
    CatchUpCursor  string          // resumable catch-up walk page cursor
    CatchUpLatest  string          // the pin the walk was started under
}

type PendingThread struct {
    RootTS    string // thread root (already archived with its page)
    DrainedTo string // newest reply fetched; drain resumes oldest=DrainedTo
    Forecast  int    // remaining reply_count estimate (budget pacing only)
}
```

`ConvState` loses `Threads` and the incremental window cursors
(`incr_cursor`/`incr_max_ts`) — old checkpoints carrying those keys load
cleanly; an upgraded mid-window checkpoint re-walks at most one window
into idempotent upserts. Merge rules: generations gate everything (newer
wins wholesale, older is ignored); within a generation the
further-advanced watermark wins, carrying its offset with it;
`SweptThrough`/`Cursor` merge further-advanced-wins per conversation;
page cursors, pins, and `PendingThreads` follow non-empty-wins /
emptiness-never-clears.

## Probe ledger

| Behavior | Verdict | Where |
|---|---|---|
| `threads:replies` returns replies only (no roots) | verified | sandbox + production workspace |
| Search completeness for replies after a date | verified | production workspace |
| New search API works without AI-features flag | verified | production workspace |
| Unjoined public channels appear in results | verified — filter is load-bearing | sandbox (`#msgvault-probe-unjoined`) |
| `in:<#ID>` scoping reliable; name form observed unreliable | verified / observed | production + sandbox |
| Date modifiers use current profile tz at query time | verified (tz changed mid-test; messages re-filed) | sandbox |
| Date filing uses IANA zone with HISTORICAL DST rules (not flat current offset) | verified — winter day boundary at the winter offset while queried in summer; brackets within minutes | production workspace (multi-year corpus, profile tz `America/New_York`) |
| DST transition day served as a 23-hour span | verified — `on:` day starts at pre-transition midnight, ends at post-transition midnight | production workspace |
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
  not-done conversations skipped; multi-page sweep days; the overlapped
  floor recovers late-indexed replies past an advanced watermark and
  clock-skew-hidden window arrivals;
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
