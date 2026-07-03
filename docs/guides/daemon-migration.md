---
title: Daemon Migration Guide
description: What changes when CLI commands route through the msgvault daemon, and how to adapt scripts, environment variables, and workflows.
---

Starting with the 0.17.0 release, msgvault CLI commands that touch the archive no
longer open the SQLite database directly. Instead, every archive-access
command talks to a msgvault daemon over HTTP — either a remote server you
configured under `[remote]`, or a local background daemon that the CLI starts
automatically. This page explains what that means day to day, what you will
see on your terminal, and the gotchas to know about when upgrading.

## What changed, in one table

| | Before | After |
|---|---|---|
| `msgvault stats`, `search`, `tui`, ... | CLI opens `msgvault.db` directly | CLI queries the daemon over HTTP; a local daemon is auto-started if needed |
| `msgvault sync-full`, `import-*`, ... | CLI process performs the sync/import | The daemon runs the operation and streams output back to your terminal |
| Two commands writing at once | Both open SQLite; risk of `database is locked` and WAL contention | The daemon is the single writer; concurrent operations queue with a visible `Waiting:` message |
| Local vs remote | Different code paths and capabilities | Identical behavior; `[remote].url` just changes which daemon answers |
| Long-running daemon + CLI | CLI could bypass a running `msgvault serve` | The running daemon owns the archive; the CLI always goes through it |

Your archive format is unchanged: still plain SQLite plus Parquet under
`~/.msgvault/`. Like most releases, 0.17.0 does add schema migrations
relative to 0.16.x (new `messages` columns); they run automatically the
first time the daemon starts on the upgraded binary, which is part of why
the first startup on a large archive can take longer than usual. If you
want a safety net, back up `msgvault.db` before upgrading.

## What you will see

The first archive-access command after upgrading starts a background daemon
and prints one line to stderr:

```
Starting local msgvault daemon (pid 51234). Logs: ~/.msgvault/serve.log
```

If startup takes longer than a couple of seconds (large archives may run
migrations), the CLI reports progress every few seconds until the daemon is
ready, then runs your command as usual. The daemon stays alive for 20 minutes
after the last request (`[server].daemon_idle_timeout`, `"0s"` to disable) and
then exits on its own; the next command starts it again.

Command output is unchanged: syncs, imports, and other long operations stream
their normal stdout/stderr through the daemon back to your terminal, and
Ctrl+C still cancels them.

## Managing the daemon

New lifecycle subcommands:

```bash
msgvault serve status    # URL, pid, version, API schema, uptime
msgvault serve start     # start the background daemon explicitly
msgvault serve stop      # stop it
msgvault serve restart   # stop + start
msgvault serve           # foreground server (Docker/NAS; never idle-stops)
```

You rarely need these — auto-start handles the common case — but
`serve restart` is the fix whenever the daemon needs to pick up changes
(a new binary, an edited `config.toml`, or new environment variables).

When you upgrade the msgvault binary, the CLI notices the running daemon is
older and restarts it automatically before issuing your command. This is the
`[server].daemon_auto_restart = "newer"` default; set `"never"` if a
supervisor (systemd, Docker) owns the daemon lifecycle, or `"always"` to
restart on any version difference.

## One writer, visible waits

The daemon serializes mutating operations: while a sync, import, embeddings
build, or deletion is running, another mutating command waits its turn. Short
waits (under ~10 seconds) are absorbed silently. Longer waits retry
automatically and tell you what they are waiting for:

```
Waiting: msgvault embeddings build has been running for 42m (Ctrl+C to cancel).
```

Read-only commands — searches, stats, the TUI, `logs`, `list-deletions`,
`embeddings list`, SQL `query` — are exempt and run immediately even while a
long operation holds the archive.

Scheduled work yields to you: if the daemon is mid-way through a scheduled
sync or embedding pass when you run a command, the scheduled job checkpoints,
steps aside within a few seconds, and resumes at its next scheduled run. Your
interactive command does not queue behind background jobs.

Interrupted syncs stay resumable. Cancelling a sync (Ctrl+C, daemon shutdown,
or a scheduler yield) keeps its checkpoint, and the next
`msgvault sync-full` for that account picks up where it left off — relevant
for initial multi-hour Gmail syncs.

## Gotcha: environment variables

This is the most common surprise. Commands now execute inside the daemon
process, which does **not** inherit your shell's environment. Most
environment variables are deliberately not forwarded; a short allowlist is
passed through from your shell automatically:

| Variable | Used by |
|---|---|
| `MSGVAULT_IMAP_PASSWORD` | `add-imap` |
| `MSGVAULT_ENABLE_REMOTE_DELETE` | `delete-staged` |
| The variable named by `[vector.embeddings].api_key_env` | `embeddings build` / `embed` |

If an operation needs any other environment variable (for example a proxy
setting), export it before starting the daemon — in the shell that runs
`msgvault serve start`, in your systemd unit, or in your Docker environment —
and `msgvault serve restart` after changing it.

## Gotcha: OAuth and browser flows

`add-account`, `add-o365`, `add-teams`, and `add-synctech-sms-drive` run
their browser authorization in your terminal's process *before* handing off
to the daemon, so the consent screen opens in your local browser exactly as
before.

With a `[remote]` server configured, tokens live on the remote host, so
authorization happens there — same as pre-daemon remote behavior. For
headless remote setups, keep using `add-account --headless` or
`msgvault export-token` to push a locally minted token to the server (see
[Remote Deployment](/guides/remote-deployment/)).

## Gotcha: `--local` means "local daemon"

If you have `[remote].url` configured, `--local` now selects the **local
daemon** rather than opening SQLite in the CLI process. There is no supported
way for a foreground CLI command to open the archive directly while a daemon
is running; the daemon owns the archive.

## New files in the data directory

You will see a few new files under `~/.msgvault/`:

- `serve.log` — background daemon log (shown by `msgvault logs`)
- `daemon.lock`, `db.write.lock`, `serve.background.lock` — ownership and
  launch locks
- a daemon runtime record with the daemon's port, version, and API schema,
  which is how CLI processes discover the auto-selected port

By default the daemon binds `127.0.0.1` on an auto-selected port. Set
`[server].api_port` for a stable port (required for remote/NAS deployments),
and note that binding a non-loopback address still requires an `api_key`.

## Deprecated flags (as of 0.17.0)

Engine and cache selection are now daemon-level configuration, so these
per-invocation flags are deprecated in 0.17.0 and hidden, with removal
planned for a later release:

| Deprecated flag | Replacement |
|---|---|
| `tui --force-sql`, `mcp --force-sql` | `[analytics].engine = "sql"` |
| `tui --no-cache-build` | `[analytics].auto_build_cache = false` |
| `tui --no-sqlite-scanner`, `mcp --no-sqlite-scanner` | daemon-managed; use `[analytics].engine = "sql"` if needed |

The old flags still work in 0.17.0 and print a deprecation notice.

## FAQ

**Do I have to run a server now?**
No. The CLI manages a local background daemon transparently: it starts on
demand, idles out after 20 minutes, and restarts itself on binary upgrades.
Running `msgvault serve` yourself is only for always-on deployments.

**Is my archive now exposed over the network?**
No. The auto-started daemon binds to `127.0.0.1` only. Network exposure
requires explicitly setting `bind_addr` and an `api_key` in `[server]`.

**Why does my command say "Waiting: ..."?**
Another mutating operation (often a scheduled sync or an embeddings build)
holds the archive. Your command retries automatically and runs as soon as the
operation finishes or yields; Ctrl+C cancels the wait. Read-only commands
never wait.

**A command fails with `env "..." is not allowed through the daemon CLI runner`.**
You passed an environment variable that is not on the forwarding allowlist.
Set it in the daemon's environment instead (then `msgvault serve restart`),
or — for embedding API keys — name it in `[vector.embeddings].api_key_env` so
the CLI forwards it for you.

**The CLI says the daemon was started with a different api_key.**
You edited `[server].api_key` after the daemon started. Run
`msgvault serve restart`.

**Can I still open the database with `sqlite3` or other tools?**
For reads, yes — the archive remains standard SQLite. Avoid external writes
while the daemon is running; it is the write owner.

**How do I see what the daemon is doing?**
`msgvault logs` (with `--follow`, `--level`, `--grep`) shows the selected
daemon's logs — including a remote daemon's — and `msgvault serve status`
shows version, uptime, and vector-search state.

**Something is off after upgrading. What is the first thing to try?**
`msgvault serve restart`. It re-reads config, picks up the current binary and
environment, and re-registers the runtime record. See
[Troubleshooting](/troubleshooting/) for more.
