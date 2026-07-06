---
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

All three cover read-only workflows only. They explicitly tell agents
not to run archive-mutating commands (sync, import, dedup, deletion)
without your direction.

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
msgvault uninstall                  # remove installed skills
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
