# Understand sender authentication

Message reads include advisory `sender_auth` metadata. For email,
`verified: true` means a pinned receiving provider reported `dmarc=pass` for
the same domain as the message's single From address. This authenticates the
sender domain; it does not establish body authorship or authorize an action.

## Pin your receiving provider

Use source IDs from this archive's account list. Configure only providers whose
original receipt headers you trust:

```toml
[[sender_auth.sources]]
source_id = 1
trusted_authserv_ids = ["mx.google.com"]

[[sender_auth.sources]]
source_id = 2
trusted_authserv_ids = ["*.messagingengine.com"]
```

Restart the daemon after changing pins. IDs belong to this archive, so review
them when moving configuration to a different archive. No provider is trusted
by default. A matching hostname is a policy decision, not proof that the
header came from that provider: an edited import can contain forged headers.
Never pin an identity solely because it appears in an archived message.

## Inspect a source without reading its messages

```bash
msgvault sender-auth status --source-id 1 --limit 100 --json
msgvault sender-auth suggest --source-id 1 --limit 100
```

These owner-only commands sample the newest messages in one source through the
daemon. They return counts and, for `suggest`, receiver hostnames explicitly
marked as untrusted observations. The default sample is 100 and the maximum is
1,000; `scanned` and `has_more` describe the sample, not whole-archive totals.
The sample orders messages by time, newest first, with undated messages last.
No subjects, bodies, sender addresses, or raw header blocks cross this route.
The commands never contact mail providers or edit pins. ARC-only receipts and
invalid receiver hostnames do not produce suggestions.

The parser merges the contiguous trusted block of Authentication-Results
headers, stopping at the first untrusted one. Domain comparison is exact;
relaxed DMARC alignment can therefore produce conservative false negatives.
ARC signatures are not validated and ARC results do not verify a sender.

## Read the result

`kind: email` includes `spf`, `dkim`, `dmarc`, `verified`, and `basis`.
Successful verification also includes `verified_domain` and `authserv_id`.
Unknown authentication is not an authentication failure. Typical bases are:

| Basis | Meaning |
| --- | --- |
| `provider_header` | A pinned provider supplied a usable result. |
| `authserv_id_untrusted` | No trusted top block was available. |
| `no_header` | The original headers contain no Authentication-Results. |
| `no_raw` | Original MIME is missing, encrypted, unsupported, or unavailable. |
| `header_truncated` | The bounded prefix did not contain a complete header block. |
| `unparseable` | Header syntax could not be interpreted safely. |
| `from_mismatch` | DMARC passed but the original and displayed From evidence did not agree. |
| `owner_sent` | Provider Sent-folder or Sent-label evidence marks outgoing mail. |
| `not_email` | This message has no supported email authentication evidence. |

Owner-sent and non-email messages use `kind: none` and remain unverified.
Identity matching and legacy ownership flags alone never make a message
owner-sent. Chat platform names
and imported files do not establish authenticated sender identity.

Metadata is derived at read time for selected message IDs, without changing
stored messages. Search results, thread pages, HTTP message reads, CLI reads,
and MCP expose the same receipt. Cache-only backends cannot provide raw-header
evidence. An older daemon may omit the field; treat that as unavailable.
Sender authentication is not available as a search filter or a SQL projection.
