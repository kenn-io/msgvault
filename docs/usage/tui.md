---
last_edited: "2026-11-20"
title: Interactive TUI
description: Browse messages and people, search your archive, save attachments, and stage email deletion from the terminal.
---

Use the TUI to browse your archive right from the terminal. You can group messages by sender, list, or date, open a conversation, find an attachment, or look up a person before your next conversation.

1. Run `msgvault tui`.
2. Press `m` to cycle through Email, Texts, Meetings, and People. The TUI skips Texts or People if those modes are unavailable.
3. Move with `j`/`k` or the arrow keys, press `Enter` to open a row, and press `Esc` to go back. Press `?` to see the shortcuts for your current mode.

## Launch

```bash
msgvault tui
```

The TUI always talks to a msgvault HTTP server. If you haven't set `[remote].url`, it starts or reuses the local background daemon, which owns database access, analytics engine selection, and cache rebuilds.

The daemon opens its HTTP health endpoint and API routes before running analytics cache maintenance, and you can connect while startup work is still running.

With the default `engine = "auto"`, aggregate views start on live SQL (`sql-fallback`). If `auto_build_cache = true` and the cache is missing or stale, the daemon builds it in the background and switches aggregate views to DuckDB once the cache is ready and DuckDB opens. If the build or open fails, auto mode stays on live SQL. With `engine = "duckdb"`, analytics stay unavailable and return `503` until the required cache is ready. With `engine = "sql"`, aggregate views always use live SQL. You can configure engine selection in `config.toml`:

```toml
[analytics]
engine = "auto"          # auto, sql, or duckdb
auto_build_cache = true  # build a stale/missing cache in the background at startup
```

Opening aggregate views does not start a cache build mid-session. Scheduled syncs and ingest commands can refresh the cache, and you can run `msgvault build-cache` to build or repair it on demand. To skip automatic startup maintenance, set `auto_build_cache = false` and run `msgvault build-cache` when you want a build. See [Configuration: analytics](/docs/configuration/#analytics).

### Local and remote

If your `config.toml` has a `[remote]` section, the TUI connects to that remote server automatically, and all views, drill-downs, search, and filtering work the same as in local-daemon mode. Pass `--local` only when you want to force this machine's local daemon instead of the configured remote server.

```bash
# Connects to remote server if [remote] is configured
msgvault tui

# Force this machine's local daemon
msgvault tui --local
```

Deletion staging and attachment export both use the selected daemon. When you connect to a remote server, staged deletion manifests live on that remote host, while attachment export streams the bytes from the daemon and writes the zip file on the CLI machine.

### Email account and collection scopes

In Email mode, press `A` to pick `All Accounts`, a single account, a receiving address, or a named collection. When confirmed addresses divide a source's mail, the picker lists each address under that source along with an **Unattributed** entry for mail that no confirmed address claims; see [Searching](searching.md#find-mail-by-the-address-that-received-it). A collection is a daemon-owned group of accounts. While one is active, the title shows `Collection: <name>` and its exact member sources scope aggregates, message lists, fast search, statistics, and deletion-target inspection. An empty collection shows no results and makes no scoped HTTP read.

Collections require a daemon with API schema `2.17.0` or newer. If the daemon is older or unavailable, the account selector stays usable and hides collection rows. Switching the Email scope returns you to the top-level view and clears your current search and selection. Texts and Meetings keep their own account or source selectors; changing one mode's scope leaves the other modes unchanged.

Collections with multiple sources offer Fast search only. Deep search works on single-source collections, and Semantic search requires an individual account or All Accounts, subject to its other filter limits.

To stage messages for deletion, every selected message must belong to one source. You can stage messages from a single source inside a larger collection, and the TUI rejects any selection that spans multiple sources. The TUI does not offer deduplication operations; use the collection-scoped CLI commands for those.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-senders.svg" alt="msgvault TUI showing the Senders view with message counts and sizes" loading="lazy">
</figure>
From any aggregate view, press `a` to list all individual messages in that view, then press `Enter` on a message to read its headers and body.

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

## View modes

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

### Time view

Press `t` from any view to jump straight to the Time view, which groups messages by time period. Once you are in Time view, pressing `t` again cycles through monthly, daily, and yearly granularity:

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

## Text messages

Press `m` to switch modes. If no text/chat engine is available, the TUI skips Texts, while Meetings stays in the cycle even before you configure a meeting source. See [Text Messages](/docs/usage/text-messages/) for local chat imports, [Microsoft Teams](/docs/usage/teams/) for Teams sync, and [Discord](/docs/usage/discord/) for guild channel and thread sync.

Text mode provides the following view types. Press `Tab` to cycle through them (`Shift+Tab` goes back):

| View | Description |
|---|---|
| Conversations | Aggregate by individual conversation |
| Contacts | Aggregate by contact phone number or identifier |
| Contact Names | Aggregate by contact display name (falls back to phone/ID when unavailable) |
| Sources | Aggregate by text service provider |
| Labels | Aggregate by custom labels or categories |
| Time | Aggregate by time period (year/month/day) |

Navigation in Text mode works the same as in Email mode. Press `Enter` to drill into a conversation and view its messages, `Esc` or `Backspace` to go back, and `/` to search. The stats display shows message count and total size for text conversations.

## Meetings

Meetings mode lets you browse archived transcripts and notes from [meeting sources](/docs/usage/meetings/), including Omi, Granola, Plaud, Circleback, Notion, Muesli, Twilio, Bland, Twenty, and Chatwoot. It shows a flat, newest-first list with each meeting's date, title, organizer, and source. Press `Enter` to open a meeting's transcript and notes, `Esc` or `Backspace` to return to the list, and the left or right arrow keys to step between meeting details.

In the meeting list, press `/` to search titles, people, transcripts, and notes. Meeting search stays scoped to meeting transcripts, even if your query includes explicit message-type syntax. Inside a detail view, press `/` to find text in the current transcript, then press `n` and `N` to jump between matches.

Press `A` to pick all meeting sources or a single configured source. That choice is independent of the Email account filter. If an older remote daemon does not provide source identity, the **Source** column displays `—` instead of a misleading blank value.

Meetings mode disables selection and deletion keys. Meeting sync is read-only, and browsing a transcript never modifies the source service.

## People

People brings your contact directory into the terminal. Press `/` to search names and identifiers, `Enter` to open a person, and `Tab` or `Shift+Tab` to move between their detail tabs. Each person's view pulls together their profile information, attributes, inboxes, meetings, files, and dated activity. See [People, Profiles, and Source Identities](/docs/usage/people/) for curation, tracking, and provider setup.

The Relationship calendar groups days in your system's named timezone. If your system zone can't be named, such as on Windows, it falls back to UTC and displays "UTC" next to the year. Dated activity lists show local time.

### Person briefs

In the People browser, a person's **Overview** tab shows their **Last time we talked** brief, a short summary you can read before your next conversation. Press `b` to view its details and sources, then press `b` or `Esc` to go back.

Press `:` to run `brief enroll`, `brief generate`, or `brief reject <reason>`. Enrolling a person also turns on tracking. Generating a brief sends eligible message text to your consented provider and spends its budget. A person needs a saved profile first; see [person briefs](/docs/usage/people-briefs/) for setup and supported sources.

## Drill down and sub-group

Press `Enter` on any row to drill into it. For example, selecting a sender lists their individual messages. Press `Esc` or `Backspace` to go back.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-drilldown.svg" alt="msgvault TUI drill-down showing messages from a specific sender" loading="lazy">
</figure>
From a drill-down message list, press `g` to group those filtered messages by another dimension, much like an interactive pivot table. When you drill in from Senders or Recipients, that first grouping skips the matching name view; drilling into a sender opens Recipients rather than Sender Names. After that, each `g` press cycles through every view except the one you drilled into. For example, if you drill into a sender and press `g`, the TUI opens Recipients, and each further `g` press cycles through Recipient Names, Domains, Labels, Lists, Time, and Sender Names.

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

## Search

Press `/` to open a search bar that filters the current view as you type and highlights matching text in the results. At the aggregate level, such as Senders or Domains, search runs on the daemon's configured analytics engine, so the default local-daemon setup queries DuckDB over Parquet whenever the cache is usable.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-search-sender.svg" alt="msgvault TUI search filtering senders by name with highlighted matches" loading="lazy">
</figure>
In a message list, press `Tab` while the search bar is open to switch search modes:

| Mode | What it searches | When to use it |
|---|---|---|
| Fast | Cached metadata, including subject and snippet | Narrow a large list quickly |
| Deep | Subjects and full message bodies | Find a word absent from the snippet |
| Semantic | Hybrid keyword and vector ranking over active messages | Describe a topic in your own words |

The TUI only offers modes that can preserve your current scope. Semantic search requires a configured [vector index](/docs/usage/vector-search/) and free text, and it is unavailable inside sender-name, recipient-name, or mailing-list drill-downs and for named collection scopes. Deep search works on single-source collections; collections with several sources offer Fast search.

Press `Enter` to keep the search, or press `Esc` to clear it and restore the previous list. While the search bar is open, press `↑` and `↓` to recall recent searches from the current session. Deep search loads another page when you reach the bottom of the list, while semantic results are bounded by the server's ranking window.

You can narrow a question step by step: search the sender list, open a sender, and then search their messages. See [Searching](/docs/usage/searching/) for query operators, including `list:` and `conversation_id:`.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-search-drilldown.svg" alt="Drilled into search result showing messages from a specific sender" loading="lazy">
</figure>
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-search-subject.svg" alt="Searching within a sender's messages by subject keyword with highlighted matches" loading="lazy">
</figure>
</div>

## Filter messages

Press `f` to open the filter modal, which gives you two independent toggles you can combine:

| Filter | Effect |
|---|---|
| Only with attachments | Show only messages that have attachments |
| Hide deleted from source | Exclude messages marked deleted from their source |
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-filter-modal.svg" alt="msgvault TUI filter modal with checkbox toggles for attachments and hide deleted" loading="lazy">
</figure>
Move between options with `↑`/`↓`, toggle a filter with `Space` or `x`, and press `Enter` or `Esc` to apply and close. Active filters appear in the title bar, for example `[Attachments]` and `[Hide Deleted]`, and apply across aggregates, drill-downs, sub-aggregates, search results, and stats.

## View email threads

From any message list after drilling into a sender, label, domain, or other group, press `T` to open the full email thread for the highlighted message. The TUI renders the entire conversation inline in the terminal, showing the sender, date, and body text for each message.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-thread.svg" alt="msgvault TUI showing a full email thread conversation" loading="lazy">
</figure>
Press `Esc` to return to the message list.

## Save an email

Open an email with `Enter`, then press `s` to save its original MIME content, including attachments, as `message-<id>.eml` in the directory where you launched the TUI. Saving does not overwrite existing files; repeated saves append `_1`, `_2`, and so on. A result dialog shows the saved path or reports an error if the email could not be saved. Saving requires raw email content in the archive.

If you are connected to a remote server, the TUI writes the file on the machine running the TUI. In message lists and aggregate views, `s` still cycles the sort field.

## Keyboard shortcuts

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

## Mark emails for deletion

Select rows with `Space` and press `d` to review them for staging. If nothing is selected, `d` targets the highlighted row. Press `D` to target the highlighted aggregate group, or all messages matching the current message list and filter.

Fast and Deep searches keep the active search when resolving deletion targets, including matches past the visible page. Semantic search lets you stage explicit result rows, and does not offer an all-matching deletion because its results are a bounded ranking. All selected messages must belong to one source.
<figure class="screenshot" data-lightbox>
  <img src="/docs/assets/generated/tui-selection.svg" alt="msgvault TUI with rows selected for deletion staging" loading="lazy">
</figure>
Before anything changes, a confirmation dialog shows the exact number of messages to stage. The TUI does not delete messages right away; it puts them into a deletion batch that you review and run separately with `msgvault delete-staged`.
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

The TUI writes exported files on the machine where it runs, even when the archive lives on a remote daemon. Set `[data].export_dir` to change the destination; otherwise files go to `exports/` under your local data directory. Individual downloads pick a new filename if one already exists, and the result dialog shows the saved path along with any per-file failures.

Opening an archived attachment downloads it locally first. URL-backed attachments open their HTTP or HTTPS address instead and cannot be downloaded from this dialog. The TUI may block executable or active-content formats from opening automatically; download those files when you want to inspect them yourself.

## Settings

Outside an input or modal, press `,` to edit settings on the selected daemon. Pick a category and field with the arrow keys, press `Enter` to edit, press `Space` to toggle a boolean, and press `Ctrl+S` to save. If you have unsaved changes, `Esc` prompts before discarding them. Fields marked restart-required stay pending until the daemon restarts. Settings displays the daemon host's restart command with custom option and service guidance, and `PgUp` and `PgDn` scroll long views. Named person-enrichment provider policies are read-only here; edit them in [Web Settings](/docs/web-ui/#settings-and-restart-behavior).

## Performance

For the default SQLite archive path, aggregate views query DuckDB over Parquet metadata exports instead of running repeated joins across the full archive when grouping by sender, domain, label, or date. Message bodies stay in the archive and load for detail views and full-text search. Configure `[analytics].engine` if you need to force live SQL or require DuckDB.

See [Data Storage](/docs/architecture/storage/) for details on how this works.
