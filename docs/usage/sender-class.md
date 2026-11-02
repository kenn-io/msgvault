---
last_edited: 2026-11-02
title: Read a message's sender class
description: Understand authentication-gated sender classes and exact-mailbox relationship evidence returned on message reads.
---

# Read a message's sender class

Message details, search results, lists, and thread rows expose three derived
fields. They describe the sender identity and its relationship to the archive
owner. They do not establish that the content is true, identify who wrote each
part, or authorize an action.

| Field | Values | Meaning |
|---|---|---|
| `sender_class` | `unverified`, `verified`, `known`, `contact`, `self` | Authentication-gated identity class |
| `sender_class_basis` | `source_owned`, `auth_failed`, `no_auth_evidence`, `curated_person`, `owner_sent_to`, `dmarc_pass`, `platform`, `invalid_relationship`, `evidence_unavailable` | Why the class was returned |
| `sender_relationship` | `contact`, `known`, `none` | Exact mailbox relationship, independent of message authentication |

For incoming email the ladder is `unverified < verified < known < contact`.
Every class above `unverified` requires an aligned DMARC pass from an explicitly
trusted receiver; see [sender authentication](sender-auth.md). An address bound
to a curated person earns `contact`. A real source-native owner send to the
exact address earns `known`. Drafts, mailing lists, large broadcasts, deleted
messages, display names, linked aliases, and text inside messages cannot
establish that relationship.

A message that fails authentication stays `unverified` even when the address is
a contact:

```json
{
  "sender_class": "unverified",
  "sender_class_basis": "auth_failed",
  "sender_relationship": "contact"
}
```

`self` is separate from the incoming ladder. It requires the source's native
ownership signal. Matching an owner address or an inferred identity cannot
establish it. Chat and SMS sources receive no automatic platform authentication.
Raw file imports cannot establish native ownership or an owner-send relationship,
including older imports whose ownership flag came from a matching From address.

Classes are computed when read. Promoting a person or recording a qualifying
send changes earlier messages' relationship immediately, without reindexing.
Delegated reads use only relationship evidence reachable through the token's
allowed sources.

If an immutable From identity is missing, a cached sender disagrees with that
identity, or a bounded evidence lookup cannot finish, the response is
`unverified` with `evidence_unavailable` and omits `sender_relationship`. An
explicit `none` means the lookup completed and found no qualifying relationship.
Older daemons that omit the fields also remain unavailable to the client.

These fields are metadata on ordinary reads such as `get_message`,
`list_messages`, `search_messages`, and `list_thread`. There is no stored class
column or `sender_class:` search operator.
