# Understand call frequency

Import call observations and inspect how often you called each person. The
daemon keeps original evidence, explicit participant bindings and uncertain
fields. An invitation or conference link never counts as an answered call.
These commands require daemon API schema 3.24.0 or newer.

## Import a supplied record

`calls import` accepts one JSON request from a file or stdin. It uses the
selected local or remote daemon and owner authentication. Delegated agent
tokens cannot import or read call history.

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
| `signal_desktop` | One supplied `callsHistory` JSON row. Account identifies the Signal account. Bindings may name its exact direct peer ID. Group/Adhoc rows do not supply a joined roster, so participant bindings are rejected. |
| `google_calendar` | One expanded event JSON object. Account must be the stable **calendar ID**, keeping event IDs from different calendars separate. Bind attendee email addresses to archive participants; every binding stays invited even when the RSVP says accepted. Recurring masters and events without a start are rejected. |
| `normalized` | A version-1 observation with provider/account, record ID, kind, direction, outcome, owner participation, occurrence time, precision, participants and original `raw` JSON. Its account must equal the request account; outer bindings must be absent. See the [HTTP contract](../api-server.md#call-history). |

The request limit is 1 MiB; its record and original raw payload are each limited
to 512 KiB. An observation allows 100 participants and 32 phone/link entry
points. No importer opens protected databases, retrieves encryption keys,
changes connected account bindings, or requests additional platform permissions.

## Read the evidence and counts

All commands return JSON. `calls list` includes source/message/record IDs,
typed participant attendance and preserved scalar claims. It includes
withdrawn observations for inspection, but excludes archive/source-deleted
messages. Original provider JSON remains in the archive's original-message
storage, outside the list response.

`calls summary` ranks current durable people by answered count, then person ID.
Unbound participants remain visible in evidence but have no guessed person.
Multiple identities currently bound to one person count once per call.

| Field | Meaning |
| --- | --- |
| `answered` | One observation explicitly establishes answered status, owner joined and that person joined. Partial claims from separate observations cannot combine to earn credit. |
| `missed`, `declined`, `cancelled` | Explicit actual-call outcomes, separate from answered calls. |
| `unknown` | Associated call evidence lacks sufficient outcome or attendance information. |
| `conflicting` | Exact-identity observations disagree about an answer versus nonattendance, or report incompatible negative outcomes. |
| `scheduled` | Scheduled meeting observations, including cancelled invitations. |
| `scheduled_cancelled` | The cancelled subset of `scheduled`; it does not increase actual-call counts. |
| `example_message_id` | One associated observation to inspect, not proof that every counted event was answered. |

Deduplication requires the exact provider/account/native-call ID. Without that
ID, only reimport of the same source record replaces evidence; time proximity,
phone numbers and similar titles never merge calls. Group identities are
formed before the time window is applied, using the earliest active
observation. `--after` is inclusive and `--before` exclusive.

Native withdrawal prevents a stale active reimport of that observation.
Other observations of the call remain usable. Explicit archive deletion or
garbage collection removes the tombstone too; importing an old export afterward
can recreate it. Scheduled cancellation is an update, not a sticky withdrawal.

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
- **Calendar:** [conference entry points](https://developers.google.com/workspace/calendar/api/v3/reference/events)
  and `tel:` links remain scheduled evidence. HTTPS links on Meet, Teams,
  Cal.com and Zoom.us hosts are retained; custom hosts are not recognized.
  Date-only events use midnight UTC with day precision, not an invented call
  time. Date-time values require an RFC3339 offset. Calendar phone numbers
  are entry points, not person identity or attendance.
- **Slack:** [huddle history objects](https://docs.slack.dev/reference/objects/conversation-object/)
  can contain room timestamps and participant history. The
  [third-party Calls API](https://docs.slack.dev/apis/web-api/using-the-calls-api)
  is not a general historical call-log API. No automatic adapter is included.
- **Discord:** [private-channel call objects](https://docs.discord.com/developers/resources/message#message-call-object)
  describe participants and end time. The existing guild bot source does not
  establish access to private historical calls. No alternate account or client
  is used.
- **macOS:** Apple documents [Phone/FaceTime history synchronization](https://support.apple.com/en-nz/guide/icloud/mmc2662aa6e0/icloud).
  [CallKit](https://developer.apple.com/documentation/CallKit) describes call
  integration, not a general historical export API. Protected call databases
  and additional privacy permissions are outside this importer.

Call observations do not create ordinary message-contact activity. The
per-person Store summary is available for relationship-tier integration;
this change does not alter tier scores.
