# PST Appointments Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans
> with superpowers:test-driven-development to implement this plan task-by-task.
> Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Preserve Outlook appointment objects and expose supported timed event
facts through the existing archive readers.

**Architecture:** Read appointment properties through the pinned PST parser's
property contexts. Preserve a versioned JSON envelope, then let the existing
transactional calendar projection decode it. The first slice projects timed
singles and cancellations, and visibly unexpandable timed series. All-day and
exception objects retain raw evidence pending the reader prerequisites below.

**Tech Stack:** Go, `github.com/mooijtech/go-pst/v6` v6.0.2, SQLite,
PostgreSQL, the existing `calevent` and `calrecur` packages, testify.

**Spec:** [#1200](https://github.com/kenn-io/msgvault/issues/1200), calendar
events and meetings design v3 §8.5, and Wave 1 plan v3.1 (including its binding
amendments). Those coordination documents remain outside this repository.
This plan records the PST-specific decisions needed to implement that roadmap.

**Status:** Proposed, unimplemented. Base: T2
`be6070debdd44093e4e52feced82b8089bb2fa1d`, stacked on #1248. The
central GPT-6-Astra XHIGH review and coordinator approval are required before
product code. An internal preflight review is advisory and grants no approval.
Publishing this plan does not establish approval.

## Global Constraints

- Work only on the assigned PST branch and worktree. Preserve unrelated work.
- Consume the Wave 1 Produces interfaces exactly. `calevent` never imports
  `store`. An interface conflict requires coordinator review before continuing.
- Only `calendar_event` messages project. No email eligibility changes here.
- Every source copy keeps its message row and raw data. Typed tables are rebuilt
  from raw data and metadata in the same transaction.
- `messages.metadata` keeps its current fields. No stored occurrences, stored
  automatic links, provider writes, or new archive-read grants.
- SQLite and PostgreSQL remain behaviorally equivalent. Route SQL through
  Store. No `SELECT DISTINCT` with JOINs; no body scans in list queries.
- Read queries use the current snapshot and `SourceScope`. Preserve v3.1's
  28-hour candidate margin, 2000-series and 20000-exception caps, and
  `ErrQueryTooBroad`; never return a partial page across those caps.
- Synthetic fixtures only, with reserved `example.com` / `example.org`
  identities. Do not inspect a user's PST or add content from existing binary
  fixtures whose synthetic provenance has not been established.
- Every local Go command is prefixed `GOMAXPROCS=2 GOFLAGS=-p=2` and uses
  `-tags "fts5 sqlite_vec"` when the command accepts build tags. Iterate with
  focused package tests, serialized on this host. Use testify directly,
  `require` for prerequisites and `assert` for independent checks, `(want, got)`.
- After Go changes, run `go fmt ./...` and `go vet ./...` before committing;
  run `make lint-ci` once before implementation push. At most one broad gate:
  `flock -w 300 /tmp/local-gate.lock env GOMAXPROCS=2 GOFLAGS=-p=2 make test`.
  If the lock stays busy for five minutes, push and rely on PR CI.
- No remote test service and never `roborev review`. Do not run Go checks for
  this Markdown-only stage. If the fork PR has no CI runs, report that and stop
  waiting. CI is the final implementation verification.
- Run the private-data scrub before every push. Publish one draft PR against
  `main`, through remote `fork`; begin its body `Part of #1200.` and add
  `Stacked on #1248.`. Describe benefit and review boundaries without command-run
  sections or private infrastructure identifiers.
- **Migration gate:** this slice introduces no DDL and no required data
  migration. If source inspection or review establishes either is necessary,
  stop for a coordinator allocation after the serialized Wave 1 schema chain.
  Do not assign a sibling `SchemaVersion` increment or guess a PostgreSQL
  digest. Decoder-only additions do not justify a global projection-version
  bump. A later required backfill needs its own allocated migration.

## Review Focus

1. Named properties use file-local IDs. Remapped IDs and equal numeric LIDs
   in different property sets must not populate the wrong event field (Task 1).
2. PST time values cross two epochs. A property reader's Unix nanoseconds
   must not receive a second FILETIME conversion (Tasks 1 and 2).
3. Unsupported recurrence and embedded exceptions must not appear as a
   confirmed single occurrence or a guessed complete agenda (Task 2).
4. All-day dates cross DST and zones. Missing authoring-zone evidence must
   not silently shift civil dates or assume UTC (Task 2).
5. Re-import, interruption and storage errors must preserve raw evidence,
   existing email behavior, and a checkpoint before any failed item (Task 3).

## What the source supports today

The following findings come from production source at the base above and the
locally cached, pinned dependency, rather than a live archive.

| Path | Verified behavior and consequence |
|---|---|
| `internal/pst/reader.go:ExtractMessage` | Accepts only `*properties.Message`. Appointment objects return nil. It exposes email headers, bodies and timestamps, without appointment facts. |
| `internal/importer/pst_import.go:ImportPst` | Skips those nil entries. Folder traversal, labels, checkpoints, execution ownership and batching already exist. Existing email raw is preserved on re-import. |
| `internal/pst/mime.go:BuildRFC5322` | Reconstructs email MIME. It does not preserve the PST container or arbitrary MAPI properties. An appointment needs a different raw format. |
| go-pst `pkg/message.go:GetMessage` | Recognizes exact `IPM.Appointment` plus several meeting-request classes as `*properties.Appointment`; other classes can fall back to `*properties.Message`. Class eligibility must be checked independently. |
| go-pst `pkg/properties/appointment.pb.go` | Declares scalar start/end, all-day, sequence, cancellation flags, recurrence flags, exception replace time, attendee display strings, organizer alias and location/join fields. It does not declare the binary recurrence, global-object-ID or timezone structures, common subject/body fields, or recipient rows. Getters alone cannot establish complete support. |
| go-pst `pkg/property_context.go:Populate` | Writes original property IDs to generated MessagePack keys. Named-property resolution is commented out. Use property contexts plus the file's name map, not generated appointment getters. |
| go-pst `pkg/name_to_id_map.go` | Exposes `File.NameToIDMap.GetPropertyID(lid, propertySet)`. Numeric names include the property-set identity in the lookup; string-name maps lose that distinction. This slice uses numeric names only. |
| go-pst `pkg/property_reader.go:GetDate` | Converts PT_SYSTIME to Unix nanoseconds. The existing email extraction helper expects FILETIME; do not reuse it for appointment reader results. Repairing email timestamps is outside this task. |
| `internal/store/calendar_event_projection.go` | Registered raw decoders feed the existing event and attendee tables in the message transaction. Unsupported raw clears the typed projection; malformed recurrence can retain a series with `recurrence_error`. |
| `internal/store/migrate_calendar_event_projection.go` | The existing one-time migration selects `calendar_event` messages. It cannot recover objects an older importer skipped, and its completed ledger does not rerun for a newly registered format. |

Synthetic JSON event examples in `internal/calevent/google_test.go` and
`internal/store/calendar_event_projection_test.go`, and the synthetic
`pstlib.Message` in `internal/pst/reader_internal_test.go`, establish the
existing test patterns. `support.pst` and `32-bit.pst` have no synthetic
provenance document in this tree; do not inspect their personal payloads or
derive new fixtures from them.

Microsoft documents [file-local named-property mapping](https://learn.microsoft.com/en-us/openspecs/office_file_formats/ms-pst/0d6b4781-92c5-4d49-b24b-b783557098d1),
[UTC appointment starts](https://learn.microsoft.com/en-us/office/client-developer/outlook/mapi/pidlidappointmentstartwhole-canonical-property),
[the explicit all-day flag](https://learn.microsoft.com/en-us/office/client-developer/outlook/mapi/pidlidappointmentsubtype-canonical-property),
[recurrence blobs and their exceptions](https://learn.microsoft.com/en-us/office/client-developer/outlook/mapi/pidlidappointmentrecur-canonical-property),
[cancellation flags](https://learn.microsoft.com/en-us/office/client-developer/outlook/mapi/pidlidappointmentstateflags-canonical-property),
and [GlobalObjectId-to-UID conversion](https://learn.microsoft.com/en-us/openspecs/exchange_server_protocols/ms-oxcical/ac8c1e68-de58-4230-a2e2-79aae754a055).
Use these contracts; do not infer a format from the generated getter names.

## Raw evidence and projection contract

### Eligibility and raw format

Import exact `IPM.Appointment` objects only in this slice, in any non-skipped
folder. Folder names are labels, not eligibility evidence. Meeting requests,
responses, cancellations in mail folders, custom appointment classes, contacts
and tasks retain their previous handling. Calendar objects themselves can carry
the cancellation flag; that is supported here. Email invite projection belongs
to its own workstream.

Preserve `raw_format = "pst_appointment_json"`, envelope `version = 1`, in
`message_raw`. The format is extracted evidence, not a byte-for-byte PST backup.
Keep the PST file if full container recovery is needed.

`internal/pstcal` owns the JSON format without importing the parser or Store:

```go
const RawFormat = "pst_appointment_json"
type Property struct {
    ID uint16                  // original file-local property ID
    Type uint16                // original MAPI type
    Set string                 // canonical GUID for named numeric properties
    LID uint32                 // zero for ordinary PidTag properties
    Data []byte                // exact value bytes, JSON base64
}
type RawAppointment struct {
    Version int
    ArchiveID, EntryID, FolderID, FolderPath, MessageClass string
    CalendarTimeZone string    // explicit IANA override, empty if absent
    Properties []Property      // sorted by (Set, LID, ID, Type)
}
func Marshal(raw RawAppointment) ([]byte, error)
func Unmarshal(raw []byte) (RawAppointment, error)
```

Use snake_case JSON tags, deterministic encoding, and explicit version rejection.
Preserve property presence, including zero values. The property collector reads
an allowlist: common class, subject, Unicode/plain/HTML body, creation and last
modification; appointment start/end, subtype, sequence, state/busy/response,
recurring/is-recurring/is-exception, exception replace time, location,
organizer alias, required/optional/resource display lists and NetShow URL;
binary GlobalObjectId, CleanGlobalObjectId, AppointmentRecur, TimeZoneStruct and
start/end/recur TimeZoneDefinition. Resolve named fields by numeric LID and
PSETID, never by hard-coded file-local property ID. Record actual bytes rather
than serializing generated protobuf structs. Unsupported properties, recipient
tables, RTF compression and attachments are outside this envelope's preservation
claim; no fabricated attachment records.

Use protocol LIDs, not generated `properties.csv` numbers: for example, that
file lists `appointment_start_whole` as 264205, outside a uint16 LID.
The canonical keys this collector uses are:

| Property set | Numeric IDs / LIDs and MAPI types |
|---|---|
| Ordinary PidTag | MessageClass `0x001A` string; Subject `0x0037` string; Body `0x1000` string; Html `0x1013` binary; CreationTime `0x3007` time; LastModificationTime `0x3008` time; MessageCodepage `0x3FFD` int32; InternetCodepage `0x3FDE` int32. |
| PSETID_Appointment `00062002-0000-0000-C000-000000000046` | Sequence `0x8201` int32; BusyStatus `0x8205` int32; Location `0x8208` string; StartWhole `0x820D` time; EndWhole `0x820E` time; SubType `0x8215` bool; Recur `0x8216` binary; StateFlags `0x8217` int32; ResponseStatus `0x8218` int32; Recurring `0x8223` bool; ExceptionReplaceTime `0x8228` time; TimeZoneStruct `0x8233` binary; OrganizerAlias `0x8243` string; NetShowUrl `0x8248` string; TimeZoneDefinitionStartDisplay `0x825E`, EndDisplay `0x825F`, Recur `0x8260`, all binary. |
| PSETID_Meeting `6ED8DA90-450B-101B-98DA-00AA003F1305` | GlobalObjectId `0x0003` binary; IsRecurring `0x0005` bool; RequiredAttendees `0x0006`, OptionalAttendees `0x0007`, ResourceAttendees `0x0008`, all string; IsException `0x000A` bool; CleanGlobalObjectId `0x0023` binary. |

Accept string types `0x001F` (Unicode) and `0x001E` (String8); int32 `0x0003`,
bool `0x000B`, time `0x0040` and binary `0x0102`. Prefer valid Unicode when
both string forms exist. Preserve original String8/HTML bytes and both codepage
properties. Decode ordinary String8 fields using MessageCodepage; use
InternetCodepage for body/HTML when present, falling back to MessageCodepage
for a String8 body when no InternetCodepage was saved,
through `golang.org/x/text/encoding` and the existing charset support. This slice
does not recover an inherited folder codepage: absent/unknown encoding permits
ASCII only. Non-ASCII values without a proven encoding keep the object raw-only
and report `text_encoding_unknown` instead of publishing corrupted event text.
Never use replacement characters or lossy decoding to supply an SMTP identity
or UID. Do not call the dependency's UTF-8-assuming `GetString8` for these fields.
Microsoft specifies [MessageCodepage and folder fallback](https://learn.microsoft.com/en-us/office/client-developer/outlook/mapi/pidtagmessagecodepage-canonical-property)
and [InternetCodepage for body/HTML](https://learn.microsoft.com/en-us/office/client-developer/outlook/mapi/pidtaginternetcodepage-canonical-property).

Bound each selected value to 16 MiB and total selected values to 32 MiB before
allocating their buffers; cap the envelope at 64 MiB and properties at 1024.
The effective envelope limit is also `PstImportOptions.MaxMessageBytes`.
Truncation is not success: report an item error and block checkpoint advance.
These bounds cover the new reads, not allocations the dependency made while
opening a message. Validate lengths, scalar type/size, duplicate keys,
GUIDs and version before projecting. Unknown numeric properties can be preserved
but never interpreted as a known field by positional coincidence.
Reject duplicate `(Set, LID, Type)` keys for named values and `(ID, Type)` for
ordinary values; Unicode and String8 variants can coexist and use the precedence
above. Two remapped IDs cannot define the same named value/type twice.

### Identity, updates and cancellations

Keep the existing source and `source_message_id = pst-<archiveID>-<EntryID>`.
`archiveID` remains the existing first-4-KiB fingerprint, including its limits:
it is neither a mailbox identity nor a full-file digest. Do not change email
dedup keys in this task.

Set `Provider = "pst"`; `Namespace` and `CalendarID` are
`pst:<archiveID>:<FolderID>`, where FolderID is the folder's node ID, not its
display path. This keeps series lookup within a source copy and folder even
when files share node numbers. Check identity fields against importer-supplied
values before storing raw.

For a validated GlobalObjectId, clear its instance-date bytes and use the
uppercase hexadecimal clean ID as `ProviderEventID = "goid:" + cleanHex` for
singles and masters. Top-level exceptions are raw-only in this first slice;
their later projection would append `"@" + Original.Key()` and set
`SeriesProviderID = "goid:" + cleanHex`, exactly the master's identifier.
Without a valid global ID, use `ProviderEventID = "entry:" + EntryID`, leaving
`ICalUID` and `SeriesProviderID` empty. Never claim a fuzzy master association.
Duplicate appointment objects stay separate messages. Unsupported series never
expand. Do not enable exception aliasing against duplicate masters in this slice.

Populate `ICalUID` only by the documented GlobalObjectId conversion: decode
and validate the length/header, export its UTF-8 third-party UID when the Data
prefix proves that form, otherwise hex-encode the validated ID with the
instance date cleared. A malformed ID supplies no UID. Do not use Internet
Message-ID, an Exchange distinguished name, or arbitrary bytes as a UID.

Re-import of the same item compares the deterministic envelope hash and
persists changed appointment snapshots; the email skip-and-header-repair branch
stays intact. A stored higher appointment sequence rejects a lower sequence;
equal sequences compare last-modification instants when both exist. A missing
version clock permits only identical replay or filling an absent raw snapshot;
a changed snapshot with incomparable clocks reports an item conflict rather
than silently overwriting evidence. Compare and write in the same locked Store
transaction. Do not order copies in different namespaces or replace one file's
messages with another file's objects.

Project `Sequence` from the appointment sequence, preserving missing-vs-zero
in raw. `AppointmentStateFlags & 0x00000004` means `Status = "cancelled"`.
Otherwise use `confirmed`; BusyStatus and a mailbox RSVP do not establish event
tentativeness. A cancellation retains the message and raw, with `HasBounds =
false` when its bounds are absent. Neither Deleted Items membership nor absence
from a later PST export proves cancellation or source deletion.

### Recurrence and exceptions

Binary MAPI recurrence translation and embedded exception extraction are
deferred. Preserve their binary properties now. A recurring flag or recurrence
blob makes the row `Kind = "series"`, with its own `SeriesProviderID` and the
reserved recurrence line `X-MSGVAULT-PST-UNSUPPORTED:binary-recurrence`.
The existing `calrecur.ParseSet` rejects that line, producing a nonempty
`recurrence_error` and no conservative end. This is deliberately an
unexpandable series, never a weekly rule reconstructed from display text or
an empty-recurrence single. Its raw first-instance bounds are not a series end.

A separately stored, top-level exception remains raw-only, with
`pst_projection_issue = "exception_projection_deferred"`. Its later projection
needs both a validated clean global ID and `ExceptionReplaceTime`, whose UTC
instant becomes timed `Original`; all-day originals require an explicit
authoring zone. Preserve effective start/end independently. Never publish a
fabricated standalone single. Embedded exception bodies,
attendee overrides and deleted-instance arrays cannot be recovered by this
slice's readers. They remain in the blob or original PST, and the series remains
unexpandable even when a top-level exception is present.

Supported events use existing `evt:<message-id>` refs. Do not change the ref
grammar or teach another provider's identifier parser to interpret PST IDs.
**Exception prerequisite:** `resolveOccurrenceRefAlias` currently picks the
first same-source master. Before enabling PST exceptions, the coordinator must
allocate ambiguity-safe resolution within `SourceScope`, with real Store tests
for duplicate conflicting masters and an ungranted conflicting master. Until
that behavior is agreed and verified, no PST exception rows or aliasing ship.

### Time, detail and attendees

Read raw PT_SYSTIME FILETIME bytes with a checked conversion. When testing the
upstream reader, check the error from `GetDate`, then use `time.Unix(0, value)`.
Never pass
Unix nanoseconds into `windowsFiletimeToTime`. Timed start/end are UTC, and
must be present with end strictly after start for a non-cancelled single.
Do not substitute creation, sent or import time as calendar bounds.

Add optional `PstImportOptions.CalendarTimeZone string` and CLI
`--calendar-time-zone <IANA zone>`. Validate it before starting import and
preserve it in each raw envelope. It is an explicit authoring-zone override,
not a source-wide rewrite. Binary timezone rules and Windows timezone labels
are preserved but not interpreted in this first slice.

Timed singles need no zone to retain exact UTC bounds. All-day objects remain
raw-only with `pst_projection_issue = "all_day_projection_deferred"`, including
when an override was provided. **All-day prerequisite:** T2's
`currentCalendarZone(ctx, r, sourceID)` reads only source configuration, so
filling `EventTimeZone` does not establish event-level zone precedence. Before
enabling projection, the coordinator must confirm that the final occurrence
reader honors each PST object's explicit authoring zone, tested with two PST
files in one source using different overrides and a later source-zone change.
Do not change the shared interface unilaterally.

The later all-day decoder must use the subtype flag and derive civil dates
from UTC bounds in that zone, require local midnight and increasing dates, and keep the
exclusive end date. Use civil-day arithmetic across DST. Without a usable zone,
archive the raw object and report `all_day_zone_required`, with no typed row.
An override captured today preserves evidence for that later decoding; it does
not enable all-day agenda results in this slice.

Subject/plain/HTML body come from ordinary PidTag fields. Typed `Description`
uses plain body only, capped at 4096 UTF-8 bytes with
`DescriptionTruncated`; raw retains the uncapped selected value. `Location`
comes from the appointment location. A usable HTTP(S) NetShow URL supplies
`JoinURL`, and shared `calevent.JoinKey` supplies its comparison key. Do not
mine arbitrary body links or treat a workspace URL as a join URL.

Use `net/mail` to parse explicitly addressed entries in the three appointment
attendee lists, with semicolon separation outside quoted display names. Names
without SMTP addresses and Exchange DNs remain raw evidence, not guessed email
addresses. Required and optional lists supply those roles; the resource list
supplies `room` through `IsRoomAttendee(email, true)`. OrganizerAlias must be a
valid SMTP address. Deduplicate equal normalized emails with room precedence,
then organizer, required and optional. All attendee RSVPs remain `unknown`:
ResponseStatus describes the mailbox copy, not each invitee. Set
`CopySelfRSVP` from that value when known; do not mark a guessed attendee self.
Create no people, participants or message-recipient rows from these lists in
this slice. Rooms remain queryable as typed attendees and never become people.

## Files and task interfaces

| Files | Responsibility |
|---|---|
| New `internal/pstcal/raw.go`, `raw_test.go` | Evidence envelope and validation, without parser/Store dependencies. |
| New `internal/pst/appointment.go`, `appointment_test.go` | Eligibility, named reads, byte limits and synthetic parser fixtures. |
| New `internal/calevent/pst.go`, `pst_test.go` | Pure envelope-to-event decoder, registered under the new raw format. |
| New `internal/store/pst_appointments.go`, `pst_appointments_test.go` | Atomic snapshot comparison and persistence using existing projections. |
| Modify `internal/importer/pst_import.go`; new `pst_appointments.go`, `pst_appointments_test.go` | Dispatch, appointment preparation, batching/counters and failure checkpoints. |
| Modify `cmd/msgvault/cmd/import_pst.go` and its tests | Optional zone flag, early validation and appointment-limit reporting. |
| Modify `docs/usage/importing.md`, `docs/cli-reference.md` | Capabilities, override usage and rerun/rebuild limits after implementation. |

### Task 1: Preserve bounded appointment evidence

**Consumes:** go-pst v6.0.2 `Message`, `PropertyContext`,
`NameToIDMap.GetPropertyID`, `PropertyReader`; existing folder node IDs and
archive fingerprint. Do not modify the dependency or existing email extractor.

**Produces:** the `pstcal` interfaces above, plus:

```go
func ExtractAppointment(ctx context.Context, msg *pstlib.Message,
    archiveID, folderID, folderPath, calendarZone string,
    maxBytes int64) (pstcal.RawAppointment, bool, error)
// bool=false: ineligible class. Eligible unreadable or oversized items error.
```

- [ ] Write failing `TestExtractAppointmentNamedPropertiesRemapped` and
  `TestExtractAppointmentPropertySetCollision`: build synthetic Heap-on-Node
  allocation bytes, real `PropertyContext` / `PropertyReader` and a numeric name
  map. Assert identical start/location after remapping file-local IDs; an equal
  LID in the wrong set never wins. Include original binary bytes in assertions.
- [ ] Add `TestExtractAppointmentClassEligibility`,
  `TestExtractAppointmentTimeEpoch`, `TestAppointmentRawRoundTrip`,
  `TestAppointmentRawLimitsAndInvalidLengths`, and
  `TestExtractAppointmentContextCancelled`. Pin exact 2027-03-02T17:00:00Z,
  explicit false/zero preservation, rejection of unsupported envelope versions,
  duplicate semantic keys, truncated FILETIME, and 16/32/64-MiB limits.
- [ ] Add `TestAppointmentString8Codepage`: synthetic Windows-1252 location
  bytes decode to `Café`; Unicode takes precedence; absent/unknown codepage
  preserves non-ASCII raw but supplies no guessed attendee address. HTML and
  String8 use their distinct codepage properties.
- [ ] Run `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec"
  ./internal/pstcal ./internal/pst -run 'Test.*Appointment'`. Observe missing
  extractor/format failures; after implementation expect PASS.
- [ ] Implement the format and collector. Use checked fixed-width reads for
  inline scalars (upstream `ReadAt` / `Size` dereference its heap reader), and
  size-checked heap reads for strings, times and blobs. Synthetic byte builders
  stay in test files and execute real parser readers, not fake command stubs.

### Task 2: Project the supported event facts

**Consumes:** Task 1 raw envelope; T2 `calevent.Event`, `Attendee`, `Decoder`,
`Register`, `IsRoomAttendee`, `JoinKey`; T1 `calrecur.Identity` and `ParseSet`.

**Produces:**

```go
func decodePSTAppointment(raw, metadata []byte,
    sc SourceContext) (Event, bool, error) // internal/calevent
// Registers pstcal.RawFormat. No changes to the shared Decoder signature.
```

- [ ] Write failing `TestPSTTimedSingleAndCancelled`,
  `TestPSTGlobalObjectIDAndUID`, `TestPSTExceptionProjectionDeferred`,
  `TestPSTBinaryRecurrenceUnexpandable`, `TestPSTAllDayProjectionDeferred`,
  `TestPSTAllDayEvidenceAcrossDST`, `TestPSTAttendeeRolesRoomsUnknownRSVP`,
  `TestPSTDescriptionAndJoinURL`, and `TestPSTMalformedBoundsKeepRaw`.
- [ ] Assert UTC 2027-03-02T17:00:00Z–18:00:00Z; cancel flag 4 with sparse
  bounds; original versus moved exception evidence with no typed exception;
  unrelated malformed GOID yields no UID; third-party UID `meeting@example.org`;
  no guessed RRULE from
  display text. A recurring row's reserved line must actually fail `ParseSet`.
  For America/Los_Angeles on 2027-03-14, preserve the override and instants
  08:00Z–07:00Z next day. Neither a present nor a missing zone enables all-day
  projection before its reader prerequisite. Assert room precedence, unknown
  per-attendee RSVP and UTF-8-safe description cap.
- [ ] Run `GOMAXPROCS=2 GOFLAGS=-p=2 go test -tags "fts5 sqlite_vec"
  ./internal/calevent -run 'TestPST'`. Observe decoder-missing failures,
  implement the decoder, then expect PASS, including Google decoder regressions
  in the focused package's full test run.

### Task 3: Persist appointments through the importer

**Consumes:** Tasks 1–2; existing `MessagePersistData`,
`PersistMessageContext`, projection refresh, sync ownership/checkpoints and
source-scoped snapshot readers. No T4/T5 interface changes.

**Produces:**

```go
func (s *Store) PersistPSTAppointmentContext(ctx context.Context,
    data *MessagePersistData) (messageID int64, changed bool, err error)
func persistPSTAppointment(ctx context.Context, st *store.Store,
    sourceID int64, labelIDs []int64, raw pstcal.RawAppointment) (bool, error)
// importer helper: true only for added/updated evidence; false for replay/stale.
// PstImportOptions gains CalendarTimeZone string; existing fields stay intact.
```

- [ ] Write failing `TestPSTAppointmentAtomicProjection`,
  `TestPSTAppointmentReplayUpdateAndStaleSequence`,
  `TestPSTAppointmentIncomparableUpdateConflicts`,
  `TestPSTAppointmentUnprojectedRawRetained`,
  `TestPSTAppointmentNamespaceAndScope`, and
  `TestPSTAppointmentProjectionFailureRollsBack`. Persist synthetic envelopes
  through real Store transactions; read typed rows and preserved raw. Reject
  lower sequence, apply newer comparable snapshots, preserve stable message IDs,
  and prove trigger-induced projection failure leaves no partial message/raw.
- [ ] Write failing `TestPSTAppointmentDispatchAndCheckpoint` against the
  production per-item dispatcher using synthetic parser property contexts and
  a real Store. Mix email, appointment, request and contact objects. Exercise
  a failed appointment before a later successful item and batch boundary;
  checkpoint must stay before the failure. Assert folder labels, isolated file
  namespaces, and unchanged email re-import/header repair behavior.
- [ ] Write failing `TestPSTAppointmentIteratorErrorBlocksCheckpoint`: a real
  synthetic parser failure before per-item dispatch, followed by a successful
  later folder, must not checkpoint beyond the failed folder/item. Cover both
  `GetMessageIterator` setup failure and `msgIter.Err()` after `Next` fails.
  Genuine iterator failures set sticky `checkpointBlocked` and `HardErrors`;
  an expected empty-folder result is not a failure. Assert the final sync is
  failed. This extends the current log-only paths because the parser can fail
  before the appointment extractor.
- [ ] Run focused failing cases separately with the command prefix and tags
  above for `./internal/store` and `./internal/importer`. Implement the atomic
  Store prepare/write path and importer dispatch, preserving the outer folder
  iterator, sync ownership and existing email ingestion hook. Then run those
  focused cases to PASS before package regression runs, one package at a time.
- [ ] Add zone-flag tests against the real CLI option parser, including invalid
  IANA input before sync start. Add `AppointmentsAdded`, `AppointmentsUpdated`,
  `AppointmentsSkipped` and `AppointmentsUnprojected` summary counters as subsets
  of the existing all-item message counters, preserving email-only totals.
  Preserve existing metadata and add `pst_projection_issue` for bounded reasons
  from decoder errors; show those reasons/counts in the CLI summary. Invalid raw
  envelopes fail before writing; supported raw with a projection limit stays
  archived even when no typed row can be produced. Unprojected raw is a
  successful archive write with a visible limit; extraction/storage/conflict
  errors block progress.
- [ ] Update the owning import guide and CLI reference with the supported
  appointment subset, original-PST retention, zone override, raw-only recurring
  behavior and `--no-resume` recovery for old skipped appointments.

### Task 4: Verify rebuild limits and publish the implementation

**Consumes:** Tasks 1–3 and the existing calendar projection backfill.
**Produces:** a reviewed implementation commit and updated draft PR.

- [ ] Write failing `TestPSTAppointmentRebuildFromRaw` and
  `TestPSTAppointmentOldImportCannotBackfillSkippedItems`. In a synthetic Store,
  delete only derived rows, invoke the existing backfill helper, and assert event,
  attendee and unexpandable-series facts rebuild from the new envelopes. A
  synthetic old email-only import gains no invented calendar rows. A second
  identical persistence writes no typed rows; test using real write rejection.
- [ ] Run focused Store tests RED then GREEN. Exercise the same cases with the
  documented PostgreSQL test configuration when available; otherwise report
  the missing backend proof and rely on its CI lane. No digest changes for this
  no-schema slice. No migration registration to revive the completed v1 ledger.
- [ ] Run required formatting, vet, lint and the single serialized broad gate
  within the host limits above; inspect `git diff` and `git status`, scrub the
  public diff/commit/PR body, and commit the logical implementation as
  `feat(pst): archive appointments as calendar events`.
- [ ] Push to `fork`, update the draft PR's benefit and boundaries, and inspect
  `gh pr checks`. Report exact HEAD and remaining limits. Leave the issue open
  for maintainer review; do not mark ready, merge, or change a live archive.

## Rebuild and delivery boundaries

An older PST import stored neither appointment raw nor appointment messages.
Recovery needs the original PST and a fresh traversal using `--no-resume`.
There is no database-only backfill for that lost input. Newly imported envelopes
project during persistence. Existing raw-bearing calendar rows can rebuild
through the current Store helper when a supported rebuild path invokes it;
this plan does not advertise a new rebuild CLI or a rerun of a completed ledger.

All-day and top-level exception projection wait for their explicit reader
prerequisites; central review must approve this bounded scope. Full binary
recurrence, embedded exceptions, structured recipient-table RSVPs, automatic
Windows-zone interpretation, custom classes and cross-export update arbitration
remain follow-ups. Supporting them must decode the preserved facts,
define any newly required migration and get its allocation before code changes.
No UI, live calendar control, transcript attendance, or relationship-count
changes belong to this workstream.

Plan commit and draft PR title:
`docs(calendar): plan PST appointment import and projection`.
After publishing this plan, product work remains stopped until the external
`W2e-PLAN-APPROVED` gate exists and the coordinator explicitly approves the
reviewed revision.
