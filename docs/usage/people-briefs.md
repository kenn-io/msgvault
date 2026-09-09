---
last_edited: "2026-09-08"
title: Person Briefs
description: Remember what someone recently shared before your next conversation, with links to the archived sources.
---

A person brief gives you a short summary of what someone recently shared,
with references to the archived messages behind it. Read it before a call or
message to remember what was going on and what you might ask about next.

Briefs currently use the person's own messages from supported chat and text
sources: Apple Messages/iMessage, Beeper, Discord, Facebook Messenger, Google
Messages, Slack/Slackdump, SyncTech SMS, Teams, and WhatsApp. Email, meeting
transcripts, documents, and your own replies are excluded. An email-only
contact cannot receive a brief yet.

## Generate your first brief

First, [configure a provider](/docs/usage/people-automation/#configure-a-provider)
and consent to sending it archive text. Restart the daemon after enabling sweeps or
changing the selected provider. The profile must allow sensitive content and
include `conversation_text` in its allowed sources.

Find the person's stable profile ID with `msgvault person list`. If they do not
have a profile yet, [promote them](/docs/usage/people/#promote-a-durable-person)
first. Replace `7` below with that profile ID:

```bash
msgvault person brief enroll 7 --track
msgvault person brief generate 7
msgvault person brief show 7
```

Enrollment enables briefs for that person. `--track` also turns on profile
tracking if needed; without it, enrollment requires the person to be tracked
already. Tracking alone does not enable briefs.

Generation sends eligible archive text to your selected provider and spends
its configured budget. The separate profile extraction step may update facts
from a limited set of messages. Attributes suggested by the brief stay in its
saved structure for your review; they do not change profile facts. The command
waits for the attempt to finish. If no eligible messages exist within the profile's source and date limits, it produces no brief.

If brief generation fails, profile extraction still commits. Automatic retries
use the sweep's retry delay instead of repeating the brief on every daemon tick.
Running `person brief generate` again bypasses that delay.

## Read and manage briefs

In the Web UI, open **Directory**, select the person, and find **Last time we
talked** on the Overview tab. Enroll or generate there, expand a sentence to see
its sources, and use the history to inspect version dates and status. To read
earlier paragraphs and sources, run `msgvault person brief history 7 --json`.
If a source cannot be tied to an individual sentence, the card labels it as a source for the whole
brief. Turning enrollment off hides the brief and history in this card; saved
versions remain available through the CLI and API.

In the TUI People browser, the Overview tab shows the brief. Press `b` to read
its details and sources, then `b` or `Esc` to return. Press `:` to enter
`brief enroll`, `brief generate`, or `brief reject <reason>`. TUI enrollment also
turns on tracking.

| Command | Effect |
|---|---|
| `msgvault person brief show 7` | Read the current brief |
| `msgvault person brief history 7 --limit 5` | List the five newest versions with their dates and status |
| `msgvault person brief history 7 --json` | Read saved paragraphs and sources as JSON |
| `msgvault person brief generate 7` | Generate now, using provider budget |
| `msgvault person brief reject 7 --reason "merges two different threads"` | Remove the current brief from view and let the next eligible run replace it; the reason is optional |
| `msgvault person brief unenroll 7` | Stop future generation while keeping saved versions |

Every command accepts `--json` to print the daemon's response. MCP assistants
can read the current brief with `get_person_profile`; MCP does not generate,
reject, or enroll briefs. See [MCP brief text](/docs/usage/chat/#brief-text-is-data)
and the [brief API](/docs/api-server/#get-apiv1peopleidbrief) for response details.

## When briefs refresh

Scheduled sweeps generate a brief for an enrolled, tracked person who has none,
or whose latest brief was rejected. Otherwise, they wait for new activity and
either a brief at least seven days old or a planned contact date within three
days. The sweep checks these conditions even when profile extraction has
already caught up with the archive.

`generate` bypasses those timing and new-activity checks. It still requires
enrollment, an enabled provider with consent, eligible messages, and available
budget. Regeneration saves a new dated version and keeps earlier versions in
history. Rejecting a brief hides it until a replacement is generated.

The default paragraph limit is 560 characters. You can change generation timing,
input limits, and output limits in
[brief configuration](/docs/configuration/#peoplesweepbrief), or disable
brief generation for everyone without removing enrollments.

## Sources and saved versions

Each retained statement cites archive items. If a source is later deleted,
edited, or reassigned, its citation is marked unsupported. Msgvault trims whole
items to fit the paragraph limit; `dropped_item_count` records those removals
and content rejected during validation.

Saved briefs live in your archive. They are not published to CardDAV or included
in people search. Generating one sends message text to your consented provider;
reading an existing one makes no provider call. Suggested profile facts pass
through the same evidence checks as other automatically extracted facts.

Merging profiles keeps the surviving person's brief history. The other
person's versions are removed, and their enrollment transfers only if the
survivor was not already enrolled.
