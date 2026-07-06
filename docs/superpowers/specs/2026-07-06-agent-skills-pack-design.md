# Agent Skills Pack for msgvault

**Date:** 2026-07-06
**Status:** Approved design

## Goal

Ship a pack of agent skills (per the open SKILL.md standard used by Claude
Code, Codex, and others) that teaches coding agents the msgvault read-only
CLI workflows — search/retrieval, attachment export, and analytics — so
agents don't relearn the CLI from scratch each session. Skills are generated
from templates embedded in the binary and installed with
`msgvault skills install`, avoiding per-agent duplication of skill files.

## Scope

**In scope:** three read-only workflow skills, an `internal/skills` package
with embedded templates, `msgvault skills install|uninstall` commands, a
drift-guard test tying skill content to the real cobra command tree, and a
docs guide.

**Out of scope:** skills for archive management (sync, accounts, dedup,
deletion staging/execution), Claude plugin packaging, project-scope install
defaults, MCP server changes.

## Package layout

```
internal/skills/
├── skills.go            # Render(), Install(), Uninstall(), agent detection
├── skills_test.go
└── templates/
    ├── _basics.md.tmpl          # shared partial: conventions + safety
    ├── msgvault-search.md.tmpl
    ├── msgvault-attachments.md.tmpl
    └── msgvault-analytics.md.tmpl
cmd/msgvault/cmd/skills.go       # cobra: msgvault skills install|uninstall
cmd/msgvault/cmd/skills_test.go  # installer CLI tests + cobra drift guard
```

Templates are embedded via `go:embed` and rendered with `text/template` at
install time. Template context is `{Version}` only; rendered content is
agent-independent (one SKILL.md works in both Claude and Codex per the open
standard). The `_basics.md.tmpl` partial is included by all three skills so
shared conventions have a single source.

## Skill content

Each skill directory contains a single `SKILL.md` with YAML frontmatter
(`name` matching the directory, `description` written for auto-trigger
precision, ≤1024 chars) followed by the shared basics section and the
workflow-specific body.

### Shared basics (partial)

- What msgvault is (offline archive of email/messages across accounts).
- All commands work transparently against a local or remote daemon; no
  connection setup needed. `--local` forces local when a remote is
  configured.
- Always pass `--json` on read commands for structured output; `query` uses
  `--format json|csv|table` instead.
- Message ID conventions: JSON exposes both the internal numeric `id` and
  `source_message_id` (Gmail ID); `show-message`, `export-eml`, and
  `export-attachments` accept either.
- Safety boundary: the commands in these skills perform **no
  archive/source-destructive operations** (they may refresh derived
  indexes/caches such as the FTS index or Parquet cache). Commands like
  `delete-staged`, `delete-deduped`, `deduplicate`, `sync-*`,
  `embeddings build`, and `add-*` mutate the archive or external services
  and require explicit user direction — they are not part of these
  workflows.

### `msgvault-search` — find and read messages

- Query operator table: `from:`, `to:`, `cc:`, `bcc:`, `subject:`,
  `label:`/`l:`, `has:attachment`, `before:`/`after:` (YYYY-MM-DD),
  `older_than:`/`newer_than:` (7d/2w/1m/1y), `larger:`/`smaller:`
  (message size, e.g. 5M), `message_type:` (email, sms, whatsapp, teams,
  …). Bare words and quoted phrases are FTS prefix terms (implicit AND;
  no OR/NEAR passthrough). Bare domains work in address operators
  (`from:example.com`).
- Modes: `--mode=fts` (default; supports `--offset`, filter-only queries),
  `--mode=vector|hybrid` (require built embeddings and free-text terms;
  no `--offset`; `--explain` adds per-signal scores).
- Scoping: `--account` vs `--collection` (mutually exclusive),
  `--message-type`, `--limit/-n`.
- Search-result JSON field reference (`id`, `subject`, `snippet`,
  `from_email`, `sent_at`, `has_attachments`, `attachment_count`,
  `labels`, …).
- Reading messages: `show-message <id> --json` for full body + metadata;
  `export-eml <id> [-o path|-]` for raw MIME.
- Pagination recipe with `--limit`/`--offset` (fts only).

### `msgvault-attachments` — locate and export attachments

- Core recipe: `search 'has:attachment …' --json` → `show-message <id>
  --json` to read `attachments[]` (`filename`, `mime_type`, `size`,
  `content_hash`, optional `url`) → `export-attachment <content_hash>`.
- `export-attachment` writes **binary to stdout by default** — always pass
  `-o <filename>` unless binary stdout is intended (piping). `--base64`
  and `--json` variants for inline consumption.
- `export-attachments <msg-id> -o <dir>` exports all attachments with
  original (sanitized) filenames, never overwriting (`_1` suffixes).
- No CLI-level attachment filters exist (no MIME-type/per-file-size
  operators; `larger:`/`smaller:` are whole-message size): filter the
  `attachments[]` JSON with `jq` before exporting. Include the
  hash+filename loop recipe.
- Caveats: URL-backed attachments are links, not exportable bytes;
  storage is content-addressed by SHA-256, so identical attachments in
  different messages share one hash and one blob.

### `msgvault-analytics` — aggregates and SQL

- First reach: `stats [--account|--collection]`,
  `list-senders|list-domains|list-labels --json [--limit] [--after]
  [--before]`, `list-accounts --json`.
- Custom questions: `query "<sql>" --format json` over the Parquet
  analytics views. Document the schemas of `v_messages`, `v_senders`,
  `v_domains`, `v_labels`, `v_threads` plus the base views (`messages`,
  `participants`, `message_recipients`, `labels`, `message_labels`,
  `attachments`, `conversations`, `sources`).
- Notes: analytics cache counts include source-deleted messages (may
  exceed `stats` active counts); message bodies are not in the analytics
  cache; `stats` is text-only (no `--json`).

## Installer behavior

`msgvault skills install`:

- **Agent detection:** `~/.claude` exists → install to
  `~/.claude/skills/<name>/SKILL.md`; `~/.codex` exists →
  `~/.codex/skills/<name>/SKILL.md`. Installs to all detected agents;
  errors with guidance if none found.
- **Flags:** `--agent claude|codex` (repeatable) restricts detection.
  `--dir <path>` installs **only** into that skills root and conflicts
  with `--agent` (mutually exclusive). `--force` overwrites hand-edited
  files.
- **Marker-based overwrite:** rendered files end with
  `<!-- generated by msgvault vX.Y.Z — re-run 'msgvault skills install' to update -->`.
  Overwrite detection matches the stable phrase `generated by msgvault`
  (not the version, which defaults to `dev` and is ldflag-filled).
  Files without the marker are skipped with a warning unless `--force`.
- `msgvault skills uninstall` removes `msgvault-*` skill directories whose
  SKILL.md carries the marker; same `--agent`/`--dir` flags.
- **Root pre-run skip list:** `skills` is added to the
  `PersistentPreRunE` skip list in `cmd/msgvault/cmd/root.go` alongside
  `version`/`update`/`quickstart`/`openapi`/`completion`, so installing
  skills never loads archive config or creates the msgvault home.

## Testing

- **Render tests** (`internal/skills`): every template renders without
  error; frontmatter parses as YAML; `name` matches the skill directory
  name; `description` is non-empty and ≤1024 chars; marker present.
- **Cobra drift guard** (`cmd/msgvault/cmd/skills_test.go`, so it can use
  the real `rootCmd` without exposing test-only API): extract every
  `msgvault <subcommand> [flags]` invocation from rendered skill bodies
  (fenced code blocks and inline code), resolve each against
  `rootCmd.Find`, and assert the command exists and every referenced
  `--flag` is defined on it. Skills fail CI when the CLI surface changes
  under them.
- **Installer tests** with temp `$HOME`: agent detection, install to both
  agents, `--agent` restriction, `--dir` exclusivity with `--agent`,
  marker-respecting overwrite, skip-with-warning on edited files,
  `--force`, uninstall (only marker-bearing dirs removed).

## Documentation

- New `docs/guides/agent-skills.md`: what the pack is, install/uninstall,
  per-agent notes (Claude Code, Codex), how skills relate to the MCP
  server (skills teach the CLI; MCP is the tool-call interface — both
  can coexist).
- Changelog entry; CLAUDE.md quick-commands addition.
- Skill content is sourced from the current cobra CLI (the drift guard
  enforces this); prose docs like `docs/usage/exporting.md` are not a
  source of truth (known stale for `export-eml`).
