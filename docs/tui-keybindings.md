# msgvault TUI Keybindings Reference

## Navigation

| Key | Action |
|-----|--------|
| `j` / `↓` | Move cursor down |
| `k` / `↑` | Move cursor up |
| `PgDn` / `Ctrl+D` | Page down |
| `PgUp` / `Ctrl+U` | Page up |
| `Home` / `g` | Go to first row |
| `End` / `G` | Go to last row |
| `Enter` | Drill down (aggregate → message list → message detail) |
| `Esc` / `Backspace` | Go back to previous level |

In message detail view, `←`/`h` and `→`/`l` navigate to the previous/next message in the list.

## View Cycling

| Key | Action |
|-----|--------|
| `Tab` / `g` | Cycle forward through aggregate views |
| `Shift+Tab` | Cycle backward through aggregate views |

View order: **Senders → Sender Names → Recipients → Recipient Names → Domains → Labels → Time**

## Time View

| Key | Action |
|-----|--------|
| `t` | Jump directly to Time view; if already in Time view, cycle granularity (Year → Month → Day) |

## Sorting

| Key | Action |
|-----|--------|
| `s` | Cycle sort field (Name → Count → Size → ...) |
| `r` / `v` | Reverse sort direction (asc ↔ desc) |

In message list, `s` cycles: Date → Size → Subject.

## Filters

### Account filter — `A` (uppercase)

Opens the account selector modal. Use `j`/`k` to move, `Enter` to apply, `Esc` to cancel.

> Note: lowercase `a` opens "All Messages" for the current aggregate row — it does **not** open the account modal.

### Attachment / display filter — `f`

Opens the filter modal with toggle checkboxes:
- Attachments only
- Hide deleted from source

`Space` or `x` toggles the highlighted option. `Enter` or `Esc` applies and closes the modal.

## Search

| Key | Action |
|-----|--------|
| `/` | Open inline search bar |
| (type query) | Results filter live as you type (fast metadata search) |
| `Tab` | Toggle fast (metadata) ↔ deep (FTS5 body) search — message list only |
| `Enter` | Commit search and close search bar |
| `Esc` | Cancel search and restore previous results |

In message detail, `/` opens an in-message find bar. `n` / `N` jump to next / previous match.

## Selection

Available in aggregate view and message list.

| Key | Action |
|-----|--------|
| `Space` | Toggle selection on current row |
| `S` (uppercase) | Select all visible rows |
| `x` | Clear all selections |

## Deletion Staging

| Key | Action |
|-----|--------|
| `d` / `D` | Stage selected items for deletion (selects current row first if nothing selected) |

Staging opens a confirmation modal. Press `Y` (or `Enter`) to confirm, `N` (or `Esc`) to cancel.

Staged deletions are **not executed** until you run `msgvault delete-staged` with `MSGVAULT_ENABLE_REMOTE_DELETE=1`.

## Other Actions

| Key | Action |
|-----|--------|
| `a` (lowercase) | View all messages for the current aggregate / filter context |
| `T` | View thread/conversation for the current message |
| `e` | Export attachments — opens selection modal (message detail view only) |
| `m` | Toggle Email / Texts mode (only when Texts engine is configured) |

## Quit

Press `q` to open the quit confirmation modal, then `Y` or `Enter` to confirm.
Press `N`, `Esc`, or `q` again to cancel and return to the TUI.

> Quit is a two-step action — `q` alone does nothing irreversible.

## Help

Press `?` to open the in-app keyboard shortcut reference. `j`/`k` or `PgDn`/`PgUp` scroll it; any other key closes it.
