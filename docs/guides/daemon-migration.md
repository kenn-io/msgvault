---
last_edited: 2026-08-30
title: Daemon Migration Guide
description: What changes when CLI commands route through the msgvault daemon, and how to adapt scripts, environment variables, and workflows.
---

Starting in release 0.17.0, msgvault CLI commands that touch the archive no longer open the SQLite database directly. Every archive-access command talks to a msgvault daemon over HTTP, either a remote server configured under `[remote]` or a local background daemon that the CLI starts automatically.

## What changed, in one table

| | Before | After |
|---|---|---|
| `msgvault stats`, `search`, `tui`, ... | CLI opens `msgvault.db` directly | CLI queries the daemon over HTTP; a local daemon starts automatically if needed |
| `msgvault sync-full`, `import-*`, ... | CLI process performs the sync/import | The daemon runs the operation and streams output back to your terminal |
| Two commands writing at once | Both open SQLite; risk of `database is locked` and WAL contention | The daemon is the single writer; concurrent operations queue with a visible `Waiting:` message |
| Local vs remote | Different code paths and capabilities | Identical behavior; `[remote].url` selects which daemon answers |
| Long-running daemon + CLI | CLI could bypass a running `msgvault serve` | The running daemon owns the archive; the CLI always goes through it |

Your archive format stays the same: plain SQLite plus Parquet under `~/.msgvault/`. Like most releases, 0.17.0 adds schema migrations relative to 0.16.x (new `messages` columns). Those migrations run automatically the first time the daemon starts on the upgraded binary, which is part of why first startup on a large archive can take longer than usual. If you want a safety net, back up `msgvault.db` before upgrading.

## What you see in the terminal

The first archive-access command after upgrading starts a background daemon and prints one line to stderr:

```
Starting local msgvault daemon (pid 51234). Logs: ~/.msgvault/serve.log
```

If startup takes longer than about 2 seconds (for example, when a large archive runs migrations), the CLI prints a first progress report after about 2 seconds and reports progress about every 10 seconds until the daemon is ready, then runs your command as usual. The daemon stays alive for 20 minutes after the last request (`[server].daemon_idle_timeout`, or `"0s"` to disable) and then exits on its own. The next command starts it again.

If the SQLite archive lacks the From-address index, startup builds it once. Large archives may take several minutes. The daemon logs `building index` and `built index` for `idx_message_recipients_email_from` in `serve.log`.

Command output stays the same. Syncs, imports, and other long operations stream their normal stdout and stderr through the daemon back to your terminal, and Ctrl+C still cancels them.

## Manage the daemon

Use the `daemon` subcommands to inspect or control the background process:

```bash
msgvault daemon status   # URL, pid, version, API schema, uptime
msgvault daemon start    # start the background daemon explicitly
msgvault daemon stop     # stop it
msgvault daemon restart  # stop + start
msgvault serve           # foreground server (Docker/NAS; never idle-stops)
```

Auto-start handles day-to-day use, and you rarely need these commands. Run `daemon restart` whenever the daemon needs to pick up a new binary, an edited `config.toml`, or updated environment variables.

When you upgrade the msgvault binary, the CLI notices that the running daemon is older and restarts it automatically before running your command. That behavior comes from the default `[server].daemon_auto_restart = "newer"`. Set `"never"` if a supervisor such as systemd or Docker owns the daemon lifecycle, or `"always"` to restart on any version difference. If a supervisor runs `msgvault serve`, also set `[server].daemon_auto_start = false` so a CLI command never starts a competing daemon before the supervisor does.

## One writer, visible waits

The daemon serializes mutating operations. While a sync, import, embeddings build, or deletion is running, another mutating command waits its turn. Short waits (under ~10 seconds) are absorbed silently. Longer waits retry automatically and tell you what they are waiting for:

```
Waiting: msgvault embeddings build has been running for 42m (Ctrl+C to cancel).
```

Read-only commands, including searches, stats, the TUI, `logs`, `list-deletions`, `embeddings list`, and SQL `query`, are exempt and run immediately even while a long operation holds the archive.

Scheduled work yields to you. If the daemon is partway through a scheduled sync or embedding pass when you run a command, the scheduled job checkpoints, steps aside within a few seconds, and resumes at its next scheduled run. Your interactive command does not queue behind background jobs.

Interrupted syncs remain resumable. Cancelling a sync through Ctrl+C, daemon shutdown, or a scheduler yield keeps its checkpoint, and the next `msgvault sync-full` for that account picks up where it left off. This is relevant for initial multi-hour Gmail syncs.

## Gotcha: environment variables

Commands now run inside the daemon process, which does **not** inherit your shell's environment. Most environment variables are deliberately not forwarded; only a short allowlist passes through from your shell automatically.

Starting in v0.20.0, remote deletion remains permanently opt-in. The invoking CLI can enable it durably with `[deletion] remote_enabled = true` or for one command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`. Both mechanisms are permanent, with no planned automatic removal of the guardrail. The CLI forwards its effective consent with the operation. A remote daemon's own `[deletion]` section is not server policy for a command invoked elsewhere. Staging, listing, inspecting, and dry-running deletion batches remain ungated.

| Variable | Used by |
|---|---|
| `MSGVAULT_IMAP_PASSWORD` | `add-imap` |
| `MSGVAULT_BEEPER_TOKEN` | `add-beeper` |
| `MSGVAULT_MATRIX_LOGIN_SECRET`, `MSGVAULT_MATRIX_RECOVERY_SECRET` | `add-matrix` |
| `MSGVAULT_DISCORD_TOKEN` | `add-discord` |
| `MSGVAULT_SLACK_TOKEN` | `add-slack` |
| `MSGVAULT_ENABLE_REMOTE_DELETE` | One-command `delete-staged` consent; durable consent uses invoking-CLI config |
| The variable named by `[vector.embeddings].api_key_env` | `embeddings build` / `embed` |

If an operation needs any other environment variable, such as a proxy setting, export it before starting the daemon (in the shell that runs `msgvault daemon start`, in your systemd unit, or in your Docker environment), and run `msgvault daemon restart` if you change it later.

## Gotcha: OAuth and browser flows

`add-account`, `add-o365`, `add-teams`, and `add-synctech-sms-drive` run their browser authorization in your terminal's process *before* handing off to the daemon, so the consent screen opens in your local browser as before.

When a `[remote]` server is configured, tokens live on the remote host, so authorization happens there, matching pre-daemon remote behavior. For headless remote setups, keep using `add-account --headless` or `msgvault export-token` to push a locally minted token to the server (see [Remote Deployment](/docs/guides/remote-deployment/)).

## Gotcha: `--local` means "local daemon"

If you have `[remote].url` configured, `--local` now selects the **local daemon** rather than opening SQLite in the CLI process. There is no supported way for a foreground CLI command to open the archive directly while a daemon is running; the daemon owns the archive.

## New files in the data directory

You see a few new files under `~/.msgvault/`:

- `serve.log`: background daemon log, shown by `msgvault logs`.
- `daemon.lock`, `db.write.lock`, `serve.background.lock`: ownership and launch locks.
- A daemon runtime record holding the daemon's port, version, and API schema, which CLI processes use to discover the auto-selected port. The daemon checks this record every 30 seconds and republishes it if it is missing, so a record removed out from under a running daemon heals itself without a restart.

By default, the daemon binds `127.0.0.1` on an auto-selected port. Set `[server].api_port` for a stable port (required for remote and NAS deployments), and keep in mind that binding a non-loopback address still requires an `api_key`.

## Deprecated flags (as of 0.17.0)

Engine and cache selection are now daemon-level configuration, so these per-invocation flags are deprecated in 0.17.0 and hidden, with removal planned for a later release:

| Deprecated flag | Replacement |
|---|---|
| `tui --force-sql`, `mcp --force-sql` | `[analytics].engine = "sql"` |
| `tui --no-cache-build` | `[analytics].auto_build_cache = false` |
| `tui --no-sqlite-scanner`, `mcp --no-sqlite-scanner` | daemon-managed; use `[analytics].engine = "sql"` if needed |

The old flags still work in 0.17.0 and print a deprecation notice.

## FAQ

**Do I have to run a server now?**
No. The CLI manages a local background daemon automatically: it starts on demand, idles out after 20 minutes, and restarts itself on binary upgrades. Run `msgvault serve` yourself only for always-on deployments.

**Is my archive now exposed over the network?**
No. The auto-started daemon binds to `127.0.0.1` only. Network exposure requires explicitly setting `bind_addr` in `[server]`. A non-loopback daemon still requires an API key, and creates one in `<data_dir>/tokens/server-api-key` if none is configured.

**Why does my command say "Waiting: ..."?**
Another mutating operation (often a scheduled sync or an embeddings build) holds the archive. Your command retries automatically and runs as soon as that operation finishes or yields; press Ctrl+C to cancel the wait. Read-only commands never wait.

**A command fails with `env "..." is not allowed through the daemon CLI runner`.**
You passed an environment variable that is not on the forwarding allowlist. Set it in the daemon's environment instead and run `msgvault daemon restart`, or, for embedding API keys, name it in `[vector.embeddings].api_key_env` so the CLI forwards it for you.

**The CLI says the daemon was started with a different api_key.**
You edited `[server].api_key` after the daemon started. Run `msgvault daemon restart`.

**Can I still open the database with `sqlite3` or other tools?**
For reads, yes. The archive remains standard SQLite. Avoid external writes while the daemon is running. The daemon is the write owner.

**How do I see what the daemon is doing?**
Run `msgvault logs` (with `--follow`, `--level`, and `--grep`) to read the selected daemon's logs, including a remote daemon's logs, and `msgvault daemon status` to check version, uptime, and vector-search state.

**Something is off after upgrading. What is the first thing to try?**
Run `msgvault daemon restart`. It re-reads config, picks up the current binary and environment, and re-registers the runtime record. See [Troubleshooting](/docs/troubleshooting/) for more.
