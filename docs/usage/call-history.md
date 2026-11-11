# Understand call frequency

Import call observations and inspect how often you called each person. The
daemon keeps original evidence, explicit participant bindings and uncertain
fields. Synced Google Calendar events stay in the calendar archive; invitations never count as calls.
These commands require daemon API schema 5.2.0 or newer.

## Import a supplied record

`calls import` accepts one JSON request from a file or stdin; run it once per record. It uses the
selected local or remote daemon and owner authentication. The call import,
list, and summary routes are owner-only.

```sh
msgvault calls import observation.json
msgvault calls summary --after 2026-01-01T00:00:00Z
msgvault calls list --person-id 42 --limit 20
```

This synthetic Signal example binds a native direct-call peer to an **existing**
archive participant. Replace the account, record and binding with evidence you
are authorized to import; `42` is a participant ID here, not a person ID.

```json
{
  "format": "signal_desktop",
  "account": "example-signal-account",
  "record": {
    "callId": "example-call",
    "peerId": "example-peer",
    "mode": "Direct",
    "type": "Audio",
    "direction": "Outgoing",
    "status": "Accepted",
    "timestamp": 1790841600000,
    "endedTimestamp": null
  },
  "bindings": {"example-peer": 42}
}
```

Supported formats:

| Format | Record and binding contract |
| --- | --- |
| `signal_desktop` | One supplied `callsHistory` JSON row. Signal `Deleted` rows are rejected. Account identifies the Signal account. Bindings may name its exact direct peer ID. Group/Adhoc rows do not supply a joined roster, so participant bindings are rejected. |
| `normalized` | A version-1 observation with provider/account, record ID, direction, outcome, owner participation, occurrence time, participants and original `raw` JSON. Its account must equal the request account; outer bindings must be absent. Reimporting the same `record_id` replaces its evidence. See the example below. |

This normalized request binds the call to existing archive participant `42`. Timestamps use RFC3339; `ended_at` is optional.

```json
{
  "format": "normalized",
  "account": "example-signal-account",
  "record": {
    "version": 1,
    "provider": "signal",
    "account": "example-signal-account",
    "record_id": "example-call",
    "direction": "outgoing",
    "outcome": "answered",
    "owner_participation": "joined",
    "occurred_at": "2026-10-01T09:00:00Z",
    "ended_at": "2026-10-01T09:02:00Z",
    "duration_seconds": 120,
    "participants": [{"participant_id": 42, "participation": "joined"}],
    "raw": {"callId": "example-call", "status": "Accepted"}
  }
}
```

Allowed values: `direction`: `incoming`, `outgoing`, `unknown`; `outcome`: `answered`, `missed`, `declined`, `cancelled`, `unknown`; `owner_participation`: `joined`, `not_joined`, `unknown`; participant `participation`: `joined`, `invited`, `unknown`.

A normalized record may include `source_label` as a display name for its source. A nonempty label must contain non-whitespace text, contain no NUL, and be at most 2,048 bytes.
The first supplied label fills an empty source display name unless it conflicts with another source’s identifier or alias. Imports still succeed on a conflict, and reimports preserve an existing name.

The request limit is 1 MiB; its record and original raw payload are each limited
to 512 KiB. The supplied-record importer does not open protected databases or
retrieve encryption keys. Neither importer changes privacy settings, connected
account bindings, or OAuth scopes.

## Import macOS Phone and FaceTime history

Run the reader on the Mac with access to the call database. It sends each
observation to the selected local or remote daemon.

```sh
msgvault calls import-macos --account example-call-history
msgvault calls import-macos --account example-call-history --db /path/to/CallHistory.storedata
```

Reuse the same `--account` for later imports of this history. On macOS the
default path is `~/Library/Application Support/CallHistoryDB/CallHistory.storedata`.
If access is denied, grant Full Disk Access to the terminal or application in
System Settings, or use an accessible copy. A supplied copy needs its matching
`-wal` file, if present. The reader creates a private SQLite snapshot that includes
committed WAL data. Errors report how many records were imported; rerun to update
the same records.

Only explicit originated and answered flags establish direction and attendance.
An unanswered row does not distinguish missed, declined or cancelled calls, and
duration does not establish attendance. Names, addresses, call types, service
bundle IDs and group/device metadata remain in raw evidence. Retrieve that JSON
with the owner-authenticated `GET /api/v1/cli/message/raw?id=<message_id>` endpoint.

The reader does not infer archive participants from addresses or group IDs. To
attribute a call, import a normalized observation with the same `macos` provider
and account, a separate `record_id`, preserved evidence and raw JSON, and explicit
bindings to existing participants. Reimporting the macOS database updates the
reader’s record without replacing the separate manual observation. The archive
keeps these as separate observations and does not group or combine their claims.
Updating the reader’s own `record_id` instead is temporary: the next macOS
import replaces its evidence and participant bindings.

## Read the evidence and counts

All commands return JSON. `calls list` includes source/message/record IDs,
typed participant attendance and preserved scalar claims. It excludes source-deleted
and archive-deleted messages. Original provider JSON remains in the archive's original-message
storage, outside the list response.

Undated Google Voice calls are left out of `calls list` and `calls summary`.
Imported calls are searchable by their call text.

`calls summary` ranks current durable people by answered count, then person ID.
Unbound participants remain visible in evidence but have no guessed person.
Multiple identities currently bound to one person count once per call.

| Field | Meaning |
| --- | --- |
| `answered` | One observation explicitly establishes answered status, owner joined and that person joined. Partial claims from separate observations cannot combine to earn credit. |
| `missed`, `declined`, `cancelled` | Explicit actual-call outcomes, separate from answered calls. |
| `unknown` | Associated call evidence lacks sufficient outcome or attendance information. |
| `example_message_id` | One associated observation to inspect, not proof that every counted event was answered. |

Reimporting the same source record replaces its evidence. Time proximity,
phone numbers and similar titles never merge calls. `--after` is inclusive and
`--before` exclusive.

## Coverage and limitations

- **Beeper/hosted iMessage:** live call-record validation remains pending.
  No personal examples have been validated. The [documented message API](https://developers.beeper.com/desktop-api-reference/go/resources/messages/methods/list/)
  has no structured CALL type; automatic extraction is not enabled.
- **Signal:** the [bridge converter](https://github.com/mautrix/signal/blob/main/pkg/connector/handlesignal.go)
  emits incoming/end notices. Those notices alone establish neither answer nor
  talk duration. The native adapter follows [Signal's call dispositions](https://github.com/signalapp/Signal-Desktop/blob/main/ts/types/CallDisposition.std.ts):
  group `Accepted` does not imply `Joined`. Unknown enums stay unknown; end
  minus start never becomes talk duration. Epoch timestamps do not identify
  the original time zone.
- **Calendar:** synced events stay in the [calendar archive](calendar.md). Invitations never count as calls.
- **macOS:** Use the [client-side Phone and FaceTime reader](#import-macos-phone-and-facetime-history) with existing database access.

Call observations do not create ordinary message-contact activity. The
per-person Store summary is available for relationship-tier integration;
this change does not alter tier scores.
