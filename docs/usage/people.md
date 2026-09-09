---
last_edited: "2026-09-08"
title: People and Profiles
description: Find people across your archive, keep their details together, and understand your contact history.
---

Find someone across email, chats, and meetings, then keep their contact details
and the things you want to remember in one profile. Start in the Web UI's
**Directory** or the [TUI People browser](/docs/usage/tui/#people).

| I want to… | Start here |
|---|---|
| Save someone's name, details, or notes | [Create a profile](#promote-a-durable-person) |
| Remember what they recently shared | [Person briefs](/docs/usage/people-briefs/) |
| Find files you exchanged | [Person files](#find-files-related-to-a-person) |
| Keep profile facts current from messages | [Profile automation](/docs/usage/people-automation/) |
| Look up public profile information | [External enrichment](/docs/usage/people-enrichment/) |
| Sync contacts with an address book | [CardDAV contacts](/docs/usage/people-carddav/) |
| Tell msgvault which accounts and aliases are mine | [Source identities](#discover-source-identities) |

Ordinary profile editing and identity discovery use your archive. Briefs,
automatic fact extraction, semantic person search, and external enrichment
need separate provider configuration and consent. You can use the Directory
without enabling them.

## Understand the records

- A **source identity** is an address or handle that belongs to you in one
  source. It helps msgvault recognize messages you sent.
- An **observed person** groups addresses and handles linked by archive
  evidence. Matching display names alone do not combine people.
- A **person profile** is a saved record with a stable person ID. Your curated
  details stay attached when archive links change.

Observed contacts use **participant IDs**. Saved profiles use **person IDs**.
Commands name the ID they require; the two are not interchangeable. Promotion
creates a profile from an observed person. Subscribed CardDAV contacts can also
create profiles when imported.

## Promote a durable person

Open an observed contact in Directory and promote it, or use its participant
ID with the CLI. Replace `42` with that participant ID and `7` with the person
ID returned by promotion:

```bash
msgvault person promote 42
msgvault person list
msgvault person get 7
msgvault person set-display-name 7 "Alex Example"
```

Repeating promotion returns the same profile. Archive observation alone does
not promote people. Linking another cluster into a promoted one expands that
profile's participant bindings. Linking two clusters that already belong to
different profiles reports a conflict instead of silently merging curated
data. Unlinking evidence does not move or delete profile bindings.

Your display-name override is also used in analytics, search results, and
exports. Original addresses and message content stay available. Clear the
override with `--clear`:

```bash
msgvault person set-display-name 7 --clear
```

`person delete` is permanent. It removes the profile bindings and retires the
vCard UID forever; promoting the same observed cluster later creates a new
person and UID.

## Keep private notes

Save context in your own words with the Notes field. It preserves line breaks
and keeps earlier values in attribute history:

```bash
msgvault person notes get 7
msgvault person notes set 7 --text "Met at the community workshop."
msgvault person notes append 7 --text "Ask how the garden project is going."
msgvault person notes set 7 --text @person-notes.txt
```

`--text -` reads standard input. Append adds a new line atomically, so two
concurrent appends do not overwrite each other. When replacing text in an
automated workflow, `set --expected-value-id <id>` rejects a concurrent change.
Notes are marked sensitive and
excluded from semantic person search and MCP's general `get_person_profile`
response. The separate MCP `get_person_notes` tool can read them. If you publish
a profile to CardDAV, Notes map to the vCard `NOTE` property.

## Record employment and relationships

Organizations have their own profiles. Employment connects a person to an
organization and can retain a title, role, department, location, and dates:

```bash
msgvault organization create "Example Cooperative" --domain example.com
msgvault employment add --person 7 --organization 3 --title "Engineer" --start 2024-03
msgvault employment list --person 7
msgvault employment end 9 --end 2026-08
```

Use the IDs returned by your commands. `employment set-primary` chooses the
primary current job. Ending a job keeps its history. Organization commands
also support profile edits, custom attributes, retirement, and merging
duplicates; `organization show 3 --history` includes superseded profile rows.

Relationships connect two saved people and have a direction. The forward label
reads “source person is the ___ of target person.” List the available types
before choosing one:

```bash
msgvault relationship-type list
msgvault person relationship add 7 parent 12 --from 2020
msgvault person relationship list 7 --include-ended
```

The same stored relationship appears from both people's perspectives, with the
appropriate forward or reverse label. Dates accept `YYYY`, `YYYY-MM`, or
`YYYY-MM-DD`. Use `person relationship end <relationship-id> <until-date>` to
keep an ended relationship in history. `relationship-type create` adds your
own types; symmetric types require identical forward and reverse labels.

## Find a person by what you remember

Directory name and identity searches work without an embedding provider. For a
description such as “people who enjoy gardening,” enable
[semantic people search](/docs/configuration/#vectorpeople), grant its separate
consent, and build the embeddings:

```bash
msgvault person provider status --semantic-embeddings
msgvault person provider consent --semantic-embeddings --yes
msgvault daemon restart
msgvault embeddings build
msgvault person search "people who enjoy gardening" --limit 10
```

This searches saved profiles, using current names, categories, locations,
employment, relationships, and eligible searchable attributes. It does not
search your messages or private Notes. Sensitive attributes and `how_we_met`
are excluded. Search text and the curated person documents go to the configured
embedding endpoint. `--limit` defaults to 20 and accepts 1–100; `--json` includes
person IDs and scores.

## Find files related to a person

Use a saved person ID to find attachments across their linked identities:

```bash
msgvault person files 7 --direction from_person --filename proposal
msgvault person files 7 --mime-family pdf --after 2026-01-01
msgvault person files 7 --lane documents --query "project proposal"
msgvault person files 7 --lane visual --query "garden plan"
msgvault person files 7 --lane all --query "project proposal" --json
```

The default `metadata` lane lists archived attachments without semantic search.
`--direction` accepts `from_person`, `to_person`, and `group`; it is repeatable.
`--filename` matches a case-insensitive substring. MIME families are `image`,
`pdf`, `audio`, `video`, `text`, `document`, `archive`, and `other`.

The `documents` and `visual` lanes need their respective indexes; see
[document search](/docs/usage/document-indexing/) and
[visual search](/docs/usage/vector-search/). Both require `--query`, as does
`all`. The `all` result reports each lane separately, including unavailable
lanes; its metadata results do not use the semantic query. Document search
cannot apply filename or MIME-family filters.

Each lane has its own cursor. Use `--cursor` with one lane to continue a result;
`all` cannot take a cursor. `--limit` is per lane, defaults to 100, and accepts
1–100. `--after` is inclusive and `--before` exclusive; both accept a date or
RFC3339 timestamp.

## Catch up before your next conversation

[Person briefs](/docs/usage/people-briefs/) summarize recent chat and text
messages with citations. The guide covers enrollment, generation, reading in
Directory or the TUI, refresh timing, and saved versions. Email-only contacts
cannot receive a brief yet.

## Configure provider-backed person sweeps

[Profile automation](/docs/usage/people-automation/) maintains facts for people
you choose to track. Follow that guide to check and consent to a provider,
change its policy, recover stale consent after an upgrade, and inspect or pin
automatic facts. [External enrichment](/docs/usage/people-enrichment/) has a
separate setup for looking up public information.

## Merge duplicate profiles and reverse a merge

Merge two durable profiles only after reviewing both people. The first person
survives with the same ID and vCard UID; the second person's participants and
profile data move to it, and the retired UID becomes an alias. Both current
revisions and an idempotency key are required:

```bash
msgvault person merge 7 12 \
  --survivor-revision 4 \
  --absorbed-revision 2 \
  --idempotency-key merge-7-12
```

Conflicting single-value attributes remain reviewable instead of being
dropped. Inspect the merge and decide each candidate explicitly:

```bash
msgvault person merge-history 7
msgvault person merge-show 42
msgvault person merge-show 42 --snapshot
msgvault person merge-candidate 18 \
  --person-id 7 --revision 5 --decision accepted
```

A split creates a new person and a new vCard UID. Select absorbed participant
lineage with repeated `--participant` flags. Omit `--participant` only when the
absorbed profile had no participants:

```bash
msgvault person split 7 \
  --merge-id 42 \
  --participant 91 \
  --revision 5 \
  --idempotency-key split-42-91
```

An exact reversal restores the two pre-merge profiles when their lineage and
dependencies are still intact. A partial split moves participant-attributable
data instead of guessing; use `--json` to inspect ambiguous or unrestored rows.
An active merge prevents deletion of its current person; complete the split
first.

Profiles with an active CardDAV publication cannot be merged. This prevents a
local merge from silently reassigning a UID that an external address book is
already syncing.

Merge snapshots are durable audit data. They retain both profiles' merge-time
values after later live-profile edits or redaction. A subset copies a complete
merge packet only with `--include-attributes`, `--include-profiles`, and
`--include-vcard-resources`; treat that output as containing historical
personal data.

## Store typed attributes

Every archive starts with the same seeded person-field catalog. The
definitions are system-owned and cannot be deleted; labels, descriptions, and
display order can be edited. Sensitive fields are not searchable and stay out
of provider inference unless a provider profile opts in. The seeded descriptive fields, including `ask_me_about` and `how_we_met`,
accept up to 280 characters per value. The private `notes` field is a separate
multiline text field. Older 120-character descriptive-field limits are widened
on the next store open.

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

List fields and values, set scalar values, and retain superseded history:

```bash
msgvault attribute-definition list --object-type person
msgvault person attributes list 7
msgvault person attributes set 7 primary_channel --value email
msgvault person attributes set 7 ask_me_about --ordinal 0 --value "release engineering"
msgvault person attributes set 7 ask_me_about --ordinal 1 --value databases
msgvault person attributes list 7 --history
```

Setting a value supersedes the current value at the same slug and ordinal; it
does not overwrite history. `person attributes clear` closes the current value
and also retains it in history. Use `--dry-run` to validate a set or clear, and
`--expected-value-id` for compare-and-swap protection when automating updates.

Scalar `--value` input handles text, integer, real, boolean, date, and timestamp
definitions. Structured record and JSON values use `--value-json` with inline
JSON, `@path`, or `-` for standard input.

## Create a portable field definition

Custom fields are metadata rows, not runtime database migrations. Their
universal IDs and slugs are stable; labels and descriptions can change.
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

`attribute-definition rename` changes presentation metadata without changing
stored references. Deletion is limited to user-created definitions that still
have no stored values; shipped and non-deletable definitions are protected.

For automation, run `msgvault openapi` or read `/openapi.json` from the daemon.
The contract includes source identities, person profiles, attribute
definitions, and historized person-attribute routes, and the generated Go
client exposes the same operations.

## Discover source identities

Full and incremental email sync enrich identities already confirmed for the
source with strong sender evidence from trusted Sent metadata. Sync does not
confirm first-time aliases; review them with `msgvault identity discover` and
apply strong candidates with `msgvault identity discover --apply`.
Recipient-only evidence remains a review candidate: receiving mail at an
address does not by itself prove that the address is you.

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

Weak evidence is never applied implicitly. Confirm an exact weak candidate
deliberately with a repeatable flag:

```bash
msgvault identity discover --source-id 14 --apply \
  --confirm you+archive@example.com
```

`--json` suppresses progress and returns the final structured result. Discovery
does not modify source messages or provider state.

## Import an owned identity list

`identity import` accepts either a text file with one identifier per line or a
JSON array/envelope. It validates and previews by default:

```bash
msgvault identity import --source-id 14 --file aliases.txt
msgvault identity import --source-id 14 --file aliases.json --apply
printf '%s\n' you@example.com you+news@example.com | \
  msgvault identity import --source-id 14 --stdin --apply
```

Exactly one of `--file` and `--stdin` is required. `--signal` changes the
recorded evidence name from its `manual` default. Imported provider state is
reporting metadata only; imports never remove a previously confirmed identity.

## Fastmail alias inventory

An optional Fastmail JMAP token can add masked and send-as addresses to the same
review. Select exactly one archive source by `source_id`, or by an unambiguous
account identifier or display name:

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

Enabled, disabled, and deleted aliases are strong historical evidence; pending
aliases remain review-only, and wildcard identities are rejected. Set
`auto_confirm_identities = true` to refresh and apply strong Fastmail evidence
after successful mailbox syncs. A changed mailbox refreshes immediately; a
no-change sync rechecks only when the last successful provider refresh is more
than 24 hours old or the prior attempt failed.

The API token is stored in `config.toml`; protect that file like the rest of the
msgvault data directory.
