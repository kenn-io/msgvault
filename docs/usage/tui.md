---
last_edited: "2026-09-08"
title: Interactive TUI
description: Browse messages and people, search your archive, save attachments, and stage email deletion from the terminal.
---

The TUI lets you explore your archive from the terminal. Group messages by
sender, list, or date; open a conversation; find an attachment; or review a
person before your next conversation.

1. Run `msgvault tui`.
2. Press `m` to cycle through Email, Texts, Meetings, and People. Unavailable
   Texts and People modes are skipped.
3. Use `j`/`k` or the arrow keys to move, `Enter` to open a row, and `Esc` to go
   back. Press `?` for the shortcuts available in the current mode.

## Launch

```bash
msgvault tui
```

The TUI always talks to a msgvault HTTP server. Without `[remote].url`, it starts or reuses the local background daemon. The daemon owns database access, analytics engine selection, and cache rebuilds.

The daemon starts its HTTP health endpoint and API routes before analytics cache
maintenance. Clients can connect while startup work runs.

With `engine = "auto"` (the default), aggregate views initially use live SQL
(`sql-fallback`). When `auto_build_cache = true` and the cache is stale or
missing, the daemon builds it in the background; after the cache is ready and
DuckDB opens successfully, aggregate views switch to DuckDB. A failed build or
open leaves auto mode on live SQL. With `engine = "duckdb"`, analytics remain
unavailable until the required cache is ready; analytics routes return `503`
during initialization. `engine = "sql"` always uses live SQL. Configure engine
selection in `config.toml`:

```toml
[analytics]
engine = "auto"          # auto, sql, or duckdb
auto_build_cache = true  # build a stale/missing cache in the background at startup
```

Opening aggregate views does not trigger a build mid-session. Scheduled syncs
and ingest commands can refresh the cache, and `msgvault build-cache` builds or
repairs it on demand. Set `auto_build_cache = false` to skip automatic startup
maintenance; use `msgvault build-cache` for an explicit build. See
[Configuration: analytics](/docs/configuration/#analytics).

### Local And Remote

When your `config.toml` has a `[remote]` section configured, the TUI connects to the remote server automatically. All views, drill-downs, search, and filtering work the same as local-daemon mode. Use `--local` only when you want to force this machine's local daemon instead of the configured remote server.

```bash
# Connects to remote server if [remote] is configured
msgvault tui

# Force this machine's local daemon
msgvault tui --local
```

Deletion staging and attachment export use the selected daemon. When connected to a configured remote server, staged deletion manifests are saved on that remote host; attachment export streams bytes from the daemon and writes the zip file on the CLI machine.

### Email Account and Collection Scopes

Press `A` in Email mode to choose `All Accounts`, an individual account, or a
named collection. A collection is a daemon-owned group of accounts, and the
title shows `Collection: <name>` while its exact member sources scope
aggregates, message lists, fast search, statistics, and deletion-target
inspection. An empty collection shows no results and makes no scoped HTTP read.

Collections require a daemon with API schema `2.17.0` or newer. Older or
unavailable daemons keep the account selector usable and hide collection rows.
Changing the Email scope returns to the top-level view and clears the current
search and selection. Texts and Meetings keep separate account or source
selectors; changing one mode's selection does not change another mode's scope.

Collections with multiple sources offer Fast search only. Deep search is
available for single-source collections; Semantic search requires an individual
account or All Accounts, subject to its other filter limits.

Deletion staging requires the selected messages to belong to one source.
You can stage messages from one source within a larger collection, but a
selection spanning sources is rejected. The TUI does not offer deduplication
operations; use the collection-scoped CLI commands for those.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-senders.svg" alt="msgvault TUI showing the Senders view with message counts and sizes" loading="lazy">
</figure>
Press `a` from any aggregate view to show all individual messages in that view. Press `Enter` on a message to view its full detail, including headers and body.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-all-messages.svg" alt="msgvault TUI showing all messages list" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">All messages</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-message-detail.svg" alt="msgvault TUI showing a single message detail view" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Message detail</figcaption>
  </figure>
</div>

## View Modes

Email has eight aggregate views. Press `g` or `Tab` to cycle through them:

| View | Description |
|---|---|
| Senders | Aggregate by sender email address |
| Sender Names | Aggregate by sender display name (falls back to email when no name is set) |
| Recipients | Aggregate by recipient email address |
| Recipient Names | Aggregate by recipient display name (falls back to email when no name is set) |
| Domains | Aggregate by sender domain |
| Labels | Aggregate by Gmail label |
| Lists | Aggregate by the email's `List-Id` header; press `l` to jump here |
| Time | Aggregate by time period (year/month/day) |

<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-labels.svg" alt="msgvault TUI Labels view showing Gmail label breakdown" loading="lazy">
</figure>

### Time View

Press `t` from any view to jump directly to the Time view. The Time view aggregates messages by time period. When already in Time view, pressing `t` cycles between monthly, daily, and yearly granularity:

<div style="display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 0.5rem;">
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-time-monthly.svg" alt="Time view: monthly granularity" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Monthly</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-time-daily.svg" alt="Time view: daily granularity" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Daily</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-time-yearly.svg" alt="Time view: yearly granularity" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Yearly</figcaption>
  </figure>
</div>

## Text Messages

Press `m` to switch modes. Texts is skipped when no text/chat engine is
available, but Meetings remains in the cycle even before a meeting source has
been configured. See [Text Messages](/docs/usage/text-messages/)
for local chat imports, [Microsoft Teams](/docs/usage/teams/) for Teams sync, and
[Discord](/docs/usage/discord/) for guild channel and thread sync.

Text mode provides the following view types. Press `g` to cycle through them:

| View | Description |
|---|---|
| Conversations | Aggregate by individual conversation |
| Contacts | Aggregate by contact phone number or identifier |
| Contact Names | Aggregate by contact display name (falls back to phone/ID when unavailable) |
| Sources | Aggregate by text service provider |
| Labels | Aggregate by custom labels or categories |
| Time | Aggregate by time period (year/month/day) |

Navigation and interaction in Text mode work the same as Email mode. Press `Enter` to drill into a conversation and view individual messages. Press `Esc` or `Backspace` to go back. Use `g` to re-aggregate from a drill-down view, `/` to search, and `f` to filter. The stats display shows message count and total size for text conversations.

## Meetings

Meetings mode is a read-only browser for archived transcripts and notes from
[meeting sources](/docs/usage/meetings/), including Granola, Circleback, and
Notion. It shows a flat, newest-first list
with each meeting's date, title, organizer, and source. Press `Enter` to open
the transcript and notes, `Esc` or `Backspace` to return to the list, and the
left/right arrow keys to move between meeting details.

Press `/` in the meeting list to search titles, people, transcripts, and notes.
Meeting search is always scoped to meeting transcripts, including when the
search text contains explicit message-type syntax. In the detail view, `/`
finds text within the current transcript; use `n` and `N` to move between
matches.

Press `A` to select all meeting sources or one configured source. This source
choice is independent of the Email account filter. If an
older remote daemon does not provide source identity, the Source column shows
`—` instead of a misleading blank value.

Selection and deletion keys are disabled in Meetings mode. Meeting sync is
read-only, and browsing a transcript never changes the source service.

## People

People brings the contact directory into the terminal. Press `/` to search
names and identifiers, `Enter` to open a person, and `Tab` or `Shift+Tab` to
move between their detail tabs. The browser brings profile information,
attributes, inboxes, meetings, files, and dated activity together. See
[People, Profiles, and Source Identities](/docs/usage/people/)
for curation, tracking, and provider setup.

### Person briefs

In the People browser, a person's Overview tab shows their **Last time we
talked** brief: a short summary to read before your next conversation. Press
`b` for its details and sources, then `b` or `Esc` to return.

Press `:` to enter `brief enroll`, `brief generate`, or `brief reject <reason>`.
Enrollment also enables tracking. Generation sends eligible message text to
your consented provider and spends its budget. A person needs a saved profile
first; see [person briefs](/docs/usage/people-briefs/)
for setup and supported sources.

## Drill-down and Sub-grouping

Press `Enter` to drill into any row. For example, selecting a sender shows their individual messages. Press `Esc` or `Backspace` to go back.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-drilldown.svg" alt="msgvault TUI drill-down showing messages from a specific sender" loading="lazy">
</figure>
From a drill-down view, press `g` to re-aggregate the filtered messages by a different dimension. You can think of this like an interactive pivot table. The cycle skips the dimension you drilled into — and when drilling from an email address view (Senders or Recipients), it also skips the corresponding name view since it would be redundant. For example, drilling into a sender and pressing `g` cycles through Recipients, Recipient Names, Domains, Labels, and Time.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-subgroup-recipients.svg" alt="Sub-grouped by Recipients after drilling into a sender" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">A sender's email grouped by recipient</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/docs/assets/generated/tui-subgroup-time.svg" alt="Sub-grouped by Time after drilling into a sender" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">A sender's mail grouped by month</figcaption>
  </figure>
</div>

## Searching

Press `/` to open a search bar that filters the current view in real time. Matching text is highlighted in the results. At the aggregate level (Senders, Domains, etc.), search uses the daemon's configured analytics engine, so the default local-daemon setup uses DuckDB over Parquet when the cache is usable.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-search-sender.svg" alt="msgvault TUI search filtering senders by name with highlighted matches" loading="lazy">
</figure>
In a message list, press `Tab` while the search bar is open to choose a mode:

| Mode | What it searches | When to use it |
|---|---|---|
| Fast | Cached metadata, including subject and snippet | Narrow a large list quickly |
| Deep | Subjects and full message bodies | Find a word absent from the snippet |
| Semantic | Hybrid keyword and vector ranking over active messages | Describe a topic in your own words |

The TUI offers only modes that can preserve the current scope. Semantic search
needs a configured [vector index](/docs/usage/vector-search/) and free text.
It is unavailable inside sender-name, recipient-name, or mailing-list
drill-downs and for named collection scopes. Deep search supports collections
with one source; collections with several sources offer Fast search.

Press `Enter` to keep the search, or `Esc` to clear it and restore the prior
list. While the search bar is open, `↑` and `↓` recall recent searches from
this session. Deep search loads another page when you navigate to the bottom;
semantic results are bounded by the server's ranking window.

You can progressively narrow a question: search the sender list, open a sender,
then search their messages. See [Searching](/docs/usage/searching/) for query
operators, including `list:` and `conversation_id:`.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-search-drilldown.svg" alt="Drilled into search result showing messages from a specific sender" loading="lazy">
</figure>
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-search-subject.svg" alt="Searching within a sender's messages by subject keyword with highlighted matches" loading="lazy">
</figure>
</div>

## Filtering

Press `f` to open the filter modal. The modal presents two independent toggles that you can combine:

| Filter | Effect |
|---|---|
| Only with attachments | Show only messages that have attachments |
| Hide deleted from source | Exclude messages marked deleted from their source |
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-filter-modal.svg" alt="msgvault TUI filter modal with checkbox toggles for attachments and hide deleted" loading="lazy">
</figure>
Use `↑`/`↓` to navigate, `Space` or `x` to toggle a filter, and `Enter` or `Esc` to apply and close. Active filters are shown in the title bar (e.g. `[Attachments]`, `[Hide Deleted]`). Filters apply to all views: aggregates, drill-downs, sub-aggregates, search results, and stats.

## Viewing Email Threads

From any message list (after drilling into a sender, label, domain, etc.), press `T` to open the full email thread for the highlighted message. This renders the complete conversation inline in the terminal, including sender, date, and body text for each message in the thread.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-thread.svg" alt="msgvault TUI showing a full email thread conversation" loading="lazy">
</figure>
Press `Esc` to return to the message list.

## Save an email

Open an email with `Enter`, then press `s` to save its original MIME content,
including attachments, as `message-<id>.eml` in the directory where you launched
the TUI. Existing files are preserved: repeated saves add `_1`, `_2`, and so on.
The result dialog shows the saved path or the error if the email could not be
saved. This requires raw email content in the archive.

When connected to a remote server, the file is saved on the machine running the
TUI. In message lists and aggregate views, `s` continues to cycle the sort field.

## Keyboard Shortcuts

| Key | Action |
|---|---|
| `j` / `k`, `↑` / `↓`, or `Ctrl+N` / `Ctrl+P` | Navigate rows |
| `Enter` | Drill down into selection |
| `T` | View full email thread |
| `Esc` / `Backspace` | Go back |
| `m` | Cycle Email, Texts, Meetings, and People; skip unavailable modes |
| `g` / `Tab` | Cycle aggregate views; `Tab` cycles search modes while typing in a message-list search |
| `s` | Save email as `.eml` in message detail; cycle sort field in lists |
| `v` / `r` | Reverse sort direction in Email and Texts |
| `t` | Jump to Time view (cycle granularity when already in Time) |
| `l` | Jump to Lists view from an Email aggregate or message list |
| `a` | Show all individual messages in current view |
| `A` | Filter by account or named Email collection; select a source in Meetings mode |
| `f` | Open filter modal |
| `Space` | Toggle selection |
| `S` / `x` | Select all visible rows / clear selection |
| `d` | Review staging for selected rows, or the current row when nothing is selected |
| `D` | Review staging for the current aggregate row or all messages matching the current list/filter |
| `/` | Search |
| `e` | Browse attachments in message detail |
| `,` | Open Settings (keyboard-only; unavailable while a search box or modal is open) |
| `?` | Help |
| `q` | Quit |

## Marking Emails for Deletion

Use `Space` to select rows and `d` to review them for staging. With no selection,
`d` uses the highlighted row. Press `D` to target the highlighted aggregate
group, or all messages matching the current message list and its filter.

Fast and Deep searches preserve the current search when resolving deletion
targets, including matches beyond the visible page. Semantic search allows
staging explicit result rows; it does not offer an all-matching deletion because
its results are a bounded ranking. The selection must belong to one source.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-selection.svg" alt="msgvault TUI with rows selected for deletion staging" loading="lazy">
</figure>
A confirmation dialog shows exactly how many messages will be staged before anything happens. Messages are not deleted immediately; they are placed in a deletion batch that you review and execute separately with `msgvault delete-staged`.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-deletion.svg" alt="msgvault TUI deletion confirmation dialog showing bulk staging" loading="lazy">
</figure>
See [Deleting Email](/docs/usage/deletion/) for the full deletion workflow.

## Save or open attachments

Open a message and press `e` to browse its attachments:

| Key | Action |
|---|---|
| `Space` | Toggle an attachment in the ZIP selection |
| `a` / `n` | Select all / select none |
| `Enter` | Save selected attachments as a ZIP |
| `d` | Download the highlighted attachment as a file |
| `o` | Open the highlighted attachment with the system's default app |
| `Esc` | Close the attachment browser |

Files are written on the machine running the TUI, including when the archive
is on a remote daemon. Set `[data].export_dir` to choose the directory; the
default is `exports/` under the local data directory. Individual downloads
choose a new filename when one already exists. The result dialog reports the
saved path and any per-file failures.

Opening archived content first downloads it locally. URL-backed attachments
open their HTTP or HTTPS address and cannot be downloaded through this dialog.
Executable or active-content formats may be blocked from automatic opening;
use download when you need to inspect such a file yourself.

## Settings

Press `,` outside an input or modal to edit settings on the selected daemon.
Use the arrow keys to choose a category and field, `Enter` to edit, `Space` to
toggle a boolean, and `Ctrl+S` to save. `Esc` asks before discarding unsaved
changes. Fields marked restart-required remain pending until the daemon
restarts. Named person-enrichment provider policies are read-only here; edit
them in [Web Settings](/docs/web-ui/#settings-and-restart-behavior).

## Performance

The default SQLite archive path uses DuckDB over Parquet metadata exports for
aggregate views. This avoids repeated joins over the full archive when grouping
by sender, domain, label, or date. Message bodies stay in the archive and are
loaded for detail views and full-text search. Configure `[analytics].engine`
if you need to force live SQL or require DuckDB.

See [Data Storage](/docs/architecture/storage/) for details on how this works.
