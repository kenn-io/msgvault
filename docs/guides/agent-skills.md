---
last_edited: 2026-11-17
title: Agent skills
description: Teach coding agents the msgvault CLI with installable skills.
---

msgvault ships a pack of agent skills as `SKILL.md` files that follow the open
agent-skills standard used by Claude Code, Codex, and other coding agents. Each
skill teaches an agent one msgvault workflow so it can work with your archive
without relearning the CLI in every session.

## Included skills

| Skill | Teaches |
|---|---|
| `msgvault-search` | Query syntax, search modes (fts/vector/hybrid), reading messages, exporting raw email |
| `msgvault-attachments` | Finding attachments, filtering by type/size, exporting by content hash or message |
| `msgvault-analytics` | Built-in aggregates, raw SQL over the analytics views, time-series queries |
| `msgvault-feedback` | Record heard, accepted or declined feedback, link accepted actions, and keep change evidence and report-back reminders in Kata |

Search, attachments, and analytics cover read-only workflows. Feedback also
guides task writes within your explicit request. Every skill requires your
direction before archive mutations such as sync, import, dedup, or deletion.

## Install the skills

Run the installer to detect installed agents and write the skills to each
agent's user-level skills directory:

```bash
msgvault skills install
```

msgvault detects installed agents at `~/.claude`, `~/.codex`, or the directories
set by `CLAUDE_CONFIG_DIR` and `CODEX_HOME`, and writes the skills to each
agent's user-level skills directory, such as `~/.claude/skills/` or
`~/.codex/skills/`. You can also target one agent, pick a directory, force an
overwrite, or uninstall:

```bash
msgvault skills install --agent claude     # only one agent
msgvault skills install --dir .agents/skills   # explicit directory (e.g. project scope)
msgvault skills install --force            # overwrite hand-edited copies
msgvault skills uninstall                  # remove installed skills
```

Installed files include a generation marker. Re-running `skills install` after
an upgrade refreshes those files; files you have hand-edited with the marker
removed are skipped unless you pass `--force`, and `skills uninstall` never
deletes files without the marker.

## Skills and the MCP server

The skills teach agents the msgvault *CLI*, while the
[MCP server](../usage/chat.md) exposes msgvault as *tool calls*. They work side
by side: MCP fits chat apps such as Claude Desktop, and skills fit terminal
coding agents that already run shell commands and can pipe `--json` output
through `jq`. CI validates skill content against the CLI's command tree, so
examples stay accurate as the CLI evolves.

MCP clients can read the same guides without installing files on disk. Call
`msgvault.skills {}` to list them, and `msgvault.skills {"name":"workflow"}` to
read the [agent quickstart](agent-quickstart.md). The list contains `workflow`
followed by every skill bundled with the running binary, so new skills appear
without extra configuration. Skills installed on disk do not change what
`msgvault.skills` returns. The tool is read-only, reads no archive data, and
works in delegated sessions.

## Follow through on feedback

After upgrading msgvault, run `msgvault skills install` again to get the
feedback skill. You can ask your agent to record feedback from a specific person
or message, review an earlier decision, or follow through on an accepted change.
Summarizing feedback alone does not authorize task writes.

The skill stores the record in Kata using existing
[person agendas](../usage/people.md) and
[archive citations](../usage/kata-issues.md). Configure the daemon's Kata
integration and give your agent separate Kata CLI or MCP access for decision
comments, ownership, and reminders. Before combining writes, the agent verifies
that both interfaces reach the same service and project; without that evidence,
it can prepare a draft and explain what is missing. msgvault has no automatic
feedback extraction, transition enforcement, or new background scheduler.

Hearing feedback does not mean accepting it. Accepting names the change you
agreed to and its action, while declining records your reason. A completed
action needs evidence of the actual change. A separate report-back reminder
stays open after that action closes, assigned to the agreed responsible actor
and scheduled at your agreed time. Missing responsibility or timing remains
visible. Kata delivers reached schedules to that actor's inbox; the skill sends
nothing.

Reporting back requires a sent-message reference, recipient, and time, or your
explicit waiver and reason; a draft does not count. A combined feedback and
action record stays open until both the change and report-back are resolved. In
a later session, the agent reads the saved decisions, linked action, and
reminder again instead of relying on conversation memory.

Reads stay within the person, source, and time window you requested, excluding
Private Notes and sensitive observations. Put private feedback only in a
suitable private destination, quote the minimum needed, or keep a neutral source
pointer when quoting is inappropriate. Missing or inaccessible archive evidence
remains a stated gap, and sending or sharing a response needs its own
authorization.
