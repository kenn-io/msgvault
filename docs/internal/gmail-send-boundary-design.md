# Proposed Gmail sending boundary

Status: proposal, unbuilt. Msgvault creates and manages drafts; the owner sends
from their mail client. This note does not authorize a sending implementation.

[Issue #666](https://github.com/kenn-io/msgvault/issues/666) explicitly descopes
sending. Its slice plan covers drafts, restricted access, and sender selection.
[PR #1023](https://github.com/kenn-io/msgvault/pull/1023) preserves the operator
send boundary when exposing draft operations through MCP. Keep that boundary
until maintainers approve a separate sending slice.

## What a future slice would need

1. Add an explicit, disabled-by-default per-source send policy on the daemon
   host. Draft opt-in must never enable sending. Client flags, requests, and
   delegated credentials cannot change host policy.
2. Add a separate `draft.send` permission. Check the source and the current
   draft sender through `Grant.AllowsSender`, before returning protected draft
   content, policy results, or contacting Gmail. A `draft.create`, `draft.edit`,
   or `draft.delete` grant must never imply permission to send.
3. Require the caller's current revision and a managed, active Gmail draft with
   no pending operation. Check the current provider message before sending.
   An external edit requires review and a new revision; do not send it silently.
4. Recheck the selected From against primary or accepted Gmail send-as entries.
   `treatAsAlias`, a different domain, and SMTP relay settings do not replace
   verification. The local confirmed identity and frozen sender grant remain
   separate checks.
5. Request an approved send-capable OAuth scope only through explicit owner
   re-consent. Preserve the union of existing permissions; first resolve scope
   aliases and test expanded Google `userinfo.*` names. Broad existing tokens
   must still obey the host send policy and separate agent permission.
6. Record a durable operation intent before submission and an outcome afterward,
   including actor/grant identity, source, sender, draft ID, revision, provider
   IDs, and timestamps. Exclude message text, tokens, and SMTP credentials from
   audit logs. Preserve the sent message and reconcile the draft's lifecycle.
7. Treat a lost response as uncertain. Never automatically retry a send or
   infer success merely because a draft disappeared. Define reconciliation
   evidence and an owner recovery workflow before adding a send endpoint.

## Decisions still required

Maintainers must approve the provider API and supported entry points, the exact
host configuration field, audit storage and retention, scope upgrade UX, and
recovery rules. Decide whether the first slice is owner-only CLI or also permits
sender-scoped agents through HTTP and MCP. IMAP needs a separate submission and
Sent-copy design; existing draft support supplies neither SMTP configuration nor
send authorization.

The current draft contracts are owned by the
[CLI reference](../cli-reference.md#draft-reply). Gmail's
[send-as resource](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.settings.sendAs)
and [draft send method](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.drafts/send)
define provider behavior, not msgvault authorization.
