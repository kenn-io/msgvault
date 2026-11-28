---
last_edited: "2026-10-14"
title: Chatwoot
description: Archive shared Chatwoot inboxes, employee and contact messages, media, and calls.
---

# Archive Chatwoot inboxes

Archive conversations from a self-hosted Chatwoot instance or Chatwoot Cloud.
Each inbox becomes a separate source for search and account filters, and
messages keep the contact or employee who sent them. Voice notes keep any
available transcripts, and voice calls also appear as linked meetings.

## Connect an account

Any non-loopback Chatwoot instance must use HTTPS.

1. Create a user API access token in your Chatwoot profile. The account API
   follows that user's permissions. Pick a user with access to every inbox you
   want to archive; an agent-bot token cannot enumerate message history.
2. Find the numeric account ID in your Chatwoot account URL, then set the token
   in the environment of the machine running the msgvault daemon. For example,
   set `MSGVAULT_CHATWOOT_TOKEN` in the daemon service's environment and
   restart the service. The CLI does not transfer a caller's token to a remote
   daemon.
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

4. Register the inboxes and run your first sync:

    ```bash
    msgvault add-chatwoot support
    msgvault sync-chatwoot support
    ```

See the [configuration reference](../configuration.md#chatwoot) for all
available fields. Re-run `add-chatwoot` after adding or renaming inboxes or
changing the include filter. Sync processes selected inboxes already registered
in the archive. Renaming the profile or inbox does not create another archive
source; instance URL, account ID, and inbox ID determine its identity.

## Choose what to capture

Sync includes resolved, pending, snoozed, and open conversations along with
public system activity. By default, msgvault leaves out private notes unless
you set `include_private = true`. Once private notes are archived, sync refuses
to run with `include_private` turned off. If you turn it on later, run
`sync-chatwoot --full` to backfill skipped notes.

When someone deletes a message in Chatwoot, Chatwoot replaces its text and
removes its files. When a later sync sees the deletion, msgvault keeps your
archived text, files, and any linked call meeting, and marks them as deleted
from the source. Messages deleted before their first sync are not archived.

In the web UI, call details include **Open meeting** to open the recording and
transcript, and meeting details include **Open conversation** to return to the
original call and its messages.

Employees stay distinct from contacts and bots, and the current conversation
assignee describes routing rather than replacing historical sender attribution.
An outgoing customer reply is not automatically attributed to the archive
owner; set `self_agent_ids` for Chatwoot users whose sent replies and outgoing
calls represent that owner. For an inbound call, the customer is the organizer
and the handling agent is an attendee. Captain assistant replies keep their own
sender, and contacts without an email address retain their phone or provider
identity.

If you remove an account identity, msgvault un-marks its earlier messages and
calls and keeps it removed during later syncs. Removing an agent from
configuration drops importer-owned identity evidence, while removing and then
reintroducing its configured ID grants ownership again. Manually confirmed
identities remain.

Media downloads are enabled by default, with a separate 4 GiB per-file cap for
call recordings set by `recording_max_bytes`, and a 250 MiB cap for other media
set by `max_media_mb`. Setting either value to zero selects its default. To
archive only metadata and existing transcripts, set `media = false`, or run
`sync-chatwoot --no-media` to defer downloads for one run. Location and
fallback attachments can contain metadata without downloadable files, and
source access, unavailable files, size limits, or download failures can leave
metadata without stored bytes. If a replacement recording fails to download,
the archive keeps the existing readable recording.

Eligible recent files retry during normal syncs for seven days from the
resource's first failure, and signed URL rotation and reconciliation preserve
that deadline. Older files need `--full`, which also allows another attempt
after expiry without extending automatic retries. An oversized call recording
retries after you raise `recording_max_bytes` enough or the resource changes,
while other oversized media uses `max_media_mb`.

msgvault checks media destinations against the shared network safety policy and
checks and pins each redirect before connecting. Private media addresses are
allowed only when the URL origin exactly matches the configured Chatwoot
origin, which supports self-hosted instances without trusting other media URLs.
Chatwoot builds file URLs from its `FRONTEND_URL` setting, so set `url` to that
same origin. Files served from another private host, such as self-hosted MinIO
storage on an internal address, fail to download and retry like other failed
files.

```bash
msgvault sync-chatwoot support --inbox 7 --limit 100
msgvault sync-chatwoot support --full
```

`--limit` bounds how many history messages per conversation this run processes,
while pending media and call refreshes run in addition to that limit. Any
unfinished history resumes on the next run, including a full reconciliation
interrupted by the limit. Running `--full` rereads every conversation's history
in place, picking up edits to messages already in the archive, and later full
runs resume that saved work until it completes. If you omit the profile
argument, commands select all configured profiles, and a failing profile or
inbox does not stop healthy ones from being processed.

## Find voice notes and calls

When Chatwoot provides voice-note transcripts, you can search them alongside
their chat messages. A voice call keeps its original timeline message and a
linked meeting record with observed status, direction, contact, employee,
duration, and transcript. Missed calls still appear, and an unavailable
duration or transcript stays unavailable.

Twilio recordings come from Chatwoot's stored recording URL, while WhatsApp
call recordings can arrive as audio attachments. msgvault archives both source
shapes as real audio in shared attachment storage, detects the served audio
format, and requires neither Twilio credentials nor a transcription API key.
Chatwoot's own transcription settings and availability determine whether it
supplies text. Optional msgvault attachment processing has its own
configuration and consent; see
[Docbank document search](../configuration.md#search-document-text-through-docbank).

### Send call audio to Docbank

Stored call recordings use the
[shared audio delivery path](../configuration.md#send-stored-audio-to-docbank).
To send them, enable `[integrations.docbank]` and set
`all_sources_upload_consent = true`. Without that consent, discovery records
local state and sends no audio. An optional `asr_profile` requests processing
for recordings without source text, while Docbank controls its own processing
consent.

The current audio path accepts WAV and MP3 files up to 1 GiB. Larger recordings
remain archived under the separate recording cap, and unsupported formats and
sizes keep a local status without an upload. Raising `recording_max_bytes`
changes capture only.

## Understand sync limits

Each sync lists conversations by latest activity, keeping a ten-minute overlap
open for equal timestamps until a completed scan settles it. Afterward, a quiet
inbox with no saved history or recent artifact work costs one activity listing
plus the account's agent lookup. An inbox that was initially empty archives its
first conversation on the next sync. Saved artifact work adds a conversation
detail request when the conversation is absent from the listing, plus range
reads and exact-ID reads for capped or oversized responses. The saved artifact
position gives later IDs a turn even while earlier calls remain pending.

A 10,000-request budget bounds each inbox sync, and unfinished history takes
turns with new conversations so a long history cannot hold up newer ones. If
one conversation becomes inaccessible, sync continues archiving accessible
conversations. A conversation-detail 404 retires that conversation's saved
work, an artifact-range 404 clears its artifacts, and an exact-artifact 404
retires that artifact. A history-range 404 retains pending history and reports
failure, as do other access errors. To resume it, restore the token user's
inbox or team access; `--full` uses the same authorization.

A recording or transcript can arrive after a call or voice note without
updating the conversation's activity. msgvault rechecks every call, and any
audio still waiting for a transcript, for seven days after the message, then
stops. Stored files and account-policy skips leave the file refresh list. After
raising a size cap, use `--full` to reread skipped files; recording
replacements can also retry during the seven-day call window.

Every 24 hours by default, a reconcile lists every conversation and rereads the
whole history of each conversation updated since the previous reconcile began.
Chatwoot's commit-time `updated_at` catches messages saved out of order even
when their creation times move activity backward. Backdated or out-of-order
activity can wait up to `reconcile_interval_hours`, which defaults to 24 hours.
Missing update evidence causes a conservative reread, and reconcile also reads
any other conversation whose newest message is missing from the archive. Quiet
conversations have their archived history reread only by `--full`. An excluded
private tail can still cause a range reread during overlap or reconciliation,
and future-dated conversations can add listing and head checks until clocks
agree, within the existing request budget.

To enumerate history reliably when message timestamps and IDs have different
order, msgvault requires Chatwoot's combined message-ID range API. It returns
at most 1,000 messages per range, a limit unchanged since Chatwoot added the
range in 2023, and msgvault splits ranges that reach it. The importer checks
that the server honors combined ranges before depending on them and reports
incompatible versions instead of claiming complete history. Your Chatwoot
account's plan or security settings can also disable API access, and permission
changes can reduce the visible archive scope.

This integration reads messages and media, and its sync command does not send
messages or delete source records. Read the
[CLI reference](../cli-reference.md#sync-chatwoot) for flags and the
[meeting guide](../usage/meetings.md) for meeting queries. To remove archived
bytes now excluded by your media settings while keeping message text, use
[purge-excluded-media](../cli-reference.md#purge-excluded-media).
