---
last_edited: "2026-09-08"
title: Person Briefs
description: Remember what someone recently shared before your next conversation, with links to the archived sources.
---

A person brief summarizes what someone recently shared and links back to the
archived messages behind it. Read one before a call or message to refresh your
memory on what was going on and what you might ask about next.

Briefs draw from the person's own messages across supported chat and text
sources: Apple Messages/iMessage, Beeper, Discord, Facebook Messenger, Google
Messages, iMazing Messages, Inline, Matrix, Slack/Slackdump, SyncTech SMS,
Teams, Telegram, and WhatsApp. Email, meeting transcripts, documents, and your
own replies are excluded, and an email-only contact cannot receive a brief yet.

## Generate your first brief

Before generating a brief, meet these prerequisites:

1. [Configure a provider](/docs/usage/people-automation/#configure-a-provider)
   and consent to sending it archive text. Make sure the provider profile allows
   sensitive content and includes `conversation_text` in its allowed sources.
2. Restart the daemon after enabling sweeps or changing the selected provider.
3. Find the person's stable profile ID with `msgvault person list`. If they do
   not have a profile yet, [promote them](/docs/usage/people/#promote-a-durable-person)
   first.

Then enroll the person, generate the brief, and read it, replacing `7` with their
profile ID:

```bash
msgvault person brief enroll 7 --track
msgvault person brief generate 7
msgvault person brief show 7
```

Enrollment turns on briefs for that person. Passing `--track` also enables
profile tracking if it is not already on; without `--track`, the person must
already be tracked. Tracking alone does not enable briefs.

Running `generate` sends eligible archive text to your selected provider, spends
its configured budget, and waits for the attempt to finish. If no eligible
messages exist within the profile's source and date limits, no brief is
produced. A separate profile extraction step may update facts from a limited set
of messages, while any attributes suggested by the brief stay in its saved
structure for your review and do not change profile facts.

If brief generation fails, profile extraction still commits. Automatic retries
follow the sweep's retry delay instead of retrying on every daemon tick, and
running `person brief generate` again bypasses that delay.

## Read and manage briefs

In the Web UI, open **People**, select the person's **Details** tab, and find
**Last time we talked**. From that card, you can enroll the person or generate a
brief, expand a sentence to see its sources, and open the history to inspect
version dates and status. To read earlier paragraphs and sources, run
`msgvault person brief history 7 --json`. If a source cannot be tied to an
individual sentence, the card labels it as a source for the whole brief. Turning
enrollment off hides the brief and history in the card, while saved versions
remain available through the CLI and API.

In the TUI People browser, the Overview tab shows the brief. Press `b` to read
its details and sources, then press `b` or `Esc` to return. Press `:` to run
`brief enroll`, `brief generate`, or `brief reject <reason>`. Enrolling in the
TUI also turns on tracking.

| Command | Effect |
|---|---|
| `msgvault person brief show 7` | Read the current brief |
| `msgvault person brief history 7 --limit 5` | List the five newest versions with their dates and status |
| `msgvault person brief history 7 --json` | Read saved paragraphs and sources as JSON |
| `msgvault person brief generate 7` | Generate now, using provider budget |
| `msgvault person brief reject 7 --reason "merges two different threads"` | Remove the current brief from view and let the next eligible run replace it; the reason is optional |
| `msgvault person brief unenroll 7` | Stop future generation while keeping saved versions |

Every command accepts `--json` to print the daemon's response. MCP assistants
can read the current brief with `get_person_profile`, though MCP does not
generate, reject, or enroll briefs. For response details, see
[MCP brief text](/docs/usage/chat/#brief-text-is-data) and the
[brief API](/docs/api-server/#get-apiv1peopleidbrief).

## When briefs refresh

Scheduled sweeps generate a brief for any enrolled, tracked person who has none
or whose latest brief was rejected. Otherwise, sweeps wait for new activity
along with either a brief that is at least seven days old or a planned contact
date within three days. The sweep checks these conditions even when profile
extraction has already caught up with the archive.

Running `generate` bypasses those timing and new-activity checks, though it
still requires enrollment, an enabled provider with consent, eligible messages,
and available budget. Regenerating saves a new dated version and keeps earlier
versions in history, while rejecting a brief hides it until a replacement is
generated.

The default paragraph limit is 560 characters. You can change generation timing,
input limits, and output limits in
[brief configuration](/docs/configuration/#peoplesweepbrief), or turn off brief
generation for everyone without removing enrollments.

## Sources and saved versions

Each retained statement cites archive items. If a source is later deleted,
edited, or reassigned, its citation is marked unsupported. To fit the paragraph
limit, msgvault trims whole items, and `dropped_item_count` records both those
removals and any content rejected during validation.

Saved briefs live in your archive; they are not published to CardDAV or included
in people search. Generating a brief sends message text to your consented
provider, whereas reading an existing one makes no provider call. Suggested
profile facts go through the same evidence checks as other automatically
extracted facts.

When you merge profiles, msgvault keeps the surviving person's brief history and
removes the other person's versions. The absorbed person's enrollment transfers
only if the survivor was not already enrolled.
