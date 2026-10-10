---
title: Review an Inbox Rollout
description: Review provider capabilities, safety boundaries, and recovery before enabling native inbox controls.
last_edited: "2026-10-08"
---

# Review an inbox rollout

Native inbox control is unreleased. This checklist helps an operator decide
whether to enable it for an exact source. Implementation and synthetic tests
provide evidence for review; they do not authorize deployment, new credentials,
expanded grants, or live inbox, contact, or calendar changes.

## Establish the installed boundary

1. Record the installed daemon version, API schema, and caller-specific
   operation discovery. A merged PR or a tool name on another daemon does not
   establish what the selected daemon exposes.
2. Read capabilities for each selected source. Record unsupported,
   permission-required, and unavailable actions separately. Keep source and
   account identity exact; IMAP targets also need mailbox, UIDVALIDITY, and UID.
3. Compare sync activity and per-item failures with committed archive state.
   Record provider ingestion completeness as unknown unless source-specific
   evidence establishes it. A recent `last_sync_at` or analytics publication
   does not prove that every provider message has been ingested. Compare
   `provider_ingestion` with candidate availability and SQL `cache` metadata;
   see [source sync status](../api-server.md#get-apiv1sourcesstatus) and
   [SQL queries](../api-server.md#post-apiv1query) for their separate meanings.
4. Verify a recoverable archive backup and the procedure for stopping new
   mutations without losing stored receipts. Review the
   [backup guide](backup.md) before changing an installed archive.

## Check dependencies across the workflow

Review each boundary used by the selected workflow. Record evidence from the
selected daemon, provider, and client. The linked references own the exact
contracts and recovery rules.

| Boundary | Required evidence | Limit or dependency | Owning reference |
|---|---|---|---|
| Provider target | Live capabilities and readback identify the exact existing message, mailbox, folder, or tag. | Unsupported actions and missing permissions cannot be replaced with guessed state or new provider objects. | [Inbox control](../api-server.md#inbox-control-unreleased) |
| Caller authority | Caller-specific discovery and the daemon authorize the selected source and action. | Read, draft, profile, and calendar permissions remain separate; tool presence alone does not authorize a write. | [MCP write controls](chat.md#write-controls) |
| Provider ingestion | Source-specific evidence accounts for ingestion gaps and reported sync errors. | A recent sync timestamp cannot establish completeness. | [Source status](../api-server.md#get-apiv1sourcesstatus) |
| Archive and search | Message details and the selected analytics generation agree after publication. | Cache publication resolves archive/search differences, not missing provider ingestion. | [SQL cache metadata](../api-server.md#post-apiv1query) |
| Contact publication | Publication preview and remote readback through sync confirm the selected address book and properties. | A local name does not establish the remote vCard's `FN`; address-book permissions and conflict handling still apply. | [Selected-person publication](people-carddav.md#sync-and-publish-selected-people) |
| Events delivery | The intended native client route proves discovery, subscription, callback verification, revocation, expiry, retry, deduplication, and restart recovery with synthetic data. | Native Events requires the owner credential. A client using a separate gateway also needs independent evidence of that gateway's forwarding. | [Events](chat.md#events) |
| Mutation recovery | The original intent, signed preview, per-item keys, receipts, and native readback establish each outcome. | Daemon leases do not exclude other provider clients. Inspect unknown outcomes before creating another intent; reconciliation performs no new provider mutation. | [Inbox recovery](../api-server.md#inbox-control-unreleased) |

## Review the proposed permissions and behavior

- Select the smallest source/action grants needed. Preserve existing profile,
  calendar, and HTTP write opt-ins; source read or draft grants do not authorize
  tagging or contact changes.
- Review existing native tag mappings. Todo, reply-needed, Watch, Delegated,
  uncertain, and conflicting classifications retain Inbox. Classification does
  not mark read, send, create a folder, or enable automatic archive rules.
- Review preview and confirmation through the intended client. Preserve the
  whole signed proposal and per-item keys. Reading context and previewing must
  produce zero provider writes.
- Record upstream concurrency limits. Daemon leases exclude local sync but
  cannot stop another provider client. Graph's `If-Match` behavior is not an
  established atomic-write guarantee. Preserve user-authored Beeper composer
  text when the provider cannot offer an atomic edit precondition.
- Review Contacts and Events independently. A local contact rename does not
  establish the remote vCard's `FN`; verify publication preview and remote
  properties. Verify Events through the intended native client transport,
  including revocation and delivery recovery. If the client uses a gateway,
  verify its forwarding independently; direct backend checks do not prove it.

## Approve and observe a bounded canary

The operator must approve any deployment and live write separately. Choose one
explicit synthetic target on an approved test account before considering a
real inbox. Keep its original request, preview, operation key, and receipt.

Compare native readback and archive reconciliation with the intended change.
Confirm Inbox, read state, unrelated tags, and user-authored drafts are
preserved where required. Check subsequent sync and distinguish archive
changes from analytics-cache publication.

Stop new writes on identity, authority, revision, or provider-state conflicts.
For a lost response, inspect the original receipt or replay the exact request
with the same key. Inspect or reconcile an unknown outcome before creating a
new intent. Reconciliation never sends another provider mutation; receipt
retention is part of recovery.

Use the [HTTP inbox contract](../api-server.md#inbox-control-unreleased) for
exact fields, errors, provider limits, and partial-batch recovery, and the
[MCP guide](chat.md#inbox-control-unreleased) for client confirmation.
