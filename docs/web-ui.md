---
last_edited: "2026-10-02"
title: Web UI
description: Browse messages and files, maintain people, and monitor archive work from your browser.
---

# Web UI

The Web UI lets you search across your archive, read messages, browse files,
maintain your contact directory, and see whether sync and indexing work has
finished. It is embedded in the release binary and served by `msgvault serve`;
you do not need a separate web application process.

| Your question | Workspace |
|---|---|
| Where is that message, conversation, or meeting? | Everything |
| Who have I been in contact with? | Relationships, People, and Domains |
| Where is an attachment, image, or video? | Files |
| What do I know about this person? | Directory |
| Which identity matches or profile facts need my decision? | Reviews |
| Can I return to this search later? | Saved views |
| Did sync, enrichment, or indexing finish? | Operations and Sources |
| What is staged for deletion? | Deletions |
| How do I change the daemon's configuration? | Settings |

The sidebar groups the workspaces as **People** (Relationships, Directory,
Reviews), **Archive** (Everything, Files, Saved views), and **Manage** (Sources,
Operations, Deletions, Settings). Its footer shows archive status and opens
**Keyboard shortcuts**. Choose **Collapse sidebar** to shrink it to an icon rail;
the browser remembers the choice. Below 900 pixels wide the sidebar becomes a
menu that **Open navigation** opens.

The top bar holds global search, the theme toggle, and the **Display** menu.
The Display menu sets a temporary density (**Auto**, **Compact**, or
**Comfortable**) and, after you change the theme, offers **Use daemon theme**.
Both overrides last for this tab only. [Settings](#settings-and-restart-behavior)
holds the saved defaults.

Global search works from every workspace. Press `/` outside a text field to
focus it. In Everything and Files, results follow your query as you type, and
`Enter` moves focus to the results. In any
other workspace, `Enter` opens Everything with your query and search mode.

## Start and discover the URL

```bash
msgvault build-cache
msgvault serve
```

`build-cache` prepares the analytical tables before you open the browser. You
can also run `serve` directly and let the default startup maintenance build a
missing or stale cache in the background.

Foreground startup prints `API server: http://HOST:PORT`. The default
`server.api_port = 0` chooses a free port. Local CLI commands discover that
address through the private daemon runtime record under the configured msgvault
home; browsers should use the printed URL. Configure a fixed port for a stable
remote bookmark:

```toml
[server]
bind_addr = "127.0.0.1"
api_port = 8080
```

The default loopback deployment is trusted. If an API key is active and the
request is not loopback-trusted, `/` still loads the public shell and the UI asks
for the key. A successful login creates an expiring, in-memory browser session.
Daemon restarts, logout, expiry, and API-key activation invalidate sessions.
Existing bearer-key API clients are unchanged.

## Remote access and HTTPS

For remote access, set a strong API key and bind deliberately:

```toml
[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = "replace-with-a-long-random-key"
```

HTTPS at a reverse proxy is recommended. Forwarded scheme and host headers are
accepted only from explicitly trusted proxy addresses or CIDRs:

```toml
[server]
trusted_proxies = ["127.0.0.1", "192.0.2.8/32"]
```

Do not add a whole client network merely to silence proxy warnings. When HTTPS
is known through a trusted proxy, the browser cookie uses `Secure`. Plain HTTP
on an encrypted private network is supported as an explicit tradeoff, but the UI
warns that its session cookie travels without TLS. `HttpOnly` and
`SameSite=Strict` do not encrypt that traffic.

## Explore and search

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/static/analytical-light-compact-darwin.png" alt="Everything workspace with the grouped sidebar, global search, Save view button, and compact email rows in light theme" loading="lazy">
  <figcaption>Browse archived email in Everything. Select the image to view it at full size.</figcaption>
</figure>

The screenshots use a curated public Enron research-data fixture. Authentic
names and message text are intentional; the repository's `docs-fixtures`
branch records provenance, attribution, and the content review. This fixture
contains email only; it does not illustrate chat, calendar, or attachment content.

Everything opens as a compact table of logical entries, newest first: one row
per email, calendar event, meeting note, other durable item, or chat
conversation. Raw chat fragments appear only after drilling into a
conversation. Search, Filters, Show as, and Group by form a shareable view.
Ordinary tabs use short URLs such as `?workspace=everything&mode=full_text`.
Filters, layout changes, and the selected item appear in the link only when
they differ from the defaults. Keyboard focus, scroll position, and choices
from other workspaces stay out of the link; browser history keeps them so Back
and Forward restore them.

One toolbar sits above the results:

- **Filters** opens the filter panel.
- **Show as** switches between Table, Timeline, and Files. Choosing Files opens
  the Files workspace.
- **Group by** adds a grouping level. Add more levels to group within groups.
- **Sort** offers Newest first, the only order Everything has today.
- **Columns** shows or hides table columns. It appears for the ungrouped table.
- **Preview position** appears when the results are wide enough for a side
  preview.
- The count at the end reports items, or groups when the view is grouped.

Each active search, filter, and grouping appears as a removable chip below the
toolbar. Select the chip's remove button, such as **Remove search** or **Remove
Person filter**, to drop it. **Save view…** in the page header saves the current
context; see [Saved views](#saved-views).

Selecting rows opens a selection bar. **Review for deletion…** opens the
[Deletions](#deletions) workspace. The button is disabled when the daemon
reports that staging is unavailable for the selection, and the bar states the
reason beside it, for example "None of the selected items can be deleted from
their source." Export and **Open selection in source** explain their
unavailability the same way.

Search mode is always explicit:

- **Full text** matches words in the text index.
- **Semantic** ranks only content covered by the current embedding generation.
- **Hybrid** combines keyword matches with semantic ranking where it is
  available.

A coverage notice below the toolbar reports semantic coverage. Disabled, building, stale,
incomplete, unavailable, and ready are different states; msgvault never silently
changes the requested mode. Semantic-only results cannot include unembedded
content. Hybrid retains full-text coverage and labels the semantic contribution.

When a search fails, the UI explains what happened and keeps your query.
**Timed out** means the selected search backend did not finish within the request budget; it is an
error, not an empty result, and the query and filters remain available to retry.
**Incompatible mode** means the daemon, browser contract, or current index
cannot safely honor the selected search mode. Update or rebuild the named
component, or deliberately select a supported mode. Msgvault does not quietly
substitute full-text search for either state.

## Read messages

Open a `/messages/<id>` link to go directly to an archived message. CLI
`search --json` and `show-message --json` responses include a `web_url` when
the selected daemon has an HTTP address. The link uses that daemon's address;
the person opening it still needs access to the archive. Links to chat messages
open a bounded part of the conversation around the selected message, with
controls to load earlier or later messages.

Click an entry in Everything to open its preview below the results. On wide
windows, choose **Preview position → Right** to read beside the results.
Drag the divider to resize either layout, or focus it and use the arrow keys.
Double-click the divider to reset its size. The browser remembers your layout
choice and each layout's size. Narrow windows use the preview below the results
and restore your right-side layout when there is room again.

HTML email follows the app's dark theme by replacing sender-defined text,
background, and border colors. Images keep their original colors. Choose
**Use original colors** above a message to see its authored colors on a white
background, or **Use app colors** to return to dark reading. This override
applies to the open message. Light mode preserves designed email colors.

## Meeting context and follow-ups

Filter Everything to meetings to see **Meeting activity and follow-ups**.
Select meeting rows to export their context as JSON or Markdown, with transcripts
included only when requested. Participant/domain reading panes, Directory, and
Relationships show meeting metrics and recorded actions for their current scope.
**Open archived meeting** opens the source evidence; Back restores the originating
view. See the [meeting guide](usage/meetings.md#export-context-and-read-follow-ups)
for selection limits, source coverage, unknown duration, and action filters.

## Cache states

The web tables share one analytical cache across message types. When it is missing,
building, stale, or unavailable, the UI names that state and offers the
available recovery steps. Run `msgvault build-cache` for an explicit rebuild, or leave `analytics.auto_build_cache =
true` for daemon startup to build a stale cache. With `analytics.engine =
"duckdb"`, startup fails if no usable cache can be produced.

## Files and containing context

Files is the single file view. It is a searchable table of attachment date,
filename, type, size, person or domain, source, containing item, and content
availability. Archived images and PDFs open in application-controlled viewers.
Metadata-only, missing, unsupported, and previewable content remain distinct.
From a file, navigate to its containing item and then its email or chat
conversation.

Files shares Everything's toolbar, with **Show as** set to Files. Choose Table
or Timeline there to return to Everything. **Sort** offers Newest first, Oldest
first, Filename A–Z, Filename Z–A, Largest first, and Smallest first. The
Filename field and the **Type** menu (Images, PDFs, Audio, Video, Text,
Documents, Archives, Other) narrow the table. Grouping replaces the table with
groups and hides Sort, Filename, Type, and Visual search; active Filename and
Type filters stay visible as chips.
**Save view…** saves the search, filters, and grouping, but not the Filename
filter, Type filter, or file sort.

In a person's Media & files section, choose a media gallery or file table and
narrow the relationship to **From them**, **To them**, or **Group
conversations**. These directions describe the containing messages; they do not
identify people pictured in an image.

Turn on **Visual search** to describe image or video content, or supply a JPEG,
PNG, or WebP query image. The UI discloses that the query goes to the
configured provider. It requires the separate
[visual index](/docs/usage/vector-search/#visual-attachment-search); ordinary
filename browsing does not use that provider.

### Images in email

The reader displays archived inline images and leaves remote images unloaded
until you choose **Load images**. That choice applies to the current item and
fetches images through the daemon. Moving to another item resets it.

For unattended, offline preservation of remote images, see
[Archive Remote Email Images](/docs/usage/remote-images/). Reader permission
to load an image and permission to archive remote images during ingest are
separate choices.

## People and domains

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/static/relationships-dark-comfortable-darwin.png" alt="Relationships workspace showing a selected person's activity calendar and email timeline in dark theme" loading="lazy">
  <figcaption>Select a person to explore their activity and messages.</figcaption>
</figure>

People combines identifiers backed by explicit archive identity evidence; it
does not merge records merely because their display names match. Select a
person to inspect contextual activity across email, chat, calendar events, and
meeting notes, plus the files associated with that person. The active search
and filters continue to scope both the timeline and file table.

Relationships lists People or Domains; switch with the facet control, and use
the filter field to narrow the list. For a selected person, **Messages** and
**Files** switch the view. **Open in Directory** opens the person's durable
profile, or starts promotion when none exists. **Same person…** links
identities that belong together.

People in this workspace are observed identity clusters. Source identities
that mean “me,” explicit durable profile promotion, display-name overrides, and
typed profile attributes are separate curated operations; see [People,
Profiles, and Source Identities](/docs/usage/people/).

### Directory and Reviews

Directory holds durable people: the profiles you explicitly curate and keep
across sources. Search by name, email, or organization. **Filters** opens a
panel with Contact state, Category, Organization, Primary channel, and two date
fields, **Last contacted after** and **Last contacted before**. Each active
filter also appears as a chip; select its remove button to drop it. The order
menu sorts by Name, Most recently contacted, or Least recently contacted.

Its person detail has seven sections: Overview, Profile, Organizations,
Connections, Network, Media & files, and Maintenance. The header offers **Open
relationship** (when the person has a linked source identity), **Review facts**
(opens Reviews on that person's facts), and **More actions** (Rename person,
View profile history, and Delete person). Maintenance holds
profile-maintenance tracking, CardDAV publication, and merge history. Edit
structured profile information, attributes, employment, and typed
relationships here. Curated display names also appear in message views,
analytics, and exports while source identifiers remain available.

The Overview tab's **Last time we talked** card summarizes the person's recent
chat and text messages. Enroll the person, generate a brief, and expand a
sentence to check its sources. You can reject a brief or inspect the dates and status of earlier
versions. Generation requires a consented provider and uses its budget; see
[person briefs](/docs/usage/people-briefs/)
for setup and supported sources.

The Network tab can request one, two, or three hops and optionally include
ended records. It visualizes at most 250 nodes and 500 connections, while an
always-present list groups the same connections by hop for keyboard and screen
reader use. Person and organization names in this view come from durable
profiles. Edges come only from curated typed relationships and employments
(including shared organizations), never messages, participant co-occurrence,
or inferred communication activity.

Reviews brings together identity matches, facts, and imported
relationships; the **Review type** control switches between them. Inspect the
evidence before accepting or rejecting a candidate. Conflicts between existing
profiles require an explicit merge decision. Facts starts with a person: search
by name in the **Person** field to see that person's evidence, claims,
decisions, and pins. **Open person profile** returns to their Directory entry.
Merge history and reversal follow the boundaries documented in
[People](/docs/usage/people/).

Domains provides the same activity-and-files analysis for an exact domain
fact. A domain is not treated as an inferred organization identity. Selecting
a grouped person or domain in Everything opens its inspector in the current
context, including chronologically ordered related files.

## Saved views

Saved views persist useful analytical contexts in the daemon, so the same
library is available from every authenticated browser connected to this
single-user archive. A view records its query, explicit search mode, filters,
grouping, presentation, sort, and visible columns.
Selection is intentionally not saved. The inspector stays pinned; the browser
does not save or apply inspector pin preferences.

Choose **Save view…** in the Everything or Files page header, name the view, add
an optional description, and select **Save**. A view saved from Files does not
keep the Filename filter, Type filter, or file sort. The Saved views workspace
lists each view with a summary of its query, filters, grouping, and
presentation. Each row offers **Open**, **Edit**, and **Delete**; the button
names include the view's name. **Open** applies the view in Everything, or in
Files for a Files view. **Edit** changes the name and description. **Delete**
asks you to confirm with **Confirm delete** and removes the view for every
connected browser.

Each record carries a schema version. An incompatible record remains visible,
but cannot be opened or edited: automatic migration is not attempted. Select
**Remove incompatible** for that view, confirm, and save the current context
again. Updates and deletion use
the record revision as an optimistic-concurrency guard. If another browser
changes the view first, msgvault reports a conflict and requires you to reload
and review the latest revision instead of overwriting it.

The daemon validates every definition against the version-1 vocabulary
before saving it, so any stored view can be opened here, run by an API client
through `POST /api/v1/saved-views/{id}/run`, or executed by an AI assistant
with the [MCP server](/docs/usage/chat/#saved-views)'s `run_saved_view` tool.
All three read the same records and see the same revisions.

## Sources and sync status

Sources is a status workspace. Each row shows a source's name, readable type and
identifier, schedule, status, last successful sync, and an action. The schedule
reads as a sentence, with the cron expression in its tooltip.

The **Status** chip is **Syncing**, **Completed**, **Completed with errors**,
**Failed**, or **Never synced**. A running sync also shows its processed, added,
and error counts. Otherwise the time of the latest result appears under the
chip, and the last successful sync has its own column. A latest result older
than 24 hours adds "This result may be out of date." A failed status request
remains an error rather than becoming an empty source list.

A source with a sync error message, item errors, or a scheduler error has
**Show details for {name}**, which opens the run-level errors, item-level
errors, and scheduler error. **Sync history** in the page header opens
Operations filtered to source sync.

**Sync now** is available only when that source reports the capability. When it
is not, the Action column says why in words: "Imported file — nothing to sync",
"Sync in progress", "Scheduler unavailable", "Sync not set up", or "Sync
unavailable". The daemon's reason code stays in the text's tooltip.
Meeting-import sources show "On-demand API source" instead, because meetings
arrive through the API rather than a sync. A `202
Accepted` response means the daemon accepted the request, not that work has
finished. While the page is visible, the UI polls source status with bounded
backoff to show the run and live progress; it opens no streaming connection.
Status and **Sync now** requests time out after 20 seconds. Sync errors offer
**Refresh** to check source status without starting another sync. If **Sync
now** times out, refresh before trying again; the daemon may have accepted it.
Polling pauses while the tab is hidden and resumes when it is visible again.
A failed first load keeps its error on screen
until you select **Retry**. When the scheduler holds the sync lock without an
active run, the UI makes up to eight automatic refresh attempts, including
failed or timed-out requests; after that it stops automatic refresh, shows
"Automatic refresh paused. Source status may be stale.", and you can select
**Refresh** to poll again. If the accepted run never appears, the UI says "The
sync was requested, but it hasn't started yet. Refresh to check again." rather
than claiming success. Conflicting runs and unavailable capabilities retain
their explicit errors or reasons. Full resync, pause/resume, schedule editing,
and source add/remove are not available in Sources.

Viewing Sources does not repair runs left marked as running after a crash.
They remain active until the daemon restarts or another sync for that source
acquires its lock and recovers them. Until then, **Sync now** stays unavailable
and status polling continues while the tab is visible.

## Operations

Operations answers two different questions: what is configured and ready now,
and what happened during a particular run. Its overview groups work into five
lanes:

| Lane | Work shown |
|---|---|
| Messages | Source sync and message embeddings |
| Facts | People sweeps, person embeddings, and external enrichment |
| Contacts | CardDAV sync |
| Documents | Document extraction and document embeddings |
| Attachments | Visual embeddings |

Each lane lists its kinds of work. A row shows the kind's name, a status chip,
the latest run's start time, and any actions. When the latest success differs
from the latest run, the row adds "Last succeeded" and its time. The status chip
reads:

- **Queued** or **Running** while a run is active.
- **Off** when the work is not configured and no run is active.
- **Succeeded**, **Partial**, **Failed**, or **Cancelled** for the latest run.
- **No runs yet** when the work is configured, history is available, and
  nothing has run.

A lane the daemon cannot report shows **Status unavailable**. A kind whose
history cannot be read shows "History unavailable" instead of looking like no
work has ever run.

An Off row offers **Set up** when Settings can turn the work on. Message
embeddings, person embeddings, and visual embeddings open Settings on Search
and focus their setting. Person enrichment opens Person enrichment, and person
sweeps open People sweep. Document extraction and document embeddings are
configured in `config.toml` on the daemon host, so their rows say so and link to
[Document indexing setup](/docs/usage/document-indexing/#configure-the-policy)
and [Document search setup](/docs/usage/document-indexing/#semantic-and-hybrid-document-search).
Document embeddings also need semantic search.

Rows link to source status, CardDAV settings, or detailed document and visual
index status. The latter show coverage and the current prerequisites for
processing. The workspace offers **Start CardDAV sync**, **Build visual
index**, or **Resume visual index** only when the daemon advertises that
action. Source **Sync now** remains in Sources. Document extraction still
requires the explicit CLI upload workflow in
[Document Indexing](/docs/usage/document-indexing/).

**Refresh operation status** in the page header reloads the status lists every
five minutes and whenever you select it, and it shows how long ago they were
updated. It never touches run history. Filter history by lane, kind of work,
state, and start date. **Reload run history** at the end of the filter row
restarts history from page one and replaces the loaded rows. History otherwise
changes only when you change a filter or load more. If paging history becomes
inconsistent after a change, use **Restart operation history** to load a fresh
snapshot. The filters and selected run are kept in the URL, so browser Back and
Forward restore the view.

The run history table lists Kind, Trigger, State, Started, Duration, and
Counters. Trigger is Manual, Scheduled, or a dash when none was recorded. Queued,
running, succeeded, partial, failed, and cancelled are distinct states. Counters
name each unit once, as in "20 messages processed · 20 added", and leave out
zero counts. "No counters" means none were reported or every count was zero;
open the run to see them all. A failed or partial run
shows the daemon's error sentence under its state.

Open a run to inspect its progress, outcome, timestamps, every counter, and
available diagnostics. For a failed run, the detail shows the error sentence
first and the machine-readable code beneath it as "Code: {code}".

## Deletions

Everything supports explicit row selection and select-all-matching for the
current query and filters. **Review for deletion…** in the selection bar, or `d`
and `D`, opens the Deletions workspace. The daemon first preflights the
selection and reports any unavailable action before the UI offers a separate
staging confirmation. The workspace lists, inspects, and cancels manifests; it
cannot execute deletion against a provider. Use the explicit
`msgvault delete-staged` CLI workflow for that final operation.

Without a selection, the workspace says "Nothing selected for deletion" and
points back to Everything. With one, the **Review selection** panel shows the
item count and estimated size, how many items can be staged and how many will be
skipped, and when the review expires, as a relative time such as "in 8 minutes"
with the exact time in a tooltip. When the daemon reports that staging is
unavailable, the panel gives the reason as a sentence and disables **Stage
deletion…**. **Dry run** reports matched, staged, and skipped counts without
staging anything. **Stage deletion…** opens a confirmation, and only its red
**Confirm stage deletion** button creates the manifest. Staging covers Gmail and
Microsoft Graph mail only and does not execute deletion. Arriving from
Everything runs the review at once and, when staging is available, opens the
confirmation.

The manifests table lists ID, Description, Items, Status, and Created. Status is
Pending, In progress, Completed, Failed, or Cancelled. **Inspect** opens a
manifest's detail beside the table, or below it at 900 pixels wide or narrower;
**Close manifest detail** closes it. **Cancel** appears for Pending and In
progress manifests and asks for **Confirm cancel manifest**. With no manifests,
the workspace says "No staged deletions".

## Keyboard controls

Tab keeps its normal browser meaning. Outside inputs and content viewers:

| Key | Action |
|---|---|
| `j` / `k`, `↓` / `↑` | Move to the next or previous row |
| `H` / `L` | Open the previous or next item in the reader |
| `Home` / `End` | Move to the first or last row |
| `PgUp` / `PgDn` | Move up or down one page |
| `Enter` | Open or drill into the focused row |
| `Esc` | Close the current layer or restore the prior context |
| `/` | Focus search |
| `Space` | Toggle selection of the focused row |
| `Shift+Space` | Extend the selection to the focused row |
| `A` | Select all visible rows |
| `x` | Clear the selection |
| `d` / `D` | Review the selected or all matching items for deletion |
| `f` | Open Filters |
| `g` | Open Group by |
| `s` | Open Sort |
| `r` | Show sort order; in Files, point to the Sort menu |
| `?` | Open the searchable Keyboard shortcuts dialog |
| `Cmd/Ctrl+K` | Open the command palette |

Select **Keyboard shortcuts** in the sidebar, or press `?`, to search this list
in the app. Destructive keys open a review; they never execute deletion
immediately. Shortcuts are suspended while typing and inside message/file
content.

## Settings and restart behavior

Settings edits the daemon's `config.toml` from the browser. For every
`config.toml` setting the daemon supplies the category, section, label,
description, and allowed values, so the browser never decides on its own what
a setting means. Those categories are Appearance, Daemon, Archive, Search,
Sources, Attachments, Person enrichment, and Integrations. Larger categories
split into titled sections, for example Search has separate sections for the
text embedding provider, the embedding schedule, and visual attachment search.
The CardDAV account and People sweep categories are separate browser-owned
workflows with their own save actions; they are not part of the daemon's
settings catalog. The selected category is part of the page link, so reload and
Back keep it. A link to an unknown category opens Appearance.

Each row shows the setting name and one sentence about what it does. Limits
live on the control itself: a number input carries its minimum and maximum,
and a syntax hint such as the accepted duration format sits under the control
only when the syntax needs one. Settings where zero means "off", such as an
attachment size cap that falls back to the provider default, show a switch.
Switch it off and the row states what happens instead; switch it on and a
value input appears, starting from a suggested value. Rows you have changed
carry an amber dot. A save bar appears at the bottom of the page only while you
have unsaved changes. It counts them and offers **Discard** and **Save
changes**. **Save changes** stays disabled while a number field is empty. When
the bar disappears after a save or discard, focus moves to the category
heading.

Schedules are one line. A Presets menu offers common schedules such as every
hour, every day at 03:00, or weekdays at 09:00, plus Off for schedules that
can be empty and Custom. Choosing Custom opens the expression editor beside
the menu, starting from the preset you had. The five fields are tinted, and
while the editor has focus or the pointer is over it a small card names the
fields (minute, hour, day, month, weekday) and says in plain English when the
schedule runs; a mistake names the field and the problem before you save. A
Time zone menu at the end of the line runs the schedule in a chosen IANA zone
instead of the daemon's own clock, shown as "Server time"; the choice is
stored as a `CRON_TZ=` prefix on the schedule. The CardDAV account form uses
the same field, and the Sources and CardDAV status views describe stored
schedules the same way.

Each category states once, in a line under its heading, how its changes take
effect:

- "Saved changes apply right away — no restart needed." Appearance uses this
  line.
- "Saved changes apply after the daemon restarts." Every other `config.toml`
  category uses this line.
- "Most saved changes apply after the daemon restarts. Rows that differ are
  marked." A category that mixes both uses this line.
- "Set in config.toml on the daemon host." A category whose settings are all
  host-managed uses this line.

After a save that needs a restart, the page shows "Saved. Restart the daemon to
apply these changes." until the daemon restarts.

Some controls save through their own buttons instead of **Save changes**:
provider credentials, person-enrichment providers, the CardDAV account, and
People sweep. Above the first such control, the page says "These save
immediately when you use their buttons — not with Save changes." Two of them
also apply right away and say so beside their controls: person-enrichment
provider API keys and the CardDAV account.

Saving makes targeted edits to `config.toml` while preserving comments. A stale
edit is rejected after another browser or a hand edit changes the
configuration; reload before saving again.

A saved Appearance theme or density applies to the open tab at once, unless a
Display menu override is active in that tab. The override still wins. The
Appearance page explains how to return: choose “Use daemon theme” and Temporary
density “Auto” in the Display menu. A saved default search mode does not change
the search you have open. Tabs you open later in this browser without a mode in
their link use it.

Host-managed values, such as the listener address and the server API key
(`server.api_key`), show their current value with a Host-managed tag and no
input. Change them in `config.toml` on the daemon host. After the API key
changes and the daemon restarts, old browser sessions end and the login
screen appears.

### Provider policies and credentials

Create or edit named person-enrichment policies for Exa and SixtyFour in
Settings. Provider checks and consent still govern whether enrichment can
run; configuration alone does not authorize a provider. The TUI shows these
policies read-only. See [External Person Enrichment](/docs/usage/people-enrichment/)
for the provider lifecycle.

Provider credentials for embeddings, enrichment, and sweeps are write-only,
and so are the task integration key and the daemon's own API key. Each key is
one line: a read-only box, a pencil button, and a trash button that removes
a stored key. The box shows `None` when no key is set, or a masked
hint of the set key, its first three and last three characters, such as
`sk-…x9Q`, so you can tell which key is in place. A key under twelve
characters shows as dots instead. The pencil opens a dialog to paste the
new key, and the dialog says when it takes effect: a person-enrichment key
applies right away, the text and visual embedding keys are stored at once
but used after the daemon restarts, and the task integration key is saved
with the rest of the page. A key that comes from an environment variable
says so under the line and cannot be cleared from the browser.

Credentials have a separate revision from `config.toml`. When changing both
an endpoint or model and its credential, save the endpoint/model first, then
the credential. This binds the key to the destination it was entered for.

### CardDAV contacts

CardDAV settings manage contact accounts and discovered address books. Choose
which books participate in sync, lookup, and publishing; publishing also
enables contact sync for that book. The workspace offers incremental and full
sync, recent run history, and conflict review. A conflict shows local and
remote versions before you choose which to keep. See
[People and CardDAV](/docs/usage/people-carddav/) for setup and publishing rules.

## Optional integration states

The optional task integration is server-side and provider-neutral. Msgvault
shows disabled, discovering, authentication required, reachable but
incompatible, partial, stale, unavailable, or ready instead of presenting a
failed lookup as “no links.” Credentials never enter browser types, URLs, or
error messages. The archive remains fully usable while the integration is
absent or unhealthy.
