---
title: Interactive TUI
description: Terminal interface for exploring email, text messages, and meeting transcripts.
---


## Launch

```bash
msgvault tui
```

The TUI always talks to a msgvault HTTP server. Without `[remote].url`, it starts or reuses the local background daemon. The daemon owns database access, analytics engine selection, and cache rebuilds.

The daemon picks its analytics engine once at startup: DuckDB over the Parquet cache when a fresh, complete cache exists, live SQL otherwise. Opening aggregate views does not trigger a cache build — the daemon refreshes a stale cache after its scheduled syncs, and `msgvault build-cache` builds one on demand. When the daemon reports it fell back to live SQL (a freshly restored archive with no cache, for example), `msgvault tui` prints a notice and repeats it on the TUI's info line while views load; after `msgvault build-cache`, restart the daemon to switch it onto the cache. Configure engine selection in `config.toml`:

```toml
[analytics]
engine = "auto"          # auto, sql, or duckdb
auto_build_cache = true  # startup build under engine = "duckdb" only; scheduled-sync refreshes ignore it
```

Deprecated in 0.17.0: the old `msgvault tui --force-sql`, `--no-cache-build`, and `--no-sqlite-scanner` flags are hidden because these choices are now daemon configuration. Use `engine = "sql"` for live SQL, `auto_build_cache = false` to skip the automatic startup cache build, or `msgvault build-cache` to prebuild cache files on the daemon host. See [Configuration: analytics](/configuration/#analytics).

### Local And Remote

When your `config.toml` has a `[remote]` section configured, the TUI connects to the remote server automatically. All views, drill-downs, search, and filtering work the same as local-daemon mode. Use `--local` only when you want to force this machine's local daemon instead of the configured remote server.

```bash
# Connects to remote server if [remote] is configured
msgvault tui

# Force this machine's local daemon
msgvault tui --local
```

Deletion staging and attachment export use the selected daemon. When connected to a configured remote server, staged deletion manifests are saved on that remote host; attachment export streams bytes from the daemon and writes the zip file on the CLI machine.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-senders.svg" alt="msgvault TUI showing the Senders view with message counts and sizes" loading="lazy">
</figure>
Press `a` from any aggregate view to show all individual messages in that view. Press `Enter` on a message to view its full detail, including headers and body.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-all-messages.svg" alt="msgvault TUI showing all messages list" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">All messages</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-message-detail.svg" alt="msgvault TUI showing a single message detail view" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Message detail</figcaption>
  </figure>
</div>

## View Modes

The TUI provides seven aggregate view modes. Press `g` to cycle through them:

| View | Description |
|---|---|
| Senders | Aggregate by sender email address |
| Sender Names | Aggregate by sender display name (falls back to email when no name is set) |
| Recipients | Aggregate by recipient email address |
| Recipient Names | Aggregate by recipient display name (falls back to email when no name is set) |
| Domains | Aggregate by sender domain |
| Labels | Aggregate by Gmail label |
| Time | Aggregate by time period (year/month/day) |
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-labels.svg" alt="msgvault TUI Labels view showing Gmail label breakdown" loading="lazy">
</figure>
### Time View

Press `t` from any view to jump directly to the Time view. The Time view aggregates messages by time period. When already in Time view, pressing `t` cycles between monthly, daily, and yearly granularity:

<div style="display: grid; grid-template-columns: 1fr 1fr 1fr; gap: 0.5rem;">
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-time-monthly.svg" alt="Time view: monthly granularity" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Monthly</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-time-daily.svg" alt="Time view: daily granularity" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Daily</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-time-yearly.svg" alt="Time view: yearly granularity" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">Yearly</figcaption>
  </figure>
</div>

## Text Messages

Press `m` to cycle through Email, Texts, and Meetings. Texts is skipped when no text/chat engine is available, but Meetings remains in the cycle even before a meeting source has been configured. See [Text Messages](/usage/text-messages/) for details on importing WhatsApp, iMessage, Google Voice, Facebook Messenger, and SMS Backup & Restore conversations, and [Microsoft Teams](/usage/teams/) for Teams sync.

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

Meetings mode is a read-only browser for transcripts and notes imported from
[Granola or Circleback](/usage/meetings/). It shows a flat, newest-first list
with each meeting's date, title, organizer, and source. Press `Enter` to open
the transcript and notes, `Esc` or `Backspace` to return to the list, and the
left/right arrow keys to move between meeting details.

Press `/` in the meeting list to search titles, people, transcripts, and notes.
Meeting search is always scoped to meeting transcripts, including when the
search text contains explicit message-type syntax. In the detail view, `/`
finds text within the current transcript; use `n` and `N` to move between
matches.

Press `A` to select all meeting sources or one configured Granola or Circleback
source. This source choice is independent of the Email account filter. If an
older remote daemon does not provide source identity, the Source column shows
`—` instead of a misleading blank value.

Selection and deletion keys are disabled in Meetings mode. Meeting sync is
read-only, and browsing a transcript never changes the source service.

## Drill-down and Sub-grouping

Press `Enter` to drill into any row. For example, selecting a sender shows their individual messages. Press `Esc` or `Backspace` to go back.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-drilldown.svg" alt="msgvault TUI drill-down showing messages from a specific sender" loading="lazy">
</figure>
From a drill-down view, press `g` to re-aggregate the filtered messages by a different dimension. You can think of this like an interactive pivot table. The cycle skips the dimension you drilled into — and when drilling from an email address view (Senders or Recipients), it also skips the corresponding name view since it would be redundant. For example, drilling into a sender and pressing `g` cycles through Recipients, Recipient Names, Domains, Labels, and Time.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-subgroup-recipients.svg" alt="Sub-grouped by Recipients after drilling into a sender" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">A sender's email grouped by recipient</figcaption>
  </figure>
  <figure style="margin: 0; text-align: center;" data-lightbox>
    <img src="/assets/generated/tui-subgroup-time.svg" alt="Sub-grouped by Time after drilling into a sender" style="width: 100%; border-radius: 4px;" />
    <figcaption style="font-size: 0.8rem; color: #888; margin-top: 0.3rem;">A sender's mail grouped by month</figcaption>
  </figure>
</div>

## Searching

Press `/` to open a search bar that filters the current view in real time. Matching text is highlighted in the results. At the aggregate level (Senders, Domains, etc.), search uses the daemon's configured analytics engine, so the default local-daemon setup uses DuckDB over Parquet when the cache is usable.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-search-sender.svg" alt="msgvault TUI search filtering senders by name with highlighted matches" loading="lazy">
</figure>
Search also works after drill-down. Drill into a result, then press `/` again to search within that context. This second-level search uses deep FTS5 full-text search over message subjects and bodies. You can progressively narrow results: find a sender, drill in, then search for a specific subject or keyword.

<div style="display: grid; grid-template-columns: 1fr 1fr; gap: 0.5rem;">
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-search-drilldown.svg" alt="Drilled into search result showing messages from a specific sender" loading="lazy">
</figure>
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-search-subject.svg" alt="Searching within a sender's messages by subject keyword with highlighted matches" loading="lazy">
</figure>
</div>

## Filtering

Press `f` to open the filter modal. The modal presents two independent toggles that you can combine:

| Filter | Effect |
|---|---|
| Only with attachments | Show only messages that have attachments |
| Hide deleted from source | Exclude messages that have been deleted from Gmail |
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-filter-modal.svg" alt="msgvault TUI filter modal with checkbox toggles for attachments and hide deleted" loading="lazy">
</figure>
Use `↑`/`↓` to navigate, `Space` or `x` to toggle a filter, and `Enter` or `Esc` to apply and close. Active filters are shown in the title bar (e.g. `[Attachments]`, `[Hide Deleted]`). Filters apply to all views: aggregates, drill-downs, sub-aggregates, search results, and stats.

## Viewing Email Threads

From any message list (after drilling into a sender, label, domain, etc.), press `T` to open the full email thread for the highlighted message. This renders the complete conversation inline in the terminal, including sender, date, and body text for each message in the thread.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-thread.svg" alt="msgvault TUI showing a full email thread conversation" loading="lazy">
</figure>
Press `Esc` to return to the message list.

## Keyboard Shortcuts

| Key | Action |
|---|---|
| `j` / `k` or `↑` / `↓` | Navigate rows |
| `Enter` | Drill down into selection |
| `T` | View full email thread |
| `Esc` / `Backspace` | Go back |
| `m` | Cycle through Email, Texts, and Meetings (skips unavailable Texts) |
| `g` | Cycle view mode |
| `s` | Cycle sort field (Name / Count / Size) |
| `v` | Reverse sort direction |
| `t` | Jump to Time view (cycle granularity when already in Time) |
| `a` | Show all individual messages in current view |
| `A` | Filter by account, or by source in Meetings mode |
| `f` | Open filter modal |
| `Space` | Toggle selection |
| `d` | Stage selected for deletion |
| `D` | Stage all matching current filter |
| `/` | Search |
| `?` | Help |
| `q` | Quit |

## Marking Emails for Deletion

You can stage individual messages or bulk-delete entire aggregate groups (e.g. all emails from a sender, all messages with a given label) at once. Use `Space` to select one or more rows, then press `d` to stage them. From any aggregate view, press `D` to stage every message in the current group without selecting individual rows.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-selection.svg" alt="msgvault TUI with rows selected for deletion staging" loading="lazy">
</figure>
A confirmation dialog shows exactly how many messages will be staged before anything happens. Messages are not deleted immediately; they are placed in a deletion batch that you review and execute separately with `msgvault delete-staged`.
<figure class="screenshot" data-lightbox>
  <img src="/assets/generated/tui-deletion.svg" alt="msgvault TUI deletion confirmation dialog showing bulk staging" loading="lazy">
</figure>
See [Deleting Email](/usage/deletion/) for the full deletion workflow.

## Performance

The default SQLite archive path uses DuckDB querying Parquet metadata exports for aggregate views. This architecture delivers aggregate queries (top senders, domains, labels, time series) **hundreds of times faster** than equivalent SQLite JOINs. The Parquet analytics layer has a small footprint, so drill-down and re-aggregation feel instant even on very large archives. Configure `[analytics].engine` if you need to force live SQL or require DuckDB.

See [Data Storage](/architecture/storage/) for details on how this works.
