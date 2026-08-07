---
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

## Discover source identities

Full and incremental email sync automatically confirm strong sender evidence
from trusted Sent metadata. Recipient-only evidence remains a review candidate:
receiving mail at an address does not by itself prove that the address is you.

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

## Store typed attributes

Every archive starts with four person-field definitions:

| Slug | Type | Cardinality | Behavior |
|---|---|---|---|
| `primary_channel` | text choice | single | Writable: email, phone, SMS, chat, or in person |
| `contact_frequency` | integer days | single | Writable |
| `ask_me_about` | text | multiple | Writable and searchable |
| `last_contacted` | timestamp | single | Read-only derived field; it remains empty until its producer supplies a value |

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
