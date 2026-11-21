---
last_edited: "2026-11-18"
title: People and Profiles
description: Find people across your archive, keep their details together, and understand your contact history.
---

Find someone across email, chats, and meetings, then keep their contact details
and the things you want to remember in one profile. Start in the Web UI's
**People** workspace to browse contacts already in your archive, then select
**Save** in a person's header. You can also use the
[TUI People browser](/docs/usage/tui/#people).

Google resource addresses not linked to a person stay out of observed people
regardless of `exclude_domain_patterns`, though their addresses remain
searchable.

| I want to… | Start here |
|---|---|
| Save someone you have never messaged | [Create a person](#create-a-person) |
| Save an observed contact's details or notes | [Promote a profile](#promote-a-durable-person) |
| See which conversations still need an answer | [Open loops](open-loops.md) |
| Remember what they recently shared | [Person briefs](/docs/usage/people-briefs/) |
| Review meeting time and recorded follow-ups | [Meeting activity](meetings.md#export-context-and-read-follow-ups) |
| Find files you exchanged | [Person files](#find-files-related-to-a-person) |
| Keep profile facts current from messages | [Profile automation](/docs/usage/people-automation/) |
| Look up public profile information | [External enrichment](/docs/usage/people-enrichment/) |
| Sync contacts with an address book | [CardDAV contacts](/docs/usage/people-carddav/) |
| Tell msgvault which accounts and aliases are mine | [Source identities](#discover-source-identities) |
| Review suggested identity links | [Identity match review](#review-identity-matches) |

Ordinary profile editing and identity discovery use your archive alone. Briefs,
automatic fact extraction, semantic person search, and external enrichment each
require separate provider configuration and consent, and you can edit saved
profiles without turning any of them on.

## Understand the records

- **Source identity.** An address or handle that belongs to you in one source.
  It lets msgvault recognize messages you sent.
- **Observed person.** A group of addresses and handles linked by archive
  evidence. Matching display names alone do not combine people.
- **Person profile.** A saved record with a stable person ID. Your curated
  details stay attached when archive links change.

Observed contacts use **participant IDs**, while saved profiles use
**person IDs**. Commands name the ID they require, and the two are not
interchangeable. Promoting an observed person creates a profile from it. You can
also create a profile directly or import one from a subscribed CardDAV address
book.

## Create a person

You can create a profile from the CLI for someone you have never messaged:

```bash
msgvault person create --name "Alex Example" \
  --email alex@example.com:work --phone '+12025550123:cell' \
  --org "Example Company" --title "Engineer" \
  --address "123 Example Street" --note "Met at a conference" --json
```

Only `--name` is required. Repeat `--email` and `--phone` for multiple contact
methods, and append `:type` to label a method such as `work`, `home`, or `cell`.
A type uses 1 to 64 letters, digits, or hyphens. Creation accepts up to 200
emails and 200 phones, and phone numbers must normalize to the international
`+<country-code><number>` E.164 format.

Passing `--org` creates a current, primary employment record. It reuses the
company that employment facts would match by name or alias, or creates a company when
none matches. If the name matches several companies, creation stops; create the
person without `--org`, then add the employment with
`msgvault employment add --person <id> --organization <id>`. Passing `--title`
requires `--org` and sets that employment's title.

Text limits are 256 characters for the name and organization, 280 for the
title, 1,000 for the address, and 10,000 for the note. The new person receives a
stable ID and a new vCard UID with no message participants, and all supplied
details are stored as user-curated data.

Creation refuses any email or phone already on a current person profile or an
observed participant cluster, and the error identifies the match. Use that saved
person instead, or run `msgvault person promote <participant-id>` for an
observed contact. To combine observed identities, use
[explicit identity linking](#review-identity-matches); creation never links them
silently.

Without `--publish`, the new person stays in your archive. Add `--publish` to
send the profile to your configured
[CardDAV write target](people-carddav.md#sync-and-publish-selected-people). If
publication fails, the person stays saved, and the command prints their ID along
with instructions to retry publication or review the preview.

API clients call `POST /api/v1/people/create` with `name`, optional `emails` and
`phones` arrays of `{ "value": "...", "type": "work" }`, and optional `org`,
`title`, `address`, and `note`. The optional `source` field defaults to `user`
and also accepts `enrichment`. CardDAV publishes an enrichment-created person
only after
[publication review](people-carddav.md#review-inferred-changes-before-they-are-exported).
A successful call returns the person with HTTP 201 and its ETag; invalid input
returns 400 and an existing contact returns 409. This endpoint does not publish;
publish separately through the CardDAV publication API. `POST /api/v1/people`
continues to promote observed participants.

The MCP `create_person` tool accepts the same creation fields except `source`.
Like `promote_person`, it requires both MCP writes and profile writes to be
enabled, and it is absent from the read-only catalog and from daemons older than
API schema 5.16.0. Because it records every value with enrichment provenance,
like other MCP profile writes, CardDAV publication requires review. It never
publishes.

People and Relationships count a meeting once for a person with a transcript,
and linked calendar events add no separate count. Filters count each meeting
with a matching transcript, and saved link changes take effect after the next
cache refresh. Calendar events without a transcript add no meeting count or
relationship signal.

Each saved profile also has a vCard UID. A new profile imported from a
subscribed CardDAV book adopts the card's UID when it is a UUID that no other
profile uses or has used, while existing profiles keep their UID. To look up a
profile by UID, run `msgvault person get --vcard-uid <uid>` with a current or
retired profile UID, or a UID from one of its bound CardDAV cards. JSON profile
reads include `vcard_uid` and `carddav_bindings`, which show the connection,
book, resource href, remote UID, and mapping status. UID lookup requires daemon
API schema 3.10.0 or newer.

## See a person's first archived appearance

Run `msgvault person get <person-id> --json` to see the earliest archived event
supported by a saved person's activity, including messages where they were
copied. Conversation rosters do not establish an appearance; a sender or
explicitly addressed person does, including meeting attendees. If only roster
evidence exists, no first appearance is shown. `first_seen_at` and
`first_seen_ref` describe that appearance, while `first_contact_at` and
interaction counts still describe direct contact. The HTTP contact-state route,
assembled profiles, MCP `get_person_profile`, and `person get --json` all carry
the same fields. `person get` includes them when daemon API schema 5.7.0 or
newer is available, and older daemons or failed history reads still return the
person. The Web UI and TUI do not yet show first appearance.

After an import, first appearance reflects new messages once the activity
projection catches up, and results marked `stale` are provisional. See
[activity configuration](/docs/configuration/#activity).

`first_contact_kind` describes the envelope evidence rather than a verified
introduction. `email_intro` is an inbound email, excluding meetings, with
exactly one third-party From address in the archived envelope matching the
recorded sender when present. You must be a recipient, the person must be in To
or Cc, and there can be at most four other non-owner recipients, counting linked
aliases once per saved person. Email with a relayed From address uses
`observed`, as does other evidence, and `introducer` identifies the sender of an
introduction email when known. These fields reflect archived evidence and
currently linked identities, not a claim about when you met offline.

## Review identity matches

msgvault can keep uncertain identity suggestions for you to review. Muesli sync
also supplies review-only suggestions when duplicate Contacts cards share an
attendee email and a proposed phone already exists in the archive; see
[duplicate Contacts review](/docs/usage/meetings/#review-duplicate-contacts-cards).
Inspect the endpoints, archive evidence, and blockers before deciding.

The archive-observation seeder skips email addresses seen on more than 20
participants because they create too many low-value pairs.

```bash
msgvault identity matches list
msgvault identity matches show <candidate-id>
msgvault identity matches accept <candidate-id> --review-token <token>
# or
msgvault identity matches reject <candidate-id> --review-token <token>
```

The token belongs to the evidence and endpoint snapshot you inspected. If
evidence changes before you submit, msgvault rejects the decision as stale; run
`show` again and use its current token. Accepting a candidate applies the
participant link in the same guarded operation. A link into one saved profile
extends its participant bindings, while a candidate spanning two different saved
profiles requires a separate person merge review.

In the Web UI, open **People → Filter → Possible duplicates**. A stale decision
refreshes the pair. Review the changed evidence before choosing **Merge** or
**Keep separate** again; your draft notes remain available.

Review notes are private profile data. Avoid putting unnecessary personal
details in them. For the matching CLI and token-guarded API, see the
[CLI reference](/docs/cli-reference/#identity-matches) and
[API reference](/docs/api-server/#identity-match-review-and-scoring).

## Optional identity scoring

Manual scoring creates identity review suggestions and records judgments. It
requires a provider key, a retention declaration, and consent to the exact
current disclosure. Before sending eligible pairs to Typesafe Jev at the fixed
endpoint in [configuration](../configuration.md#people-identity-scoring),
msgvault checks local blockers. A score only proposes an action; you still
review the candidate's evidence and token before deciding.

The provider receives raw identity data for two participants: display names,
email addresses, phone numbers, and up to eight identifiers each, including
email, phone, Beeper, Apple ID, WhatsApp, or iMessage values with service and
scope fields. Evidence includes identity classes, matching values, and source
counts. Run `status` to see the exact field limits. Message bodies and freeform
notes stay outside this packet.

Start by inspecting readiness along with the disclosed endpoint, model, packet
schema, and retention declaration:

```bash
msgvault person scoring status
msgvault person scoring consent <disclosure-fingerprint>
msgvault person scoring run --limit 20
msgvault person scoring history --limit 20
```

Consent applies only while that disclosure fingerprint remains current. To
withdraw consent while leaving the configuration enabled, run
`msgvault person scoring revoke <disclosure-fingerprint>`. Each provider attempt
has a 15-second timeout. Revocation waits for requests the daemon already
admitted, and later attempts recheck consent. Provider requests do not hold the
archive's writer gate or a database write transaction.

Scoring records redacted judgments, and provider payloads and responses are not
stored in the judgment history. See
[identity scoring configuration](/docs/configuration/#people-identity-scoring)
and the [API reference](/docs/api-server/#identity-match-review-and-scoring)
for exact settings and routes.

## Promote a durable person

The **Saved** filter starts empty until you save profiles or import contacts. To
save a contact already observed in your archive from the Web UI, open
**People**, select the person, and choose **Save** in the header. The new
profile starts with the first nonblank observed name in the linked cluster,
ordered by participant ID; a contact without an observed name keeps the
`Person N` fallback.

From the CLI, pass the contact's participant ID. Replace `42` with that
participant ID and `7` with the person ID returned by promotion:

```bash
msgvault person promote 42
msgvault person list
msgvault person get 7
msgvault person set-display-name 7 "Alex Example"
```

You can edit or clear this saved name with `set-display-name`, and repeating
promotion returns the same profile while preserving an edited or cleared name.
Archive observation alone does not promote people. Sources that assert a stable
identity link addresses automatically: Beeper user IDs, Notion user IDs, Apple
Contacts cards for Muesli attendees, and person `id` values in meeting imports.
See [how meetings connect to people](/docs/usage/meetings/#how-meetings-connect-to-people).

Linking another cluster into a promoted one expands that profile's participant
bindings, whereas linking two clusters that already belong to different profiles
reports a conflict instead of silently merging curated data. Unlinking evidence
does not move or delete profile bindings.

Your display-name override also appears in analytics, search results, and
exports, while original addresses and message content stay available. To clear
the override, pass `--clear`:

```bash
msgvault person set-display-name 7 --clear
```

`person delete` is permanent. It removes the profile bindings and retires the
vCard UID forever; promoting the same observed cluster later creates a new
person and UID.

### Browse people by last contact

Drafts and unsent messages do not set last contact, cadence, or relationship
temperature, and they stay out of the relationship calendar, relationship list,
person and domain timelines, and chat inbox summaries. Explore, search, and the
Files view still include them for lookup. A person's or domain's Files tab,
`person files`, and MCP `search_person_files` list only files on sent or
received mail. A contact you have only drafted to reads as existing with no sent
or received messages and no relationship calendar, while a person ID that does
not exist reads as not found.

Cached views update after a cache refresh. Full checks at startup and during
explicit cache builds detect sent-state changes. Scheduled post-sync refreshes
check indexed changes first. If those already require a build,
`min_rebuild_interval` can delay both the full sent-state check and the visible
update. If indexed state is clean, msgvault checks sent state before applying
that interval. Ordinary requests use a cache marker check and do not scan the
archive for sent-state changes.

To browse promoted people with most recent contact first, run
`person directory`. It queries the selected daemon and returns a page of up to
50 people by default:

```bash
msgvault person directory --last-contact-after 2026-06-01
msgvault person directory --sort last_contact_asc
msgvault person directory --sort name
msgvault person directory --query "Alex Example"
msgvault person directory --address alex@example.test
msgvault person directory --last-contact-after 2026-06-01 --last-contact-before 2026-07-01 --json
```

`--query` runs the Directory's lexical search over current curated names,
contact points, and organizations, and works without semantic search.
`--address` matches a literal, case-insensitive substring of addresses and
identifiers on the person's currently bound participants or current curated
contact points, with `%`, `_`, and backslash treated as ordinary characters.
Superseded contact points and observed participants without a saved person are
excluded, and a cleared saved display name stays cleared without observed names
replacing it.

You can combine flags to narrow results, for example
`person directory --query Alex --address example.test`. Address filtering
requires an up-to-date daemon; the CLI reports an upgrade error if the selected
daemon cannot apply it. Lookup does not create profiles or change identity
links.

Address-only lookups can scan all saved people when matches are sparse or
absent, and a remote PostgreSQL database can make those database round trips
slower. When you know a name or organization, combine `--query` with `--address`
to narrow the candidates first.

`--last-contact-after` and `--last-contact-before` are inclusive and accept
`YYYY-MM-DD` dates or RFC3339 timestamps with offsets and fractional seconds. A
date alone means midnight UTC, so an upper bound of `2026-07-01` includes the
start of that day. The default sort is `last_contact_desc`; pass
`last_contact_asc` for oldest contact first or `name` for name order.

The table shows ID, display name, and last contact in UTC, with `-` for missing
names and timestamps. JSON returns the Directory envelope with a `people` array
and optional `next_cursor`, keeping the daemon's fields and omitting
`last_contact_at` when no timestamp exists.

When output includes `Next cursor`, pass it unchanged with the same query,
address, date bounds, and sort to read the next page; for JSON, use the
`next_cursor` value:

```bash
msgvault person directory --last-contact-after 2026-06-01 --cursor "<next_cursor>"
```

`person list` continues to show the full unpaginated profile collection, and its
`--json` output remains an array of profiles.

### Keep in touch with contact lists

A person's contact list is their `contact_frequency` attribute, an interval in
days. Lists A, B, C, and D correspond to 21, 60, 180, and 365 days, while any
other interval stays a custom list. Set a list with `person attributes set`,
where `--list` writes the matching interval through the same audited attribute
writer:

```bash
msgvault person attributes set 7 contact_frequency --list A
msgvault person attributes set 7 contact_frequency --value 45
```

The Directory derives each person's due date from their last contact plus the
interval and stores nothing else. Filter and order the roster by that cadence to
get a reach-out queue:

```bash
msgvault person directory --cadence overdue --sort most_overdue
msgvault person directory --cadence due --due-within-days 7 --sort next_due
msgvault person directory --cadence no_list
```

| `--cadence` | Matches |
|---|---|
| `overdue` | Due date has passed |
| `due` | Due on or before the end of the UTC date `--due-within-days` days from today (0 to 3660) |
| `no_list` | No contact list |
| `unknown` | Has a list but no trustworthy due date: no last contact yet, or contact state is still being recomputed |

Both `most_overdue` and `next_due` put the earliest due date first, with people
without a due date last. Cadence filters combine with the other Directory
filters, and each page carries the presets and a coverage count of the matching
people before the cadence filter. A stale contact projection never produces a
due date, so a person whose contact state is still being recomputed reads as
`unknown` until it settles. Cadence filters and sorts require daemon API schema
5.18.0; the CLI reports an upgrade error for older daemons.

MCP clients read the same queue through `list_directory_people` and set lists
with `set_person_contact_list`. Signal tiers never assign a list automatically.

## Keep private notes

In **People → Details**, select **More fields** to add notes, pronunciation, and
custom fields. You can manage field definitions, connection types, and
organizations in **Settings → People**, and open **More actions → History** in
the header to see replaced values and merge history.

Use the Notes field to save context in your own words. It preserves line breaks
and keeps earlier values in attribute history:

```bash
msgvault person notes get 7
msgvault person notes set 7 --text "Met at the community workshop."
msgvault person notes append 7 --text "Ask how the garden project is going."
msgvault person notes set 7 --text @person-notes.txt
```

Pass `--text -` to read from standard input. Appending adds a new line
atomically, so two concurrent appends do not overwrite each other. When
replacing text in an automated workflow, pass `set --expected-value-id <id>` to
reject a concurrent change. Notes are marked sensitive and excluded from
semantic person search and MCP's general `get_person_profile` response, though
the separate MCP `get_person_notes` tool can read them. If you publish a profile
to CardDAV, Notes map to the vCard `NOTE` property.

## Record employment and relationships

Organizations have their own profiles. Employment connects a person to an
organization and can store a title, role, department, location, and dates:

```bash
msgvault organization create "Example Cooperative" --domain example.com
msgvault employment add --person 7 --organization 3 --title "Engineer" --start 2024-03
msgvault employment list --person 7
msgvault employment end 9 --end 2026-08
```

Use the IDs returned by your commands. Run `employment set-primary` to choose
the primary current job; ending a job keeps its history. Organization commands
also support profile edits, custom attributes, retirement, and merging
duplicates, and `organization show 3 --history` includes superseded profile
rows.

Relationships connect two saved people and have a direction. The forward label
reads "source person is the ___ of target person." List the available types
before choosing one:

```bash
msgvault relationship-type list
msgvault person relationship add 7 parent 12 --from 2020
msgvault person relationship list 7 --include-ended
```

The same stored relationship appears from both people's perspectives with the
matching forward or reverse label. Dates accept `YYYY`, `YYYY-MM`, or
`YYYY-MM-DD`. Run `person relationship end <relationship-id> <until-date>` to
keep an ended relationship in history. You can add your own types with
`relationship-type create`; symmetric types require identical forward and
reverse labels.

## Find a person by what you remember

Directory name and identity searches work without an embedding provider. To
search by a description such as "people who enjoy gardening", enable
[semantic people search](/docs/configuration/#vectorpeople), grant its separate
consent, and build the embeddings:

```bash
msgvault person provider status --semantic-embeddings
msgvault person provider consent --semantic-embeddings --yes
msgvault daemon restart
msgvault embeddings build
msgvault person search "people who enjoy gardening" --limit 10
```

This searches saved profiles across current names, categories, locations,
employment, relationships, and eligible searchable attributes. It does not
search your messages or private Notes, and sensitive attributes and `how_we_met`
are excluded. Search text and curated person documents go to the configured
embedding endpoint. `--limit` defaults to 20 and accepts 1 to 100, and `--json`
includes person IDs and scores.

## Find files related to a person

Pass a saved person ID to find attachments across their linked identities:

```bash
msgvault person files 7 --direction from_person --filename proposal
msgvault person files 7 --mime-family pdf --after 2026-01-01
msgvault media search "project proposal" --person 7 --direction from_person
msgvault person files 7 --lane visual --query "garden plan"
msgvault person files 7 --lane all --query "project proposal" --json
```

The default `metadata` lane lists archived attachments on sent or received mail
without semantic search. `--direction` is repeatable and accepts `from_person`,
`to_person`, and `group`. `--filename` matches a case-insensitive substring, and
MIME families are `image`, `pdf`, `audio`, `video`, `text`, `document`,
`archive`, and `other`.

The `visual` lane needs [visual search](/docs/usage/vector-search/). Both the
`visual` lane and the `all` lane require `--query`. The `all` result reports
each lane separately, including unavailable lanes, and its metadata results do
not use the semantic query. To search document text through Docbank, run
`media search --person <id>`.

Each lane has its own cursor. Pass `--cursor` with one lane to continue a
result; `all` cannot take a cursor. `--limit` applies per lane, defaults to 100,
and accepts 1 to 100. `--after` is inclusive and `--before` is exclusive, and
both accept a date or RFC3339 timestamp.

## Catch up before your next conversation

[Person briefs](/docs/usage/people-briefs/) summarize recent chat and text
messages with citations. The guide covers enrollment, generation, reading in
**People** **Details** or the TUI, refresh timing, and saved versions.
Email-only contacts cannot receive a brief yet.

## Keep a person’s follow-ups in view

Enable the [Kata connection](../configuration.md#integrationskata) to see live
todos on a person's **Details** tab in **People**. You can create or unlink
tasks there; to link existing tasks, use the
[CLI](../cli-reference.md#person-agenda) or API. Editing and completing tasks
happens in Kata. Tasks stay in Kata, and this connection requires Kata API
schema 0.21.0 or later.

## Configure provider-backed person sweeps

[Profile automation](/docs/usage/people-automation/) maintains facts for people
you choose to track. Follow that guide to check and consent to a provider,
change its policy, recover stale consent after an upgrade, and inspect or pin
automatic facts. [External enrichment](/docs/usage/people-enrichment/) has a
separate setup for looking up public information.

## Merge duplicate profiles and reverse a merge

Merge two durable profiles only after reviewing both people. The first person
survives with the same ID and vCard UID, the second person's participants and
profile data move to it, and the retired UID becomes an alias. Pass both current
revisions and an idempotency key:

```bash
msgvault person merge 7 12 \
  --survivor-revision 4 \
  --absorbed-revision 2 \
  --idempotency-key merge-7-12
```

Conflicting single-value attributes stay reviewable rather than being dropped.
Inspect the merge and decide each candidate explicitly:

```bash
msgvault person merge-history 7
msgvault person merge-show 42
msgvault person merge-show 42 --snapshot
msgvault person merge-candidate 18 \
  --person-id 7 --revision 5 --decision accepted
```

A split creates a new person and a new vCard UID. Select absorbed participant
lineage with repeated `--participant` flags, and omit `--participant` only when
the absorbed profile had no participants:

```bash
msgvault person split 7 \
  --merge-id 42 \
  --participant 91 \
  --revision 5 \
  --idempotency-key split-42-91
```

An exact reversal restores the two pre-merge profiles when their lineage and
dependencies are still intact. A partial split moves participant-attributable
data instead of guessing; pass `--json` to inspect ambiguous or unrestored rows.
An active merge prevents deletion of its current person; complete the split
first.

Profiles with an active CardDAV publication cannot be merged. This keeps a local
merge from silently reassigning a UID that an external address book is already
syncing.

Merge snapshots are durable audit data that retain both profiles' merge-time
values after later live-profile edits or redaction. A subset copies a complete
merge packet only with `--include-attributes`, `--include-profiles`, and
`--include-vcard-resources`; treat that output as containing historical
personal data.

## Store typed attributes

Every archive starts with the same seeded person-field catalog. These
definitions are system-owned and cannot be deleted, though you can edit their
labels, descriptions, and display order. Sensitive fields are not searchable and
stay out of provider inference unless a provider profile opts in. The seeded
descriptive fields, including `ask_me_about` and `how_we_met`, accept up to 280
characters per value, while the private `notes` field is a separate multiline
text field. Older 120-character descriptive-field limits widen on the next store
open.

| Slug | Label | Type | Cardinality | Sensitive | Behavior |
|---|---|---|---|---|---|
| `primary_channel` | Primary channel | text choice | single | no | Writable: email, phone, SMS, chat, or in person |
| `contact_frequency` | Contact frequency | integer days | single | no | Writable |
| `ask_me_about` | Ask me about | text | multiple | no | Writable and searchable |
| `last_contacted` | Last contacted | timestamp | single | no | Read-only derived field; it remains empty until its producer supplies a value |
| `notes` | Notes | text | single | yes | Private free-form notes; maps to the vCard `NOTE` property |
| `location` | Location | text | single | no | Where this person lives or is based |
| `birthplace` | Born in | text | single | no | Where this person was born |
| `membership` | Membership | text | multiple | no | Groups and communities |
| `religion` | Religion | text | single | yes | Religious identity or affiliation |
| `politics` | Politics | text | single | yes | Political views or affiliation |
| `personality` | Personality | text | multiple | yes | Traits and working style |
| `family_pets` | Pets | text | multiple | no | Pets in this person's family |
| `interests_fun_now` | Fun now | text | multiple | no | Activities this person enjoys now |
| `interests_fun_growing_up` | Fun growing up | text | multiple | no | Activities this person enjoyed growing up |
| `favorites_food` | Favorite food | text | multiple | no | Foods this person especially likes |
| `favorites_place` | Favorite place | text | multiple | no | Places this person especially likes |
| `how_we_met` | How we met | text | single | no | How you and this person first met; the one seeded field about your relationship rather than the person alone. Not searchable, so it stays out of the semantic person document |

Contact points, addresses, dates such as birthdays, categories, organizations
and employment, and typed relationships such as partner or child are structured
records rather than attributes. Manage them with `msgvault person`,
`msgvault organization`, `msgvault employment`, and `msgvault person
relationship`, or through the `/api/v1/people` routes in the OpenAPI contract.

List fields and values, set scalar values, and keep superseded history:

```bash
msgvault attribute-definition list --object-type person
msgvault person attributes list 7
msgvault person attributes set 7 primary_channel --value email
msgvault person attributes set 7 ask_me_about --ordinal 0 --value "release engineering"
msgvault person attributes set 7 ask_me_about --ordinal 1 --value databases
msgvault person attributes list 7 --history
```

Setting a value supersedes the current value at the same slug and ordinal
without overwriting history, and `person attributes clear` closes the current
value while keeping it in history. Pass `--dry-run` to validate a set or clear,
and `--expected-value-id` for compare-and-swap protection in automated updates.

Scalar `--value` input handles text, integer, real, boolean, date, and timestamp
definitions, while structured record and JSON values use `--value-json` with
inline JSON, `@path`, or `-` for standard input.

## Create a portable field definition

Custom fields are metadata rows rather than runtime database migrations. Their
universal IDs and slugs are stable, while labels and descriptions can change.
Validate a definition locally before creating it:

```bash
msgvault attribute-definition create --dry-run --definition '{
  "object_type": "person",
  "slug": "favorite_project",
  "label": "Favorite project",
  "value_type": "text",
  "field_type": "text",
  "cardinality": "single",
  "is_searchable": true,
  "is_audited": true
}'

msgvault attribute-definition create --definition @favorite-project.json
```

Run `attribute-definition rename` to change presentation metadata without
changing stored references. Deletion is limited to user-created definitions that
have no stored values; shipped and non-deletable definitions are protected.

For automation, run `msgvault openapi` or read `/openapi.json` from the daemon.
The contract includes source identities, person profiles, attribute definitions,
and historized person-attribute routes, and the generated Go client exposes the
same operations.

## Discover source identities

Full and incremental email sync enrich identities already confirmed for the
source with strong sender evidence from trusted Sent metadata. Sync does not
confirm first-time aliases; review them with `msgvault identity discover` and
apply strong candidates with `msgvault identity discover --apply`. Recipient-only
evidence stays a review candidate. Receiving mail at an address does not by
itself prove that the address is yours.

Preview all evidence for one source without changing the archive:

```bash
msgvault list-accounts
msgvault identity discover --source-id 14
```

Use the numeric source ID when account identifiers or display names are not
unique. After reviewing the classifications, apply strong evidence:

```bash
msgvault identity discover --source-id 14 --apply
```

Weak evidence is never applied implicitly. To confirm an exact weak candidate
deliberately, pass the repeatable `--confirm` flag:

```bash
msgvault identity discover --source-id 14 --apply \
  --confirm you+archive@example.com
```

Pass `--json` to suppress progress and return the final structured result.
Discovery does not modify source messages or provider state.

Adding, confirming, or removing an owned address rechecks only matching
messages, while analytics still rebuilds the full archive after ownership
changes.

## Import an owned identity list

`identity import` accepts a text file with one identifier per line or a JSON
array or envelope, and validates and previews by default:

```bash
msgvault identity import --source-id 14 --file aliases.txt
msgvault identity import --source-id 14 --file aliases.json --apply
printf '%s\n' you@example.com you+news@example.com | \
  msgvault identity import --source-id 14 --stdin --apply
```

Pass either `--file` or `--stdin`; exactly one is required. `--signal` changes
the recorded evidence name from its `manual` default. Imported provider state is
reporting metadata only, and imports never remove a previously confirmed
identity.

## Gmail profile evidence

For a Gmail source, passing `--provider` includes the address returned by its
authenticated Gmail profile. The daemon uses the source's existing OAuth app or
service-account credentials without opening a browser or requesting wider
permissions. The profile must match the selected source under Gmail's account
equivalence rules before it can supply strong `oauth` evidence.

```bash
# Preview provider and archived evidence together
msgvault identity discover --source-id 14 --provider

# Confirm strong candidates from that preview
msgvault identity discover --source-id 14 --provider --apply
```

Previewing does not confirm ownership. Ordinary sync only adds `oauth`
provenance to matching addresses that are already confirmed; it does not restore
removed identities.

## Fastmail alias inventory

You can add an optional Fastmail JMAP token to include masked and send-as
addresses in the same review. Select exactly one archive source by `source_id`,
or by an unambiguous account identifier or display name:

```toml
[[fastmail]]
source_id = 14
api_token = "replace-with-a-Fastmail-API-token"
auto_confirm_identities = false
```

Fetch the inventory only when requested:

```bash
msgvault identity discover --source-id 14 --provider
msgvault identity discover --source-id 14 --provider --apply
```

Enabled, disabled, and deleted aliases count as strong historical evidence,
pending aliases remain review-only, and wildcard identities are rejected. Set
`auto_confirm_identities = true` to refresh and apply strong Fastmail evidence
after successful mailbox syncs. A changed mailbox refreshes immediately, whereas
a no-change sync rechecks only when the last successful provider refresh is more
than 24 hours old or the prior attempt failed.

The API token is stored in `config.toml`; protect that file like the rest of the
msgvault data directory.
