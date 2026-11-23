---
last_edited: "2026-11-08"
title: Web UI
description: Browse messages and files, maintain people, and monitor archive work from your browser.
---

# Web UI

Use the Web UI to search your archive, read messages, browse files, maintain
your contact directory, and check whether sync and indexing jobs have finished.
It comes embedded in the release binary and runs under `msgvault serve`, and
you do not need a separate web application process.

AI assistants reach the same archive through the [MCP server](usage/chat.md),
and scripts use the [HTTP API](api-server.md). Any saved views, staged
deletions, or people edits you make in one place show up in the others.

| Your question | Workspace |
|---|---|
| Where is that message, conversation, or meeting? | Messages |
| Who have I been in contact with? | People |
| Where is an attachment, image, or video? | Files |
| What do I know about this person? | People → Details |
| Which identity matches or profile facts need my decision? | People: Possible duplicates or Facts |
| Can I return to this search later? | Saved views in the sidebar |
| Did sync, enrichment, or indexing finish? | Status dot, Past runs, and Sources |
| What is staged for deletion? | Staged deletions |
| How do I change the daemon's configuration? | Settings |

The sidebar gives you **Messages**, **People**, **Files**, and **Sources**,
with saved views nested under **Messages** or **Files**. At the bottom of the
sidebar, you have **Settings**, **Help**, and a status dot. **Staged
deletions** appears in the sidebar whenever a listed batch is pending, running,
or failed. When that entry is hidden, you can still reach its history with **Go
to Staged deletions** in the command palette. To shrink the sidebar to an icon
rail, select **Collapse sidebar**, and your browser remembers that choice. On
windows narrower than 900 pixels, **Open navigation** opens a drawer instead.

Across the top bar, you have global search and **Display**. **Display** sets
the theme and density for the current tab, and on the ungrouped Messages table
it also offers **Columns**. **Reader position** appears when the results area
is at least 960 pixels wide. To clear temporary theme and density overrides and
return to the saved defaults in [Settings](#settings-and-restart-behavior),
select **Use Settings defaults**.

Global search works from every workspace. Press `/` outside a text field to
focus the search box. In Messages and Files, results update as you type, and
pressing `Enter` moves focus into the results. In any other workspace, `Enter`
opens Messages with your query and search mode.

## Start and discover the URL

```bash
msgvault build-cache
msgvault serve
```

Running `build-cache` prepares the analytical tables before you open the
browser. You can also run `serve` directly and let default startup maintenance
build a missing or stale cache in the background.

When the daemon starts in the foreground, it prints `API server: http://HOST:PORT`.
By default, `server.api_port = 0` picks a free port. Local CLI commands find
that address through the private daemon runtime record under your configured
msgvault home, while browsers use the printed URL. If you want a stable remote
bookmark, configure a fixed port:

```toml
[server]
bind_addr = "127.0.0.1"
api_port = 8080
```

The default loopback deployment is trusted. When an API key is active and a
request is not loopback-trusted, `/` still loads the public shell and the UI
prompts you for the key. A successful login creates an expiring, in-memory
browser session that ends on daemon restart, logout, expiry, or API-key
activation. Existing bearer-key API clients work the same way as before.

## Remote access and HTTPS

For remote access, set a strong API key and bind to the address you intend to
expose:

```toml
[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = "replace-with-a-long-random-key"
```

Using a reverse proxy for HTTPS is recommended. The daemon accepts forwarded
scheme and host headers only from proxy addresses or CIDRs you explicitly
trust:

```toml
[server]
trusted_proxies = ["127.0.0.1", "192.0.2.8/32"]
```

Do not add an entire client network only to silence proxy warnings. When the
daemon sees HTTPS through a trusted proxy, it marks the browser session cookie
as `Secure`. You can run plain HTTP over an encrypted private network as an
explicit tradeoff, though the UI warns you that the session cookie travels
without TLS. `HttpOnly` and `SameSite=Strict` do not encrypt that traffic.

## Messages and search

At 640px or narrower, message rows display the sender and time, the subject,
and a one-line preview, while conversation rows show their participants. Group
rows show the name, item count, size, and latest date. **Columns** still
controls which parts appear.

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/web/messages-light-compact.png" alt="Archived email table with global search, Save view button, and compact email rows in light theme" loading="lazy">
  <figcaption>Browse archived email. Select the image to view it at full size.</figcaption>
</figure>

The screenshots use a curated public Enron research-data fixture. Its authentic
names and message text are intentional, and the repository's `docs-fixtures`
branch records provenance, attribution, and the content review. This fixture
contains email only, and it does not show chat, calendar, or attachment
content.

Messages opens as a compact table of logical entries, newest first, with one
row per email, calendar event, meeting note, other durable item, or chat
conversation. Raw chat fragments appear only after you drill into a
conversation. Your Search, Filter, Group by, and Show as choices form a
shareable view. Ordinary tabs keep short URLs such as
`?workspace=everything&mode=full_text`, adding filters, layout changes, and the
selected item to the link only when they differ from the defaults. Keyboard
focus, scroll position, and choices from other workspaces stay out of the link
and live in browser history instead, so Back and Forward restore them.

One toolbar sits above the results:

- **Filter.** Narrow results by Kind, Account, Identity, and optional From and
  To dates. A new date range uses UTC, while Month and Year drills create dates
  in your browser's time zone and keep them local when you edit them. From must
  be on or before To.
- **Group by.** Add a grouping level, including Entry kind. Month and Year list
  newest first in your time zone, with bars showing each group's share of all
  items. You can add more levels to group within groups.
- **Show as.** Switch between Table, Timeline, and Files.
- **Sort.** Shows the fixed newest-first Messages order.
- **Result count.** The count at the end reports items, or groups when the view
  is grouped.

On a phone-width window, **View** holds Show as, Group by, and Sort.

Every active search, filter, and grouping appears as a removable chip below the
toolbar. To drop one, select its remove button, such as **Remove search** or
**Remove Person filter**. To save the current view, select **Save view…** in
the page header; see [Saved views](#saved-views).

Selecting rows opens a selection bar that shows the estimated size beside the
count. Select **Review deletion…** to open a review dialog over Messages; see
[Deletions](#deletions). When the daemon reports that staging is unavailable
for your selection, the button is disabled, and you can hover over or focus it
to see why. Export displays unavailable reasons the same way.

Search mode is always explicit:

- **Full text.** Matches words in the text index.
- **Semantic.** Ranks only content covered by the current embedding
  generation.
- **Hybrid.** Combines keyword matches with semantic ranking where it is
  available.

A coverage notice below the toolbar reports semantic coverage across six
distinct states: disabled, building, stale, incomplete, unavailable, and ready.
msgvault never silently switches the mode you asked for. Semantic-only results
cannot include unembedded content, while Hybrid keeps full-text coverage and
labels the semantic contribution.

If a search fails, the UI explains what happened and preserves your query.
**Timed out** means the selected search backend did not finish within the
request budget. The UI reports this state as an error rather than an empty
result, and your query and filters remain available to retry. **Incompatible
mode** means the daemon, browser contract, or current index cannot safely honor
the mode you selected. Update or rebuild the named component, or switch to a
supported mode. msgvault does not quietly fall back to full-text search for
either state.

### Recording matches

When Docbank is configured, Messages also searches recording transcripts and
displays matches in a **Recordings** block above the ordinary results. This
search sends your query text to the configured Docbank service.

- **When it runs.** Recording search runs for a **Full text** query of plain
  words in **Table** view, with no grouping and either no filters or a single
  **Person** filter selecting one person. Punctuation, quoted phrases,
  Semantic, Hybrid, other filters, and grouping make recording search
  unavailable. The search starts 300 ms after you pause typing.
- **What it shows.** Up to 20 matching excerpts appear with their source title,
  date, filename, transcript origin, and supplied timing. Matches carry their
  own count and coverage, and incomplete coverage stays visible even when zero
  matches come back.
- **Opening a match.** Selecting a match opens its message and current
  transcript in the reader without clearing your search. Modified clicks open
  the message in a new tab.
- **Scope limits.** If your search exceeds 4,096 distinct versions or source
  selectors, add a **Person** filter. If that person still exceeds the limit,
  use person-scoped recording search in the CLI.
- **Availability.** **Recordings** appears once the daemon confirms that
  Docbank is configured. Pending or failed checks keep it hidden, and a failed
  check retries after 30 seconds.
- **Freshness and failures.** Results expire 30 seconds after the request
  starts; select **Refresh** for current matches. Transient failures offer
  **Retry**, and changing your query or **Person** filter retries a refused
  search.

When attachment mirroring is enabled, the same request adds an **Attachments**
block. Each row shows the message title and date, filename, and extracted
excerpt, and opens the containing message. Browser attachment search takes
plain words like recording search; for attachment queries with punctuation, use
`msgvault media search`. An empty block with no error means no matches turned
up in extracted text from mirrored attachments. If you have more than 4,096
mirrored files excluding images, audio, and video, msgvault searches them in
batches of 4,096 and interleaves results by their position within each batch,
so order across batches is approximate. Attachment failures show the API error
message with **Retry** for transient failures, and "More may match" flags
result limits. Recording failures appear beside attachment results, and both
lists follow the same **Person** filter.

## Read messages

Open a `/messages/<id>` link to jump straight to that message in the reader.
CLI `search --json` and `show-message --json` responses include a `web_url`
whenever the selected daemon has an HTTP address. That link uses the daemon's
address, and anyone opening it still needs access to the archive. Links to chat
messages open a bounded slice of the conversation around the selected message,
with controls to load earlier or later messages.

Select an entry in Messages to open its preview below the results. On wide
windows, choose **Display → Reader position → Right** to read beside the
results instead. You can resize either layout by dragging the divider or by
focusing it and using the arrow keys, and double-clicking the divider resets
its size. Your browser remembers both your layout choice and each layout's
size. On windows 640 pixels wide or narrower, the reader fills the results
area, and **Back to results** or Escape returns you to the list. Above 640
pixels, the preview drops below the results whenever the results area is
narrower than 960 pixels, then restores your right-side layout once there is
room again. Escape closes the reader. Next/previous result shortcuts move
between ordinary message previews and are inactive while a recording match is
open.

In dark theme, HTML email adapts by replacing sender-defined text, background,
and border colors while keeping images in their original colors. To view an
email's authored colors on a white background, select **Use original colors**
above the message, or select **Use app colors** to return to dark reading. That
override applies only to the open message. Light mode preserves designed email
colors.

Below the message body, each expanded message lists its files and recordings
and groups linked Tasks and Kata issues under **Tasks**. Select a linked sender
or recipient to open that exact person, then use Back to return to the message.
The **⋯** menu offers **Add to task…**, **Download original** for email,
**Export meeting…** for a meeting transcript, and plain text or HTML when
available. Inside **Add to task…**, you can create a task, search existing
tasks, and set priority and labels; outbound task metadata appears under
**What's sent**. Disabled Tasks connections offer **Set up**, while other
service or index failures offer **Open Settings**.

### Related copies

Select **Related copies** above a message to see its other archived copies,
along with replies and forwards that name it, across accounts and
conversations. Each row lists the account, whether that account sent or
received the copy (or holds it as a draft), the date, and the connecting
evidence: the same message, a reply, a forward, shared references, a relation
through another copy, or, for sources that keep the provider's own threads such
as Gmail, the same thread as the email or one of its exact copies. Messages
already shown in the conversation do not get a row, though messages outside its
loaded window still appear. Select a row to read that copy. The panel loads
only when you open it and makes no request while closed.

A warning appears whenever the opened email, a copy, or any email in the
archive has not been indexed yet, because it could be a related copy the panel
cannot see. The warning also tells you when an original message is missing, its
format or identifiers could not be read, or it has no stored Message-ID.
Replies that name a copy without a stored Message-ID cannot find it. The next
sync indexes pending emails; for a source that no longer syncs, run
`msgvault repair-derived`. If the lookup hits its 500-email or 5,000-identifier
limit, the panel tells you which limit stopped it. An empty panel reports only
what the search found and never claims that no related copy exists. Only the
owner can use this panel.

## Meeting context and follow-ups

Filter Messages to meetings to see the meeting count and known duration, and
select **Action items** to filter recorded follow-ups by status or assignee.

To export meeting context:

1. Select the meeting rows you want to include.
2. Select **Meeting context…** in the selection bar.
3. In the popover, choose JSON or Markdown, and optionally turn on **Include
   transcript**.
4. Select **Export meeting context**.

People and domain views display meeting metrics and recorded actions for their
current scope. Select **Open archived meeting** to inspect the source evidence,
then use Back to return to where you started. See the
[meeting guide](usage/meetings.md#export-context-and-read-follow-ups) for
selection limits, source coverage, unknown duration, and action filters.

## Cache states

The web tables share one analytical cache across message types. In auto mode,
Messages serves live archive rows during a full rebuild and shows a notice
naming the operations that still need the cache. If a rebuild fails to produce
a usable cache, those live rows remain available alongside recovery steps.
Other cache-dependent views show the cache state and recovery steps instead. A
live pagination cursor expires when the archive changes or the replacement
cache publishes. Reload the view to continue.

Setting `analytics.auto_build_cache = false` skips automatic startup rebuilds
and their live Messages path. To rebuild explicitly, run
`msgvault build-cache --full-rebuild`, then restart any daemon that is already
running. Leave `analytics.auto_build_cache = true` if you want daemon startup
to rebuild a stale cache. With `analytics.engine = "duckdb"`, startup fails if
no usable cache can be produced.

## Files and containing context

At 640px or narrower, files switch to stacked rows with the filename above the
date, size, and sender. Type, source, containing message, and person-file
direction are hidden at this width, and availability appears only for files
without local content.

Files is the only file view in the Web UI. It shows a searchable table of
attachment date, filename, type, size, person or domain, source, Message, and
content availability. Archived images, PDFs, text, CSV/TSV, and audio open directly in the file viewer, where
you can move through the current results with Previous/Next or the ←/→ keys.
When the archive records a hash, the list marks file contents as not checked.
Opening a file checks whether its storage can be opened, though reading its
bytes can still fail. The viewer distinguishes missing files, open errors,
metadata-only files, unsupported files, and previewable content. From any file,
you can jump to its containing item and from there into its email or chat
conversation, or choose **⋯ → Add to task…** to file the message to Tasks or
Kata.

Files shares the Messages toolbar with **Show as** set to Files. Choose Table
or Timeline there to switch back to Messages. **Sort** offers Newest first,
Oldest first, Filename A–Z, Filename Z–A, Largest first, and Smallest first,
while **Filter** adds Filename and Type (Images, PDFs, Audio, Video, Text,
Documents, Archives, Other). Adding a grouping replaces the table with groups
and hides **Sort** and **Visual search**; Filename and Type stay editable in
**Filter**, and active filters remain visible as chips. Selecting **Save
view…** saves your search, filters, and grouping, without saving the Filename
filter, Type filter, or file sort.

Inside a person's Files tab, you can switch between a media gallery and a file
table and narrow the relationship to **From them**, **To them**, or **Group
conversations**. Those directions reflect the containing messages and do not
identify people pictured in an image.

To describe image or video content, or to search with a JPEG, PNG, or WebP
query image, turn on **Visual search**. The UI tells you that the query goes to
the configured provider. Visual search requires the separate
[visual index](/docs/usage/vector-search/#visual-attachment-search); ordinary
filename browsing does not use that provider.

On a phone-width window, **Visual search** lives inside **Filter**, and **Clear
filters** clears the visual query and query image there too.

### Images in email

The reader shows archived inline images automatically and holds off on remote
images until you select **Load images**. That choice applies only to the
current item and fetches images through the daemon; moving to another item
resets it.

If you want unattended, offline preservation of remote images, see
[Archive Remote Email Images](/docs/usage/remote-images/). Allowing the reader
to load an image and allowing ingest to archive remote images are separate
choices.

## People and domains

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/web/people-dark-comfortable.png" alt="People workspace showing a person list, the selected person's activity calendar, and their messages in dark theme" loading="lazy">
  <figcaption>Select a person to explore their activity and messages.</figcaption>
</figure>

People groups identifiers backed by explicit archive identity evidence and
never merges records only because their display names match. Select any person
to see their activity across email, chat, calendar events, and meeting notes,
along with their associated files. Message filters continue to scope both
Messages and Files.

The People workspace lists either **People** or **Domains**. You can narrow the
list with the global **Search people** field, **Filter**, and sort. The one-way
sender filter applies to Closest, while saved profiles sort by **Most recent**,
**Name**, or **Least recently contacted**. **Saved** and profile filters show
saved profiles, including people with no archived messages. To link observed
identities, check multiple observed people and select **Merge**. To delete
saved profiles in bulk, select them from the list.

Selecting a person or domain opens three tabs: **Messages**, **Files**, and
**Details**. **Messages** uses the same table and reader as the Messages
workspace. A person's events and meetings appear when an account owner
attended, while email and chat keep their participant scope. **Details** holds
contact fields, custom attributes, Work, Connections, Network, Open loops,
Tasks, Facts, Last time we talked, Meetings, and Linked addresses, with the
header and tabs staying visible as you scroll. To save an observed person,
select **Save** in the header. Selecting the name or avatar also opens
**Details**, and **Merge with…** beside the person's menu lets you find another
saved profile or observed person. For a saved profile, **Files** covers all
bound identities, while **Messages** and the calendar open its first observed
identity cluster. Profiles without archived activity open directly on
**Details**.

The Activity calendar opens to the latest active year, showing the first month
of contact beside its title. Select a day to narrow **Messages** and its
conversation reader. **Details** keeps that day selected; select the day again
or choose **Clear date** to restore the shared date filters. Editing a message
filter also clears the selected day without copying its bounds to **Files**.
People and domains use your browser's local time zone, and domain days count
individual sent messages, including chats that span several days. Calendars
summarize the whole person or domain, while message filters narrow the table
and **Files**. Switching people, changing years, or moving to **Files** clears
the day selection. **Messages** groups chats by conversation, and a selected
day limits the opened conversation to that day.

People in this workspace are observed identity clusters. Marking source
identities that mean "me", explicitly promoting durable profiles, overriding
display names, and editing typed profile attributes are separate curated
operations; see [People, Profiles, and Source Identities](/docs/usage/people/).

### Saved profiles and possible duplicates

Choose **Filter → Saved** to view the profiles you curate across sources. You
can search by name, email, or organization, or open **Filter → Profile
filters** to filter by Contact state, Category, Organization, Primary channel,
and two date fields, **Last contacted after** and **Last contacted before**.
Each active filter appears as a chip with a remove button. The **Sort** menu
orders profiles by **Most recent**, **Name**, or **Least recently contacted**.

When you select a saved profile, msgvault opens **Messages** and the calendar
if the person has archived activity, or **Details** if they do not. Each field
value has its own row that you can click to edit. Below the existing values,
the add line offers **Add phone**, **Add email**, **Add address**, **Add
birthday**, and **More fields**. Once a phone, email, or address kind is
filled, it moves into **More fields**. After you fill in a birthday, open
**Date** in **More fields** to edit dates. Removing a value asks for
confirmation and keeps the row visible until the save succeeds.

**More actions** in the person header holds **Rename…**, **Keep this profile
updated**, **Publish to address book…**, **History**, and **Delete…**. **Keep
this profile updated** is a switch inside the menu; the line beneath it shows
when you turned it on, and a save started there finishes even after the menu
closes. When People lookups are not set up, that item reads **Set up…** and
opens them. **History** lists merges first, followed by names, email and phone,
and each field, with earlier values folded under each group. Sensitive
attributes stay hidden until you reveal them.

Below the profile fields, **Work** and **Connections** keep employment and
typed relationships editable. Use each row's **⋯** menu to edit, end, or delete
a job, mark it primary, or edit or delete a connection. **Add job** can create
an employer directly in its Organization picker. If the person has current jobs
and none is primary, **Add job** offers a **Primary job** option, off by
default; to replace an existing primary job, use that job's **⋯** menu. In the
Organization dialog, **Delete organization** lists everyone who holds a job
there, and you need to delete those jobs first, including ended jobs. Imported
connections appear under **From address books**. If a save conflicts with
changes made elsewhere, the dialog shows the saved record beside your form with
**Keep yours** (which saves your form) and **Use saved**. Ended jobs and
connections tuck into past-job and past-connection folds.

**Network** shows current jobs and connections across **Direct**, **2 steps**,
or **3 steps**, up to 250 people and organizations. **Direct** shows a one-line
summary that names the people and organizations directly linked to this person,
while the deeper views list names along with their relationship and the path
through another person or organization, marking links at the same step with
**with**. Select any name to open that person or organization. **Network**
stays hidden for someone with no direct connections unless loading it fails.

**Tasks** holds linked tasks and custom lists. **Facts** opens the fact ledger.
**Add task** in the Tasks header opens a title and list form with **Add task**
and **Cancel**. An empty list reads **No tasks yet**; when Tasks is off, **Set up…** opens settings.
**Meetings** reads **No meetings yet** once both meeting metrics and follow-ups
load empty. **Last time we talked** manages brief enrollment, generation,
rejection, sentence sources, and earlier versions. Generating a brief requires
a consented provider and uses its budget; see
[person briefs](/docs/usage/people-briefs/).

You can manage organizations, connection types, and field definitions under
**Settings → People**, where catalog edits save immediately.

To review potential merges, choose **Filter → Possible duplicates** and compare
names, addresses, activity, and evidence. The People badge counts waiting pairs
and displays **500+** once it reaches 500. Both **Merge** and **Keep separate**
offer **Undo** before recording your decision, and **History** lists earlier
decisions. Merging two saved profiles requires choosing a survivor and then
confirming the refreshed pair. **Evidence and notes** keeps the reviewed
details and decision note beside the pair.

Open **Facts** in a person's **Details** to inspect claims, evidence, and
decisions. **Claims** opens first, and choosing a field shows its saved value
above its claims. **Claim Details** links to the decision and sources. Scored
decisions display the claim score and its maximum, with each score part in
**Details**, while older decisions without a result say **Result unavailable**.
The closed title tells you how many fields Facts found and when it last found
them. Select **Manage fields…** to open **Settings → People → Fields**.

Imported address-book connections appear beneath **Connections** with their
status and source, including unmatched values. **Linked addresses** lists
archive identities, including addresses brought in by a merge, with **Unlink**
for linked profiles. It omits the profile's own emails and phones that already
appear in the profile fields, while linked profiles keep their **Unlink**
controls. To separate saved profiles after a merge:

1. Open **More actions → History** and select the merge, for example "Neri Bloom
   merged into Mira Vale". Its two profiles appear side by side with their
   addresses.
2. Select **Split…** to open the next step in the same dialog.
3. Check the addresses you want to move to a new person.
4. Select **Split into new person**. The dialog ends with **Split complete**
   and shows the two people that now exist.

Merge history and reversal follow the boundaries in
[People](/docs/usage/people/).

**Domains** gives you the same activity and files analysis for an exact domain
fact rather than an inferred organization identity. Selecting any group in
Messages opens its inspector with the item count, date range, activity, and the
people who appear with it most. Person and domain inspectors also show related
files and meeting activity when meetings exist.

## Saved views

Saved views store analytical contexts in the daemon, so every authenticated
browser connected to this single-user archive shares the same library. Each
view records its query, explicit search mode, filters, grouping, presentation,
sort, and visible columns. Row selection is intentionally not saved. The
inspector stays pinned, and the browser does not save or apply inspector pin
preferences.

To save a view:

1. Select **Save view…** in the Messages or Files page header.
2. Give the view a name and an optional description.
3. Select **Save**.

A view saved from Files does not keep the Filename filter, Type filter, or file
sort. You can open any saved view from its row under **Messages** or **Files**,
and hover over it to see a summary and description in its tooltip. The row's
**⋯** menu offers **Rename**, **Edit description…**, and **Delete**. **Rename**
edits the name inline; press Enter to save or Escape to cancel. **Delete**
hides the view and gives you eight seconds to select **Undo** before removing
it for every connected browser.

An incompatible view stays dimmed with its reason in the tooltip, and its menu
offers only **Delete**. Updates and deletions carry the record revision. If
another browser changes the view first, msgvault reports a conflict or restores
the row after a failed deletion.

The daemon validates new definitions against the saved view vocabulary. Schema
3 preserves local date bounds, schema 2 uses UTC dates, and schema 1 views keep
pipes as literal text. You can open a stored view here, run it from an API
client with `POST /api/v1/saved-views/{id}/run`, or run it from an AI assistant
with the [MCP server](/docs/usage/chat/#saved-views)'s `run_saved_view` tool.
All three read the same records and see the same revisions.

## Sources and sync status

Sources displays a table with **Source**, **Status**, **Last good sync**, and
**Sync now**. **Status** shows Running, Succeeded, Partial, Failed, Paused,
Queued, Cancelled, or Never synced. Contacts accounts appear in their own
group. Sources stays usable while contacts status loads, and contacts-only runs
keep polling.

Select any source to open its panel and inspect its Schedule, Next sync, and
attachment rules. Select **Edit in Settings › Sources** to open the editor for
settings shared by that source type. The panel also lists Runs, item errors,
and **Sync now**, showing a latest run's error ahead of a scheduler error.
Select **Sync history** to open Past runs filtered to source sync.

A contacts panel shows its repair reason and **Open Settings › Contacts** for
sign-in recovery. That editor keeps the selected OAuth app and gives you
commands for both shells. Mail sync instructions in the source panel give
`msgvault sync --source-id N`, or `msgvault sync-full --source-id N` for a
Gmail or IMAP source that has never synced successfully.

**Sync now** is available only when the source reports that capability. When it
is not, the **Sync now** column explains why in plain words: "Imported file —
nothing to sync", "Sync in progress", "Scheduler unavailable", "Sync not set
up", or "Sync unavailable". Meeting-import and message-import sources both show
"On demand" instead, because their data arrives through the API rather than a
sync.

A `202 Accepted` response means the daemon accepted the request, not that the
work has finished. While the tab is visible, the UI polls source status with
bounded backoff to show the run and live progress without opening a streaming
connection; polling pauses when the tab is hidden and resumes when you return.
Status and **Sync now** requests time out after 20 seconds. If a sync fails,
select **Refresh** to check source status without starting another sync. If
**Sync now** times out, refresh before trying again; the daemon may have
accepted the request. A failed first load keeps its error on screen until you
select **Retry**.

When the scheduler holds the sync lock without an active run, the UI makes up
to eight automatic refresh attempts, including failed or timed-out requests.
After that, it pauses automatic refresh and shows "Automatic refresh paused.
Source status may be stale." Select **Refresh** to poll again. If an accepted
run never appears, the UI shows "The sync was requested, but it hasn't started
yet. Refresh to check again." instead of claiming success. Conflicting runs and
unavailable capabilities keep their explicit errors or reasons. Full resync,
pause/resume, schedule editing, and source removal are not available in
Sources.

To add a source, select **Add source…** (or **Add your first source** on an
empty Sources page). You can choose Gmail, Microsoft 365 or Outlook.com, IMAP
mailbox, or Contacts. Choosing Contacts opens **Settings › Contacts**. The
other options ask for an address, or an IMAP server and username, and then show
the command to run, quoted for macOS or Linux or for Windows when the two
differ. Selecting **Copy** changes the button label to "Copied" for two
seconds. Run the command, then select **Done** to reload Sources.

Viewing Sources does not repair runs left marked as running after a crash. They
stay active until the daemon restarts or another sync for that source acquires
its lock and recovers them. Until then, **Sync now** stays unavailable and
status polling continues while the tab is visible.

## Background jobs and past runs

While the tab is visible, job status refreshes every 30 seconds on every page
and whenever the window regains focus.

Select the footer status dot to view queued jobs, running jobs, and unresolved
failures. The dot reads **Idle** when the list is empty, **Queued** while work
waits to run, **Running** while work is active, and **Needs attention** while a
latest run failed or the scheduler reports an error. Failed rows display the
daemon's run or item error. If a status read fails, the menu keeps the last
known jobs and reports that status is unavailable. Select any job to open its
source, contact settings, index status, or owning setting. Scheduled source
syncs show Queued while waiting to execute, and a previous sync failure stays
visible until execution starts.

To browse history, select **Past runs…** in the status menu. You can filter by
**Kind** (including all runs in a lane), **State**, or dates, select **Show
more** for the next page, and select **Refresh** to reload the current history.
Run links preserve filters and selection when shared or restored with Back. If
history changes while you are paging, select **Restart** to reload the first
page. Unavailable history is labeled by kind.

Select a run to inspect its timestamps, counters, error, related status, and
advertised actions, then select **All runs** to return to the list. Related
status opens Sources, Contacts settings, or live visual index counts. **Start
CardDAV sync**, **Build visual index**, and **Resume visual index** appear only
when the daemon advertises them. Sources keeps **Sync now** and opens filtered
history through **Sync history**, while Contacts settings opens filtered
history through **View CardDAV operations**.

**Settings › People › Fields** lists system and custom field definitions, plus
Work, with descriptions, kind, value type, cardinality, sensitivity, and active
state. Use **Search fields** to filter the displayed names, descriptions, and
metadata. Custom fields offer **Edit**.

**Settings Overview** shows current feature status, and **Details** opens live
counts for configured Visual search, including features with no past run.

Old Operations links with a run, status, or history filter open Past runs over
Settings Overview, while links without them open Settings Overview. Old Saved
views links open Messages.

## Deletions

Messages supports selecting individual rows as well as selecting all rows that
match your current query and filters. Select **Review deletion…** in the
selection bar, or press `d` or `D`, to open a review dialog over Messages. The
daemon preflights your selection before enabling staging.

The dialog shows the selected count and estimated size, eligible and skipped
counts, exclusions, and when the review expires. Semantic search reviews warn
that they cover active messages only. Select **Dry run** to check matched,
staged, and skipped counts without creating a batch, or select **Stage N
messages** to create the batch and view a success summary with its ID. Select
**Done** to return to Messages. Staging supports Gmail and Microsoft Graph
mail; run `msgvault delete-staged` to execute deletion.

The **Staged deletions** workspace lists batches by creation date, message
count, and status. Select a row to see its account and inspect its description,
summary, execution counts, and failed message IDs; if the account cannot be
read, the panel shows "Account unavailable". **Cancel batch** appears for
staged or running batches and asks for confirmation before cancelling. Turn on
**Show cancelled** to include cancelled batches in page history. When there are
no batches, the workspace says "Nothing staged". The sidebar entry appears
while a listed batch is pending, running, or failed, and **Go to Staged
deletions** in the command palette always opens the history.

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
| `⇧ Space` | Extend the selection to the focused row |
| `A` | Select all visible rows |
| `x` | Clear the selection |
| `D` / `⇧ D` | Review the selected or all matching items for deletion |
| `f` | Open Filters |
| `g` | Open Group by |
| `s` | Open Sort |
| `r` | Show sort order; in Files, point to the Sort menu |
| `?` | Open the searchable Keyboard shortcuts dialog |
| `Ctrl K`, `⌘ K` on Mac | Open the command palette |

Select **Help** in the sidebar, or press `?`, to search this list inside the
app. Destructive keys always open a review and never execute deletion
immediately. Shortcuts pause while you are typing and inside message or file
content, except that `Ctrl K` or `Command K` on Mac still opens the command
palette while typing in Help's search.

The command palette lists **Go to**, **Saved views**, **Actions**, and
**Appearance**, and remembers up to five recent commands across reloads. Choose
**Group by…** to pick an available grouping, or search for a dimension
directly. Type a query to find commands, search the archive from any workspace,
or choose **Find person…** to filter People. Help's **Search commands instead**
carries your query straight into the palette.

## Settings and restart behavior

Settings opens on **Overview**, showing status and setup or build actions for
background features. Select **Refresh feature status** to update **Overview**
after background work changes. **Build index** appears when the daemon offers
it and asks for confirmation before starting a full rebuild.

Settings is organized into nine categories: **Overview**, **Server**, **Search
& AI**, **People lookups**, **People**, **Tasks**, **Contacts**, **Sources**,
and **Advanced**. **Sources** includes source and attachment settings, and
**Advanced** includes archive settings and **Appearance**. **People lookups**
holds named enrichment providers, People sweep profiles, and **Fields kept
updated** (the fields **Keep this profile updated** may change), with sensitive
fields folded until you open them.

The daemon supplies allowed values and validation, while Settings provides
category and selected field labels, groups controls into the category rail, and
keeps tuning and host-managed values behind disclosures. The selected category
lives in the page link, so reload and Back keep you there. Old category links
redirect to their replacement category, and unknown categories open
**Overview**.

Each row shows the setting name, with limits built into the control itself:
number inputs carry their minimum and maximum, and syntax hints such as the
accepted duration format appear under a control only when the syntax needs one.
Settings where zero means "off", such as an attachment size cap that falls back
to the provider default, use a switch. Switch it off and the row tells you what
happens instead; switch it on and a value input appears starting from a
suggested value. Rows you have edited carry an amber dot.

A save bar appears at the bottom of the page only while you have unsaved
changes, counting them and offering **Discard** and **Save changes**. **Save
changes** stays disabled while any number field is empty. While a save is in
progress, fields and resource actions wait, though category navigation remains
available. When the bar disappears after a save or discard, focus moves to the
category heading.

Schedules fit on a single line. A **Presets** menu offers common schedules such
as every hour, every day at 03:00, or weekdays at 09:00, along with **Off** for
schedules that can be empty and **Custom**. Choosing **Custom** opens the
expression editor beside the menu starting from your previous preset. The five
fields are tinted, and while the editor has focus or your pointer hovers over
it, a small card names the fields (minute, hour, day, month, weekday) and
explains in plain English when the schedule runs. If you make a mistake, it
names the field and the problem before you save. A **Time zone** menu at the
end of the line runs the schedule in a chosen IANA zone instead of the daemon's
own clock, shown as "Server time", and stores your choice as a `CRON_TZ=`
prefix on the schedule. The CardDAV account form uses the same field, and the
Sources and CardDAV status views describe stored schedules the same way.

The **Save changes** bar covers ordinary settings, and its edits survive
category changes. A few controls save through their own buttons instead:
provider credentials, person-enrichment providers, contact accounts, and People
sweep. The bar stays visible on **Contacts** while ordinary settings have
unsaved changes. Leaving Settings or reloading the page prompts you first when
either the bar or one of those forms has unsaved edits; switching categories
keeps the bar's edits but drops unsaved edits in those forms. After a save that
needs a restart, Settings shows `msgvault daemon restart` to run on the
computer running msgvault. Reuse any custom `--home` and `--config` options, or
restart the daemon's service.

Saving makes targeted edits to `config.toml` while preserving your comments. If
another browser or a hand edit changes the configuration first, msgvault
rejects the stale edit; reload before saving again.

A theme or density saved under **Advanced** applies to the open tab at once
unless a **Display** menu override is active in that tab, in which case the
override still wins. To return to the saved values, choose **Use Settings
defaults** in **Display**. A saved default search mode does not change the
search you already have open, and tabs you open later in this browser without a
mode in their link use it.

Host-managed values, such as the listener address and the server API key
(`server.api_key`), show their current value under **Set in config.toml**.
Change them in `config.toml` on the daemon host. After the API key changes and
the daemon restarts, old browser sessions end and the login screen appears.

### Provider policies and credentials

Create or edit named person-enrichment policies for Exa and SixtyFour in
Settings. Provider checks and consent still govern whether enrichment can run,
and configuration alone does not authorize a provider. The TUI shows these
policies read-only. See
[External Person Enrichment](/docs/usage/people-enrichment/) for the provider
lifecycle.

Provider credentials for embeddings, enrichment, and sweeps are write-only,
along with the task integration key and the daemon's own API key. Each key sits
on one line with a read-only box, a pencil button, and a trash button that
removes a stored key. The box shows `None` when no key is set, or a masked hint
showing the first three and last three characters, such as `sk-…x9Q`, so you
can tell which key is in place. A key under twelve characters shows as dots
instead. Select the pencil button to open a dialog where you can paste a new
key. A person-enrichment key applies right away. Text and visual embedding keys
are stored at once, and their dialog notes that the daemon uses them after a
restart. The task integration key saves with **Save changes**. If a key comes
from an environment variable, the line says so underneath and you cannot clear
it from the browser.

Credentials carry a separate revision from `config.toml`. When you change both
an endpoint or model and its credential, save the endpoint or model first, then
save the credential. This binds the key to the destination it was entered for.

### CardDAV contacts

CardDAV settings manage your contact accounts and discovered address books.
Choose which books participate in sync, lookup, and publishing; publishing also
enables contact sync for that book. **Sync now** lets the daemon decide whether
to reconcile the full address book. To force a full sync, run
`msgvault sync-carddav --full`. The workspace also offers recent run history
and conflict review, where you can compare local and remote versions before
deciding which to keep. See [People and CardDAV](/docs/usage/people-carddav/)
for setup and publishing rules.

## Optional integration states

The optional task integration is server-side and provider-neutral. Instead of
reporting a failed lookup as "no links.", msgvault shows disabled, discovering,
authentication required, reachable but incompatible, partial, stale,
unavailable, or ready. Credentials never enter browser types, URLs, or error
messages, and the archive remains fully usable while the integration is absent or
unhealthy.
