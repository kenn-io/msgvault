# Keep a Saved View accurate

Saved Views let the Web UI, CLI, and agents use the same query and filters.
Pin an entry that the query misses, or hide a matching entry that does not
belong. These corrections stay with the view when its filters change.

In the Web UI, open a view from the sidebar. The header shows its current
pinned and hidden counts. **Save changes** saves the current query and display
settings while preserving those entries. **Save view…** creates a separate view.

Use an `entry_key` from a run row to correct membership:

```bash
msgvault views run 'Review queue' --json
msgvault views pin 'Review queue' 'source:1:message:provider-id'
msgvault views exclude 'Review queue' 'source:1:message:other-id'
msgvault views show 'Review queue'
msgvault views unpin 'Review queue' 'source:1:message:provider-id'
msgvault views unexclude 'Review queue' 'source:1:message:other-id'
```

Commands accept a view ID or its exact name. `show` lists pins with `+` and
hidden entries with `-`; a text `run` marks pinned rows with `+`. `run` returns
one page; use `--limit` (1–100, default 100) and the returned `--cursor` to
continue. `--json` preserves the item metadata in the response. Item commands
retry once if another writer changes the revision.

A pin bypasses the query's matching predicates. A hidden entry is removed even
when it matches or was pinned. Corrections affect entry lists, groups, counts,
and files. Source access restrictions still apply. Use conversation keys for
chats so all messages in the conversation participate together.

Each view allows 500 pinned and 500 hidden entries, within a combined 128 KiB
stored representation. An unknown or invalid key rejects the entire edit and
reports the offending key and reason. Adding an entry to one mode removes it
from the other. Source merges and deduplication preserve corrections using the
surviving entities; an exclude wins when entries collapse together. Undoing
deduplication leaves the correction on the survivor.

Deleted targets are omitted and counted as stale. An item edit clears stale
references. Removing a source prunes its references while preserving the other
sources. Deleting a view leaves archive messages intact.

The MCP tools `get_saved_view`, `run_saved_view`, and `update_saved_view` expose
the same records. See the [Saved Views API](../api-server.md#saved-views) for
fields, concurrency, and errors. These commands require daemon API schema
3.45.0 or newer.
