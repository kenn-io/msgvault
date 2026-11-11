---
last_edited: 2026-11-06
title: Agent Skills
description: Teach coding agents the msgvault CLI with installable skills
---

msgvault ships a pack of agent skills — `SKILL.md` files following the
open agent-skills standard used by Claude Code, Codex, and other
coding agents. Each skill teaches an agent one msgvault workflow so it
can use your archive effectively without relearning the CLI every
session.

## The skills

| Skill | Teaches |
|---|---|
| `msgvault-search` | Query syntax, search modes (fts/vector/hybrid), reading messages, exporting raw email |
| `msgvault-attachments` | Finding attachments, filtering by type/size, exporting by content hash or message |
| `msgvault-analytics` | Built-in aggregates, raw SQL over the analytics views, time-series queries |
| `msgvault-feedback` | Record heard, accepted or declined feedback, link accepted actions, and keep change evidence and report-back reminders in Kata |

Search, attachments and analytics cover read-only workflows. Feedback also
guides task writes within your explicit request. All skills require your
direction before archive mutations such as sync, import, dedup or deletion.

## Install

```bash
msgvault skills install
```

This detects installed agents by directory (`~/.claude`, `~/.codex`)
and writes the skills to each agent's user-level skills directory
(`~/.claude/skills/`, `~/.codex/skills/`). Options:

```bash
msgvault skills install --agent claude     # only one agent
msgvault skills install --dir .agents/skills   # explicit directory (e.g. project scope)
msgvault skills install --force            # overwrite hand-edited copies
msgvault skills uninstall                  # remove installed skills
```

Installed files carry a generation marker. Re-running `skills install`
after upgrading msgvault refreshes them; files you have hand-edited
(marker removed) are skipped unless you pass `--force`, and
`skills uninstall` never removes files without the marker.

## Skills and the MCP server

The skills teach agents the msgvault *CLI*; the [MCP
server](../usage/chat.md) exposes msgvault as *tool calls*. They
coexist: MCP suits chat apps (Claude Desktop), while skills suit
terminal coding agents that already run shell commands and can pipe
`--json` output through `jq`. Skill content is validated against the
CLI's command tree in CI, so examples stay correct as the CLI evolves.

## Follow through on feedback

The feedback skill is newer than the latest release. After upgrading msgvault,
run `msgvault skills install` again. Ask your agent to record feedback from a
specific person or message, review your earlier decision, or follow through on
an accepted change. Summarizing feedback alone does not authorize task writes.

The skill keeps the record in Kata using existing
[person agendas](../usage/people.md) and
[archive citations](../usage/kata-issues.md). Configure the daemon's Kata
integration and give the agent separate Kata CLI or MCP access for decision
comments, ownership and reminders. Before combining writes, the agent verifies
that both interfaces reach the same service and project. Without that evidence
it can prepare a draft and explain what is missing. There is no automatic
feedback extraction, transition enforcement or new background scheduler.

Hearing feedback does not mean accepting it. Acceptance names the change you
agreed to and its action; declining records your reason. A completed action
needs evidence of the actual change. A separate report-back reminder stays
open after that action closes, assigned to the agreed responsible actor and
scheduled at your agreed time. Missing responsibility or timing stays visible.
Kata delivers reached schedules to that actor's inbox; the skill sends nothing.

Reporting back needs a sent-message reference, recipient and time, or your
explicit waiver and reason. A draft does not count. A combined feedback/action
record stays open until both the change and report-back are resolved. In a
later session, the agent reads the saved decisions, linked action and reminder
again instead of relying on conversation memory.

Reads stay within the person, source and time window you requested. Private
Notes and sensitive observations are excluded. Put private feedback only in a
suitable private destination, quote the minimum needed, or keep a neutral
source pointer when quoting is inappropriate. Missing or inaccessible archive
evidence remains a stated gap. Sending or sharing a response needs its own
authorization.
