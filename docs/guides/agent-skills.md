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
msgvault skills uninstall                  # remove installed skills
```

Installed files carry a generation marker. Re-running `skills install`
after upgrading msgvault refreshes them; files you have hand-edited
(marker removed) are skipped unless you pass `--force`, and
`skills uninstall` never removes files without the marker.

## Skills and the MCP server

MCP clients can read the bundled guides without installing skill files. Call
`msgvault.skills {}` to list names, titles and summaries, then
`msgvault.skills {"name":"workflow"}` for the MCP quickstart. The other names
are `msgvault-search`, `msgvault-attachments` and `msgvault-analytics`.

Named calls return Markdown as text and the guide's metadata in structured
content. The three CLI skills include MCP command mappings and explain
differences in search syntax, paging, scope and attachment exports. Use the
connected server's tool list for actual availability and parameters.

The guide tool is read-only and present in owner and delegated sessions,
including sessions without archive-read permissions. Existing authentication
and archive grants still apply. It serves only these shipped guides, rejects
unknown or invalid names and extra arguments, and never reads paths or
executes a skill. Skill content remains validated against the CLI command
tree in CI.
