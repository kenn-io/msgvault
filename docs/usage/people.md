---
last_edited: "2026-09-03"
title: People, Profiles, and Source Identities
description: Discover the addresses that mean you inside each source, curate stable person profiles, and store typed profile attributes.
---

Msgvault keeps three related concepts separate so archive evidence is not
mistaken for user-curated data:

- A **source identity** is an address or handle that means you inside one
  ingestion source. It drives sent-message classification and deduplication.
- An **observed person** is an identity cluster assembled from explicit archive
  links. Equal display names alone never merge people.
- A **durable person profile** is an observed cluster you explicitly promote.
  It has a stable numeric ID and vCard UID, so curated data survives later
  identity-link changes.

The Web UI's People and Relationships workspaces use observed identity
evidence. The commands on this page add explicit source identities and durable
profile data through the selected local or remote daemon.

## Configure provider-backed person sweeps

People sweeps can maintain supported profile fields from bounded archive
evidence. Provider access is disabled until one exact named profile passes a
synthetic check and you separately consent to its privacy policy.

Add a profile with explicit custom values when you do not want catalog
discovery. This path does not contact models.dev:

```bash
export ZAI_API_KEY="..."
msgvault person provider add glm --custom \
  --protocol openai_chat \
  --endpoint https://api.z.ai/api/paas/v4 \
  --model glm-5.3 \
  --auth bearer \
  --credential-env ZAI_API_KEY \
  --retention-posture provider-declared \
  --training-posture provider-declared \
  --source conversation_text \
  --source meeting_text \
  --source-since 2026-01-01 \
  --allow-sensitive \
  --reasoning-effort max \
  --yes
```

`--allow-sensitive` is required for real sweeps: any packet with seed or
context evidence is marked sensitive, so a profile without this flag fails on
every real sweep. The flag permits sending that verbatim archive text to the
selected provider; without it only the synthetic capability check, which
sends no archive text, can run.

Omit `--custom` to allow interactive onboarding to consult models.dev for
discovery hints. Models.dev is used only by `provider add`; it is not a runtime
dependency and never receives archive content or credentials. A catalog
suggestion never chooses where a credential is sent either: onboarding pairs a
credential only with an endpoint you passed explicitly via `--endpoint` or
with the first-party API hosts compiled into msgvault. The add command
checks the selected endpoint with fixed synthetic input and saves the exact
negotiated protocol behavior. It does not grant archive egress consent.

Review the saved policy, then consent explicitly and run a bounded sweep:

```bash
msgvault person provider status glm
msgvault person provider consent glm --yes
msgvault person provider use glm
msgvault person sweep run --limit 5
msgvault person sweep status
```

`provider consent <name>` can grant the checked profile while scheduling is
still disabled. `provider use <name>` selects that profile and enables people
sweeps, so the following manual run and future scheduled runs use it.
`provider add` never switches the active selection or enables sweeps: it only
publishes the profile and records its synthetic check. A profile can exist
without being selected as long as people sweeps stay disabled.

`add`, `use`, `remove`, `check`, `consent`, and `status` all accept `--json`.
`status <name> --json` reports the profile, its recorded `check`, and its
`consent` state, which together are every gate a sweep must pass, so an agent
can decide the next step without parsing prose.

`provider use` and `provider remove` edit the `config.toml` on the machine
where you run them, so run them on the daemon host; against a configured
remote daemon they refuse rather than edit a config file that daemon never
reads. A running daemon keeps the people sweep configuration it started
with: manual and proxied commands re-read `config.toml`, but scheduled
sweeps keep the startup selection until you run `msgvault daemon restart`,
which both operations remind you about on success.

Use `--api-key-stdin` during `provider add` to store a profile-specific key
outside `config.toml`, or `--credential-env NAME` to store only an environment
variable name. Never put the secret value in a command argument. Stored keys
are supported on Linux and macOS only; on other platforms `provider add`
refuses a stored credential before reading it, so pass `--credential-env`
there. Custom local gateways can use `--custom`; their synthetic check still
calls the configured endpoint.

GLM 5.3, Kimi K3, OpenRouter, Venice, open-agent-api, Gemini, Anthropic, and
OpenAI Responses are examples of profiles over the supported HTTP protocols,
not presets or provider-name branches. A gateway uses the protocol it
exposes. OpenRouter and Venice may forward data to upstream operators, so
review the complete routing path and its privacy terms. Use subscription and
logged-in endpoints only as their terms allow. The `codex_app_server`
protocol is present but release-gated: every Codex operation currently fails
closed until its executable isolation gate ships, and `provider add` cannot
create it; see the Codex app-server profiles section of the configuration
reference.

Msgvault never switches providers automatically. A locally invalid response
may receive one repair call on the same resolved profile, credential, endpoint,
and model. Any profile edit needs a fresh exact check and consent. Live
credential checks are useful operator verification but are not CI tests.

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

## Promote a durable person

People returned by the Web UI or API include participant IDs. Promote any
participant in one identity cluster to reserve a stable profile:

```bash
msgvault person promote 42
msgvault person list
msgvault person get 7
msgvault person set-display-name 7 "Alex Example"
```

Promotion is explicit and idempotent; observed people are not promoted
automatically. Linking another cluster into a promoted one expands that
profile's participant bindings. Linking two clusters that already belong to
different profiles reports a conflict instead of silently merging curated
data. Unlinking evidence does not move or delete profile bindings.

Clear only the display-name override with `--clear`:

```bash
msgvault person set-display-name 7 --clear
```

`person delete` is permanent. It removes the profile bindings and retires the
vCard UID forever; promoting the same observed cluster later creates a new
person and UID.

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
of provider inference unless a provider profile opts in.

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
| `how_we_met` | How we met | text | single | no | How you and this person first met; the one seeded field about your relationship rather than the person alone |

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
