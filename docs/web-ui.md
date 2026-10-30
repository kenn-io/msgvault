---
last_edited: "2026-10-29"
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
| Where is that message, conversation, or meeting? | Messages |
| Who have I been in contact with? | People |
| Where is an attachment, image, or video? | Files |
| What do I know about this person? | Directory |
| Which identity matches or profile facts need my decision? | Reviews |
| Can I return to this search later? | Saved views under Messages and Files |
| Did sync, enrichment, or indexing finish? | Status dot, Past runs, and Sources |
| What is staged for deletion? | Staged deletions |
| How do I change the daemon's configuration? | Settings |

The sidebar lists **Messages**, **People**, **Files**, and **Sources**. Saved views nest under Messages or Files. The footer holds **Settings**, **Help**, and a status dot. Directory and Reviews remain available through the command palette. **Staged deletions** appears for waiting or running batches and for seven days after execution finishes. Use **Go to Staged deletions** in the command palette to open its history when the sidebar entry is hidden. Choose **Collapse sidebar** to shrink it to an icon rail; the browser remembers the choice. Below 900 pixels wide, **Open navigation** opens a drawer.

The top bar holds global search and **Display**. Display sets theme and density for this tab. On the ungrouped Messages table, it also shows **Columns**. **Reader position** appears when the results are at least 960 pixels wide. **Use Settings defaults** clears temporary theme and density choices. [Settings](#settings-and-restart-behavior) holds the saved defaults.

Global search works from every workspace. Press `/` outside a text field to
focus it. In Messages and Files, results follow your query as you type, and
`Enter` moves focus to the results. In any
other workspace, `Enter` opens Messages with your query and search mode.

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
  <img src="/docs/assets/static/analytical-light-compact-darwin.png" alt="Archived email table with global search, Save view button, and compact email rows in light theme" loading="lazy">
  <figcaption>Browse archived email. Select the image to view it at full size.</figcaption>
</figure>

The screenshots use a curated public Enron research-data fixture. Authentic
names and message text are intentional; the repository's `docs-fixtures`
branch records provenance, attribution, and the content review. This fixture
contains email only; it does not illustrate chat, calendar, or attachment content.

Messages opens as a compact table of logical entries, newest first: one row
per email, calendar event, meeting note, other durable item, or chat
conversation. Raw chat fragments appear only after drilling into a
conversation. Search, Filter, Group by, and Show as form a shareable view.
Ordinary tabs use short URLs such as `?workspace=everything&mode=full_text`.
Filters, layout changes, and the selected item appear in the link only when
they differ from the defaults. Keyboard focus, scroll position, and choices
from other workspaces stay out of the link; browser history keeps them so Back
and Forward restore them.

One toolbar sits above the results:

- **Filter** holds Kind, Account, Identity, and optional From and To dates. Dates include the selected UTC calendar days. From must be on or before To.
- **Group by** adds a grouping level. Add more levels to group within groups.
- **Show as** switches between Table, Timeline, and Files.
- **Sort** shows the fixed newest-first Messages order.
- The count at the end reports items, or groups when the view is grouped.

Each active search, filter, and grouping appears as a removable chip below the
toolbar. Select the chip's remove button, such as **Remove search** or **Remove
Person filter**, to drop it. **Save view…** in the page header saves the current
context; see [Saved views](#saved-views).

Selecting rows opens a selection bar. **Review deletion…** opens a review dialog over Messages; see
[Deletions](#deletions). The button is disabled when the daemon
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

### Recording matches

With Docbank configured, Messages also searches recording transcripts and
shows matches in **Recordings** above the ordinary results. Recording search
sends the query text to the configured Docbank service.

- **When it runs:** a **Full text** query of plain words, in **Table** view,
  with no grouping and either no filters or one **Person** filter selecting one
  person. Punctuation, quoted phrases, Semantic, Hybrid, other filters and
  grouping leave recording search unavailable. The search starts once typing
  pauses for 300 ms.
- **What it shows:** up to 20 matching excerpts with source title, date,
  filename, transcript origin and supplied timing. Matches have their own count
  and coverage; incomplete coverage stays visible even with zero matches.
- **Opening a match:** the reader shows its message and current transcript
  while keeping your search. Modified clicks open the message in a new tab.
- **Scope limits:** if the search exceeds 4,096 distinct versions or source
  selectors, add a Person filter. If that person still exceeds the limit, use
  person-scoped recording search in the CLI.
- **Availability:** Recordings appears once the daemon confirms Docbank is
  configured. Pending or failed checks keep it hidden; a failed check retries
  after 30 seconds.
- **Freshness and failures:** results expire 30 seconds after the request
  starts; use **Refresh** for current matches. Transient failures offer
  **Retry**. A query or Person filter change retries a refused search.

On unreleased builds, the same request adds an **Attachments** block when attachment mirroring is enabled. Rows show the message title and date, filename, and extracted excerpt, and open the containing message. Browser attachment search takes plain words, like recording search. Use `msgvault media search` for attachment queries containing punctuation. An empty block without an error reports no matches in extracted text from mirrored attachments. A scope with more than 4096 mirrored files excluding images, audio, and video returns `media_search_scope_limit` in `attachments.error`. The block names the limit and suggests a Person filter to narrow the search. If a Person filter already exceeds the limit, choose one with fewer attachments. Other attachment failures show the API error message, with Retry for transient failures. "More may match" marks other result limits. Recording failures appear beside attachment results. Both lists follow the same Person filter.

## Read messages

Open a `/messages/<id>` link to go directly to an archived message. CLI
`search --json` and `show-message --json` responses include a `web_url` when
the selected daemon has an HTTP address. The link uses that daemon's address;
the person opening it still needs access to the archive. Links to chat messages
open a bounded part of the conversation around the selected message, with
controls to load earlier or later messages.

Click an entry in Messages to open its preview below the results. On wide
windows, choose **Display → Reader position → Right** to read beside the results.
Drag the divider to resize either layout, or focus it and use the arrow keys.
Double-click the divider to reset its size. The browser remembers your layout
choice and each layout's size. Narrow windows use the preview below the results
and restore your right-side layout when there is room again. Escape closes the reader. Next/previous result shortcuts move between ordinary message previews and are inactive while a recording match is open.

HTML email follows the app's dark theme by replacing sender-defined text,
background, and border colors. Images keep their original colors. Choose
**Use original colors** above a message to see its authored colors on a white
background, or **Use app colors** to return to dark reading. This override
applies to the open message. Light mode preserves designed email colors.

Each expanded message lists its files, recordings, linked tasks, and Kata issues below the body. Click a linked sender or recipient to open that exact person; Back returns to the message. The **⋯** menu offers **Add to task…**, **Download original** for email, **Export meeting…** for a meeting transcript, and plain text or HTML when available. **Add to task…** keeps task creation, existing-task search, priority, labels, and the outbound metadata disclosure together. Disabled Tasks connections offer **Set up**; other service or index failures offer **Open Settings**.

## Meeting context and follow-ups

Filter Messages to meetings to see the meeting count and known duration. Choose **Action items** to filter recorded follow-ups by status or assignee.
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
filename, type, size, person or domain, source, Message, and content
availability. Archived images and PDFs open in application-controlled viewers.
Metadata-only, missing, unsupported, and previewable content remain distinct.
From a file, navigate to its containing item and then its email or chat
conversation.

Files uses the same toolbar as Messages, with **Show as** set to Files. Choose Table
or Timeline there to return to Messages. **Sort** offers Newest first, Oldest
first, Filename A–Z, Filename Z–A, Largest first, and Smallest first. The
**Filter** menu includes Filename and Type (Images, PDFs, Audio, Video, Text,
Documents, Archives, Other). Grouping replaces the table with groups and hides
Sort and Visual search; Filename and Type remain editable in Filter, and active
filters stay visible as chips.
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

Select a day in a person's relationship calendar to show that day's activity
in the message timeline. Dates use your browser's local time zone. Select the
same day again or **Clear date** to restore the previous date range. Other
filters stay active; switching people, calendar years, or to **Files** clears
the day selection.

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
a grouped person or domain in Messages opens its inspector in the current
context, including chronologically ordered related files.

## Saved views

Saved views persist useful analytical contexts in the daemon, so the same
library is available from every authenticated browser connected to this
single-user archive. A view records its query, explicit search mode, filters,
grouping, presentation, sort, and visible columns.
Selection is intentionally not saved. The inspector stays pinned; the browser
does not save or apply inspector pin preferences.

Choose **Save view…** in the Messages or Files page header, name the view, add
an optional description, and select **Save**. A view saved from Files does not
keep the Filename filter, Type filter, or file sort. Open the view from its row under Messages or Files. Its tooltip shows a summary and description. The row's **⋯** menu offers **Rename**, **Edit description…**, and **Delete**. Rename edits the name inline; press Enter to save or Escape to cancel. Delete hides the view and offers **Undo** for eight seconds before removing it for every connected browser.

An incompatible view stays dimmed with its reason in the tooltip. Its menu offers only Delete. Updates and deletion carry the record revision. If another browser changes the view first, msgvault reports a conflict or restores the row after a failed deletion.

The daemon validates new definitions against the schema 2 vocabulary. It reads
schema 1 views with pipes as literal text. A stored view can be opened here,
run by an API client through `POST /api/v1/saved-views/{id}/run`, or executed by an AI assistant
with the [MCP server](/docs/usage/chat/#saved-views)'s `run_saved_view` tool.
All three read the same records and see the same revisions.

## Sources and sync status

Sources lists Source, Status, Last good sync, and Sync now. Status shows Running, Succeeded, Partial, Failed, Paused, Queued, Cancelled, or Never synced. Contacts accounts appear in their own group. Sources stays available while contacts status loads, and contacts-only runs continue polling.

Select a source to open its panel. Schedule, Next sync, and attachment rules show the current values. **Edit in Settings › Sources** opens the existing editor for settings shared by that source type. The panel also shows Runs, item errors, and **Sync now**. A latest run's error appears before a scheduler error. **Sync history** opens Past runs filtered to source sync.

A contacts panel shows its repair reason and **Open Settings › Contacts** for sign-in recovery. That editor retains the selected OAuth app and provides commands for both shells. Mail sync instructions in the source panel also label POSIX and Command Prompt commands.

**Sync now** is available only when that source reports the capability. When it
is not, the Sync now column says why in words: "Imported file — nothing to sync",
"Sync in progress", "Scheduler unavailable", "Sync not set up", or "Sync
unavailable". Meeting-import sources show "On demand" instead, because meetings
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

While the tab is visible, job status refreshes every 30 seconds on every page and when the window regains focus.

The footer status dot opens queued and running jobs and unresolved failures. It shows **Idle** when the list is empty, **Queued** while work awaits execution, **Running** while work runs, and **Needs attention** while a latest run failed or the scheduler reports an error. Failed rows show the daemon's run or item error. If a status read fails, the menu keeps the last known jobs and reports that status is unavailable. Select a job to open its source, contact settings, index status, or owning setting. Scheduled source syncs show Queued while waiting to execute. A previous sync failure stays visible until execution starts.

Choose **Past runs…** in the status menu to browse history. Filter by Kind, including all runs in a lane, State, or dates, and choose **Show more** for the next page. **Refresh** reloads the current history. Run links preserve filters and selection when shared or restored with Back. If history changes during paging, **Restart** reloads the first page. Unavailable history is labeled by kind.

Select a run for its timestamps, counters, error, related status, and advertised actions. **All runs** returns to the list. Related status opens Sources, Contacts settings, or live document and visual index counts. **Start CardDAV sync**, **Build visual index**, and **Resume visual index** appear only when the daemon advertises them. Sources keeps **Sync now** and opens filtered history through **Sync history**. Contacts settings opens filtered history through **View CardDAV operations**.

Settings Overview shows current feature status. **Details** opens live counts for configured Document extraction, Document search, and Visual search, including features with no past run. Document extraction and document search are configured on the daemon host; see [Document indexing setup](/docs/usage/document-indexing/#configure-the-policy). Document extraction still requires the explicit CLI upload workflow in [Document Indexing](/docs/usage/document-indexing/).

Old Operations links with a run, status, or history filter open Past runs over Settings Overview; links without them open Settings Overview. Old Saved views links open Messages.

## Deletions

Messages supports individual row selection and select-all-matching for the current query and filters. **Review deletion…** in the selection bar, or `d` and `D`, opens a review dialog over Messages. The daemon preflights the selection before enabling staging.

The dialog shows the selected count and estimated size, eligible and skipped counts, exclusions, and review expiration. Semantic search reviews warn that they cover active messages only. **Dry run** reports matched, staged, and skipped counts without creating a batch. **Stage N messages** creates the batch and leaves a success summary with its ID. **Done** returns to Messages. Staging supports Gmail and Microsoft Graph mail; use `msgvault delete-staged` to execute deletion.

The Staged deletions workspace lists batches by creation date, message count, account, and status. Select a row to inspect its description, summary, execution counts, and failed message IDs. **Cancel batch** appears for staged or running batches and asks for confirmation before cancelling. **Show cancelled** includes cancelled batches in page history; cancelled-only batches do not keep the sidebar entry visible. With no batches, the workspace says "Nothing staged". The sidebar entry remains visible for active batches and for seven days after execution finishes.

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

Select **Help** in the sidebar, or press `?`, to search this list
in the app. Destructive keys open a review; they never execute deletion
immediately. Shortcuts are suspended while typing and inside message/file
content.

## Settings and restart behavior

Settings opens on Overview, with status and setup or build actions for
background features. Use **Refresh feature status** to update Overview after
background work changes. **Build index** appears when offered by the daemon and
asks for confirmation before a full rebuild. Its categories are Overview,
Server, Search & AI, People lookups, Tasks, Contacts, Sources, and Advanced.
Sources includes source and attachment settings. Advanced includes archive
settings and Appearance. People lookups includes named enrichment providers and
People sweep profiles.

The daemon supplies allowed values and validation. Settings supplies category and selected field labels, groups settings into the category rail and keeps tuning and host-managed values behind disclosures. The selected category is part of the page link, so reload and Back keep it. Old category links open their replacement category; unknown categories open Overview.

Each row shows the setting name. Limits live on the control itself: a number input carries its minimum and maximum,
and a syntax hint such as the accepted duration format sits under the control
only when the syntax needs one. Settings where zero means "off", such as an
attachment size cap that falls back to the provider default, show a switch.
Switch it off and the row states what happens instead; switch it on and a
value input appears, starting from a suggested value. Rows you have changed
carry an amber dot. A save bar appears at the bottom of the page only while you
have unsaved changes. It counts them and offers **Discard** and **Save
changes**. **Save changes** stays disabled while a number field is empty. During a save, fields and resource actions wait; category navigation stays available. When
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

The Save changes bar covers ordinary settings, and its edits survive category
changes. Some controls save through their own buttons instead: provider
credentials, person-enrichment providers, contact accounts, and People sweep.
Above them, the page says "These save immediately when you use their buttons —
not with Save changes." The bar stays visible on Contacts while ordinary settings have unsaved changes. After a save that
needs a restart, Settings shows “Saved. Restart msgvault to apply these
changes.”

Saving makes targeted edits to `config.toml` while preserving comments. A stale
edit is rejected after another browser or a hand edit changes the
configuration; reload before saving again.

A theme or density saved under Advanced applies to the open tab at once, unless a
Display menu override is active in that tab. The override still wins. To return to the saved values, choose **Use Settings defaults** in Display. A saved default search mode does not change
the search you have open. Tabs you open later in this browser without a mode in
their link use it.

Host-managed values, such as the listener address and the server API key
(`server.api_key`), show their current value under Set in config.toml. Change them in `config.toml` on the daemon host. After the API key
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
with Save changes. A key that comes from an environment variable
says so under the line and cannot be cleared from the browser.

Credentials have a separate revision from `config.toml`. When changing both
an endpoint or model and its credential, save the endpoint/model first, then
the credential. This binds the key to the destination it was entered for.

### CardDAV contacts

CardDAV settings manage contact accounts and discovered address books. Choose
which books participate in sync, lookup, and publishing; publishing also
enables contact sync for that book. Sync now lets the daemon choose whether
to reconcile the full address book. To force a full sync, run `msgvault sync-carddav --full`. The workspace also offers recent run
history and conflict review. A conflict shows local and
remote versions before you choose which to keep. See
[People and CardDAV](/docs/usage/people-carddav/) for setup and publishing rules.

## Optional integration states

The optional task integration is server-side and provider-neutral. Msgvault
shows disabled, discovering, authentication required, reachable but
incompatible, partial, stale, unavailable, or ready instead of presenting a
failed lookup as “no links.” Credentials never enter browser types, URLs, or
error messages. The archive remains fully usable while the integration is
absent or unhealthy.
