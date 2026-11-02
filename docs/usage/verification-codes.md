---
last_edited: 2026-11-02
title: Verification Codes and Links
description: Notice verification arrivals without exposing detected credentials to archive readers.
---

Unreleased: msgvault can hide detected sign-in codes, magic links, password-reset
links, and account or subscription confirmations from scoped readers and MCP
clients. Readers can still see that a verification arrived and which archived
message contains it. Original messages remain in the archive.

Detection uses message content and sender hints. It applies to archived email
and chat messages, including Beeper, SMS/MMS, iMessage, RCS, WhatsApp, Teams, Slack, Discord,
Matrix, Inline, Messenger, and Chatwoot.
Long chat messages receive the same checks as short ones. Detection does not
authenticate a sender or establish that a link is safe to open.

## Default reads

Default masking requires daemon API schema 3.36.0 or newer. Recent-arrival
listing below requires schema 3.37.0 or newer.

Owner-daemon HTML and raw-MIME protection is blocked on the
[canonical-body update](https://github.com/kenn-io/msgvault/pull/1685). Until
that update lands, a credential present only in HTML beside a plain-text
alternative can escape owner MCP masking and raw-MIME refusal. Keep this
unreleased stack in draft until the combined read paths are checked.

Delegated tokens mask detected credentials unless their grant includes
`verification.read` on the message's actual source. The modifier must accompany
the operation's read permission, such as `message.read` or `search.read`.
`verification.read` alone grants no read operation and exposes no read tool.

Owner MCP sessions also mask by default. To reveal values for an owner session,
start it with:

```bash
msgvault mcp --unmask-verification
```

Delegated sessions reject this flag. A tool argument cannot override either
the owner session setting or a source grant. Existing grants without the new
modifier start masked after upgrading. Expiry never authorizes automatic reveal.

CLI, Web UI, and ordinary owner HTTP message reads remain complete. The arrival
endpoint described below has its own explicit reveal opt-in for owner keys.
See [scoped archive reads](../cli-reference.md#scoped-archive-reads) for issuing
and managing grants.

## What readers receive

Msgvault checks canonical content before making excerpts, body pages, keyword
matches, or message summaries. Detected values are replaced in returned human
text, including text and HTML bodies, subjects, snippets, participant names,
addresses, labels, and attachment metadata. Encoded and grouped variants of a
detected value are covered. Derived excerpts that cannot be protected are
withheld. Source, provider, conversation, and message identifiers stay intact.

Fixed placeholders are `[verification code masked]` and
`[verification link masked]`. A message with a finding also carries metadata:

```json
{
  "kind": "otp_code",
  "confidence": "high",
  "received_at": "2026-11-02T09:00:00Z",
  "expires_at": "2026-11-02T09:10:00Z",
  "expired": false,
  "masked": true,
  "masked_fields": ["body_text"]
}
```

This is an excerpt of the `verification` object. Optional fields include sender,
hosts, and detection signals. Kind, sender, and expiry are claims derived from
untrusted message content. `masked` and `masked_fields` describe what the server
did. A cue without an extractable value can produce metadata without a value.

If canonical message content or its authorized source cannot be resolved,
protected reads fail closed with `verification_unavailable`.

## Notice a recent arrival

With daemon API schema 3.37.0 or newer, MCP exposes `list_verifications` when
its backend supports it. Delegated callers need `search.read` on the source.
For example, call the tool with:

```json
{"kind":"otp_code","include_expired":false,"limit":20}
```

Optional filters are `since` (RFC3339), `sender` (exact, case-insensitive), and
`kind`. Supported kinds are `otp_code`, `magic_link`, `password_reset`,
`double_opt_in`, and `account_confirmation`.

The default window is the last hour. An older `since` is clamped to 24 hours;
a future time is rejected. The default limit is 20 arrivals, with a maximum of
100. A call scans at most 1,000 newest candidate messages by their archived
message date, then detects their content at read time.

The response contains `data`, `since`, `before`, `scanned`, and `scan_truncated`.
Each arrival has `message_id`, `source_id`, `source_message_id`,
`conversation_id`, and `verification`. `values` appears only when reveal is
authorized. `scan_truncated=true` means the result limit or candidate cap was
reached; older matching messages may be omitted. An empty bounded scan does not
prove that no code exists elsewhere in the archive.

The equivalent HTTP read is `GET /api/v1/verifications`, with the same filter
names. Owner keys must add `reveal=true` to receive values. For delegated tokens,
the server ignores that parameter and uses the source grant. `include_expired`
changes which arrivals are listed, not their reveal authority.

This is a polling read. It does not wait for a new message, sync a source,
follow a link, or send anything.

## Search and raw content limits

Masked reads withhold the HTML of detected messages. Markup can split values
across nodes or attributes, so literal replacements cannot reliably scrub it.
Available plain text still uses value masking; authorized reveal retains the
original HTML.

When a message contains more than 64 detected values or their combined text
exceeds 8 KiB, masked reads replace its nonempty content fields with
`[verification content masked]`. This bounds replacement work for unusually
dense messages. Source, provider and message identifiers remain available.

Protected searches drop a candidate when a literal query token matches one of
its detected secret values. `masked_hits_dropped` reports how many candidates
were removed. MCP search responses with drops also provide `next_offset`; use
that value for the next request, including when the current page is empty.
It advances past the original candidates rather than just the surviving rows.

This reduces literal probing. Heuristics can miss credentials, and search
membership, counts, and other queries can still disclose existence. An empty
search result is not a security guarantee.

For a detected message, masked sessions refuse raw MIME and `text/*` or
`message/rfc822` attachment bytes. Chunk continuations recheck the parent
message. Screenshots, PDFs, other binary attachment contents, and independent
attachment extraction/search are outside this protection; their bytes can
contain credentials that message detection never sees.

Masked owner MCP sessions also refuse `query_sql`, `aggregate`, and vector mode
in `search_in_message`. Arbitrary SQL projections can separate a credential
from its cue, so masking isolated result cells cannot protect those reads.
An owner can enable the session unmask flag when complete access is needed.

There is no per-call interactive reveal tool or stored classification column.
See [MCP setup](chat.md) for connection options and
[the HTTP reference](../api-server.md) for authentication.
