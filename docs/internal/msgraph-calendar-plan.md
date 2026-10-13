# Microsoft Graph Calendar Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans and
> superpowers:test-driven-development. Execute tasks in order. Each behavior
> change requires a failing test before implementation. Checkboxes track work.

**Status:** Proposed, 2026-10-13. This PR contains the plan only. Product
implementation requires independent GPT-6-Astra XHIGH review and coordinator
approval. The projection migration also requires an allocated archive version.

**Goal:** Archive Microsoft 365 and Outlook.com mailbox calendars as typed
calendar events, readable through the shared calendar and meeting surfaces.

**Architecture:** A read-only Graph connector archives singles, series masters,
changed instances and cancellations. The `msgraph_event_json` decoder feeds
T2's existing event projection. T1 expands recurrence during scoped reads;
regular occurrences are never fetched and stored as a substitute for expansion.

**Tech Stack:** Go, existing `internal/msgraph` and `internal/microsoft`,
`internal/calevent`, `internal/calrecur`, Store, SQLite and PostgreSQL, testify.
No Microsoft SDK or new recurrence library.

**Spec:** [#1200](https://github.com/kenn-io/msgvault/issues/1200), calendar and
meetings design v3 §8.1 and its shared projection, recurrence and read rules.
The Wave 1 implementation plan v3.1 amendments override older task text.
[Stack base #1248](https://github.com/kenn-io/msgvault/pull/1248) provides T2's
interfaces. Source inspection used T2 `be6070debdd44093e4e52feced82b8089bb2fa1d`.

## Global Constraints

- Own W2a only. Preserve T1/T2 Produces contracts: `calevent.SourceContext`,
  `Event`, `Attendee`, `Decoder`, `Register`, `Decode`, `IsRoomAttendee`,
  `JoinKey`, `calrecur` APIs, Store refs, `SourceScope` and snapshot readers.
  An interface change requires coordinator review before further code.
- Typed events rebuild from raw plus metadata. Preserve existing metadata
  fields. Only `calendar_event` messages project. `calevent` never imports Store.
- No stored generated occurrences, fuzzy linking, provider writes, Graph mail
  changes, Teams chat/call ingestion, Web UI or TUI work. Microsoft calendar
  writes belong to a separate workstream.
- Archived reads use `message.read` on each calendar source. `calendar.read`
  remains live free/busy. No new calendar detail grant or MCP write tool.
- Every public read uses the request's snapshot and applies source scope before
  selection, deduplication, ranking, limits and alias resolution. Exceptions
  resolve masters only within the same provider, source and calendar namespace.
- Retain v3.1 limits: candidate widening ±28 hours; at most 2000 candidate
  series and 20000 exceptions; 50000 candidates per series, 500000 per request.
  Excess returns `ErrQueryTooBroad`, never a partial occurrence page.
- Follow T1's supported recurrence subset. Unsupported provider patterns remain
  archived and produce a recurrence error; never widen the iterator's grammar.
- SQL goes through Store. No `SELECT DISTINCT` with JOINs. List/aggregate reads
  never scan `message_bodies`. Direct body lookups are for one message only.
- SQLite/PostgreSQL parity, context cancellation, useful wrapped errors and
  testify assertions (`require` for setup, `assert` for independent checks,
  expected first). All fixtures use synthetic values and reserved domains.
- No account, token, archive or deployment access during development. Test HTTP
  contracts with `httptest.Server`, real parsers and temporary synthetic stores.
- Serialize local Go commands with `GOMAXPROCS=2 GOFLAGS=-p=2`; Go tests and vet
  use `-tags "fts5 sqlite_vec"`. Before Go commits run `go fmt ./...` with
  `GOFLAGS='-p=2 -tags=fts5,sqlite_vec'` and tagged `go vet ./...` under that
  CPU cap. Run `make lint-ci` once before a product push. Planning runs no Go.
- At most one broad gate before a product push:
  `flock -w 300 /tmp/local-gate.lock env GOMAXPROCS=2 GOFLAGS=-p=2 make test`.
  A busy lock after five minutes defers proof to PR CI. No remote test runner.
- No schema version is assigned here. A required data migration takes one
  coordinator-allocated `store.SchemaVersion` increment on the integrated
  base, including both digest arrays. Do not guess a PostgreSQL digest;
  obtain it from a synthetic PostgreSQL run or CI. If neither is available,
  report the missing digest and retain an explicit failing `TODO(pg-digest)`.
- Commit logical tasks, scrub changed text, commit messages and PR copy for
  private identifiers before every push, push to `fork`, and keep a draft PR
  against `main` with `Part of #1200.` and `Stacked on #1248.`. Describe user
  benefit and boundaries; omit command-run sections. CI is final proof;
  record absent runs and move on. Never run `roborev review`.

## Production paths and provider facts

The current Graph transport in `internal/msgraph/client.go` already handles
bearer auth, bounded bodies, same-origin paging, retry and cancellation. It
has no caller-supplied `Prefer` header support. `internal/microsoft/graph_oauth.go`
shares browser/device-code auth, mailbox confirmation and token refresh across
Teams, mail and contacts, with separate scope sets and token prefixes. Calendar
support gets another capability-specific manager using that machinery.

Google ingestion is in `internal/calsync/{calsync,persist,incremental}.go` and
`cmd/msgvault/cmd/calendar.go`. Reuse its per-calendar source and scheduler
patterns, but keep the Google API/model and incremental cursor unchanged.
`store.PersistMessageContext` already writes message, raw, metadata, recipients,
body and FTS atomically and refreshes the event projection in that transaction.
Use it instead of copying Google's sequence of separate writes.

T2 registers `gcal_json` in `internal/calevent/google.go`. Its projection and
backfill live in `internal/store/{calendar_event_projection,
migrate_calendar_event_projection}.go`. An already completed
`calendar_event_projection_v1` ledger will not rerun when a decoder is added;
registration alone cannot backfill existing raw Graph evidence.

Verified provider contracts and their consequences:

- Both personal and work/school mailbox calendars support delegated
  `Calendars.Read`. Use it for descriptions; `Calendars.ReadBasic` omits body.
  Group calendars and other people's delegated/shared calendars are outside
  this first connector. See [list calendars](https://learn.microsoft.com/en-us/graph/api/user-list-calendars?view=graph-rest-1.0)
  and [permissions](https://learn.microsoft.com/en-us/graph/permissions-reference).
- [List events](https://learn.microsoft.com/en-us/graph/api/calendar-list-events?view=graph-rest-1.0)
  returns singles and series masters. A
  [master GET](https://learn.microsoft.com/en-us/graph/api/event-get?view=graph-rest-1.0)
  with `$select=...,exceptionOccurrences,cancelledOccurrences` and
  `$expand=exceptionOccurrences` supplies edits and cancellation identifiers.
  Never substitute a bounded `calendarView` download for the archive.
- [The event resource](https://learn.microsoft.com/en-us/graph/api/resources/event?view=graph-rest-1.0)
  supplies `seriesMasterId`, UTC `originalStart`, resource attendees and
  `iCalUId`. The UID differs between occurrences of one series: preserve it
  exactly, and never manufacture a common series UID from the master's UID.
- [Immutable IDs](https://learn.microsoft.com/en-us/graph/outlook-immutable-id)
  require `Prefer: IdType="ImmutableId"` on every request. Calendar container
  IDs are already stable. IDs are opaque and case-sensitive.
- Graph has six [recurrence pattern types](https://learn.microsoft.com/en-us/graph/api/resources/recurrencepattern?view=graph-rest-1.0)
  and three [range types](https://learn.microsoft.com/en-us/graph/api/resources/recurrencerange?view=graph-rest-1.0).
  Local civil time, inclusive range end dates and count semantics need explicit
  conversion before T1 can expand them.
- [dateTimeTimeZone](https://learn.microsoft.com/en-us/graph/api/resources/datetimetimezone?view=graph-rest-1.0)
  accepts Windows and IANA names. Default GET bounds are UTC. Use the original
  or recurrence zone for local-time recurrence; a UTC response is not a UTC
  recurrence. Map Windows names with pinned
  [CLDR 48 territory 001 mappings](https://github.com/unicode-org/cldr/blob/release-48/common/supplemental/windowsZones.xml).

## Design decisions

### Identity, raw evidence and source selection

`Provider="msgraph"`, `raw_format="msgraph_event_json"`,
`source_type="mscalendar"`, `message_type="calendar_event"` and
`conversation_type="calendar"`. Namespace and `CalendarID` are the mailbox
calendar's opaque ID. Each source identifier is normalized account email,
`/`, then the unchanged calendar ID; the account keys auth, never an event ID.
This separates calendar copies and their source grants. Do not lowercase IDs.

Source `sync_config` contains `account_email`, `calendar_id`,
`calendar_summary`, `mailbox_id` and canonical IANA `time_zone`. `/me` provides
verified mailbox identity; `/me/mailboxSettings/timeZone` provides the mailbox
zone under delegated `MailboxSettings.Read`. Request that scope alongside
`Calendars.Read`, `User.Read`, `offline_access`, `openid`, `email`, `profile`.
No token from mail, Teams or contacts is overwritten. Save calendar tokens with
`mscalendar_` and reauth hint `msgvault add-mscalendar %s`.

List `/me/calendars`, default to calendars whose `owner.address` matches the
verified mailbox email. Explicit `--calendar` may select only such calendars;
reject shared calendars clearly. Empty or unverifiable owners are not assumed
owned. Never request application permissions, `Calendars.ReadWrite`, or Teams
permissions. Tenant defaults to configured `common` for both account types;
existing configured tenant restrictions still apply.

Keep each received event JSON object as raw evidence, including unknown fields.
Master detail raw preserves its expanded exception list and cancellation IDs.
Child exceptions retain their own complete GET payload. Conversion only changes
the typed projection and compatible message metadata. Preserve metadata keys
`status`, `all_day`, `start`, `end`, `time_zone`, `recurrence`,
`recurring_event_id`, `original_start_time`, `ical_uid`, `sequence`,
`html_link`, `hangout_link`, `transparency`, `visibility`, `event_type`,
`organizer_email`, `calendar_id`, `account_email`; use sequence 0 because Graph
has no RFC SEQUENCE field. Add namespaced `graph_*` evidence as described below.

Single/master message key is the immutable event ID. Exception key is
`seriesMasterId|<original identity Key()>`; conversation key is
`event:<seriesMasterId>` or `event:<id>`. Raw `id` remains `ProviderEventID`.
Never parse a Graph ID with the Google instance-ID parser. Empty calendar ID,
missing series ID or missing original identity is a decode error, not a guessed
namespace or occurrence.

### Projection, attendee and read contracts

`singleInstance` maps to `single`, `seriesMaster` to `series` (series ID = own
ID), and `exception` to `exception`. Normal `occurrence` payloads are rejected
with `ok=false`; they must not create generated occurrence messages. The only
exception is a previously archived edit explicitly reconciled as a reset below.

`originalStart` is the original timed UTC identity, even after an edit moves the
start. For all-day exceptions convert it to a civil date in the original
recurrence zone supplied in `graph_series_time_zone`, never the moved event's
zone or the account's current zone. Preserve sparse cancellations with
`HasBounds=false` and `Original`; T1 inherits the master's bounds. An orphan
with an explicit original identity is kept for T2's later alias resolution.

Normalize attendee emails; `type=resource` is `room` through
`IsRoomAttendee(email, true)`, regardless of the address domain. Otherwise map
required/optional and organizer roles. Map accepted/declined/tentativelyAccepted
and notResponded to accepted/declined/tentative/needs_action; absent/unknown is
unknown. Organizer is a separate invited attendee when absent from the array.
Never turn resources into participant, sender, recipient or FTS address rows.
If the organizer address matches a resource attendee, do not create a sender.

`responseStatus` describes this calendar copy. Map its response into
`CopySelfRSVP` and mark only the mailbox owner's matching attendee as copy-self;
`isOrganizer` alone does not prove attendance. Meeting owner RSVP remains the
shared reader's confirmed-identity match. Invites alone never imply attendance.

`isCancelled` gives cancelled status, otherwise confirmed; tentative is an RSVP,
not inferred event cancellation. Map `showAs=free` to transparent, other known
busy states to opaque, `sensitivity=private/confidential` to private, normal to
default; preserve unrecognized values in raw. Use `location.displayName`,
`onlineMeeting.joinUrl` then legacy `onlineMeetingUrl`, and shared `JoinKey`.
Decode HTML `body.content` to plain text through `mime.StripHTML`;
cap description at 4096 UTF-8 bytes and set `DescriptionTruncated`.

No new public read backend, operation, capability or API schema version is
needed. T4/T6 consume the existing projected rows after their changes integrate.
W2a tests the shared Store row queries and T1 expansion on its T2 base. HTTP,
CLI and MCP end-to-end occurrence checks run at integration; their absence on
this base must not be filled by local copies of those readers. Existing live
Google calendar controls continue to accept only Google sources.

### Time zones and recurrence conversion

Use IANA names directly or pinned CLDR Windows→IANA mappings. Unknown or custom
`tzone://Microsoft/Custom` zones never silently become UTC. Keep raw zone text;
a timed single with explicit UTC bounds remains readable, while a series with
an unknown recurrence zone retains a recurrence error. An all-day event without
a reliable civil-date zone is rejected by the decoder and remains raw only.

Series zone precedence: `recurrence.range.recurrenceTimeZone`, then
`originalStartTimeZone`, then source zone. Reconstruct DTSTART's wall clock in
that zone from the returned instant. For all-day bounds use midnight in that
zone and preserve `[StartDate, EndDate)` as dates, including DST-length days.
Ordinary timed singles parse their `dateTime` in the payload's `timeZone`;
fractional seconds and explicit offsets are accepted.

Convert recurrence into T1-compatible lines with interval > 0:

| Graph pattern | RRULE fields |
|---|---|
| daily | `FREQ=DAILY;INTERVAL=n` |
| weekly | `FREQ=WEEKLY;INTERVAL=n;BYDAY=...;WKST=...` |
| absoluteMonthly | `FREQ=MONTHLY;INTERVAL=n;BYMONTHDAY=d` |
| relativeMonthly, one weekday | `FREQ=MONTHLY;INTERVAL=n;BYDAY=pXX` |
| absoluteYearly | `FREQ=YEARLY;INTERVAL=n;BYMONTH=m;BYMONTHDAY=d` |
| relativeYearly, one weekday | `FREQ=YEARLY;INTERVAL=n;BYMONTH=m;BYDAY=pXX` |

Ordinal first/second/third/fourth/last maps to 1/2/3/4/−1. Multiple weekdays
in a relative pattern are initially unexpandable: Graph's first-matching-day
semantics must not be approximated by one ordinal per weekday. Supporting that
case needs a separately reviewed conversion, not a T1 grammar change.

`numbered` appends `COUNT=n` (> 0); `noEnd` adds no bound; `endDate` appends
`UNTIL` for the final second of that local civil date, converted to UTC.
Validate range start against series DTSTART, day/month values and positive
interval/count before producing lines. Test every output with `calrecur.ParseSet`.
Unknown patterns, invalid selectors, uncertain zones and unresolved series
state produce a deterministic unsupported property, for example
`X-MSGVAULT-GRAPH-UNSUPPORTED:unresolved-cancellation`. T1 rejects it, so T2
sets `recurrence_error` and leaves the conservative end NULL. Do not erase
recurrence or turn a bad series into a single event.

### Full scans, exceptions, cancellations and reconciliation

The initial connector performs a complete replay on each sync; no event delta
cursor or finite time horizon. This is deliberately slower than a delta sync
but archives old masters and moved exceptions. A limit is a partial diagnostic
run, never evidence of deletion or a successful complete calendar scan. An
interrupted run restarts the scan; per-message keys make replay idempotent.

For each master, GET all projection fields, `exceptionOccurrences` and
`cancelledOccurrences`, expand exceptions, follow nested next links, then GET
any exception whose expanded payload lacks required fields. Every request uses
immutable-ID and UTC-response preferences, byte/page caps and context. Persist
master metadata `graph_series_complete=false` before child changes. Set it true
only after all edited and cancelled children and reset reconciliation commit.
The decoder keeps an incomplete master unexpandable. Empty arrays count as
complete only when explicitly returned; an omitted relationship is incomplete.
This avoids generating nominal instances while a scan has fetched only half
its edits. Previously archived evidence remains readable on failure.

Cancellation IDs are opaque. The v1.0 event GET example returns OID strings,
but does not promise a general ID→original-start parser. Do not rely on the
beta occurrence-ID format or invent dates by splitting IDs. Resolve a
cancellation from a returned cancellation event with `seriesMasterId` and
`originalStart`, or a previously archived raw event whose returned occurrence
ID exactly matches. A bounded GET by the cancellation ID may supply that
payload; a 404 means unresolved, not that the cancellation disappeared. Store
resolved identity evidence in `graph_cancelled_occurrences` metadata on the
master and persist a sparse cancellation child carrying that identity and the
original Graph ID in a versioned `graph_cancellation` raw envelope. The same
raw format decoder accepts this envelope; it contains the exact preserved
cancellation evidence and explicit derived identity, not a forged Graph event.

The envelope fields are `kind="graph_cancellation"`, `version=1`,
`calendar_id`, `series_master_id`, `occurrence_id`, `series_time_zone`,
`master_raw` (the exact master object), and `resolved_event_raw` (the current
or archived object used to resolve the original). Recompute `Original` from
that nested event's `originalStart` and all-day flag. Require the master
cancelled list to contain the same occurrence ID, and require matching calendar
and series IDs. Do not trust a free-standing derived date. Master metadata
entries hold `occurrence_id`, UTC `original_start` or civil `original_start_date`
and `all_day`; they are derived from the same validated envelope evidence.

If any ID cannot be resolved, keep all cancellation evidence and suppress
series expansion with the unsupported marker. The sync result reports the
number of such series. This is an explicit initial limitation: a series with
only opaque cancellation IDs may be unexpandable, including on first import.
A later successful replay may resolve it and clear the error. Do not claim
complete Outlook recurrence coverage while this limitation remains.

After a complete master detail fetch, compare its current exception and
cancellation sets with archived children through a Store helper scoped to that
source/master. If an edit or cancellation disappeared, GET the prior child by
its provider ID. If Graph returns a regular occurrence, replace that existing
child's raw snapshot and set `graph_exception_reset=true`; the decoder retains
its original identity as an exception override with the now-baseline values.
This retains refs while undoing the stale edit. It never creates a new regular
occurrence row. If it returns an edited/cancelled event, keep that evidence.
A 404 without enough original identity to reconstruct a baseline keeps the
master incomplete; a 403/401 fails the scan. Do not infer that disappearance
means cancellation or hard-delete historical evidence.

For singles/masters absent after a complete calendar replay, verify a calendar
GET by ID. Only 404 permits `deleted_from_source` marking; never local deletion.
Access failures, partial pages and limits leave flags unchanged. A reappearing
event clears only its source-deletion flag through the Store writer. Source
moves produce separate calendar copies; source scope still governs each copy.
Never enumerate or reconcile shared calendar evidence using owner sources.

## Review Focus

1. A moved all-day exception at the 2027-03-14 Los Angeles DST boundary keeps
   its original civil identity after the source zone changes. Task 1 tests it.
2. A removed exception or restored cancellation does not leave a stale override
   or a phantom nominal occurrence. Task 4 tests reset and incomplete states.
3. Graph UIDs that differ across series instances do not become a fabricated
   common UID; equal IDs in other calendar sources cannot override this series.
   Tasks 1 and 3 test this through decoding and scoped Store queries.
4. Opaque cancellation IDs, missing expanded pages and interrupted child writes
   never yield a seemingly complete recurring calendar. Tasks 2 and 4 test it.
5. New decoder registration after a completed T2 migration backfills preserved
   Graph raw without network access and a second open writes nothing. Task 3
   tests this on both dialects and includes cancellation/restart.

## Task 1: Graph decoder and local recurrence

**Files:** Create `internal/calevent/msgraph.go`, `msgraph_recurrence.go`,
`msgraph_timezone.go`, `msgraph_test.go`, `msgraph_recurrence_test.go`,
`msgraph_timezone_test.go`; add pinned CLDR XML and its Unicode license under
`internal/calevent/testdata/`. Reuse `mime.StripHTML` from
`internal/mime/parse.go`; it has no Store dependency. Do not copy its parser.

**Interfaces:** Consumes T2 `SourceContext`, `Decoder`, `Event`, `Attendee`,
`IsRoomAttendee`, `JoinKey`; T1 `Identity`, `ParseSet`, `Expand`.
Produces the registration `Register("msgraph_event_json", decodeMSGraphEvent)`
and these functions (`GraphTimeZone` is additive for the connector):

```go
func decodeMSGraphEvent(raw, metadata []byte, sc SourceContext) (Event, bool, error)
func GraphTimeZone(name string) (string, *time.Location, error)
func graphRecurrenceLines(raw []byte, start time.Time, allDay bool, zone *time.Location) ([]string, error)
```

The recurrence helper takes the raw `recurrence` sub-object; the decoder
converts its error into the unsupported property. `GraphTimeZone` returns
canonical name, location and error, and never imports Store.

- [ ] **Step 1: Add failing decoder/recurrence tests.** Use small raw JSON
  fixtures for `TestMSGraphSingleRolesRoomsAndBody`, `TestMSGraphTimedMovedException`,
  `TestMSGraphAllDayOriginalDateDST`, `TestMSGraphSparseCancellation`,
  `TestMSGraphOrdinaryOccurrenceSkipped`, `TestMSGraphOccurrenceReset`,
  `TestMSGraphNamespaceRequired`, `TestMSGraphUIDIsPerOccurrence`,
  `TestMSGraphDescriptionUTF8Cap`, `TestMSGraphUnsupportedKeepsSeries` and
  `TestMSGraphRecurrencePatternsAndRanges`. Include all six patterns, all three
  ranges, interval/count 0, Feb 29, final local end date, relative last weekday,
  multiple relative weekdays, unknown enums, incomplete-series metadata and
  unresolved cancellations. Pin representative assertions:

  ```go
  require.NoError(t, err)
  require.True(t, ok)
  assert.Equal(t, "msgraph", event.Provider)
  assert.Equal(t, "calendar-A", event.Namespace)
  assert.Equal(t, "room", event.Attendees[1].Role)
  assert.Equal(t, "2027-03-14", event.Original.Date) // moved all-day exception
  assert.Equal(t, "series-A", event.SeriesProviderID)
  assert.Equal(t, "accepted", event.CopySelfRSVP)
  assert.LessOrEqual(t, len(event.Description), 4096)
  assert.True(t, event.DescriptionTruncated)
  assert.Equal(t, []string{"RRULE:FREQ=WEEKLY;INTERVAL=1;BYDAY=TU;WKST=SU;COUNT=6"}, event.Recurrence)
  assert.Equal(t, 6, len(occurrences)) // six-week weekly fixture, before edits
  assert.ErrorIs(t, parseErr, calrecur.ErrUnsupportedRecurrence) // unsupported marker
  ```

  `TestMSGraphWindowsZones` pins Pacific Standard Time→America/Los_Angeles,
  UTC→UTC, IANA passthrough, fractional seconds, explicit offsets, and an
  unknown/custom zone error. Iterate every territory-001 CLDR mapping through
  `time.LoadLocation`, without networking. Unknown series zones still decode
  to a series with an unsupported marker; missing namespace returns an error.
- [ ] **Step 2: Run**
  `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec" ./internal/calevent -run 'MSGraph'`.
  Expected: FAIL because the raw format is unregistered or conversion is absent.
- [ ] **Step 3: Implement** these signatures and the mapping above.
  Embed the pinned mapping using `encoding/xml`/`go:embed`; no runtime download.
  Keep unsupported recurrence an Event with an error-producing line. Accept a
  versioned cancellation envelope only with valid source/master/original keys.
- [ ] **Step 4: Rerun** Step 2, then the complete `./internal/calevent` package
  with the same caps and tags. Expected: PASS, including unchanged Google tests.
- [ ] **Step 5: Format, vet, inspect diff/status and commit**
  `feat(calendar): decode archived Microsoft Graph events`.

## Task 2: Calendar auth and complete Graph retrieval

**Files:** Modify `internal/microsoft/graph_oauth.go` and its tests,
`internal/msgraph/client.go` and tests. Create `internal/mscalendar/client.go`,
`client_test.go`, `models.go`, `auth_contract_test.go`.

**Interfaces:** Consumes `msgraph.NewClient`, `GetRawLimited`, `TokenFunc`,
`GraphManager` auth/refresh and T1/T2 unchanged. Produces additive APIs:

```go
// package microsoft
func GraphCalendarScopes() []string
func NewGraphCalendarManager(clientID, tenantID, redirectURI, tokensDir string, logger *slog.Logger) *GraphManager
// package msgraph: preferences only, never bearer/header overrides
func (c *Client) GetRawLimitedWithPreferences(ctx context.Context, rawURL string, maxBytes int64, prefer []string) ([]byte, error)
// package mscalendar
const SourceType = "mscalendar"
const RawFormat = "msgraph_event_json"
type Mailbox struct { ID, Email, TimeZone string }
type Calendar struct { ID, Name, OwnerEmail string }
type EventPage struct { RawEvents [][]byte; NextLink string }
type SeriesDetail struct { Master []byte; Exceptions [][]byte; CancelledIDs []string; Complete bool }
type Client struct { graph *msgraph.Client }
type API interface {
    Mailbox(ctx context.Context) (Mailbox, error)
    Calendars(ctx context.Context) ([]Calendar, error)
    Events(ctx context.Context, calendarID, nextLink string) (EventPage, error)
    Series(ctx context.Context, calendarID, masterID string) (SeriesDetail, error)
    Event(ctx context.Context, calendarID, eventID string) ([]byte, error)
}
func NewClient(baseURL string, token msgraph.TokenFunc, qps float64) *Client
```

- [ ] **Step 1: Add failing tests** `TestGraphCalendarScopesAndTokenIsolation`,
  `TestGraphCalendarPersonalAndOrganizationMailbox`, `TestGraphCalendarStaleGrant`,
  `TestMSCalendarCompletePaging`, `TestMSCalendarSeriesAllExceptions`,
  `TestMSCalendarNestedPagingIncomplete`, `TestMSCalendarImmutableIDPreferences`,
  `TestMSCalendarHostAndByteCaps`, `TestMSCalendarContextAndGraphErrors`.
  Auth uses existing flow hooks; transport tests run the real client against
  `httptest.Server` because Graph's request/response contract cannot be
  exercised with live accounts. Assert calendar scopes include Calendars.Read
  and MailboxSettings.Read and exclude writes/mail/Teams. A stale grant returns
  the calendar reauth hint; token prefix is `mscalendar_`. Existing tokens are
  byte-for-byte unchanged. Server responses include opaque IDs with `/+=`,
  unknown JSON fields, two top-level/nested pages, missing relationship arrays,
  cross-host links, a repeated nextLink, 401/403/404/429 and oversized bodies.
  Assert exact raw preservation, all exception originals and cancellation IDs,
  missing pages → `Complete=false` or error, both preferences on every GET,
  no credential-bearing foreign request, `ctx.Err()` on cancellation.
- [ ] **Step 2: Run**
  `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec" ./internal/microsoft ./internal/msgraph ./internal/mscalendar -run 'Calendar|Preferences'`.
  Expected: FAIL for absent APIs/scopes/headers.
- [ ] **Step 3: Implement** the additive APIs. Do not mutate a shared client's
  headers; pass preference values per request, including retries and next links.
  Escape every path ID and build OData queries with `url.Values`. Cap each
  response at 64 MiB and each calendar traversal at 10000 pages, track visited
  links, and error on excess. Detail GET requests all fields used by Task 1;
  nested expansion follows its own next links. Require explicit relationship
  presence for `Complete`. Mailbox resolution reuses existing verified `/me`
  email rules and canonicalizes `/me/mailboxSettings/timeZone` through Task 1.
- [ ] **Step 4: Rerun** Step 2 and all three focused packages. Expected: PASS,
  including old mail/Teams/contacts transport and token-isolation tests.
- [ ] **Step 5: Format, vet, inspect and commit**
  `feat(calendar): read Microsoft mailbox calendars with existing Graph auth`.

## Task 3: Projection upgrade and scoped archived reads

**Files:** Modify `internal/store/migrations.go`, `store.go`,
`schema_version.go`, `schema_version_test.go`. Create
`internal/store/msgraph_calendar_projection_test.go`. Use existing
`migrate_calendar_event_projection.go` as the backfill implementation.

**Interfaces:** Consumes Task 1 registration and T2
`refreshCalendarEventProjectionWith`, `backfillCalendarEventProjectionsContext`,
`CalendarEventRow`, source-scoped snapshot queries, refs and alias helpers.
Produces new run-once key `msgraph_calendar_projection_v1`; no change to T2
signatures, event columns, indexes or `calendarEventProjectionVersion`.

- [ ] **Step 1: Confirm coordinator's archive-version allocation.** Without it,
  stop before editing this task. The new required migration is a version change
  even though this task adds no DDL. Do not reserve a numeric version here.
- [ ] **Step 2: Add failing tests** `TestMSGraphProjectionAtomicRefresh`,
  `TestMSGraphBackfillAfterCompletedCalendarV1`, `TestMSGraphBackfillRestart`,
  `TestMSGraphReopenNoWrites`, `TestMSGraphScopedCopiesAndMaster`,
  `TestMSGraphProjectionMutationsAndSubset`. Seed a temporary archive at the
  actual prior allocated schema using its real schema SQL, mark the T2 ledger
  complete, and insert synthetic compressed Graph raw only after that setup.
  Opening the upgraded store must project it without HTTP calls. Cancel after
  an observable committed row, reopen and require all rows/ledger completion.
  Instrument actual message/projection writes, not elapsed time, for reopen.
  Cover `PersistMessageContext`, raw replacement, metadata cancellation,
  ordinary-occurrence rejection, malformed recurrence, local/source deletion,
  hard-delete cascade, raw-copy dedup and subset rebuild. Include two sources
  with the same IDs but different event details; hidden source cannot affect
  returned rows or orphan→master alias resolution. Representative assertions:

  ```go
  assert.Equal(t, "msgraph", row.Event.Provider)
  assert.Equal(t, "calendar-A", row.Event.Namespace)
  assert.Equal(t, "exception", row.Event.Kind)
  assert.Equal(t, "2027-03-14", row.Event.Original.Date)
  assert.True(t, row.DeletedFromSource)
  assert.Len(t, scopedRows, 1)
  assert.Equal(t, "permitted-uid@example.com", scopedRows[0].Event.ICalUID)
  assert.Zero(t, projectionWritesOnSecondOpen)
  ```

- [ ] **Step 3: Run**
  `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec" ./internal/store -run 'MSGraph'`.
  Expected: FAIL because the completed T2 ledger bypasses new raw backfill.
- [ ] **Step 4: Register** the new run-once migration after T2's projection
  migration; call its existing resumable scanner. Keep Google rows write-free
  and decoder errors raw-only. Apply the allocated version and regenerate both
  dialect digests from real schema initialization.
- [ ] **Step 5: Rerun** Step 3 and focused `CalendarEvent|SchemaVersion|MSGraph`
  tests with the same flags. Run the same cases with `MSGVAULT_TEST_DB` pointing
  to a synthetic PostgreSQL database when available; otherwise PR PG CI must
  provide that proof. Expected: PASS and identical typed values/identity keys.
- [ ] **Step 6: Format, vet, inspect and commit**
  `feat(calendar): backfill Microsoft Graph event projections from raw`.

## Task 4: Idempotent calendar sync and safe reconciliation

**Files:** Create `internal/mscalendar/sync.go`, `persist.go`, `reconcile.go`,
`sync_test.go`, `persist_test.go`; add
`internal/store/msgraph_calendar_sync.go` and its tests for bounded archived
child/seen-ID retrieval and metadata/source-deletion mutation through Store.

**Interfaces:** Consumes Task 2 `API`, Task 1 `calevent.Decode`, Store
`PersistMessageContext(ctx, *MessagePersistData) (int64, error)` and context-aware
source, participant, conversation, sync-run and checkpoint operations. Produces:

```go
// package mscalendar
 type Options struct { AccountEmail string; Calendars []string; Limit int }
 type Result struct { CalendarsSynced, EventsAdded, EventsUpdated, UnexpandableSeries int }
 func NewSyncer(api API, st *store.Store, opts Options) *Syncer
 func (s *Syncer) Register(ctx context.Context) ([]Calendar, error)
 func (s *Syncer) Sync(ctx context.Context) (Result, error)
// package store: never perform SQL from the connector
 type GraphCalendarChild struct { MessageID int64; SourceMessageID, ProviderEventID string; Raw, Metadata []byte }
 func (s *Store) GraphCalendarChildrenContext(ctx context.Context, sourceID int64, masterID string) ([]GraphCalendarChild, error)
 func (s *Store) GraphCalendarMessagesContext(ctx context.Context, sourceID int64, afterID *int64, limit int) ([]GraphCalendarChild, error)
 func (s *Store) MergeGraphCalendarMetadataContext(ctx context.Context, sourceID, messageID int64, patch []byte) error
 func (s *Store) UpdateGraphCalendarSourceConfigContext(ctx context.Context, sourceID int64, configJSON string) error
```

The Store child helper is owner ingestion only, scoped to one source, provider
`msgraph`, namespace from that source, and master ID. It returns at most 20000
children (excess is an error) with direct raw lookups, never message bodies.
The message scanner pages by message ID, with nil meaning no lower bound,
and limit 1..100. The metadata merger checks source ownership, merges JSON
keys and refreshes the projection in one fenced transaction. The config helper
is the context-aware calendar-source equivalent of `UpdateSourceSyncConfig`,
preserving its attribution lock and mailbox-change refresh. Both use existing
Store transaction helpers. Use `MarkMessageDeletedContext` and
`ClearMessageDeletedFromSourceContext` for source flags; preserve local deletion.

- [ ] **Step 1: Add failing tests** `TestMSCalendarSourcesAndReplay`,
  `TestMSCalendarAtomicEvidence`, `TestMSCalendarResourceParticipantsExcluded`,
  `TestMSCalendarSparseCancellationAndOrphan`, `TestMSCalendarOpaqueCancellation`,
  `TestMSCalendarResetRemovedException`, `TestMSCalendarRestoreCancellation`,
  `TestMSCalendarIncompleteChildrenSuppressExpansion`,
  `TestMSCalendarAbsentEventRequires404`, `TestMSCalendarPartialNeverReconciles`,
  `TestMSCalendarCancelAndResume`, `TestMSCalendarScopeIsolation`.
  Drive the real HTTP client and syncer into temporary stores. Assert two
  calendars create two sources, repeats preserve message IDs, changed originals
  remain stable, master edits invalidate projection hashes, and canceled rows
  remain archived. Inject a failing persistence constraint to prove message,
  raw, metadata, recipients, FTS and typed rows roll back together. Assert zero
  room participants/recipients with a typed room attendee. A six-week synthetic
  series with one moved edit and one resolved cancellation expands to five
  live plus one canceled occurrence; source zone changes retain all-day keys.
  Removed edit returning `type=occurrence` restores baseline bounds on the same
  archived child; it does not add other occurrences. Opaque cancellation or
  child-write interruption leaves `recurrence_error` nonempty and no nominal
  expansion. A partial scan, nested error or 403 never marks events deleted.
  A verified 404 flags an archived single as deleted-from-source but readable;
  repeat delivery clears it. Assert replays preserve local deletion and user
  read state, and no requests use POST/PATCH/DELETE.
- [ ] **Step 2: Run**
  `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec" ./internal/mscalendar ./internal/store -run 'MSCalendar|GraphCalendar'`.
  Expected: FAIL for missing syncer/Store helper and persistence behavior.
- [ ] **Step 3: Implement** source registration, replay, child reconciliation and
  persistence as designed. Prepare human participants through context-aware
  Store helpers, then atomically persist each final snapshot with
  `PersistMessageContext` so calendar attribution locks apply. Keep source
  time zone canonical. Seed each calendar source's primary/confirmed owner
  identity through existing Store operations; shared copies remain excluded.
  Fence writes with `ScopedToSync`, use Start/Complete/FailSyncContext, and mark
  complete only after an unbounded scan succeeds. Checkpoints record counts,
  not a misleading Graph delta cursor. A limit or interruption returns an
  incomplete run; restart by replay. Avoid a new migration or projection
  version for sync bookkeeping.
- [ ] **Step 4: Rerun** Step 2, then the complete mscalendar package. Run focused
  Store parity cases in synthetic PostgreSQL or PR CI. Expected: PASS with
  atomic raw→typed refresh and replay/reset/cancellation behavior.
- [ ] **Step 5: Format, vet, inspect and commit**
  `feat(calendar): archive Microsoft calendar masters and exceptions`.

## Task 5: CLI, configuration and daemon scheduling

**Files:** Create `cmd/msgvault/cmd/mscalendar.go`, `mscalendar_test.go` and
`internal/config/mscalendar.go` with tests. Modify `internal/config/config.go`,
`cmd/msgvault/cmd/{help_layout,serve,sync_cache_refresh}.go` for command grouping,
job registration and cache-refresh classification,
`internal/api/{cli_handlers,scheduler_jobs}.go` and their tests,
`docs/{configuration,cli-reference}.md`, `docs/usage/calendar.md` and
`docs/guides/oauth-setup.md`. Inspect actual registration sites before
editing; keep implementation in the new connector files.

**Interfaces:** Consumes Task 2 manager/client, Task 4 Syncer, existing owner CLI
proxy and OAuth-preflighted mechanism. Produces `add-mscalendar <email>` and
`sync-mscalendar <name-or-email>`, plus:

```go
// package config
 type MSCalendarSource struct {
     Name, Email, Schedule string
     Calendars []string
     Enabled bool
 }
 // Config adds MSCalendar []MSCalendarSource `toml:"mscalendar"`
 func (c *Config) GetMSCalendarSource(name string) *MSCalendarSource
 func (c *Config) ScheduledMSCalendarSources() []MSCalendarSource
// package api
 func MSCalendarJobNameForAccountEmail(normalizedEmail string) string
```

`[[mscalendar]]` name defaults to normalized email. It shares `[microsoft]`
client/tenant/redirect settings. Add flags: `--tenant`, `--headless`, repeated
`--calendar`, `--no-default-identity`; sync flags: repeated `--calendar`,
`--limit`, and existing manual cache-refresh flags. Sync always replays, so do
not advertise `--full`, `--after`, `--before` or a delta cursor.

- [ ] **Step 1: Add failing tests** `TestMSCalendarCommandRouting`,
  `TestMSCalendarOAuthPreflightOnlyOnce`, `TestMSCalendarConfigDefaults`,
  `TestMSCalendarScheduledSourceJob`, `TestMSCalendarOwnerOnlyCLI`,
  `TestMSCalendarNoWriteFlags`, `TestMSCalendarManualSyncRefresh`.
  Run Cobra commands/real CLI HTTP handlers against synthetic state. Assert
  frontend authorization runs once before daemon proxy and daemon alone opens
  the archive; auth failure registers no sources. Delegated CLI caller cannot
  add/sync sources. Every source `<email>/<calendarID>` maps to the one job
  `mscalendar:<normalized-email>` even with a custom config Name. Disabled or
  unscheduled entries create no job; unscheduled enabled entries warn through
  the existing freshness pattern. Assert Google sources/flags/control routing
  are unchanged and live controls reject `mscalendar` sources. Config tests
  parse real TOML and reject duplicate names/emails, invalid cron and empty
  email. CLI input errors are usage errors, not background syncs.
- [ ] **Step 2: Run**, sequentially:
  `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec" ./internal/config -run 'MSCalendar'`;
  then the same flags with `./internal/api ./cmd/msgvault/cmd -run 'MSCalendar'`.
  Expected: FAIL for missing commands/config/job classification.
- [ ] **Step 3: Implement** commands with local constructors and daemon proxy,
  Microsoft browser/device-code preflight, and the hidden OAuth-preflighted
  flag. Add only these owner-only commands to the existing CLI run allowlist.
  Schedule account jobs through the existing scheduler, sync execution fencing,
  post-source-create migrations, interrupt handling and cache refresh. The
  seven live calendar control tools retain their existing contract. Document
  consent requirements, full replay cost, cancellation/relative-weekday
  limitations, source selection and archived `message.read` access as unreleased
  functionality; link the owning Microsoft setup guide.
- [ ] **Step 4: Rerun** Step 2 and focused Google-calendar and Microsoft-auth
  command tests. Expected: PASS. After T4/T6 integration, add scoped calendar
  HTTP/CLI/MCP fixtures using these synthetic Graph rows; owner RSVP, resource
  exclusion, moved exceptions, query caps and hidden-copy behavior must match
  the shared read contracts. Coordinate that integration without duplicating
  another workstream's readers or changing their signatures.
- [ ] **Step 5: Format, vet, lint, run the one serialized broad gate, inspect
  diff/status and scrub before commit/push.** Commit:
  `feat(calendar): add Microsoft calendar sync commands and scheduling`.

## Plan publication and implementation handoff

- [ ] Self-review each spec §8.1 item, all five Review Focus cases, task
  interfaces and v3.1 constraints against production source. Fix plan gaps
  inline. Check documentation source validation and `git diff --check`.
- [ ] Commit this plan as
  `docs(calendar): plan Microsoft Graph calendar ingestion` and push it on
  the latest T2 tip. Open the stacked draft PR with the same title. No Go
  build/test/lint is justified by this plan-only change.
- [ ] Report the exact plan path, HEAD, stack base and PR for independent
  GPT-6-Astra XHIGH review. Stop product work until the plan approval gate and
  coordinator approval both arrive. Task 3 also needs archive-version allocation.
- [ ] After implementation, report CI results or absent runs, leave the issue
  open for maintainer review and the PR draft. Do not deploy or access a live
  archive as part of review.
