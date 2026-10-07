---
last_edited: "2026-10-07"
---

# Received-account attribution design

**Historical design record.** Current storage rules are owned by
[Data Storage](../architecture/storage.md), and user-facing search behavior by
[Searching](../usage/searching.md#find-mail-by-the-address-that-received-it).

One physical inbox can receive mail for several confirmed addresses, for
example work@ forwarding into a personal Gmail. Attribution records the
receiving address without changing source provenance or creating duplicate
ingest sources. Sent mail uses its confirmed sender; calendar events use the
calendar's own mailbox rather than the sync credential or event creator.

## Attribution decision

Each email and calendar row carries at most one account: nullable
`messages.account_address`, plus `messages.account_path` (`inbound`, `sent` or
`calendar`). Conflicting evidence stays unattributed, so identity totals plus
unattributed rows equal the source total. Overlapping memberships were rejected
because their totals cannot be added like separately synced accounts. Derived
labels would mix attribution with provider labels, and virtual ingest sources
would duplicate credentials, provenance and checkpoints.

No personal archive or provider inventory was inspected. Header research used
primary documentation, and fixtures use synthetic addresses. Historical
messages may lack the documented delivery fields.

## Delivery evidence and its limits

Only parse the outer message's header block. Preserve repeated values and
their order. Never mine a forwarded attachment, quoted body or arbitrary
address-shaped text for attribution. Parse mailboxes with the real address
parser; reject malformed values and bound recovery header input.

| Path | Verified semantics | Treatment |
|---|---|---|
| Gmail forwarding | Google recommends `X-Forwarded-For` or `X-Forwarded-To` to signal forwarding, but does not specify a universal value grammar or original-recipient ordering. | Preserve these as hints. A forward destination alone cannot identify the upstream account. Use validated delivery/original-recipient matches; keep ambiguous forwarding chains unresolved. |
| Repeated `Delivered-To` | RFC 9228 describes successive delivery addresses, newest first. A final inbox can differ from the original recipient. The RFC is experimental, not a universal provider guarantee. | Retain the entire chain. Do not let a final-inbox match discard an upstream confirmed alias. Multiple supported upstream identities require stronger evidence or remain ambiguous. |
| Workspace routing | Routing can add `X-Gm-Original-To` when changing the envelope recipient. The setting is optional. | Use a valid, confirmed original-recipient match ahead of final-delivery evidence. Include the actual Workspace field, not just `X-Original-To`. |
| Fastmail forwarding | `X-Delivered-To` is the original envelope recipient; `X-Resolved-to` is the address after alias rewriting. Fastmail documented retaining `X-Delivered-To` on forwarded mail. | Prefer matched `X-Delivered-To` over `X-Resolved-to` / the final Gmail inbox. Treat the historical forwarding post as evidence of behavior, not a guarantee for all present routes. |
| Fastmail received/fetched mail | `X-Original-Delivered-to` can reflect an older Received `for` address; it can be absent for multiple recipients. | Preserve as secondary historical evidence. Do not confuse it with `X-Delivered-To`. |
| Generic `X-Original-To` | Its semantics are server-specific; it is not the documented Fastmail original-envelope field. | Support only a valid address match as a hint. Do not assume every provider inserts it or that it authenticates ownership. |
| Gmail POP fetch | Google documents the feature's retirement, with existing imports retained. No authoritative original-account grammar for `X-Gmail-Fetch-Info` was found in this research. | Support archived POP evidence through valid delivery headers or unique To/Cc matches. Preserve opaque fetch information for future rules; do not guess an identity from a host or free-form token. |
| Alias visible only in To/Cc | Author headers express addressed recipients, not necessarily envelope delivery. | Use a single confirmed match only after stronger evidence fails to resolve an identity. Two matches are ambiguous. |
| Bcc or mailing list | Visible To/Cc can omit the actual recipient. Delivery fields may include list and forwarding addresses. | Exclude unrelated list addresses from the candidate set. Allow a unique delivery match; do not infer the user from List-Id or a missing Bcc header. |

Sources checked on 2026-10-01:

- [RFC 9228](https://www.rfc-editor.org/rfc/rfc9228.html) explains delivery chains and their order.
- [Google forwarding guidance](https://support.google.com/mail/answer/175365?hl=en) documents forwarding hints.
- [Workspace routing settings](https://knowledge.workspace.google.com/admin/gmail/advanced/add-gmail-routing-settings) documents `X-Gm-Original-To`.
- [Fastmail addressing](https://www.fastmail.help/hc/en-us/articles/360058753414-Email-addressing) distinguishes envelope and resolved delivery.
- [Fastmail delivery process](https://www.fastmail.help/hc/en-us/articles/1500000278262-The-email-delivery-process) describes additional delivery evidence.
- [Fastmail forwarding history](https://www.fastmail.com/blog/x-delivered-to-header-now-left-on-forwarded-email/) documents retention of the envelope recipient.
- [Gmail POP changes](https://support.google.com/mail/answer/16604719?hl=en) says existing users can continue until January 2027; this is a historical-archive path, not a prerequisite for new routing.

These headers are attribution hints, not authenticated ownership or permission
to send.

## Attribution rules

- Candidates are the source's confirmed identities. Delivery headers never
  confirm an identity. Every input is lowercased before comparison; dots and
  plus suffixes stay significant.
- Only the outer header block is read, bounded at 256 KiB. A header value that
  is not a valid address list is skipped whole; other values still count.
- Provider evidence decides direction first: a Sent or Drafts folder role,
  Gmail's `SENT` or `DRAFT` label, an IMAP `\Draft` flag, or a draft record,
  on the message's own source. Once a row is a draft, `draft_authored` keeps it
  written after the draft is edited or deleted.
- A sent copy takes its unique confirmed sender. It never falls back to the
  source mailbox.
- Inbound mail tries, in order: original-recipient headers (`X-Gm-Original-To`,
  `X-Delivered-To`, `X-Original-To`), upstream delivery addresses
  (`Delivered-To`, `X-Resolved-To`, `X-Original-Delivered-To`), confirmed To/Cc
  matches, the source mailbox as final inbox, then the source mailbox when it
  is confirmed. Bcc never counts.
- The first two tiers exclude the source mailbox. The final inbox's own server
  stamps its address into original-recipient headers too (Fastmail's
  `X-Delivered-To`, Postfix's `X-Original-To`), so a match there only counts
  as the final inbox.
- In a source without Sent folders, such as an mbox or PST import, a message
  with no match in any inbound tier and a unique confirmed sender is a sent
  copy. It takes that sender rather than the source mailbox. A source with a
  Sent folder keeps such a message inbound, because the provider already
  decided its direction. A source that gains its first Sent folder or loses
  its last returns its mail to pending.
- More than one confirmed match at a tier leaves the row unattributed, and no
  lower tier runs.
- The source mailbox is the source identifier, or the IMAP username in the
  connection URL, when it is a valid mailbox.
- A calendar event takes the account email of the primary calendar, or a
  calendar ID that is itself a confirmed mailbox. Google group and resource
  calendars stay unattributed.
- Rows written by older versions start pending (`account_path IS NULL`).
  Every sync or import derives the source's pending rows first, and
  `msgvault repair-derived` derives them on demand.
- An identity or source-identifier change returns the rows it can affect to
  pending in its exclusive transaction, then derives them in pages after
  commit, so it never holds the identity lock for a whole mailbox.
