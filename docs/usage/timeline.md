---
title: Timeline
description: Read a bounded view of archived activity by day, person, or anchor.
---

The timeline is unreleased. It combines archived messages, meetings, calls,
open loops, and notes.
It reads the archive through the daemon and shows where a source is missing or
out of date. Start here to find activity, then follow an entry's `open` targets
to read its details. The `get_daily_note` tool opens an authored note in full.

```bash
msgvault timeline
msgvault timeline --date 2026-10-22
msgvault timeline --from 2026-10-01 --to 2026-10-22
msgvault timeline --with person:7
msgvault timeline --around msg:42 --before 5 --after 5
msgvault timeline --around conv:12 --conversation
msgvault timeline --brief
msgvault timeline --json --budget 4096
```

In `msgvault tui`, press `m` to reach Timeline mode. Use `[` and `]` to move
between days, `a` for activity around the selected entry, `w` for its first
person's relationship, `n` for the next page, and `Enter` for details.
See [Interactive TUI](tui.md#timeline).

Without a date filter, the day view covers today and yesterday. `--days N`
selects up to 31 days ending today. `--date` selects one local date; `--from`
and `--to` select an inclusive range. Set `[activity].timezone` in the daemon
configuration for local day boundaries; the default is UTC.

`--with` selects relationship history. It accepts `person:<id>`,
`participant:<id>`, `email:<address>`, a phone number, or a name. Ambiguous names
return candidates without a timeline. Resolving a reference never creates a
contact. Add `--with` to an anchor view to filter its neighbors by that person.

`--around` accepts message, conversation, event, burst, date, and datetime
references. Short message (`#42`), conversation (`c12`), and event (`e77`)
references from Markdown also work. `--before` and `--after` default to five
neighbors each, with a maximum of 50. `--conversation` shows individual messages
in the anchor's conversation.

`--tier 1,2` filters people by applied or observed tier. Noise and older cold
activity are omitted by default; use `--include-noise` or `--include-cold` to
include them. `--brief` uses a smaller budget and shorter upcoming window.
It accepts `--date` and rejects `--days`, `--from`, and `--to`.

Markdown is the default. JSON returns the same timeline envelope with entries,
gaps, freshness, omission counts, and `open` targets. The byte budget
applies to the final UTF-8 encoding, including JSON escaping: 16384 bytes by
default, 6144 with `--brief`, and 2048 through 65536 with `--budget`.
JSON output stays compact. The CLI appends a newline after the budgeted payload. Pass an
emitted `cursor` back with the same filters and format to continue. A different
archive, request, or authorization scope can invalidate continuation; retry the
original request when `cursor_expired` is returned.

Budget stubs preserve counts, times, and status. `truncated` marks omitted text
and lists, and `omitted.budget.entry_fields` counts these entries.

Text copied from the archive is untrusted content. JSON labels it
`untrusted_text`; Markdown quotes snippets. `hidden_text` warns that hidden text
was detected and suppresses the snippet. In Markdown, `↓` and `↑` count inbound
and outbound messages, `✓` marks attendance, `T1`/`T2` show tiers, `×N` gives a
message count, and a copies label counts email copies. An empty window explicitly
says `nothing archived`. Freshness warnings include recovery commands; the gap
footer distinguishes missing sections from retryable failures.

The HTTP endpoint is `GET /api/v1/timeline`, with the flags as query parameters
and `mode=day|relationship|anchor`. JSON is the API default;
`format=markdown` returns `text/markdown`. The MCP `timeline` tool defaults to
Markdown and accepts `format: "json"`. It is available when the daemon serves
API schema 3.46.0 or newer. The CLI uses the configured remote, delegated
endpoint, or local daemon and reports an upgrade message for older daemons.

A delegated token needs `message.read` on at least one admitted source.
`calendar.event.read` alone does not admit the timeline. Relationship filters
(`with`) are owner-only. Person-scoped delegated grants are deferred and are
not available in the current authorization model. Delegated results include
only authorized source data. Calls, loops, meeting suggestions, briefs, agendas,
cadence, and daily notes are owner-only and appear as gaps for delegated reads.
An `open` target is emitted only when the caller has permission to use its tool.
Numeric message and conversation IDs above 9007199254740991 require CLI or
HTTP reads; MCP numeric opener arguments are limited to that safe integer
range. String `around` references retain exact IDs, including larger IDs.

The timeline adds no stored projection or archive migration. Owner reads can
reuse the open-loop reader's evidence cache. See [CLI reference](../cli-reference.md)
for flags and [API server](../api-server.md) for authentication and transport.
