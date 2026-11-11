# Understand call frequency

Import call observations and inspect how often you called each person. The
daemon keeps original evidence, explicit participant bindings and uncertain
fields. Synced Google Calendar events stay in the calendar archive; invitations never count as calls.
These commands require daemon API schema 5.2.0 or newer. Discord imports require schema 5.3.0 or newer.

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
| `discord` | One native type-3 CALL message. Account is the owner’s Discord user ID; bindings name roster members. |
| `discord_chat_exporter` | One exported `Call` message; no participant bindings are available. |
| `discord_archived` | An archive message ID referencing live Discord JSON; account and bindings follow `discord`. |
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
to 512 KiB. No importer opens protected databases, retrieves encryption keys,
changes connected account bindings, or requests additional platform permissions.

## Import Discord call evidence

These formats require a daemon with API schema 5.2.0 or newer. The existing guild-bot source does
not retrieve private call history. This importer uses stored or supplied JSON
and does not contact Discord or need a token, new scope or additional dependency.

To map an already archived CALL message, supply its **archive message ID**, the
owner's native Discord user ID, and optional exact peer-to-participant bindings:

```sh
printf '%s\n' '{"format":"discord_archived","account":"1","record":{"message_id":123},"bindings":{"2":42}}' | msgvault calls import -
```

Alternatively, wrap one supplied native message in a `discord` request:

```json
{
  "format": "discord",
  "account": "1",
  "record": {
    "id": "100", "channel_id": "200", "type": 3,
    "timestamp": "2026-10-01T00:00:00Z",
    "call": {"participants": ["1", "2"], "ended_timestamp": null}
  },
  "bindings": {"2": 42}
}
```

All IDs above are synthetic. Discord IDs must be nonzero decimal strings with
no leading zeros. Bindings name existing **participant IDs**, not person IDs.
Bind only users in the call roster. A peer who did not join cannot be bound.
Only the call roster proves joined attendance; authors, mentions and channel
membership do not. Owner absence stays unknown. Direction, outcome and talk
duration remain unknown even when the call has an end timestamp. A bound peer
therefore contributes an unknown count, never an answered count.

For an existing DiscordChatExporter JSON file, extract a single Call message:

```sh
jq '{format:"discord_chat_exporter",account:"1",record:(.messages | map(select(.type == "Call")) | first)}' export.json > discord-call.json
msgvault calls import discord-call.json
```

Exporter messages appear in `calls list`, but lack a participant roster and
produce no per-person summary row. Native and exporter representations retain
separate original evidence under stable representation IDs. Importing both
representations of one call produces two observations in `calls list`.
Reimporting an archived/native record replaces that native observation; it does
not create another call. Later changes to the original archived message do not
automatically update or withdraw its separately imported call evidence.

The documented Discord data package's message transcript lacks structured call
type and attendance fields. That lossy shape, whole export files and ZIP files
are not accepted as single call records. Calls are never inferred from prose.

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

Call observations do not create ordinary message-contact activity. The
per-person Store summary is available for relationship-tier integration;
this change does not alter tier scores.
