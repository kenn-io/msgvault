---
last_edited: "2026-10-03"
title: Chatwoot
description: Archive shared Chatwoot inboxes, employee and contact messages, media, and calls.
---

# Archive Chatwoot inboxes

Archive conversations from a self-hosted Chatwoot instance or Chatwoot Cloud.
Each inbox becomes a separate source for search and account filters. Messages
retain the contact or employee who sent them. Voice notes retain available
transcripts; voice calls also appear as linked meetings.

!!! note "Available on main"
    Chatwoot ingestion is available in builds from `main`. It is not part of the
    latest release documented in the [changelog](../changelog.md).

## Connect an account

1. Create a user API access token in your Chatwoot profile. The account API
   follows that user's permissions. Use a user with access to every inbox you
   want to archive; an agent-bot token cannot enumerate message history.
2. Find the numeric account ID in your Chatwoot account URL. Set the token in
   the environment of the machine running the msgvault daemon. For example,
   set `MSGVAULT_CHATWOOT_TOKEN` in the daemon service's environment and restart
   the service. The CLI does not transfer a caller's token to a remote daemon.
3. Add a profile to `config.toml`:

    ```toml
    [[chatwoot]]
    identifier = "support"
    url = "https://chatwoot.example.com"
    account_id = 9
    api_key_env = "MSGVAULT_CHATWOOT_TOKEN"
    enabled = true
    schedule = "*/30 * * * *"
    # inboxes = [7, 8]           # empty includes all accessible inboxes
    # exclude_inboxes = [8]     # exclusions win
    # self_agent_ids = [201]    # only agents representing the archive owner
    ```

4. Register the inboxes, then sync:

    ```bash
    msgvault add-chatwoot support
    msgvault sync-chatwoot support
    ```

See the [configuration reference](../configuration.md#chatwoot) for all fields.
Re-run `add-chatwoot` after adding inboxes or changing the include filter. Sync
processes selected inboxes already registered in the archive. Renaming the
profile or inbox does not create another archive source; instance URL, account
ID, and inbox ID determine its identity.

## Choose what to capture

Sync includes resolved, pending, snoozed, and open conversations. It preserves
private notes and system activity by default. Set `include_private = false`
before your first sync to exclude private notes. This controls ingestion; it
does not remove notes already archived. Notes skipped while it was off are
fetched only by `sync-chatwoot --full` after turning it back on.

Employees remain distinct from contacts and bots. The current conversation
assignee describes routing and does not replace historical sender attribution.
An outgoing customer reply is not automatically attributed to the archive
owner. Set `self_agent_ids` only for Chatwoot users who represent that owner.
Removing an agent from the list, or removing its account identity, un-marks
its earlier messages and calls. Captain assistant replies keep their own sender.
Contacts without an email address retain their phone or provider identity.

Media downloads are enabled by default, with a 250 MiB cap per attachment.
Set `media = false` for metadata and existing transcripts only, or use
`sync-chatwoot --no-media` for one run; files it skipped are fetched by a later
`--full`. Location and fallback attachments can
contain metadata without downloadable files. Source access, unavailable files,
size limits, and download failures can leave metadata without stored bytes.
Failed downloads retry during later syncs for seven days.
Media destinations are checked against the shared network safety policy and
each redirect is checked and pinned before connecting. Private media addresses
are allowed only when the URL origin exactly matches the configured Chatwoot
origin, which supports self-hosted instances without trusting other media URLs.

```bash
msgvault sync-chatwoot support --inbox 7 --limit 100
msgvault sync-chatwoot support --full
```

`--limit` bounds messages handled per conversation in this run. Unfinished
history resumes on the next run, including a full reconciliation interrupted
by the limit. `--full` rereads every conversation's history in place, which
also picks up edits to messages that are already archived; later full runs
resume that saved work until it completes. With no profile argument, commands select
all configured profiles. A failing profile or inbox does not prevent healthy
ones from being processed.

## Find voice notes and calls

Voice-note transcripts are searchable with their chat messages when Chatwoot
provides them. A voice call retains its original timeline message and a linked
meeting record, with observed status, direction, contact, employee, duration,
and transcript. Missed calls still appear; an unavailable duration or transcript
stays unavailable.

Twilio recordings come from Chatwoot's stored recording URL. WhatsApp call
recordings can arrive as audio attachments. msgvault archives both source
shapes and does not require Twilio credentials or a transcription API key.
Chatwoot's own transcription settings and availability determine whether it
supplies text. Optional msgvault attachment processing has its own configuration
and consent; see [document indexing](../usage/document-indexing.md).

## Understand sync limits

Each sync lists conversations by latest activity and stops at the ones it has
already archived, so an unchanged inbox costs a couple of requests no matter
how large it is, plus one for each conversation still on the recheck list
below. Only conversations with new messages are read. Unfinished history and
new conversations take turns, so a long history can't hold up newer ones.

A recording or transcript can arrive after a call or voice note without
updating the conversation's activity. msgvault rechecks every call, and audio
still waiting for a transcript, for seven days after the message, then stops.
Files that are stored or skipped are not rechecked.

Every 24 hours by default, a reconcile lists every conversation. It rereads the
whole history of each conversation active since the previous reconcile, which
catches messages that were saved out of order, and reads any other
conversation whose newest message is missing from the archive. History of
quiet conversations that is already archived is reread only by `--full`.

Chatwoot's combined message-ID range API is required to enumerate history
reliably when message timestamps and IDs have different order. The importer
checks that contract before depending on it and reports incompatible versions
instead of claiming complete history. API access can also be disabled by the
Chatwoot account's plan or security settings. Permission changes can reduce the
visible archive scope.

The integration reads messages and media. Its sync command does not send
messages or delete source records. Read the [CLI reference](../cli-reference.md#sync-chatwoot)
for flags and the [meeting guide](../usage/meetings.md) for meeting queries.
