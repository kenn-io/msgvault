---
title: Reply to YouTube comments
description: Preview, confirm, and recover public replies to archived YouTube comments.
last_edited: 2026-11-06
---

# Reply to YouTube comments

Reply to an archived comment on your configured channel. The daemon sends the
public reply through the official YouTube Data API and stores the accepted
comment in the same video conversation.

First preview the exact text and resolved top-level parent:

```bash
msgvault youtube reply 123 --body "Example reply" --dry-run
```

The JSON receipt contains `target.parent_id`, `body`, `state`, and `token`.
Previewing reads the archive and writes a local confirmation receipt. It makes
no YouTube request and needs no OAuth credential refresh.

Review the parent and text, then copy the token into the confirmation:

```bash
msgvault youtube reply 123 --body "Example reply" --confirm TOKEN
```

Use the same archived message ID and exact text. Replying to a nested comment
posts on its top-level parent because YouTube does not support a third level.
The parent must have archived evidence that it is published and replyable.
Confirmation checks the credential's channel and the thread's current
eligibility again. A held or spam parent is never implicitly published.

The channel must have exactly one matching `[[youtube]]` profile. Credentials
come from that profile's named environment variables on the daemon host.

## Read the receipt

| State | Meaning |
| --- | --- |
| `prepared` | The preview is ready for confirmation; nothing was sent. |
| `sending` | A send was claimed. Its outcome may be unknown, including after a crash. |
| `accepted` | YouTube returned the comment; local archival needs to finish. |
| `archived` | The accepted comment is stored; `message_id` identifies it. |
| `rejected` | YouTube definitely rejected the insert. This confirmation cannot send again; a fresh preview can issue a new one. |

Reusing an accepted confirmation retries only local archival. Reusing an
archived confirmation returns its receipt. Neither sends another reply.
The same channel, video, parent and exact body share a receipt even when the
selected local target differs. This deliberately prevents a second send of
that exact operation through another nested comment or another preview.

## Resolve an uncertain send

YouTube does not offer an idempotency key for comment insertion. If the
connection fails after it accepts the reply, msgvault cannot safely retry it.
A fresh preview and another confirmation remain blocked.

If you have the explicit provider ID of the accepted reply, supply it with the
original confirmation:

```bash
msgvault youtube reply 123 --body "Example reply" \
  --confirm TOKEN --resolve-comment PROVIDER_COMMENT_ID
```

This fetches that comment and checks its owner, video, parent and exact text
against the receipt before archiving it. It never posts a comment. An absent
comment or a same-text search result alone does not prove that no send occurred;
an unresolved receipt stays blocked. Keep the receipt token for recovery.

After a definite rejection, correct the cause (for example, wait for quota to
recover), then run a fresh preview and use its new token. The old token remains
consumed. This does not reopen a `sending` or uncertain receipt.

Removing the source redacts receipt text, confirmation tokens and provider
responses in the same transaction as source removal. Only an opaque operation
fingerprint and a non-sendable `removed` tombstone remain. Reimporting the channel
cannot reopen that operation or reuse its old confirmation.
