---
last_edited: "2026-11-23"
title: CLI Reference
description: Complete command reference for all msgvault commands.
---

To find a command for a task, run `msgvault cli search "<what you want to do>"`,
then run `msgvault COMMAND --help` to see its flags and examples. This reference
follows current `main`. For shipped features and upgrade notes, see
[the latest release notes](https://github.com/kenn-io/msgvault/releases/latest).

Running `msgvault --help` groups commands by task and lists start-here commands
for common outcomes. Subcommand help lists global flag names, while root help
explains their defaults. Flags a parent command defines, such as
`calendar --account`, appear in full under Inherited Flags.

| Task | Commands and guides |
|---|---|
| Add and sync a source | [Choose a source](guides/sources.md), [sync](#sync), [sync-full](#sync-full), [source feeds add](#source-feeds-add) |
| Import local exports | [import-ics](#import-ics), [import-eml](#import-eml), [import-mbox](#import-mbox), [import-maildir](#import-maildir), [import-emlx](#import-emlx), [import-pst](#import-pst), [import-slackdump](#import-slackdump), [import-telegram](#import-telegram), [import-discord-package](#import-discord-package), [import-linkedin](#import-linkedin), [import-tiktok](#import-tiktok), [import-x](#import-x), [import-imazing-csv](#import-imazing-csv), [text imports](usage/text-messages.md) |
| Search and browse | [search](#search), [find-chat](#find-chat), [tui](#tui), [show-message](#show-message), [media search](#media-search), [embeddings](#embeddings), [multimodal](#multimodal), [eval](#eval) |
| Maintain people and contacts | [loops](#loops), [person](#person), [people guide](usage/people.md), [CardDAV](usage/people-carddav.md) |
| Edit live email tags | [message-tags](#message-tags) |
| Find a partial address | [address](#address) |
| Inspect staged room policy | [policy](#policy) |
| Organize accounts | [identity](#identity), [collection](#collection), [update-account](#update-account), [merge-account](#merge-account) |
| Read meeting evidence | [meetings](#meetings), [meeting workflow](usage/meetings.md) |
| Export | [export-messages](#export-messages), [export-eml](#export-eml), [export-attachments](#export-attachments), [create-subset](#create-subset) |
| Review and remove mail | [stage-delete](#stage-delete), [delete-staged](#delete-staged), [deduplicate](#deduplicate), [gc](#gc) |
| Back up and manage attachment storage | [backup](#backup), [pack-attachments](#pack-attachments), [purge-excluded-media](#purge-excluded-media) |
| Compare Gmail presence | [diagnose-gmail-inventory](#diagnose-gmail-inventory), [verify](#verify) |
| Repair older records | [repair-identity](#repair-identity), [repair-senders](#repair-senders), [repair-message](#repair-message), [repair-derived](#repair-derived), [repair-attachment-roles](#repair-attachment-roles), [repair-labels](#repair-labels), [repair-list-ids](#repair-list-ids), [repair-dates](#repair-dates) |
| Operate or integrate | [setup](#setup), [credentials](#credentials), [daemon](#daemon), [serve](#serve), [activity](#activity), [mcp](#mcp), [query](#query), [openapi](#openapi), [agent-token](#agent-token) |

## loops

List conversations waiting on a reply through the selected daemon, and mark them
done, snooze them, or reopen them. See [Open loops](usage/open-loops.md) for
what counts as a loop.

```bash
msgvault loops list
msgvault loops list --limit 10 --json
msgvault loops list --person 42
msgvault loops done 4521
msgvault loops done 4521 --questions 4521,4530
msgvault loops snooze 4521 --until 48h
msgvault loops reopen 4521
```

| Command | What it does |
|---|---|
| `list` | Lists loops. `--limit` from 1 to 200 (default 50), `--person <id>` for one saved person, `--json` for machine-readable output. |
| `done <message-id>` | Marks the loop done. `--questions <ids>` marks only those questions: the item's `question_message_ids` from `list --json`. |
| `snooze <message-id> --until <when>` | Hides the loop until a duration such as `48h`, a date (`YYYY-MM-DD`, midnight local time), or an RFC3339 time. Takes `--questions` like `done`. |
| `reopen <message-id>` | Clears the done or snooze decision on one question. |

`list` prints **Waiting on you** and **Waiting on them** sections, oldest first,
showing the age, person, subject, and message ID for each loop. It adds a note
when `--limit` cuts the list, when it cannot read stored messages, or when more
history remains. The daemon keeps checking in the background and reuses
completed work on later requests, while JSON output sets `coverage_incomplete`.
If the daemon stops or its background pass times out, a later incomplete request
starts another pass. When nothing is open, `list` prints `No open loops.` If you
act on a message that does not start an open loop, such as one you already
answered, the command fails with `<id> isn't an open loop.`

## cli search

Describe a task to find relevant commands. Quotes around the task are optional.

```bash
msgvault cli search "search extracted document attachments"
msgvault cli search read a conversation thread --limit 3 --json
```

Discovery searches the installed command tree locally and needs no
configuration, archive, daemon, credentials, or network access, even when you
pass delegated connection flags. It prints command paths and summaries followed
by a help hint, and it does not run any suggested command.

| Flag | Contract |
|---|---|
| `--limit` | Maximum results, default 5; range 1–20 |
| `--json` | An object with `query` and `results`; each result has `command`, `summary`, and `usage` |

Search excludes hidden, deprecated, help-only, interactive setup, and browser or
device login commands, while noninteractive children of excluded groups stay
searchable. Exclusion depends on whether a command requires interaction, not
whether its default path can prompt. Commands that accept supported credential
files or piped input stay searchable. `add-matrix` stays eligible because its
login secret can come from `--password-file`, `--login-token-file`, or stdin,
its recovery secret can come from `--recovery-file` or stdin, and
`--skip-key-backup` skips key backup. A suggested command may still require a
running daemon or a configured source. If nothing matches, the command succeeds
with an empty `results` array in JSON or a root-help hint in text. A blank task
or invalid limit exits with code 1 as a usage error. Running `msgvault cli` on
its own prints group help.

## meetings

Read archived meeting evidence through the selected daemon. See the
[meeting workflow](usage/meetings.md) for coverage and source limits.

```bash
msgvault meetings context --id 42 --id 43 --format markdown --output context.md
msgvault meetings actions --status pending --assignee alex@example.com --json
msgvault meetings metrics --domain example.com --after 2026-01-01 --json
```

### meetings links

Run `msgvault meetings links <note-id|ref> [--json]` to read the calendar copies
and notes that share an exact meeting occurrence. Pass a note ID, `msg:<id>`, or
an event `ref` from `calendar events --json`.

### meetings suggest

Run `msgvault meetings suggest <ref> [--limit N] [--json]` to rank overlapping
meeting notes and calendar events. To accept a suggestion, use `meetings link`.

### meetings link, unlink, and reset-link

Use `msgvault meetings link <ref> <ref>` to join two meeting copies or
`msgvault meetings unlink <ref> <ref>` to separate them. Either command replaces
the previous choice. To clear your choice and restore automatic matching, run
`msgvault meetings reset-link <ref> <ref>`.

### meetings context

| Flag | Contract |
|---|---|
| `--id` | Required meeting message IDs, repeatable; at most 100 |
| `--format` | `markdown` (default) or `json` |
| `--include-transcript` | Include archived transcript evidence; off by default |
| `--max-bytes` | UTF-8 content budget, default 131072; range 4096–1048576 |
| `--output`, `-o` | Output file; omitted or `-` writes the exact packet content to stdout |

Mixed or invalid selections fail. If a packet is truncated, the output
identifies the omitted content and meetings.

### meetings actions and meetings metrics

Both commands accept the following scope flags. Values within the same filter
group act as alternatives, and different groups intersect.

| Flag | Contract |
|---|---|
| `--id` | Meeting message ID, repeatable |
| `--source-id` | Source ID, repeatable |
| `--domain` | Exact participant domain, repeatable |
| `--participant-id` | Exact participant ID, repeatable |
| `--person-id` | Durable person ID; mutually exclusive with `--participant-id` |
| `--after`, `--before` | `YYYY-MM-DD` at UTC midnight; after inclusive, before exclusive |
| `--deletion` | Source deletion state: `any` (default), `active`, or `deleted` |
| `--json` | Structured response including coverage or duration bases |

ID lists and domain lists accept at most 100 values, and IDs must be positive
JavaScript-safe integers. Both commands always exclude locally deleted records.

You can also pass these flags to `meetings actions`:

| Flag | Contract |
|---|---|
| `--status` | `pending`, `completed`, `cancelled`, or `unknown`; default all |
| `--assignee` | Exact assignee email |
| `--query` | Literal title or description substring, at most 256 characters |
| `--limit` | Action rows per page, default 50; range 1–200 |
| `--cursor` | Opaque continuation cursor from the previous page |

Actions reflect the current archived source snapshot. Keep your filters
unchanged when continuing to the next page, and refresh from page one to see
newer evidence.

## Global Flags

| Flag | Description |
|---|---|
| `--config` | Path to config file (default: `~/.msgvault/config.toml`) |
| `--home` | Home directory for all data (overrides `MSGVAULT_HOME`) |
| `-v`, `--verbose` | Verbose output (implies `--log-level=debug`) |
| `--local` | Use the local daemon instead of a configured remote for archive-access commands |
| `--remote <name>` | Use the named remote from a [`[remotes.<name>]`](configuration.md#remotesname) table instead of `[remote]` |
| `--log-file <path>` | Override log file path (default: `<data_dir>/logs/msgvault-YYYY-MM-DD.log`) |
| `--log-level <level>` | Log level: `debug`, `info`, `warn`, `error` (default: `info`) |
| `--no-log-file` | Disable file logging for this run (stderr output stays on) |
| `--log-sql` | Log every SQL query at info level (verbose, for debugging) |
| `--log-sql-slow-ms <ms>` | Slow query threshold in ms (default: 100; 0 uses built-in default) |
| `--help` | Show help |
| `--agent-url <url>` | Daemon base URL for delegated mode; must be supplied together with `--agent-token-file` |
| `--agent-token-file <path>` | Path to a file containing a restricted agent token secret; must be supplied together with `--agent-url` |
| `--agent-allow-insecure` | Allow a plain-HTTP `--agent-url`; rejected by default |

---

## HTTP-Backed CLI Behavior

Commands that access archive state keep their usual stdout and stderr output
while using the same HTTP API path as remote access:

1. If you configure `[remote].url` and omit `--local`, the CLI talks to that
   remote server. Pass `--remote <name>` to select a named remote instead.
2. Otherwise, archive-access commands discover or start the local background
   daemon and talk to it over HTTP. When you set
   `[server].daemon_auto_start = false`, they connect only to a daemon that is
   already running or starting and never start a new one.
3. Passing `--local` selects the local daemon even when `[remote].url` is
   configured. It does not open SQLite inside the CLI process.
4. When you pass both `--agent-url` and `--agent-token-file`, the CLI connects
   to a remote daemon as a restricted caller. You can run `search`,
   `show-message`, `stats`, `mcp`, `calendar`, `draft-reply`, `draft-compose`,
   `draft-get`, `draft-edit`, `draft-delete`, and `draft-recover` with their
   required permissions, as described in
   [scoped archive reads](#scoped-archive-reads) and
   [managed drafts](#draft-get-draft-edit-draft-delete-and-draft-recover). In
   this mode, the CLI rejects owner configuration (`--config`, `--home`,
   `--local`, `--remote`) and never writes the token to logs or argv. It sends
   the token in the `X-Msgvault-Agent-Token` header, which generated OpenAPI
   clients do not model.

This routing gives you the same CLI behavior locally and remotely, and it avoids
opening a large SQLite database from foreground CLI processes.

The local daemon publishes its binary version and API schema version. By
default, a newer compatible CLI restarts an older local daemon before sending a
request. A source-build CLI also replaces the release daemon it was built from,
or one from an earlier release. If a running daemon comes from another source
build of the same release, the CLI leaves it running and prints the restart
command. A daemon whose API schema is older than the CLI's is still replaced,
unless `daemon_auto_restart = "never"` or `daemon_auto_start = false`. Set
`[server].daemon_auto_restart` to `newer`, `never`, or `always` to control this
lifecycle behavior; see the [restart policy](configuration.md#server).

Clients do not restart remote servers. A `[remote]` server or an `--agent-url`
daemon must run the same or a newer API schema of the same major version as the
CLI. Otherwise the CLI stops before sending archive requests and tells you to
upgrade the daemon.

---

## init-db

Initialize the archive schema through the configured remote server or the local
daemon. When no remote is configured, the CLI starts the local daemon if needed,
and the daemon handles database initialization.

```bash
msgvault init-db
```

---

## schema-version

Available in 0.22.0 and later.

Print the main archive schema version as a single decimal integer followed by a
newline. Without flags, this command prints the version this binary expects and
runs without loading configuration, opening an archive, or creating directories.

```bash
msgvault schema-version
msgvault --home /path/to/archive-home schema-version --database
```

Pass `--database` to read the configured local archive without running
migrations or starting the daemon. If the SQLite file is missing, the command
returns an error and does not create one. It rejects remote mode unless you pass
`--local` to select this machine's local archive. The stored version can be
newer than what this binary expects, and the probe still reports it without
trying to upgrade.

SQLite and PostgreSQL store the same value in `archive_metadata` under the key
`schema_version`, and the CLI probe works on either backend. A missing marker
reports `0` to indicate a legacy or uninitialized archive. Stored markers must
be positive integers; zero, negative, or malformed values return errors. The
marker records required main archive schema work only. It does not cover a
physical integrity check, API version, analytics cache version, or optional
vector backend version, and existing best-effort index and statistics work keeps
its failure behavior.

---

## migrate

Available in 0.22.0 and later.

Apply this binary's main archive schema migrations directly without starting or
contacting a daemon. Use `migrate` to update an archive before replacing an
installed executable. It can also initialize a fresh archive and is safe to run
more than once.

```bash
msgvault --home /path/to/archive-home daemon stop
msgvault --home /path/to/archive-home migrate
```

This command runs on local archives only. If a remote server is configured,
`migrate` rejects it unless you pass `--local` to select the local archive. Stop
all archive users before running it. The command acquires the daemon-owner lock
and, for SQLite, the write-owner lock before opening the archive. If you run
PostgreSQL, stop any users on other hosts yourself.

On success, `migrate` prints `Schema version: <version>`. Failure or
cancellation returns a nonzero exit status. If an archive is newer than this
binary, `migrate` rejects it before running schema DDL. The marker advances only
after required schema initialization succeeds, though a failed migration can
leave committed partial changes. Verify both a successful exit and matching
expected and stored versions before you swap binaries. Follow
[the upgrade procedure](setup.md#migrate-before-replacing-the-executable) for
backup and recovery steps.

`migrate` leaves legacy identity configuration to the existing
account-confirmation flow, while optional vector backend and analytics cache
upgrades stay owned by those subsystems. There is no downgrade mode. Neither
`schema-version` nor `migrate` accepts positional arguments.

---

## add-account

Add a Gmail account and authorize it with OAuth.

```bash
msgvault add-account <email>
msgvault add-account <email> --headless
msgvault add-account <email> --oauth-app <name>
```

| Flag | Description |
|---|---|
| `--headless` | Show instructions for headless server setup |
| `--oauth-app` | Use a named OAuth app from `[oauth.apps.<name>]` in config |
| `--force` | Delete existing token and re-authorize |
| `--readonly` | Request Gmail read-only access instead of read + write. Refused if the account already holds write access — see [OAuth Setup](/docs/guides/oauth-setup/#read-only-access) |
| `--display-name` | Set a display name for the account |
| `--no-default-identity` | Do not auto-confirm the email address as this account's "me" identity. Saved across syncs and re-authorization; only explicit `--no-default-identity=false` clears the choice. See [saved identity choice](#saved-default-identity-choice) |

If you configure `[oauth].service_account_key` or `[oauth.apps.<name>].service_account_key`, `add-account` authorizes with Google service account domain-wide delegation instead of browser OAuth. Service-account accounts do not use `--headless`, `--force`, or `--readonly`. Their scope comes from the domain-wide delegation grant in the Admin Console.

### Saved default identity choice

`add-account` (including service accounts), `add-imap`, `add-o365` (IMAP and
`--graph`), and `add-teams` save `--no-default-identity` for later syncs. If you
omit the flag when adding or re-authorizing an existing account, msgvault keeps
the saved choice. Pass `--no-default-identity=false` to re-enable automatic
defaults. New accounts confirm their default identity unless you pass the flag.

This flag does not remove existing identities, and it does not stop the one-time
legacy `[identity]` migration from adding configured addresses. If msgvault
cannot read the saved sync configuration, automatic identity confirmation logs a
warning and skips the write.

Removing the last confirmed identity also saves the no-default choice for future
syncs. To restore automatic confirmation, run the source's add command with
`--no-default-identity=false`.

---

## add-imap

Add an IMAP account to sync mail from any standard IMAP server.

```bash
msgvault add-imap --host <hostname> --username <email>
```

The command prompts you interactively for your password and never accepts it as a flag to avoid shell history exposure. For scripting or Docker, set `MSGVAULT_IMAP_PASSWORD` or pipe the password on stdin:

```bash
MSGVAULT_IMAP_PASSWORD="..." msgvault add-imap --host imap.example.com --username user@example.com
# or
echo "$PASS" | msgvault add-imap --host imap.example.com --username user@example.com
```

Before saving credentials, `add-imap` tests the connection.

| Flag | Default | Description |
|---|---|---|
| `--host` | (required) | IMAP server hostname |
| `--username` | (required) | IMAP username or email address |
| `--port` | `993` | IMAP server port (993 for TLS, 143 for STARTTLS/plain) |
| `--starttls` | `false` | Use STARTTLS instead of implicit TLS |
| `--no-tls` | `false` | Disable TLS entirely (plaintext, not recommended) |
| `--no-default-identity` | `false` | Do not auto-confirm the username as this account's "me" identity. Saved across syncs and re-authorization; only explicit `--no-default-identity=false` clears the choice. See [saved identity choice](#saved-default-identity-choice) |

msgvault saves credentials in `tokens/imap_<hash>.json` with `0600` file permissions. Use app-specific passwords when your provider supports them.

---

After you add an account, sync it with `msgvault sync-full`. IMAP accounts use the same `sync` and `sync-full` commands as Gmail. See the [Setup Guide](/docs/setup/#add-an-imap-account) for a walkthrough.

---

## message-tags

Read or update native tags on one email through the selected daemon. The
positional argument is msgvault's positive archived message ID.

```bash
msgvault message-tags 42 --json
msgvault message-tags 42 --add Label_123 --remove Label_456 --dry-run --json
msgvault message-tags 42 --add Next --remove Old --mailbox INBOX
msgvault message-tags 73 --add Next --remove Old --json
```

A plain read can also inspect a message marked deleted from its source,
including one still in Trash. Reads leave the
archive unchanged, while edits and edit previews still reject source-deleted
messages. The provider must resolve the recorded message identity. See the
[native tag API](api-server.md#native-email-tags) for the read contract.

The command picks the backend from the archived message's account:

| Account connection | Editable tags |
|---|---|
| Gmail API | Existing user label IDs |
| IMAP, including Microsoft and Fastmail accounts | Persistent custom keywords advertised by the server |
| Microsoft Graph (`msmail`) | Outlook category names |

For Gmail, pass existing user label IDs from `available_tags` instead of label
names or local label IDs. You cannot edit system labels with this command. Reads
and previews use your existing read grant, while edits need `gmail.modify` or
`mail.google.com`. If the account holds a saved read-only grant, the daemon
rejects the edit without changing the token. To get write access, re-authorize
with `msgvault add-account <account> --force`.

For IMAP, tags are custom ASCII keyword atoms up to 255 bytes, compared without
case. The mailbox must advertise persistent support for the keyword, or `\*` to
allow new keywords. An omitted `PERMANENTFLAGS` response allows all flags, while
an explicit empty list allows none. System flags such as `\Seen` and `\Flagged`
stay untouched. Each edit targets a recorded mailbox UID and UIDVALIDITY. If you
omit `--mailbox`, the command uses the current original copy, or the sole
current membership when the original is gone. If multiple copies survive, pass
`--mailbox`. If no current membership remains, sync first. See
[IMAP keywords](usage/imap.md#edit-keyword-tags).

For Microsoft Graph, tags are category names up to 255 Unicode characters
without commas. Graph compares names without case while keeping provider
spelling. Reads and previews use your existing read grant, while writes need
`Mail.ReadWrite`, which you can grant with
`msgvault add-o365 <account> --graph --mail-write`. The daemon never starts a
sign-in flow or replaces a read-only token during a tag edit. Every edit keeps
unrelated categories and checks the provider version before replacing the
category collection. If another client changed the message, read its tags and
try again. The archive shows categories as `Category: <name>` separately from
mail folders, and later syncs retain and refresh them. The available category
catalog includes only names on this message. A Graph 429 response returns `provider_write_failed`; retry after the limit
clears. Transport failures and 5xx responses return `remote_unknown`; read the
current tags before retrying.

Fastmail messages currently use IMAP, even if you configured a JMAP token for
address discovery. Because JMAP and IMAP share custom keywords, your existing
IMAP connection edits those shared tags without another credential. Outlook
categories require a Microsoft Graph account, while an IMAP connection can edit
only the keywords its server supports. File imports such as MBOX, PST, and EML
have no connected provider to update and return `unsupported_provider`.

| Flag | Contract |
|---|---|
| `--add` | Add one native tag; repeat for multiple tags, at most 100 |
| `--remove` | Remove one native tag; repeat for multiple tags, at most 100 |
| `--mailbox` | Select an exact recorded IMAP mailbox copy |
| `--dry-run` | Preview an edit without a provider write or local update |
| `--json` | Return the result, or an error with the last observed result |

To edit tags, pass at least one `--add` or `--remove` value, and do not put the
same tag in both lists. Without either flag, the command reads the current tags
and available tag IDs. Unrelated tags stay unchanged, and a retry whose changes
are already present makes no provider write.

When the response shows `verified: true`, provider readback confirmed your
requested changes at that moment, though another client can edit the message
afterward. A `--dry-run` preview does not prove write access, and the provider
can still reject a later write. If an edit partly applies or readback fails,
inspect the current tags before retrying. If the command returns
`remote_accepted_local_failed`, the provider accepted and verified the change
but local persistence failed, so sync the account.

## draft-reply

Create a reply draft from an archived message on an authorized IMAP destination,
or reply inside its original Gmail account. The daemon requires the matching
operator grant. If you omit `--from`, the reply uses the confirmed identity the
parent message was addressed to, or the source's only eligible identity. See
[IMAP drafts](/docs/usage/imap/#drafts) for the full sender rules.

```bash
msgvault draft-reply <message-id> --body <text>
msgvault draft-reply <message-id> --all --account <account> --from <address> --body <text>
msgvault draft-reply <message-id> --source-id 42 --body= --json
```

Pass `--all` to include the parent sender along with the To and Cc recipients.
msgvault deduplicates them in order, strips the destination account's confirmed
identities, and never copies the parent's Bcc recipients. Pass `--account` or
`--source-id` to choose a destination. An offline parent requires one of those selectors, and msgvault never treats
imported provenance as provider credentials.

The daemon appends the composed message to the configured literal mailbox with
the `\Draft` flag, then saves the local message and its `(mailbox, uidvalidity,
uid)` receipt. The server must advertise UIDPLUS. If the server accepts `APPEND`
without returning a receipt, the command returns `accepted_unidentified`. If the
connection drops, it returns `remote_unknown`. Inspect the mailbox before
retrying either result. If that source is currently syncing, the command returns
`sync_active`, and you can retry after the sync finishes.

To grant IMAP draft access, add an entry to the daemon host's `config.toml` and
restart the daemon:

```toml
[[imap.drafts]]
source_id = 42
enabled = true
mailbox = "Drafts"
```

Requests, Settings, source JSON, environment variables, and client configuration
never grant draft access or change the mailbox. The host policy applies per
source. Any owner caller that can reach the daemon (using an API key, browser
session, or keyless loopback) can create drafts on a granted source. A delegated
caller using a restricted agent token also needs the target source and selected
sender in the token's frozen grant; see [agent-token](#agent-token). Choosing a
`From` address only sets the local header and does not prove provider send-as
permission. Draft creation never sends mail and never moves an IMAP cursor. A
later sync reconciles the saved membership if the mailbox's UIDVALIDITY changes.

To enable Gmail drafting, add an entry per source in the daemon host's
`config.toml` and restart the daemon:

```toml
[[gmail.drafts]]
source_id = 42
enabled = true
```

Gmail accepts draft writes with `gmail.modify`, `mail.google.com`, or
`gmail.compose`. Creating a draft also lists send-as entries. That read accepts
`gmail.settings.basic`, `gmail.modify`, `gmail.readonly`, or `mail.google.com`.
When saved scope information is available, the daemon checks it. For older
tokens without saved scopes, Gmail decides whether to allow the request. The
daemon accepts `--from` when it matches the primary address or an accepted
alias. Service-account sources use the scopes in their delegated assertion.

Gmail writes use a single provider request. A 429 or rate-limit 403 is a
rejection; retry after the limit clears. A transport failure, response-read
failure, or 5xx returns `remote_unknown`. After an uncertain creation, check
Gmail using the reported RFC822 Message-ID before creating another draft.
Managed edits and deletes reconcile on retry as described below. After a
confirmed response, the archive stores the Gmail draft with its `DRAFT` label.
`draft-reply` never sends mail.

---

## draft-compose

Create a new plain-text IMAP draft on one selected live source. Pass at least
one `--to`, `--cc`, or `--bcc` value; these flags are repeatable and keep their
envelope roles.

```bash
msgvault draft-compose --account you@example.com \
  --from you@example.com --to recipient@example.com \
  --subject 'Project update' --body 'Draft text'
msgvault draft-compose --source-id 42 --to recipient@example.com \
  --bcc private@example.com --json
```

You must pass a source selector. The daemon applies the same host draft policy,
confirmed identity check, delegated sender grant, UIDPLUS requirement, and
structured provider outcomes as `draft-reply`. It stores the Bcc envelope in the
draft so your mail application can use it, and it never sends the message or
validates provider send-as rights.

### Draft to a person

To draft a message to a person instead of typing an address, list their archived
identities with [`person identities`](#person), then pass a `supported` email
address to `--to`.

```bash
msgvault person identities 7
msgvault draft-compose --account you@example.com \
  --to alice@example.com --subject 'Hi' --body 'Draft text'
```

### Beeper chat drafts

On a Beeper source, `draft-compose` puts a text draft into the composer of an
existing chat. Pass the Beeper chat ID with `--to` and the draft text with
`--body`; no other fields apply. Enable Beeper drafting per source in the daemon
host's `config.toml`, the same way as Gmail, and keep Beeper Desktop running:

```toml
[[beeper.drafts]]
source_id = 42
enabled = true
```

```bash
msgvault draft-compose --source-id 42 --to '!room:beeper.local' --body 'Draft text'
```

The chat's composer must be empty, and each chat can have at most one managed
draft. If one already exists, `draft_exists` reports it. You can run
`draft-get`, `draft-edit`, and `draft-delete` on the returned draft ID, and
`draft-delete` clears the composer. The edit body must be nonblank.

For delegated tokens, creating a draft requires `draft.create` on the Beeper
source. `draft-get` accepts `draft.edit` or `draft.delete`, `draft-edit`
requires `draft.edit`, and `draft-delete` requires `draft.delete`. Delete-only
agents receive draft metadata without committed or pending text, including in
error responses, and a creator's `draft_exists` result includes the draft ID
without its text.

`draft-get` returns the draft as Beeper last reported it, which can differ from
the text you sent because Beeper formats it. Edit and delete read the composer
first and return `draft_conflict` if someone changed it in Beeper. Because
Beeper has no conditional write, text typed between that read and the write can
still be cleared. If Beeper refuses a write before changing anything, the
command returns `provider_rejected`. When Beeper refuses the text of a new
draft, msgvault discards it so the chat stays free for another `draft-compose`.
If a write has an unknown outcome, the command returns `remote_unknown` and
keeps the pending operation, and the next edit or delete settles it from what
Beeper shows. An edit that finds an earlier delete settled, or finds that a new
draft's text was never delivered, returns `draft_discarded`. If Beeper shows
text that msgvault cannot attribute, the command returns `pending_operation`;
clear the composer in Beeper and retry. Among the other error codes,
`not_supported` means the installed Beeper Desktop does not report chat drafts,
`chat_not_found` means Beeper has no chat with that ID, `provider_unavailable`
means Beeper Desktop or its saved token cannot be reached, and
`provider_identity_mismatch` means Beeper answered for another chat or account.

---

## draft-forward

Create an IMAP draft that forwards an archived message and reuses its retained
attachment files. Select exactly one destination with `--account` or
`--source-id`, and pass at least one explicit `--to`, `--cc`, or `--bcc`. The
command never copies the parent message's Bcc recipients. This command requires
owner access and cannot run with agent tokens.

```bash
msgvault draft-forward <message-id> --account you@example.com \
  --from you@example.com --to recipient@example.com --body 'Please review'
msgvault draft-forward <message-id> --source-id 42 \
  --cc team@example.com --json
```

Every nonempty attachment must have a retained, readable file in the archive's
catalog, even if its bytes are still in the original message. Forwarding reuses
those catalog references and does not restore skipped or missing files from raw
MIME. Empty attachment parts need no stored file.

Any pending, skipped, failed, unavailable, missing, unreadable, or unsupported
occurrence stops the command before `APPEND`, and it never creates a partial
draft. The IMAP source must have an enabled `[[imap.drafts]]` entry, and the
selected sender must be a confirmed identity on that source.

The draft contains two inline plain-text parts: your editable note, followed by a
`---------- Forwarded message ----------` separator with the original headers
and text. Original attachments follow and keep their names and `Content-ID`
values when present. Attached emails (`message/rfc822`) keep their bytes with
8-bit encoding, though the command refuses any attached email that requires
binary encoding, such as one with NUL bytes, bare line breaks, or lines longer
than 998 bytes. Unknown MIME dispositions are treated as attachments.

The draft gets a new Date and Message-ID. Its subject gets one `Fwd:` prefix
after removing existing `Fw:` and `Fwd:` prefixes. The `X-Msgvault-Forward: 1`
header marks the layout for note editing, and a mail client that preserves
custom headers may keep it when you send the message. msgvault only creates the
draft.

When the server supplies a numeric `APPENDLIMIT`, msgvault checks the encoded
size before uploading. Otherwise, the server's `APPEND` response decides. This
upload check does not limit the memory used to build the draft.

With `--json`, attachment preflight refusals write a JSON object to stderr with
`status: "attachment_preflight_failed"` and a `problems` array. Each problem
includes a `reason`, along with `filename`, `part_key`, and `detail` when
available:

| Reason | Meaning |
|---|---|
| `missing_catalog_reference` | An original attachment has no matching catalog row. |
| `attachment_pending`, `attachment_skipped`, `attachment_failed`, `attachment_unavailable` | The catalog records a file that is not stored. `detail` includes its skip reason when available. |
| `unreadable_file` | Opening, reading, verifying, or closing a stored file failed. |
| `catalog_size_mismatch` | The stored file's length differs from the original MIME part. |
| `unrepresented_attachment` | A catalog row has no corresponding original attachment. |
| `attachment_reader_unavailable` | The daemon cannot access the attachment store. |
| `unrepresentable_attachment` | An attachment's metadata or bytes cannot be forwarded, or the generated draft cannot retain its catalog reference. |

## draft-get, draft-edit, draft-delete, and draft-recover

Read, edit, delete, or recover a managed draft created with `draft-reply`,
`draft-compose`, or `draft-forward`:

```bash
msgvault draft-get <draft-id> [--json]
msgvault draft-edit <draft-id> --revision <n> --body <text> [--json]
msgvault draft-delete <draft-id> --revision <n> [--json]
msgvault draft-recover <draft-id> --revision <n> [--json]
```

Creating a draft returns the opaque `draft_id` and its initial revision.

- `--revision` is required for edit, delete, and recover. Pass the current
  positive revision.
- `--body` is required for edit. Pass `--body=` to set an empty plain-text body.
- `--json` outputs one JSON result.

In a draft created with `draft-forward`, `draft-edit --body` replaces your note
and keeps the quoted text and attachments. Other drafts must be `text/plain`;
any other multipart draft returns `invalid_draft` before changing the provider
draft.

`draft-get` reads retained archive content, including discarded drafts, without
connecting to a provider or needing the source's draft mutation grant. For IMAP
drafts, editing and deleting still require the configured `[[imap.drafts]]`
policy. For Gmail drafts, editing and deleting require the configured
`[[gmail.drafts]]` policy and one of `gmail.modify`, `mail.google.com`, or
`gmail.compose`, without listing send-as entries. Deleting removes the provider
draft while keeping its archived content. Before editing or deleting a Gmail
draft, the daemon checks the current provider message ID. If someone edited the
draft in Gmail on the web, the daemon advances the local revision and returns
`changed_externally`, and your next edit or delete must pass that new revision.
None of these commands send mail.

To retry a pending Gmail edit or delete, pass its current revision. The daemon
checks Gmail before writing again. If the original message is unchanged, it
clears the pending claim and runs your requested operation. If Gmail holds a recorded replacement, the daemon finishes publishing it locally
and returns `recovered` with `revision_mismatch`. Review the new revision before
retrying. It adopts any other provider edits with `changed_externally`. A
pending delete can finish locally when Gmail confirms the draft is gone, or
retry deletion when the original draft is still there. Confirmed deletions with
unfinished local cleanup retry that cleanup without making another provider
request. If an edit retry finds that Gmail no longer has the draft, the command
returns `provider_absent` and keeps the candidate content; run `draft-delete` to
finish discarding it.

Recovery applies to IMAP drafts only. `draft-recover` refuses a Gmail draft ID
with `not_supported` for the owner and `not_permitted` for delegated tokens.
The delegated refusal does not reveal whether the draft exists. Gmail reconciliation works through
edit and delete retries instead.

Delegated tokens with `draft.create` can create Gmail reply drafts. For a Gmail
or IMAP draft, delegated `draft-get` accepts `draft.create`, `draft.edit`, or
`draft.delete`, while `draft-edit` requires `draft.edit` and `draft-delete`
requires `draft.delete`. Every command requires the source's exact type and
identifier along with the draft's archived From sender in the grant. The frozen
sender selection applies to get, edit, and delete, including when the owner
issued the token with `--sender`. If Gmail reports an external edit, the daemon
checks the adopted draft's sender again before returning it.

A grant with only `draft.delete` receives lifecycle metadata, including the
revision, from get and delete responses. Those responses omit `content`,
`raw_mime`, and `candidate_content` in both JSON and human-readable output,
including for pending and refused deletes. A matching `draft.create` or
`draft.edit` grant permits content reads, while delegated recovery still returns
metadata only.

A missing command permission returns HTTP 400 `command_not_allowed`. If a
permitted command targets another source or an unknown draft ID, it returns
`not_permitted` before running revision checks, draft policy, source locking, or
any provider request. `draft-send-as` stays owner-only.

For IMAP drafts, `draft-recover` resumes a pending operation from recorded
receipts. It can publish a known replacement or finish a confirmed removal
without `APPEND`. Delegated recovery requires `draft.edit` for an edit or active
repeat and `draft.delete` for a delete or discarded repeat, scoped to the source
in the grant. An active draft with no pending operation requires `draft.edit`,
including after a delete stopped before writing. Recovery output keeps the saved
`pending_code`. Refused results use `refusal_code`, and pending cleanup results
report the current provider result in `observation.code`. See
[Manage a created draft](usage/imap.md#manage-a-created-draft) for revision,
provider checks, retention, retry behavior, and recovery limits.

### Local chat drafts

Slack, Teams, and Discord conversations get drafts that live only in msgvault.
They never appear in the provider's composer and no command makes a provider
request.

```bash
msgvault draft-compose --conversation <conversation-id> --body <text> [--reply-to <message-id>] [--json]
msgvault draft-get --conversation <conversation-id> [--json]
```

The conversation's source owns the draft. `--reply-to` names an archived
message in the same conversation. `draft-get --conversation` lists that
conversation's drafts oldest first, so a lost draft ID can be found again;
`--json` returns one array. `draft-get`, `draft-edit`, and `draft-delete` take
the returned `chat-draft-` ID and report `location` as `msgvault`. A stale
revision returns `revision_mismatch` and leaves the draft unchanged.
`draft-recover` returns `not_supported`. Delegated tokens use the same
permissions as mail drafts, scoped to the conversation's source, except that
listing and retrieval need `draft.edit` or `draft.delete`. Removing the source or
conversation deletes its local drafts.

## draft-send-as

List Gmail send-as identities for an owner-invoked Gmail account:

```bash
msgvault draft-send-as <account> [--json]
```

Gmail accepts this read with `gmail.settings.basic`, `gmail.modify`,
`gmail.readonly`, or `mail.google.com`. Saved scope information is checked
when available. The command reports the address, display name, primary and
default flags, verification status, and whether the
address is a confirmed msgvault identity. Delegated agent tokens cannot run
this command. It does not require `[[gmail.drafts]]` and never changes Gmail.

---

## list-folders

List the selectable folders in one or all configured IMAP accounts, including
an approximate message count for each folder.

```bash
msgvault list-folders [account]
```

Use the folder names in repeated `--folder` or `--skip-folder` flags on
`sync-full` and `sync`. When the account argument is omitted, the command lists
folders for every configured IMAP account. See
[IMAP Folder Sync](/docs/usage/imap/) for examples and matching rules.

---

## add-o365

Add a Microsoft 365 or Outlook.com account using OAuth2 with XOAUTH2 IMAP authentication.

```bash
msgvault add-o365 <email>
```

The command opens your browser for Microsoft OAuth consent, then configures IMAP with XOAUTH2 automatically. The correct IMAP host is auto-detected: `outlook.office.com` for personal accounts (hotmail.com, outlook.com, live.com, msn.com) and `outlook.office365.com` for organizational accounts.

Requires a `client_id` in either `[microsoft]` or the selected `[microsoft.apps.<name>]` section of `config.toml`. See the [OAuth Setup guide](/docs/guides/oauth-setup/#microsoft-365-outlook-hotmail) for Azure AD app registration.

| Flag | Default | Description |
|---|---|---|
| `--oauth-app` | | Named `[microsoft.apps.<name>]` app. Omit to retain the source binding; pass an empty value to use `[microsoft]` |
| `--source-id` | | Reauthorize an existing source by ID when multiple sources match the mailbox; find IDs with `list-accounts --json` |
| `--tenant` | `common` | Azure AD tenant ID (restricts which accounts can authorize) |
| `--headless` | `false` | Sign in with a device code instead of a local browser |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this account's "me" identity. Saved across syncs and re-authorization; only explicit `--no-default-identity=false` clears the choice. See [saved identity choice](#saved-default-identity-choice) |
| `--graph` | `false` | Sync through the Microsoft Graph mail API instead of IMAP. Creates an `msmail` account. Needs the `Mail.Read` permission. `delete-staged` asks for `Mail.ReadWrite` on first use |
| `--sign-in` | | Microsoft sign-in name and browser login hint when it differs from the mailbox address. IMAP only; mutually exclusive with `--graph` |
| `--mail-write` | `false` | With `--graph`, request `Mail.ReadWrite` for category edits and remote deletion |

IMAP checks Microsoft's `email` claim when present, and falls back to
`preferred_username` otherwise. If your username differs from the mailbox
address, you now need `--sign-in`, including during re-authorization; msgvault
previously accepted it with a warning. `--sign-in` permits that username only
when `email` is absent, and failed checks keep your existing credentials. Graph
mail and Teams also check your Microsoft profile when token identity fields
differ or are absent, accepting your mailbox or an SMTP alias listed there.

After you add the account, sync it with `msgvault sync-full`, or use
`msgvault sync` for a `--graph` account. See
[Microsoft Graph mail sync](/docs/guides/oauth-setup/#microsoft-graph-mail-sync).

---

## add-teams

Authorize a Microsoft Teams account through delegated Microsoft Graph OAuth and
register a `teams` source.

```bash
msgvault add-teams <email>
msgvault add-teams <email> --tenant <tenant-id>
```

This stores a Teams Graph token under `tokens/teams_<email>.json`, separate from
the Microsoft IMAP token that `add-o365` uses. You need `[microsoft].client_id`
in `config.toml` and the Graph permissions listed in
[Microsoft Teams](/docs/usage/teams/).

| Flag | Default | Description |
|---|---|---|
| `--tenant` | `common` | Azure AD tenant ID to use for authorization |
| `--headless` | `false` | Sign in with a device code instead of a local browser |
| `--no-default-identity` | `false` | Do not auto-confirm the email address as this source's "me" identity. Saved across syncs and re-authorization; only explicit `--no-default-identity=false` clears the choice. See [saved identity choice](#saved-default-identity-choice) |

After you add the account, sync it with `msgvault sync-teams`.

---

## add-discord

Validate a Discord bot token and register one or more guilds as `discord`
sources. The command reads the token from a masked prompt or piped stdin and has
no token flag.

```bash
msgvault add-discord
msgvault add-discord --guild 123456789012345678
msgvault add-discord --oauth-app archive-bot \
  --guild 123456789012345678 --guild 234567890123456789
```

If the bot can access only one guild, `add-discord` selects it automatically
and echoes it for confirmation. Otherwise, repeat `--guild` to pick exact guild
IDs. Each guild becomes its own source. Setup also warns you about likely
Message Content Intent, channel-history, and private-thread access issues.

| Flag | Default | Description |
|---|---|---|
| `--guild` | sole accessible guild | Guild ID to register (repeatable/comma-separated) |
| `--oauth-app` | sole/default bot | Discord token binding label; no `[oauth.apps]` entry is required |

After you register a guild, sync it with `msgvault sync-discord`. See
[Discord](/docs/usage/discord/) for least-privilege bot setup and multi-bot
binding rules.

---

## sync-full

Download all messages from a Gmail or IMAP account. You can pass an account
identifier or display name; if you omit it, the command syncs every configured
syncable account.

```bash
msgvault sync-full [account] [flags]
```

| Flag | Description |
|---|---|
| `--limit N` | Maximum messages to download |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--query` | Gmail search query filter |
| `--noresume` | Ignore checkpoints, start fresh |
| `--folder NAME` | Scan this IMAP folder (repeatable) |
| `--skip-folder NAME` | Skip this IMAP folder (repeatable) |
| `--source-id ID` | Sync exactly one source by numeric ID; mutually exclusive with the account argument |
| `--build-cache` | Queue a cache refresh after the sync, even inside `min_rebuild_interval` |
| `--no-build-cache` | Skip the post-sync cache refresh |
| `--verbose` | Detailed progress output |

An account token can match more than one source. Pass `--source-id` when Gmail
and IMAP, or any two sources, share the same identifier.

The CLI sends the sync request to your configured remote server or local daemon
and streams the daemon's stdout and stderr back to your terminal. This keeps
local and remote full sync behavior consistent and avoids running a second
direct SQLite writer alongside `msgvault serve`.

If a full sync is interrupted, the CLI exits with status 130 and prints a resume
command to stderr that keeps your account, filters, configuration, and logging
flags. On Windows, run that command in PowerShell 7.3 or later. A fresh sync or
an IMAP scan can restart enumeration, and if the command uses relative paths,
run it from the same directory.

`sync-full` never infers source deletion from messages missing from its results.
That matters when you pass `--query`, `--limit`, or date filters, where the
listing is intentionally incomplete. Gmail source-deletion reconciliation runs
only during the complete mailbox recovery described under [`sync`](#sync).

### Gmail Spam and Trash coverage

Gmail full sync includes messages in Spam and Trash, while `--query`,
`--after`, `--before`, and `--limit` still narrow the scope. To pick up
historical Spam and Trash messages skipped by an earlier full sync, run a
complete full sync for the account with none of those options:

```bash
msgvault sync-full account@example.com
```

An idle incremental sync cannot find older omitted messages unless Gmail reports
a new history event for them. When your request scope matches, the full listing
resumes from its saved checkpoint. An unfiltered, unlimited full sync keeps your
existing History cursor, even when resumed. The next incremental sync can then
read pending label changes and deletions as well as new arrivals. If that cursor expires,
the next incremental sync runs History recovery. A first full sync seeds the
History point recorded at its start, while filtered or limited full syncs leave
the source cursor unchanged. A full sync interrupted under an earlier release
starts from the first page once. Previously archived messages are not downloaded
again or duplicated. Gmail returns only messages that still exist in the
mailbox; permanently deleted messages cannot be backfilled.

At the current 500-ID page size, a listing of N matching messages takes roughly
`ceil(N / 500)` Gmail list requests, and each newly discovered message takes one
RAW-message fetch, plus any retries. Previously archived messages still appear
in the listing, though msgvault does not download them again. Actual quota use,
transfer, storage, and first-full duration depend on mailbox size, newly
discovered mail, and retries. Plan for extra API usage and storage on large
accounts.

---

## sync

Sync new and changed messages. Gmail accounts use the Gmail History API, while
IMAP accounts scan mailboxes and skip messages already in the database. You can
pass an account identifier or display name; if you omit it, the command syncs
every account that has completed an initial full sync. Each Gmail incremental
sync starts by retrying messages an earlier sync could not download or save
until they are archived or Gmail reports them gone.

```bash
msgvault sync [account] [flags]
```

| Flag | Description |
|---|---|
| `--folder NAME` | Scan this IMAP folder (repeatable) |
| `--skip-folder NAME` | Skip this IMAP folder (repeatable) |
| `--source-id ID` | Sync exactly one source by numeric ID; mutually exclusive with the account argument |
| `--build-cache` | Queue a cache refresh after the sync, even inside `min_rebuild_interval` |
| `--no-build-cache` | Skip the post-sync cache refresh |

The CLI sends the incremental sync request to your configured remote server or
local daemon and streams the daemon's stdout and stderr back to your terminal.
The daemon serializes this work with other archive writes.

The same `--build-cache` and `--no-build-cache` flags apply to message-writing
`sync-*` commands, and the two flags are mutually exclusive. By default, manual
syncs keep a usable stale cache in place until the minimum rebuild interval
elapses, and the daemon handles any refresh after the sync command returns.

Folder filters apply to IMAP accounts only. See
[IMAP Folder Sync](/docs/usage/imap/) for examples and matching rules.

For Gmail, a normal incremental sync reads History API deletion events and marks
those messages as deleted from the source. If saving a label change or
source-deletion marker fails, the sync returns an error and keeps its previous
History position. The next sync retries those changes.

If Gmail reports that the saved history cursor has expired, msgvault recovers by
taking an unfiltered, unlimited snapshot of the complete mailbox, then applying
changes made while that snapshot ran. msgvault uses only a successfully
completed snapshot to mark missing messages as source-deleted; interrupted or
partial listings never reconcile absence.

Recovery saves both its phase and the history cursor captured before
enumeration. After an interruption, running `sync` or an unfiltered `sync-full`
resumes recovery. If the presence inventory stops early, msgvault collects it
again before reconciling absence. Once the catch-up phase is durably
checkpointed, retries read History from that saved cursor without repeating full
listings. An interruption between the reconciliation commit and that checkpoint
repeats reconciliation safely, and the source cursor advances only after catch-up
succeeds. If the saved recovery cursor also expires, msgvault starts a new
complete recovery, attempting at most one restart per invocation. During
recovery, msgvault re-reads each archived message's labels with one metadata
request without downloading MIME again. If a label read or save fails, recovery
stops at that page and the next `sync` retries it. Other per-message fetch and
ingest errors are recorded without stopping the run, and fetches that fail during
catch-up are retried on the next `sync`.

While recovery is pending, msgvault rejects a Gmail `sync-full` with query, date,
or limit filters; finish the complete recovery first. Pass `--noresume` to restart the
complete recovery from a fresh cursor.

Source deletion updates archive metadata and keeps local message and raw MIME
data intact. Search hides source-deleted messages by default; pass
`msgvault search ... --deletion-scope deleted` or `--deletion-scope any` to
include retained history. See
[Source-Deleted Messages](/docs/usage/searching/#source-deleted-messages).

---

## sync-teams

Sync Microsoft Teams chats and channels for an authorized account.

```bash
msgvault sync-teams <email>
msgvault sync-teams <email> --no-channels
msgvault sync-teams <email> --limit 100
msgvault sync-teams <email> --full
```

msgvault chooses full or incremental sync from stored cursors and checkpoints.
Pass `--full` to ignore those cursors and re-fetch every message, upserting rows
in place so importer upgrades can repair existing data without creating
duplicates.

| Flag | Default | Description |
|---|---|---|
| `--no-channels` | `false` | Sync chats only and skip team channels |
| `--limit` | `0` | Maximum messages per conversation (`0` = unlimited) |
| `--full` | `false` | Ignore stored cursor and re-fetch every message |

See [Microsoft Teams](/docs/usage/teams/) for setup, scheduling, search, and inline
media backfill.

---

## sync-discord

Sync one registered Discord guild, or every registered guild if you omit the
selector.

```bash
msgvault sync-discord [guild-id-or-name]
msgvault sync-discord 123456789012345678 --after 2025-01-01
msgvault sync-discord 123456789012345678 --full
```

To select a single guild, pass an exact guild ID or an unambiguous display name.
Without a selector, msgvault syncs sources sequentially in stable source order.
If one guild fails, later guilds still run, and the command returns an aggregate
error at the end.

| Flag | Default | Description |
|---|---|---|
| `--full` | `false` | Ignore stored cursors and re-fetch all available history, repairing rows and historical deletions |
| `--after` | — | Exclusive lower bound in `YYYY-MM-DD` or RFC3339 form |

Normal runs re-scan the configured trailing edit window, which defaults to seven
days. Passing `--full --after` bounds both re-fetch and deletion repair and
leaves earlier rows untouched. See [Discord](/docs/usage/discord/) for
per-channel and per-thread checkpoint and deletion semantics.

---

## add-omi

Validate Omi conversation and transcript access and register a configured hosted
or self-hosted account.

```bash
msgvault add-omi [identifier]
```

The command reads the Developer API key and backend root from the matching
`[[omi]]` entry. If you have only one entry, you can omit the identifier.
Registration confirms the configured primary account email. See
[Omi setup](usage/meetings.md#omi).

## sync-omi

Archive accessible Omi conversations, summaries, and transcripts. Each source
gets a resumable pass of up to five minutes per run. Run the same command again to
continue its saved creation-date window. If the provider applies a cooldown, the
output reports when requests can resume.

```bash
msgvault sync-omi [identifier]
msgvault sync-omi --limit 5
msgvault sync-omi --full --after 2026-01-01
```

If you omit the identifier, the command syncs every configured entry. After the
first sync, each run reads conversations created since the last complete sync
plus a 48-hour overlap, skipping archive writes for unchanged snapshots.
Scheduled runs require a registered source and never recreate a removed source.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum conversations processed, including unchanged records; zero is unlimited; negative values are rejected |
| `--full` | `false` | Rescan all history, picking up edits to older conversations, and rewrite derived projections in place |
| `--after` | empty | Inclusive creation-date bound in UTC (`YYYY-MM-DD`); implies `--full` |
| `--build-cache` | `false` | Queue a cache refresh after the sync, even when automatic builds are disabled or the minimum rebuild interval has not elapsed |
| `--no-build-cache` | `false` | Skip the post-sync cache refresh |

The cache flags are mutually exclusive. By default, the daemon queues a
background refresh when `[analytics].auto_build_cache` is enabled.

See [Omi coverage limits](usage/meetings.md#omi-coverage-limits) for backend filtering,
pagination, and rate limits.

## add-granola

Register a configured Granola account and validate its API key with a live API
call.

```bash
msgvault add-granola [identifier]
```

The command reads the key from the matching `[[granola]]` entry in
`config.toml`. If you have only one configured entry, you can omit the
identifier. Create Granola API keys in the desktop app's settings; they require
a Business plan. Registration also confirms the source's effective
`account_email`, even when other aliases already exist, and meeting-source
identity confirmation is mandatory.

After you add the account, sync it with `msgvault sync-granola`. If you later
change `account_email` or change aliases with `msgvault identity`, run
`msgvault sync-granola <identifier> --full` to repair existing `is_from_me`
attribution.

---

## sync-granola

Sync Granola meeting notes and transcripts.

```bash
msgvault sync-granola [identifier]
msgvault sync-granola --limit 5
msgvault sync-granola --full --after 2024-01-01
```

Sync is incremental by default and fetches only notes updated since the last
successful run. If you omit the identifier, the command syncs every configured
`[[granola]]` source. Re-fetched notes update in place, while any note whose
stored copy, metadata, and `is_from_me` attribution did not change is skipped
without invalidating the search cache or counting as updated. Passing `--full`
rewrites every fetched note and repairs existing rows without creating
duplicates. If one or more notes fail during a run, msgvault records the partial
run and returns an error without advancing the successful cursor; if other notes
were added or updated first, it refreshes the cache before returning the error. Scheduled
sync refuses a configured source that was removed from the archive and prompts
you to run `add-granola` again.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum notes per run (`0` = unlimited) |
| `--after` | — | Full-sync only notes created after this date (YYYY-MM-DD; implies `--full`) |
| `--full` | `false` | Ignore stored cursor and re-fetch every note |

See [Meeting Transcripts](/docs/usage/meetings/) for setup and what gets stored.

---

## add-pocket

Verify a configured personal Pocket API key and register its meeting source.

```bash
msgvault add-pocket [identifier]
```

If you have only one `[[pocket]]` entry configured, you can omit the identifier.
The daemon reads the key from `api_key_env` (default `POCKET_API_KEY`). Your
live account email must match `account_email`. Registration binds the source to
both that email and Pocket's stable user ID, and a different account needs a new
label.

## sync-pocket

Archive Pocket transcripts, completed summaries, and structured action items.

```bash
msgvault sync-pocket
msgvault sync-pocket personal --limit 10
msgvault sync-pocket personal --full --after 2026-01-01
```

If you omit the identifier, the command syncs every configured Pocket source.
Each run lists all recording metadata and fetches selected recordings in
least-attempted order, then by newest meeting time and recording ID. Limited
runs keep attempt order, including failed attempts, so one invalid recording
does not block older meetings. Edits update the same archived message.

| Flag | Default | Purpose |
|---|---|---|
| `--limit` | `0` | Maximum detail fetches in this run; zero means unlimited; negative values fail |
| `--after` | Unset | Include meetings dated on or after this UTC date (`YYYY-MM-DD`); implies `--full`. Recordings listed with an earlier recording date skip detail fetches |
| `--full` | `false` | Repair derived projections and invalid sync state; preserve valid attempt order |
| `--build-cache` | `false` | Request analytics cache refresh after sync |
| `--no-build-cache` | `false` | Skip analytics cache refresh after sync |

Failed or canceled runs keep committed meetings and recoverable attempt state
without advancing the last successful sync. Sections that are still processing
or missing keep their prior usable content independently, while explicit
successful empty sections clear it. Provider deletions do not remove archived
meetings.

See the [Pocket workflow](usage/meetings.md#pocket) for setup and limitations.

## add-notion-meetings

Validate a configured read-only Notion integration and register its meeting
source without printing meeting content. The probe checks a referenced section
block and reports when no section blocks are available to check.

```bash
msgvault add-notion-meetings [identifier]
```

The matching `[[notion_meetings]]` entry needs `account_email` and `token`. If
you have only one entry, you can omit the identifier.

---

## sync-notion-meetings

Sync Notion AI Meeting Notes from the attendee-visible recent window. The
summary reports partial coverage when the query has more results or a meeting
section cannot be read completely.

```bash
msgvault sync-notion-meetings [identifier]
msgvault sync-notion-meetings --limit 3
msgvault sync-notion-meetings --full --after 2026-01-01
msgvault sync-notion-meetings --probe
```

Notion returns at most 50 meeting notes and provides no discovery cursor. Every
run hydrates and checksum-verifies the selected visible meetings. Pass `--full`
to also run matching snapshots through archive upsert so cursor and archive
divergence can be repaired, and pass `--after` to filter the visible set
locally. The summary reports partial coverage when Notion indicates more records
exist. Due transcript maintenance runs outside `--limit`.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum visible meetings hydrated and verified; due maintenance is additional |
| `--after` | — | Local visible-set lower bound in `YYYY-MM-DD` form; implies `--full` |
| `--full` | `false` | Force selected snapshots through archive upsert instead of skipping matching checksums |
| `--probe` | `false` | Validate capabilities and result shape without printing meeting content |

See [Meeting Transcripts](/docs/usage/meetings/#notion-ai-meeting-notes) for setup,
privacy, retry behavior, and stored evidence.

---

## add-twilio

Check a configured Twilio account's access and register it as a meeting source
without printing details about individual calls.

```bash
msgvault add-twilio [identifier]
```

If you have only one `[[twilio]]` entry, you can omit the identifier. See
[Twilio configuration](configuration.md#twilio-sources).

## sync-twilio

Archive Twilio calls along with their recordings and retained transcripts as
meetings. Run `add-twilio` first. If you omit the identifier, the command syncs
every configured source.

```bash
msgvault sync-twilio [identifier]
msgvault sync-twilio work --limit 20
msgvault sync-twilio work --full --after 2026-01-01
msgvault sync-twilio work --probe
```

| Flag | Description |
|---|---|
| `--limit n` | Process at most n calls. 0 is unlimited |
| `--full` | Revisit every call Twilio still lists, not only those from the last seven days, including recordings that were skipped or unavailable |
| `--after YYYY-MM-DD` | Only recordings created on or after this UTC date; implies `--full` |
| `--probe` | Read one page of recordings and print a count; requires an identifier when several accounts are configured |
| `--build-cache` | Refresh analytics cache after sync |
| `--no-build-cache` | Skip analytics cache refresh; mutually exclusive with `--build-cache` |

If `--limit` stops before the end of the call list, the summary notes that the
sync paused and prints the command to continue it. See the
[meeting guide](usage/meetings.md#twilio) for retries and coverage.

## add-bland

Check a configured Bland account's access and register it as a meeting source,
without printing details about individual calls.

```bash
msgvault add-bland [identifier]
```

If you have only one `[[bland]]` entry, you can omit the identifier. See
[Bland configuration](configuration.md#bland-sources).

## sync-bland

Archive every listed Bland call, whether ended or not, with its summary,
retained transcript, and recording as a meeting. Run `add-bland` first. If you
omit the identifier, the command syncs every configured source.

```bash
msgvault sync-bland [identifier]
msgvault sync-bland work --limit 20
msgvault sync-bland work --full --after 2026-01-01
msgvault sync-bland work --probe
```

| Flag | Description |
|---|---|
| `--limit n` | Fetch at most n calls, including recent calls still missing artifacts and saved retries. 0 is unlimited |
| `--full` | Revisit every call, not only those from the last seven days, including recordings that were skipped or unavailable |
| `--after YYYY-MM-DD` | Only calls created on or after this UTC date; implies `--full` |
| `--probe` | Read one call and its postcall data and print what is available; requires an identifier when several accounts are configured |
| `--build-cache` | Refresh analytics cache after sync |
| `--no-build-cache` | Skip analytics cache refresh; mutually exclusive with `--build-cache` |

If `--limit` stops before the end of the call list, the summary notes that the
sync paused and prints the command to continue it. See the
[meeting guide](usage/meetings.md#bland) for retries and coverage.

## add-plaud

Authorize and register a configured Plaud cloud account with browser OAuth.

```bash
msgvault add-plaud [identifier]
```

If you have only one configured `[[plaud]]` entry, you can omit the identifier.
The browser callback listens on the daemon host at
`localhost:8091/callback/plaud`. A configured remote refuses the command before
proxying; instead, forward that port over SSH and run
`msgvault --local add-plaud <identifier>` on the daemon host. msgvault stores
credentials in `tokens/plaud_<identifier>.json` bound to the exact MCP endpoint.

Your live account email must match `account_email` before registration succeeds.
A source's confirmed owner cannot change under the same identifier. Pick a new
identifier for another account. See
[Plaud configuration](configuration.md#plaud-sources).

## sync-plaud

Archive Plaud cloud recordings, complete transcripts, and every note tab. Cloud
Sync and upstream transcription must already be enabled. msgvault does not
download audio and never writes changes back to Plaud.

```bash
msgvault sync-plaud [identifier]
msgvault sync-plaud work --limit 20
msgvault sync-plaud work --full --after 2025-01-01
msgvault sync-plaud work --probe
```

| Flag | Description |
|---|---|
| `--limit n` | Hydrate at most n recordings; newest first, then rotate through least recently attempted. Failed recordings retry on their next turn. 0 is unlimited; negatives fail |
| `--full` | Force archive repair, preserving stable source and file IDs |
| `--after YYYY-MM-DD` | Filter recording dates locally; implies `--full` and retains rotation progress |
| `--probe` | Print tool names, input schemas, and first-page counts without personal content or archive writes; requires an identifier when multiple accounts are configured |
| `--build-cache` | Refresh analytics cache after sync |
| `--no-build-cache` | Skip analytics cache refresh; mutually exclusive with `--build-cache` |

If you omit the identifier, the command syncs every configured Plaud entry.
Every run validates the live account owner and checks full content for edits.
Run `add-plaud` first; `sync-plaud` refuses an unregistered source before
authenticating.

Pending transcripts and notes that are still missing keep previous evidence, and
deleted recordings stay archived. Failed or canceled runs save rotation progress
and refresh committed changes before returning an error. See the
[meeting guide](usage/meetings.md#plaud) for pagination and consistency limits.

## add-circleback

Authorize a configured Circleback account with browser OAuth (Circleback's MCP
server uses OAuth with dynamic client registration).

```bash
msgvault add-circleback [identifier]
```

msgvault saves the token under `tokens/circleback_<identifier>.json`. If you
have only one configured `[[circleback]]` entry, you can omit the identifier.
The command always confirms the source's effective `account_email` and has no
identity opt-out flag.

Circleback redirects OAuth to `localhost:8090`, and a CLI configured for a
remote daemon refuses to proxy this command. Run it on the daemon host where the
token lives. Over SSH, run `ssh -L 8090:localhost:8090 user@daemon-host`, run
`msgvault --local add-circleback <identifier>` in that remote shell, and open
the printed authorization URL in your local browser if needed. On the daemon
host, `--local` selects that host's archive; on a workstation, it targets a
separate local archive.

After you add the account, sync it with `msgvault sync-circleback`. If you
change your primary email or confirmed aliases later, run a `--full` sync.

---

## sync-circleback

Sync Circleback meetings, notes, action items, and transcripts.

```bash
msgvault sync-circleback [identifier]
msgvault sync-circleback --limit 5
msgvault sync-circleback --full --after 2024-01-01
msgvault sync-circleback --probe
```

Sync is incremental by default. Each run lists meeting IDs without a date bound
so newly created backfills show up even when their scheduled date is old.
msgvault fetches full details for unknown meetings and for known meetings
created within the 48-hour refresh overlap, skipping unchanged snapshots without
invalidating the search cache. If you omit the identifier, the command syncs
every configured `[[circleback]]` source. Missing or recognized-empty
transcripts enter a bounded `pending` state and retry every six hours, normally
until seven days after the scheduled meeting time (or 48 hours when no usable
time exists). Expired retries become `unavailable`, and a later `--full` run can
check them again.

Due transcript retries count as maintenance work and run outside the new-meeting
limit. Provider, contract, missing-result, ingest, archive-recovery, and
cancellation failures fail the sync and keep the prior successful cursor.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum newly searched meetings; due maintenance items are additional (`0` = unlimited) |
| `--after` | — | Full-sync only meetings after this date (YYYY-MM-DD; implies `--full`) |
| `--full` | `false` | Ignore stored cursor and re-fetch every meeting |
| `--probe` | `false` | Print the MCP tool inventory and a sample result instead of syncing |

See [Meeting Transcripts](/docs/usage/meetings/) for setup and what gets stored.

---

## add-twenty

Validate read access and register a configured Twenty call recording source.

```bash
msgvault add-twenty [identifier]
```

The daemon-host `[[twenty]]` entry needs `account_email`, `base_url`, and
`api_key`. If you have only one entry, you can omit the identifier. Registration
checks recording, calendar, and participant access before creating the source.

---

## sync-twenty

Archive summaries and diarized transcripts from Twenty call recordings.

```bash
msgvault sync-twenty [identifier]
msgvault sync-twenty work --after 2026-01-01 --limit 10
msgvault sync-twenty work --full
msgvault sync-twenty work --probe
```

Each run reads recordings updated since the last successful run plus a
five-minute overlap, and late summaries or transcripts update the existing
meeting. Recordings that cannot be archived are skipped and reported instead of
failing the run. If you omit the identifier, the command syncs every configured
source. API failures fail the run while keeping previously committed meetings.

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum eligible meetings processed (`0` = unlimited); stopped scans report partial coverage, and the next run without `--after` continues, including a stopped `--full` rescan |
| `--after` | — | Inclusive UTC occurrence-date lower bound (`YYYY-MM-DD`) applied after reading; leaves the sync position unchanged |
| `--full` | `false` | Rescan every recording and refresh archive projections and attribution; resumes an unfinished limited rescan instead of restarting it |
| `--probe` | `false` | Check read access without printing content or writing the archive; requires one source |
| `--build-cache` | `false` | Request a cache build after manual sync |
| `--no-build-cache` | `false` | Skip the cache build after manual sync |

See [Meeting Transcripts](/docs/usage/meetings/#twenty-call-recordings) for setup,
stored evidence, duration fallbacks, and retained source deletions.

---

## add-muesli

Check a configured local Muesli database and register it as a meeting source.

```bash
msgvault add-muesli [identifier]
```

The matching `[[muesli]]` entry requires `account_email`, while `db_path`
defaults to `~/Library/Application Support/Muesli/muesli.db`. If you have only
one entry, you can omit the identifier. The command fails unless the file opens
read-only as a Muesli database on the recorder in remote mode, or on the daemon
host in same-host mode. Remote mode registers the stable source with the daemon
without sending database paths, and the next sync uploads every meeting. It
rejects changes to a source's recorder identity.

---

## sync-muesli

Archive completed meetings from a local Muesli database.

```bash
msgvault sync-muesli [identifier]
msgvault sync-muesli mac --limit 5
msgvault sync-muesli --after 2026-01-01
msgvault sync-muesli --full
```

Every run reads the entire database read-only, updating changed meetings in
place and skipping unchanged ones. msgvault resolves attendees through Apple
Contacts unless you set `contacts = false`, and the summary reports whether
Contacts was `complete`, `partial`, `unavailable`, or `off`. Meetings still
recording or processing wait for a later run, and meetings deleted in Muesli
stay archived. If you omit the identifier, the command syncs every configured
`[[muesli]]` source.

To archive retained M4A and WAV audio as attachments, set `recordings = true` in
the source's `[[muesli]]` configuration (recordings are off by default).
`recording_max_bytes` caps each file at 4 GiB by default, and the remote daemon
also enforces `[server].muesli_recording_max_bytes` (also 4 GiB by default).
Oversized recordings report a retryable error; raise both applicable caps and
sync again. Missing, incomplete, or recently written files wait for a later sync
without failing the meeting import, and the summary reports stored and deferred
recordings. You do not need recording flags for manual sync, watch, scheduled
sync, or the completion hook. See
[recording capture](usage/meetings.md#muesli-recordings).

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Maximum meetings processed per run (`0` = unlimited) |
| `--after` | — | Only meetings that start on or after this UTC date (`YYYY-MM-DD`) |
| `--full` | `false` | Rewrite every archived meeting, even unchanged ones, to refresh attribution |
| `--watch` | `false` | In remote mode, run an initial scan and serial rescans using enabled sources' 5-field cron schedules |
| `--build-cache` | `false` | Explicitly request an analytics cache refresh after the run, including an empty run |
| `--no-build-cache` | `false` | Skip the cache refresh request |

`--watch` needs an enabled source with a schedule and a configured remote
archive, and you must keep the process running. You cannot combine it with
`--limit`, `--after`, `--full`, or `--build-cache`. Transport failures retry on
the next scheduled scan, including when the daemon is unreachable or rate
limiting at startup, while an API schema mismatch or rejected credentials stop
the watcher. Watch prints a failure category and a suggested next step without
logging meeting or Contacts values; run a manual sync for full diagnostics.
Same-host automatic sync continues to use the daemon scheduler.

Routine imports follow your automatic cache-refresh policy, and unchanged remote
uploads do not request a refresh. A same-host scheduled scan always checks the
cache, so a build held back by `min_rebuild_interval` after a hook or manual import
still runs once that interval ends. Cache flags are mutually exclusive.
Remote meeting JSON is limited to 16 MiB per request, 200 participants, and 50
distinct identities and 50 enrichment entries per participant. If review-only
Contacts suggestions exceed either limit, msgvault drops them and still imports
the meeting. Other invalid or oversized records do not block later valid
meetings from importing, though the run still reports failure and names each
skipped meeting's Muesli row ID.

Remote scans read every meeting but upload only those that are new or changed
since the daemon acknowledged them, along with meetings that have pending
duplicate-card suggestions. The recorder keeps that acknowledgment state in
`<data_dir>/muesli-sync`. Passing `--full` uploads every meeting, and
`add-muesli` clears the saved record. Audio streams separately with owner
credentials, and msgvault acknowledges its hash only after attachment storage
succeeds. A lost response is safe to retry. The daemon never receives the
Muesli database or local paths. Audio uploads have the daemon's existing
30-minute read deadline, and a failed transfer retries from the beginning on the
next sync.

See [Meeting Transcripts](/docs/usage/meetings/#muesli) for setup and what gets
stored.

---

## muesli-hook

Archive the completed meeting named by Muesli's executable-launcher event.

```bash
msgvault muesli-hook < completion-event.json
msgvault muesli-hook --install /path/to/launchers
```

The command reads up to 4 KiB from stdin and requires `schemaVersion: 1`,
`event: "meeting.completed"`, `kind: "meeting"`, a positive integer `id`, and an
RFC 3339 `completedAt`. Unknown or duplicate fields fail. You must have only one
`[[muesli]]` source configured because the event carries no database identity.

Pass `--install DIR` to create a `msgvault-muesli-hook` symlink pointing to the
path used to run msgvault, keeping its `.exe` suffix when present. Installing from
a package manager's stable path, such as Homebrew's `bin` entry, keeps the hook
working across upgrades. Creating the symlink requires symlink permissions on
your platform. Reinstalling replaces an earlier hook symlink, and refuses to
replace any other file. Select that executable in Muesli's completion-hook
setting. The launcher reads msgvault's default configuration, while manual runs
accept the normal `--config` flag. In remote mode, it reads the meeting and
Contacts locally and sends them over the existing authenticated transport; on
the same host, it delegates the selected row to the local daemon. Scheduled full
rescans pick up missed hooks and later edits.

---

## archive-remote-images

Download remote `<img src>` images from archived email for offline viewing. This
command is separate from the default-off `[sync].archive_remote_images` setting.

```bash
msgvault archive-remote-images --allow-tracking
msgvault archive-remote-images --allow-tracking --source-id 1 --limit 100
```

| Flag | Default | Description |
|---|---|---|
| `--allow-tracking` | `false` | Required consent to image requests that can activate tracking pixels |
| `--source-id` | `0` | Restrict to one source; 0 includes all email sources |
| `--limit` | `0` | Maximum messages to scan; 0 scans all matching email |

Image requests go out from the archive server rather than your browser. The
server blocks private-network destinations, though public hosts can still track
requests. You must pass `--allow-tracking` even if automatic archiving is turned
on.

The command reuses images already in the archive, and you can retry failed
downloads by running it again. Successful downloads stay saved even if other
images fail. The summary prints messages scanned, images downloaded or reused,
and errors; any image error returns a nonzero exit status. Original messages
stay untouched.

---

## backfill-teams-media

Re-fetch Microsoft Teams inline hosted-content media for messages you already
imported.

```bash
msgvault backfill-teams-media <email>
msgvault backfill-teams-media <email> --only-incomplete
```

The command scans stored Teams HTML bodies for Graph `hostedContents` URLs and
downloads those images into the attachment store. Because attachments are
content-addressed, the command is safe to run more than once.

| Flag | Default | Description |
|---|---|---|
| `--only-incomplete` | `false` | Retry only messages whose inline media is still missing |

---

## backfill-discord-media

Refresh Discord attachment metadata and retry pending downloads for one guild or
every registered guild.

```bash
msgvault backfill-discord-media [guild-id-or-name]
msgvault backfill-discord-media 123456789012345678 --only-incomplete
```

By default, the command scans every archived Discord message with attachments
and skips downloads for messages that are already complete. If you omit the
selector, guilds run sequentially with the same aggregate-failure behavior as
`sync-discord`.

| Flag | Default | Description |
|---|---|---|
| `--only-incomplete` | `false` | Select only messages with pending attachment rows |

Backfill re-fetches each selected source message that has pending attachments to
get fresh signed CDN URLs. If the source message has been deleted on Discord, an
incomplete attachment cannot be recovered. See
[Discord](/docs/usage/discord/#attachment-backfill-and-limits).

---

## source facebook-comments add

Register the Page selected by a configured `[[facebook_comments]]` profile. The
daemon reads its named token environment variable and checks the Page identity
before saving the source or its confirmed owner identity.

```bash
msgvault source facebook-comments add [identifier]
```

If you omit the identifier, the command registers every configured profile. An
unknown explicit label fails and never selects another profile or falls back to
an email sync. See [Facebook Page comments](usage/facebook-comments.md).

---

## source facebook-comments sync

Archive returned organic, visitor, and ad-post comments and nested replies for
registered Facebook Pages. If you omit the identifier, the command syncs every configured
profile. Partial writes stay saved and enter cache refreshes even when another
surface fails. This command does not send replies.

```bash
msgvault source facebook-comments sync [identifier] [--max-requests 1000] [--full]
```

| Flag | Default | Description |
|---|---|---|
| `--max-requests` | `1000` | Positive maximum API reads per Page, including identity validation; unfinished scans resume |
| `--full` | `false` | Explicitly restart selected source scans from fresh cursors, preserving and updating existing rows |
| `--build-cache` | `false` | Queue a cache refresh even when automatic builds are disabled or delayed |
| `--no-build-cache` | `false` | Skip the post-sync cache refresh |

See [coverage and resuming](usage/facebook-comments.md#understand-coverage-and-resuming)
for independent surface progress, request costs, and expired-cursor recovery.

---

## source instagram-comments add

Register the professional account linked to the selected Page from a configured
`[[instagram_comments]]` profile. If you omit the identifier, the command
registers every configured profile. The daemon reads tokens on the daemon host.

```bash
msgvault source instagram-comments add [identifier]
```

## source instagram-comments sync

Archive returned media captions, comments, and replies from registered linked
professional accounts. The positive request budget includes Page and account
verification. Partial writes stay searchable, and `--full` restarts cursors
without deleting rows. If you omit the identifier, the command syncs every
profile.

```bash
msgvault source instagram-comments sync [identifier] [--max-requests 1000] [--full]
```

See [Instagram comments](usage/instagram-comments.md) for setup, identity,
resuming, and coverage limits. These commands do not send replies.

## source facebook-messages add

Register the Messenger inbox of the Page in a `[[facebook_messages]]` profile.
If you omit the identifier, the command selects every profile.

```bash
msgvault source facebook-messages add [identifier]
```

## source facebook-messages sync

Archive the Page's Messenger conversations as chats, keeping up to the 20 most
recent messages from each conversation. Pass `--full` to restart from the most
recent conversations.

```bash
msgvault source facebook-messages sync [identifier] [--max-requests 1000] [--full]
```

## source instagram-messages add

Register the inbox of the Instagram account in an `[[instagram_messages]]`
profile. If you omit the identifier, the command selects every profile.

```bash
msgvault source instagram-messages add [identifier]
```

## source instagram-messages sync

Archive the account's Instagram conversations as chats, keeping up to the 20
most recent messages from each conversation. The flags match
`source facebook-messages sync`.

```bash
msgvault source instagram-messages sync [identifier] [--max-requests 1000] [--full]
```

See [Facebook and Instagram messages](usage/meta-messages.md) for setup and
limits.

## source instagram-mentions add

Register the mentions source for `[[instagram_comments]]` profiles that set
`mentions = true`. If you omit the identifier, the command selects every
matching profile.

```bash
msgvault source instagram-mentions add [identifier]
```

## source instagram-mentions sync

Archive media where other accounts tag the linked account. An unfinished run
resumes next time, and `--full` restarts from the newest tagged
media without deleting rows.

```bash
msgvault source instagram-mentions sync [identifier] [--max-requests 1000] [--full]
```

See [Archive media that tags the account](usage/instagram-comments.md#archive-media-that-tags-the-account).

## comment moderate

Hide, unhide, or delete an archived Facebook Page or Instagram comment on Meta,
then record that change in the archive. The comment's profile must
allow the action in [`actions`](configuration.md#facebook-comments).

```bash
msgvault comment moderate <message-id> --hide|--unhide|--delete [--dry-run] [--yes]
```

| Flag | Default | Description |
|---|---|---|
| `--hide` | `false` | Hide the comment from public view |
| `--unhide` | `false` | Make a hidden comment visible again |
| `--delete` | `false` | Delete the comment from Meta; the archive keeps it and its replies, marked deleted |
| `--dry-run` | `false` | Show the comment without calling Meta |
| `--yes` | `false` | Confirm `--delete` |

Choose exactly one of `--hide`, `--unhide`, or `--delete`. See
[Hide or delete comments](usage/facebook-comments.md#hide-or-delete-comments).

## comment reply

Post a public reply to an archived Facebook Page or Instagram comment, then
archive the reply. The comment's profile must list `"reply"` in
[`actions`](configuration.md#facebook-comments). msgvault refuses to send the
same text to the same comment twice.

```bash
msgvault comment reply <message-id> --body <text> [--dry-run | --clear-unknown]
```

| Flag | Default | Description |
|---|---|---|
| `--body` | required | Reply text |
| `--dry-run` | `false` | Show the comment and reply without calling Meta |
| `--clear-unknown` | `false` | After checking the comment on Meta, allow a reply whose outcome is unknown to be sent again; sends nothing |

The command exits with code 1 if the same text was already posted, or if an
earlier send of that text has an unknown outcome. If Meta accepts the reply but
msgvault cannot archive it yet, the command still succeeds and prints a note,
and the next sync archives the reply. See
[Reply to comments](usage/facebook-comments.md#reply-to-comments).

## add-chatwoot

Register permitted Chatwoot inboxes from configured `[[chatwoot]]` profiles. The
daemon reads each profile's token environment variable and validates access
against the account API.

```bash
msgvault add-chatwoot [identifier]
```

If you omit the identifier, the command registers every configured profile.
Include and exclude inbox filters apply, and matching no inboxes returns an
error. Run `add-chatwoot` again after you add new inboxes. See
[Chatwoot setup](guides/chatwoot.md).

---

## source feeds add

Follow RSS 2.0, Atom, or JSON feeds and archive their items. See
[Feeds](usage/feeds.md).

```bash
msgvault source feeds add https://example.com/releases.atom
msgvault source feeds add --opml ~/Downloads/subscriptions.opml
```

| Flag | Default | Description |
|---|---|---|
| `--opml` | — | Follow every feed in this OPML file without fetching them; mutually exclusive with URL arguments |

The command fetches each URL first. If a URL is not a reachable feed, it reports
the error without following that URL and exits nonzero. Any items a feed lists
now are archived right away. Adding a previously removed feed follows it again.

---

## source feeds list

List followed feeds with their title, URL, last check, and last error.

```bash
msgvault source feeds list
msgvault source feeds list --all --json
```

| Flag | Default | Description |
|---|---|---|
| `--all` | `false` | Include removed feeds |
| `--json` | `false` | Output JSON |

---

## source feeds remove

Stop fetching one or more feeds while keeping their archived items searchable.

```bash
msgvault source feeds remove https://example.com/releases.atom
```

---

## source feeds sync

Fetch every followed feed, or only the URLs you specify, and archive any items
not yet in the archive. If one feed fails, the command reports it, syncs the
remaining feeds, and exits nonzero.

```bash
msgvault source feeds sync
msgvault source feeds sync https://example.com/releases.atom
msgvault source feeds sync --full
```

| Flag | Default | Description |
|---|---|---|
| `--full` | `false` | Download every feed again, even when unchanged |

Configure scheduled fetching under [`[feeds]`](configuration.md#feeds).

---

## sync-chatwoot

Sync Chatwoot conversations, media, and linked voice-call meetings. Only
registered inboxes selected by the profile's filters are synced. If you omit the
identifier, the command syncs every configured profile. Partial success stays
saved, and healthy inboxes continue even if another inbox fails.

```bash
msgvault sync-chatwoot [identifier] [--inbox 7] [--limit 100] [--full] [--no-media]
```

| Flag | Default | Description |
|---|---|---|
| `--inbox` | all included registered inboxes | Numeric inbox ID to sync; repeat for multiple inboxes; requires an identifier with several profiles; excluded or unregistered IDs are rejected |
| `--limit` | `0` | History messages per conversation this run; artifact refreshes run additionally; zero means no history limit; unfinished history resumes |
| `--full` | `false` | Reread all available history and update existing rows in place |
| `--no-media` | `false` | Defer attachment downloads while retaining metadata and available transcripts; recent files retry on later syncs |
| `--build-cache` | `false` | Queue a cache refresh after the sync, even when automatic builds are disabled or the minimum rebuild interval has not elapsed |
| `--no-build-cache` | `false` | Skip the post-sync cache refresh |

See [Chatwoot](guides/chatwoot.md#understand-sync-limits) for discovery,
refresh windows, request budgets, and how unavailable conversations are handled.
Reconciliation defaults to every 24 hours; set `reconcile_interval_hours` in
[Chatwoot configuration](configuration.md#chatwoot).

---

## add-beeper

Register the chat accounts connected to a locally running
[Beeper Desktop](/docs/usage/beeper/) as `beeper` sources, one per network.

```bash
msgvault add-beeper
msgvault add-beeper --token-file ~/beeper-token.txt
MSGVAULT_BEEPER_TOKEN="..." msgvault add-beeper
```

Beeper Desktop must be running on the same machine, and you need an access token
minted in Beeper Desktop under **Settings** > **Developer**. msgvault saves the
token at `tokens/beeper.json` and skips any accounts filtered out by
`[beeper].accounts` or `exclude_accounts` in `config.toml`.

Networks that Beeper serves natively instead of bridging, such as iMessage, do
not appear in its accounts API, so msgvault discovers them from chat data and
reports them as *found via chats*. Run `add-beeper` again after you connect a
new network in Beeper Desktop.

| Flag | Default | Description |
|---|---|---|
| `--token-file` | | Read the access token from a file instead of prompting |
| `--no-default-identity` | `false` | Do not auto-confirm each account's own identity as that source's "me" identity |

After you add the accounts, sync them with `msgvault sync-beeper`.

---

## add-matrix

Register a Matrix account as a native `matrix` source by logging in a dedicated
device named `msgvault (read-only)`. See [Matrix](/docs/usage/matrix/).

```bash
msgvault add-matrix \
  --homeserver https://matrix.example.org \
  --user-id @archive:example.org \
  --password-file /path/to/password \
  --recovery-file /path/to/recovery-key
```

| Flag | Default | Description |
|---|---|---|
| `--homeserver` | required | Base URL of the Matrix homeserver |
| `--user-id` | required | Full Matrix user ID |
| `--password-file` | prompt or stdin | Read the account password from a file |
| `--login-token-file` | — | Read a single-use `m.login.token` obtained from an SSO/login flow; mutually exclusive with `--password-file` |
| `--recovery-file` | prompt | Read the secret-storage recovery key or passphrase from a file |
| `--recovery-passphrase` | `false` | Treat the recovery secret as a passphrase |
| `--skip-key-backup` | `false` | Clear the saved backup key and keep encrypted placeholders; mutually exclusive with recovery flags |
| `--no-default-identity` | `false` | Do not auto-confirm the Matrix user ID as this source's "me" identity |

msgvault writes the access token and unlocked backup key to an owner-only file
under `tokens/`, and never saves the recovery secret. First-time setup requires
either a recovery secret or `--skip-key-backup`, while renewing without either
keeps the saved backup key. Running `add-matrix` again for a registered user
renews the login in place and keeps its history. See the
[Matrix guide](usage/matrix.md) for credential access, backup retention, and
decryption limits.

---

## sync-matrix

Sync joined rooms for every registered Matrix account. The first run backfills
history per room through `/messages`, and later runs use a saved `/sync`
`next_batch` token. If one account fails, the other accounts still sync.

```bash
msgvault sync-matrix
msgvault sync-matrix --account @archive:example.org
msgvault sync-matrix --full
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Sync only this exact Matrix user ID |
| `--full` | `false` | Re-fetch complete joined-room history, rescan archived messages for media, and retry media downloads immediately |
| `--no-media` | `false` | Record media metadata without downloading bytes this run |

Configure room selection and scheduled sync under `[matrix]`. The account's key
backup decrypts encrypted history and retries earlier placeholders after a
backup change, following the retry rules in the [Matrix guide](usage/matrix.md).
The recovered count includes messages replaced with decrypted content and
excludes edits and reactions.

The first sync after an upgrade scans previously archived messages for media and
saves a per-account completion marker. Later syncs skip that scan unless you
pass `--full`.

Later syncs fetch pending media and retry temporary errors such as 408, 425,
429, 5xx, timeouts, and connection failures. Transient failures stay pending and
retry on normal syncs with a growing backoff of 1, 2, 4, 8, 16, and then 24
hours, keeping the 24-hour interval for subsequent failures. Access denied,
missing files, hash mismatches, and decryption errors stay failed and retry on
normal syncs after one week. Passing `--full` bypasses both waits. Each media
item is attempted once per run, and the summary reports remaining pending and
failed media for the selected account and rooms.

---

## sync-beeper

Sync chats from Beeper Desktop for every registered Beeper account across all
connected networks. The first run backfills full locally available history and
can resume if interrupted, while later runs are incremental. If one account
fails, the remaining accounts still sync, msgvault rebuilds the analytics cache
for the accounts that succeeded, and the command exits nonzero listing the
failures. When an account reports errors, the output names the chats they belong
to (see [Failed chats](/docs/usage/beeper/#failed-chats)). Without `--account`,
the `[beeper]` `accounts` and `exclude_accounts` filters choose which registered
sources sync. See [Beeper](/docs/usage/beeper/).

```bash
msgvault sync-beeper
msgvault sync-beeper --account signal --account telegram
msgvault sync-beeper --full
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to sync (repeatable) |
| `--limit` | `0` | Max messages per chat this run (0 = no limit; limited backfills resume next run) |
| `--full` | `false` | Ignore stored cursors and re-fetch every message (repairs rows in place) |
| `--no-media` | `false` | Skip attachment downloads for this run |

---

## backfill-beeper-media

Retry pending Beeper attachment downloads that failed or exceeded the size cap
during `sync-beeper`. Because attachments are content-addressed, running this
command more than once is safe. If a source message was deleted in Beeper,
msgvault permanently clears its pending markers and keeps any attachments
already downloaded. Transient fetch errors keep the marker and count as still
pending, so re-running retries them. Recorded errors list the
[failed chats](/docs/usage/beeper/#failed-chats) and how to retry them.

```bash
msgvault backfill-beeper-media
msgvault backfill-beeper-media --account signal
```

| Flag | Default | Description |
|---|---|---|
| `--account` | all registered | Beeper accountID to backfill (repeatable) |

---

## add-inline

Register an [Inline account](usage/inline.md) and archive all of its accessible
chats by default. To restrict capture to specific chats, repeat `--chat-id`.
OAuth MCP is the default transport; pass `--transport cli` to use an
authenticated Inline CLI on the daemon host.

```bash
msgvault add-inline
msgvault add-inline --transport cli --cli-path inline
msgvault add-inline --chat-id 123 --chat-id 456
```

Setup verifies the signed-in user and capture scope, then writes the canonical
account identifier, transport, and optional filter to `[[inline.accounts]]`.
Re-running with IDs merges them into an existing filter or adds a filter to an
account that previously captured all chats. Re-running without IDs restores
all-chat capture. If you remove IDs while keeping a nonempty filter in
configuration, future capture stops for those chats while their archived records
stay intact; an empty filter restores all accessible chats.

Changing the transport keeps the same source identity. Every `add-inline` run
defaults to MCP. Repeat `--transport cli` whenever you update a CLI-backed
account.

OAuth requests `messages:read` and `offline_access` with no message-write
scopes, and saves credentials in private files on the daemon host. The CLI
transport uses Inline's own authentication store. In remote mode, OAuth consent
runs on the machine where you invoke the command and hands credentials to the
configured daemon for verification and storage. For the CLI transport, the
executable and its login must live on the daemon host; see the
[connection guide](usage/inline.md#connect-an-account).

| Flag | Default | Description |
|---|---|---|
| `--chat-id` | all accessible chats | Numeric Inline chat ID for an exact filter (repeatable); children must be selected separately |
| `--transport` | `mcp` | `mcp` for browser OAuth or `cli` for the authenticated Inline executable |
| `--cli-path` | `inline` | Executable path for the CLI transport, resolved on the daemon host |
| `--no-default-identity` | `false` | Do not auto-confirm the verified Inline account as this source's "me" identity |

After you add an account, run `msgvault sync-inline --probe` and
`msgvault sync-inline`. Restart the daemon after changing scheduled accounts or
chat selections.

---

## sync-inline

Archive all accessible chats for every registered Inline account, or for one
canonical account identifier, applying any explicit chat filter. All-chat mode
includes hidden and archived chats as well as accessible child threads. The
first run backfills currently readable history and can resume after an
interruption, while later runs fetch new messages and pick up newly accessible
chats.

All-chat mode needs the complete-catalog backend along with the matching MCP
server or CLI. Deploy the backend first, because an older backend can ignore the
subthread request without indicating that mismatch in the response. Unsupported
CLI commands and malformed MCP pagination still fail; see
[catalog support](usage/inline.md#check-all-chat-catalog-support).

```bash
msgvault sync-inline
msgvault sync-inline 'api.inline.chat:user:42'
msgvault sync-inline --full
msgvault sync-inline --probe
```

| Flag | Default | Description |
|---|---|---|
| `--full` | `false` | Re-fetch available history within the account's capture scope and refresh existing messages |
| `--probe` | `false` | Check identity and read contracts, reporting catalog shape and counts without importing or printing sampled message text |
| `--no-media` | `false` | Defer attachment downloads while keeping message and media metadata |
| `--limit` | `0` | Total messages of work per account this run; `0` is unlimited, and unfinished history or refresh work resumes later |
| `--build-cache` | `false` | Force the normal analytics cache refresh after this sync |
| `--no-build-cache` | `false` | Skip analytics cache refresh after this sync |

You cannot combine `--probe` with `--full`, `--no-media`, or a nonzero
`--limit`, and the cache override flags are mutually exclusive. Limited runs
resume at the next chat in your selection so a busy earlier chat cannot starve
later chats. Unfinished older history runs first during limited capture, and new
arrivals scan after that backlog finishes.

To pick up older edits, pass `--full`. A source deletion does not erase captured
text or downloaded media. Content deleted before capture and historical edit
versions are unavailable; see [capture limits](usage/inline.md#sync-and-refresh).
See [Inline configuration](configuration.md#inline) to schedule ongoing sync.

---

## backfill-inline-media

Retry eligible missing media for every registered Inline account, or for one
canonical account identifier. The command refreshes signed source URLs and
reuses content-addressed bytes already stored, while size, scope, account, and
participant policies still apply.

```bash
msgvault backfill-inline-media
msgvault backfill-inline-media 'api.inline.chat:user:42'
```

You can also pass the mutually exclusive `--build-cache` and `--no-build-cache`
flags described under `sync-inline`.

An unknown group participant count fails a positive participant cap, and message
and attachment metadata stay archived when media is skipped. See
[Inline media downloads](usage/inline.md#media-downloads) for the explicit
policy choice and source-availability limits.

---

## add-slack

Register a [Slack workspace](/docs/usage/slack/) as a `slack` source. Use a
user token (`xoxp-…`) from an internal Slack app you create, or a browser
token (`xoxc-…`) together with its raw `d` cookie. See the usage guide for
permission choices and [browser-session limits](/docs/usage/slack/#browser-session).

The command validates the credentials with `auth.test` and saves them in a
protected file at `tokens/slack_<team-id>_<user-id>.json`. Browser sessions
archive your memberships and DMs. Public-channel-only app tokens need no search,
file, or reaction permissions, and sync revisits history for thread replies
when `search:read` is absent.

```bash
msgvault add-slack
msgvault add-slack --token-file ~/slack-token.txt
MSGVAULT_SLACK_TOKEN="xoxp-..." msgvault add-slack
```

| Flag | Default | Description |
|---|---|---|
| `--token-file` | | Read the user token from a file instead of prompting |
| `--cookie-file` | | Read the raw `d` cookie for an `xoxc` browser token |
| `--no-default-identity` | `false` | Do not auto-confirm the workspace user ID as the source's "me" identity |

After you add a workspace, sync it with `msgvault sync-slack`.

---

## import-slackdump

Import a Slackdump directory or ZIP without contacting Slack. `--me` matches
your account in `users.json` by exact Slack user ID or unique profile email, and
the import keeps conversations, threads, reactions, raw JSON, identities, and
exported files. The path must live on the daemon host; msgvault does not upload
exports through a configured remote. See
[Slack](/docs/usage/slack/#import-a-slackdump-export).

```bash
msgvault import-slackdump --me you@example.com /path/to/export
msgvault import-slackdump --me U0123456789 --limit 100 /path/to/export.zip
```

| Flag | Default | Description |
|---|---|---|
| `--me` | required | Slack user ID or unique profile email in the export |
| `--limit` | `0` | Max messages imported per conversation (0 = no limit) |
| `--max-media-mb` | configured/default limit | Max exported file size in MiB (0 = configured/default limit) |
| `--no-default-identity` | `false` | Do not auto-confirm the workspace user ID as the source's "me" identity |

---

## sync-slack

Sync Slack conversations for registered workspaces. Public-channel-only tokens
can archive every public channel, including channels you have not joined, while
broader tokens archive your channel memberships, group DMs, and 1:1 DMs. The
first run backfills full history and can resume after an interruption. Later
runs are incremental and sweep for thread replies posted since the last run,
regardless of thread age. If one workspace fails, the remaining workspaces still
sync and the command exits nonzero listing the failures. The `[slack]`
`channels` and `exclude_channels` filters pick which channels sync, and
`private_channels`, `dms`, and `group_dms` independently control whether those
conversation types sync. See [Slack](/docs/usage/slack/).

```bash
msgvault sync-slack
msgvault sync-slack T0123456789
msgvault sync-slack --full
msgvault sync-slack --private-channels=false --dms=false --group-dms=false
```

| Flag | Default | Description |
|---|---|---|
| `--limit` | `0` | Max messages of work per conversation this run, thread replies included; the reply sweep gets the same budget workspace-wide (0 = no limit; every phase resumes next run so standing limited schedules converge; only the maintenance rescan is skipped) |
| `--dms` | configured | Include one-to-one DMs for this run (`true` or `false`) |
| `--full` | `false` | Start (or continue) a repair session: re-fetch every message, upserting in place (catches old thread replies and edits). Interrupted or --limit-scoped repairs resume across later runs of any kind until complete |
| `--group-dms` | configured | Include group DMs for this run (`true` or `false`) |
| `--private-channels` | configured | Include private channels for this run (`true` or `false`) |
| `--no-threads` | `false` | Skip thread-reply fetching for this run (a later threaded run pays the debt automatically) |
| `--maintenance` | `false` | Repair edits/reaction changes on recent messages (ignored by default after capture) |
| `--no-media` | `false` | Skip file downloads for this run (files become pending markers; `backfill-slack-media` fetches them later) |

When a file download fails, the summary prints the error count and the first
media error, including the Slack file ID. Each failed attachment keeps its
reason in `attachments.attachment_metadata.fetch_error`, and the sync run item
also records the error.

---

## backfill-slack-media

Retry pending Slack file downloads that failed or exceeded the size cap during
`sync-slack`. Because files are content-addressed, running this command again is
safe and never re-fetches files already downloaded. Files hosted outside
`files.slack.com` are saved as metadata-only link rows and are never downloaded.

```bash
msgvault backfill-slack-media
msgvault backfill-slack-media T0123456789
```

When downloads fail, the summary prints the error count and first media error,
and failed files stay eligible for retry. A successful retry clears the previous
failure detail from the attachment row.

---

## add-calendar

Authorize read-only Google Calendar access for an account and register its
calendars for sync. If the account already has a Gmail token, re-consent bundles
Gmail and Calendar so Gmail access is not dropped; keep both checked on the
consent screen. You must enable the Calendar API on the OAuth project. By
default, msgvault registers only owned or writable calendars.

```bash
msgvault add-calendar <email> [flags]
```

| Flag | Description |
|---|---|
| `--write` | Also request `calendar.events`, preserving existing Google scopes; source write permissions remain required |
| `--oauth-app` | Named OAuth app to use |
| `--headless` | Print token-copy instructions for a headless host instead of opening a browser |
| `--all-calendars` | Include reader/freeBusyReader (subscribed, holiday) calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--calendars` | Comma-separated calendar IDs to register |

---

## sync-calendar

Sync Google Calendar events for an account. msgvault matches the argument
against a `[[gcal]]` config entry by name or email, or treats it directly as an
email address. The first run (or `--full`) runs a full sync that registers
calendars, while later runs sync incrementally with the Calendar `syncToken`.
Events are stored as searchable records (`message_type = calendar_event`) and
become eligible for semantic search when the embedding worker runs. Cancelled
events stay in the archive marked cancelled and are never deleted. Sync is
read-only.

```bash
msgvault sync-calendar <name|email> [flags]
```

| Flag | Description |
|---|---|
| `--full` | Force a full sync (ignore stored sync tokens) |
| `--limit` | Max events per calendar (0 = unlimited) |
| `--after` / `--before` | Bound a full sync to a date range (`YYYY-MM-DD`); full sync only |
| `--calendar` | Restrict to specific calendar IDs |
| `--all-calendars` | Include reader/freeBusyReader calendars |
| `--min-access-role` | Minimum access role: `owner`, `writer`, or `reader` |
| `--oauth-app` | Named OAuth app to use |
| `--noresume` | Do not resume an interrupted full sync |

---

## calendar

List archived calendar events or control live Google Calendar events through the
daemon.

```bash
msgvault calendar events --from 2027-03-01 --to 2027-04-01
msgvault calendar events --from 2027-03-01 --to 2027-04-01 --json
```

`calendar events` expands repeating events, places edited occurrences at their
new times, and marks cancellations. Plain dates use midnight in your machine's
time zone, while RFC3339 bounds keep their explicit offset. `--to` is exclusive
and must come after `--from`. Timed rows print in your machine's time zone,
all-day rows print dates, and the `END (EXCL)` column prints the exclusive end.
Each row ID points to the effective archive message, including an edited or
cancelled copy. JSON carries the series master ID separately in
`series_message_id`. JSON also includes status, attendees, roles, RSVPs, and
events that could not be expanded. If the query is too broad, narrow the date window.
This command rejects `--account`, `--dry-run`, `--read-only`, and
`--send-updates`.

For live Google Calendar changes, see
[Calendar event setup](usage/calendar.md#control-events) for consent and source
permissions. You can pass `primary`, a calendar ID, or an alias from
`config.toml` as the calendar ID.

| Command | Purpose |
|---|---|
| `calendar create` | Create an event on a calendar |
| `calendar update` | Update an event's fields, guests, or recurrence |
| `calendar delete` | Delete an event or recurring occurrences |
| `calendar move` | Move an event to another calendar |
| `calendar respond` | Set your RSVP for an event |
| `calendar freebusy` | Show busy time ranges for calendars |
| `calendar conflicts` | Find overlapping events in a time range |

```bash
msgvault calendar create <calendar-id> --account <name|email> --summary <title> --from <start> --to <end>
msgvault calendar update <calendar-id> <event-id> --account <name|email> [field flags]
msgvault calendar delete <calendar-id> <event-id> --account <name|email>
msgvault calendar move <calendar-id> <event-id> <destination-calendar-id> --account <name|email>
msgvault calendar respond <calendar-id> <event-id> --account <name|email> --status accepted
msgvault calendar freebusy <calendar-id> --account <name|email> --from <start> --to <end>
msgvault calendar conflicts <calendar-id> --account <name|email> --from <start> --to <end> --calendars <ids>
```

| Flag | Applies to | Contract |
|---|---|---|
| `--account` | Live commands | Required OAuth account or configured source name, separate from the target calendar |
| `--dry-run` | Live commands | Verify live access and return the proposed writes without applying them; availability still reads Google |
| `--read-only` | Live commands | Reject all mutations |
| `--json` | All | Print complete results and archive receipts; dry runs and availability always print JSON |
| `--send-updates` | Writes | `none` (default), `all`, or `externalOnly` |
| `--summary`, `--description`, `--location` | Create/update | Omitted update fields are preserved; explicit empty clears |
| `--from`, `--to` | Create/update/availability | RFC3339 with offset, or local `YYYY-MM-DDTHH:MM` with `--tz` |
| `--tz` | Create/update/availability | IANA time zone; event edits require a time bound |
| `--all-day` | Create/update | Date-only `YYYY-MM-DD`; end is exclusive |
| `--attendees` | Create/update | Replace comma-separated guest list; explicit empty clears |
| `--add-attendee` | Update | Add guest emails while preserving existing guests and RSVP state; cannot combine with `--attendees` |
| `--rrule` | Create/update | Repeatable RRULE; explicit empty clears recurrence |
| `--reminder` | Create/update | Repeatable `popup:minutes` or `email:minutes` (0–40320, at most five); `default` or `none` cannot combine with overrides |
| `--scope` | Update/delete/respond | `single` (default), `all`, or `future`; future supports update/delete only |
| `--original-start` | Update/delete/respond | Original occurrence start, RFC3339 or all-day date; required for single on a series ID and future edits |
| `--destination` | Move | Required calendar ID or configured alias; standalone events only |
| `--status` | Respond | Required self RSVP: `accepted`, `declined`, or `tentative` |
| `--calendars` | Availability | Selected IDs or aliases; default is the positional calendar; at most 50 |

Commands return completed Google writes along with their archive message IDs. If
a remote write partly fails or archiving fails, the command prints the receipts
before exiting with an error; reconcile the reported event instead of repeating a
completed write.

---

## import-ics

Import a local iCalendar file as separate calendar event
messages:

```bash
msgvault import-ics calendar.ics --identifier personal-calendar
```

Pick a stable `--identifier` for each calendar. Each `VEVENT` becomes its own
event keyed by UID and original recurrence identity, keeping its own `VEVENT`
and the time zone definitions it uses; a single-`VEVENT` file stays
byte-identical. Events missing from a later file stay archived with their
previous status. Re-importing updates events by letting a higher `SEQUENCE` win,
breaking equal sequences with `DTSTAMP`, and letting a cancellation win an exact
tie. As with email, msgvault converts text from other charsets to UTF-8. A
series with unsupported recurrence stays archived and shows up as unexpandable
in `calendar events`. For an unsupported range edit, results can include earlier
occurrences while omitting later ones, and an edit that cannot be placed can
omit the series. The import summary prints how many events updated and how many
did not. Floating times use the calendar's `X-WR-TIMEZONE` when present, or UTC
otherwise. The daemon reads the file locally, and importing files through a
remote daemon is unavailable.

Files with `METHOD:ADD` are rejected as unsupported.

A cancellation keeps the event's last accepted title, times, participants, and
raw iCalendar data, marking it cancelled without erasing those details.

### Invitations in email

Archiving email extracts decoded `text/calendar` and `application/ics` MIME
parts into event messages under the same source. msgvault ignores attached
forwarded emails as well as iTIP messages using the `REPLY`, `REFRESH`,
`COUNTER`, or `DECLINECOUNTER` method. `ADD` is unsupported; msgvault keeps the
email and logs a warning. Attendee RSVPs come from the invitation snapshot,
while Google Calendar sync supplies provider-reported RSVPs.

An invitation, its updates, and its cancellation share one event. Each
contributing email keeps its full MIME and attachments, and the event survives
if you delete any of them. Invalid calendar parts leave the email intact and log
a warning with its message ID. If event extraction fails after the email is
archived, run `repair-derived` to retry it.

An email and its extracted event count as two archive records in account
listings and statistics, while provider verification counts the email once.
Hiding the email keeps the event, and removing its account removes both.

The next email re-derivation extracts invites from archived MIME automatically
after a sync or import. To run it on demand from stored MIME without a provider
connection, use [`repair-derived`](#repair-derived):

```bash
msgvault repair-derived --source-type mbox
```

## import-eml

Import RFC 5322 `.eml` files inside MailMate-style `.mailbox` directories. The
importer does not pick up loose `.eml` files outside that layout, derives
mailbox labels from the directory structure, and resumes an active checkpoint by
default.

```bash
msgvault import-eml /path/to/mail --identifier user@example.com
```

| Flag | Default | Description |
|---|---|---|
| `--identifier` | required | Source identifier for imported messages |
| `--source-type` | `eml` | Source type stored for this import |
| `--no-resume` | `false` | Start a new import rather than resume its checkpoint |
| `--checkpoint-interval` | `200` | Messages between saved checkpoints |
| `--no-attachments` | `false` | Do not write attachment files |
| `--no-default-identity` | `false` | Do not confirm the identifier as this source's identity |

See [local email import](usage/importing.md) for file discovery and repeat-import
behavior, and [remote images](usage/remote-images.md) for the separate opt-in
that can cause network requests during email imports.

---

## import-mbox

Import a local MBOX archive into msgvault.

```bash
msgvault import-mbox <identifier> <export-file>
```

The export file can be a plain mbox file with any extension, or a `.zip` archive
containing one or more `.mbox` or `.mbx` files.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `mbox` | Source type recorded in database (e.g., `hey` or `google-groups`) |
| `--label` | — | Label(s) to apply to imported messages (repeatable, or comma-separated) |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

For Google Groups Takeout, pass `--source-type google-groups` to keep Groups
thread IDs and labels and skip auto-confirming the group identifier as "me".
Import a Groups-only ZIP or an extracted group MBOX; non-message files are
skipped.

See [Importing Local Email](/docs/usage/importing/) for usage examples.

---

## import-maildir

Import a Maildir archive without modifying its files:

```bash
msgvault import-maildir ~/Maildir --identifier you@example.com
```

The importer reads regular message files in `cur` and `new` and skips `tmp`,
symlinks, and mailbox bookkeeping files. The root mailbox gets the `INBOX`
label, while nested directories and Maildir++ folders such as `.Projects.Go`
become slash-separated labels (`Projects/Go`). Point the command at a stable
snapshot of the mailbox so delivery and flag renames cannot race with the
import.

msgvault stores raw MIME, bodies, recipients, and attachments. As with other
local email imports, attachment-write failures are logged; the original
attachment bytes stay in raw MIME, though rerunning does not retry those writes.
Exact duplicate raw messages in the same source are stored once and gather all
folder and flag labels. Rerunning adds missing messages and labels without
removing archived messages or labels, and moving a message from `new` to `cur`
or changing filename flags does not create another copy.

Maildir flags map to `DRAFT`, `STARRED`, `PASSED`, `REPLIED`, and `TRASH`
labels. Messages without the seen (`S`) flag get `UNREAD`, and trashed messages
are still archived.

| Flag | Default | Description |
|---|---|---|
| `--identifier` | — | Required account identifier |
| `--source-type` | `maildir` | Source type stored for the archive |
| `--no-resume` | `false` | Start a fresh run instead of resuming a checkpoint |
| `--checkpoint-interval` | `200` | Save progress after this many messages |
| `--no-attachments` | `false` | Skip writing attachment files |
| `--no-default-identity` | `false` | Skip confirming the identifier as the source identity |

Interrupted imports resume by rescanning the tree and skipping stored messages.
Messages larger than 128 MiB fail with an error and stay unimported.

---

## import-emlx

Import Apple Mail `.emlx` files into msgvault. You can auto-discover accounts
from macOS `Accounts4.sqlite` or pass explicit arguments.

```bash
# Auto-discover accounts (reads ~/Library/Accounts/Accounts4.sqlite)
msgvault import-emlx

# Specify mail directory
msgvault import-emlx <mail-dir>

# Legacy form: explicit identifier and directory
msgvault import-emlx <identifier> <mail-dir>
```

Pass an Apple Mail mailbox tree containing `.mbox` or `.imapmbox` directories
with `Messages/` subdirectories of `.emlx` files, or point directly at one
`.mbox` directory. Labels come from directory names.

If the import succeeds but its analytics cache refresh fails, your imported
messages stay saved and searchable. The command exits nonzero and states in the
error that the import succeeded. Follow the cache error's
[builder-limit guidance](configuration.md#analytics), then run
`msgvault build-cache` to refresh analytics without repeating the import.

The importer also handles Apple Mail's `N.partial.emlx` files, whose attachments
live outside the MIME payload in a sibling `Attachments/N/` directory. It
restores cached attachments directly inside the message's outer multipart, up to
the message size limit. Attachments inside nested parts, such as some forwarded
messages, are not restored, and attachments without a cached file stay absent.
An unreadable cached file or directory counts as an error and leaves the `.emlx`
file to retry on the next import. Re-importing a partial message adds newly
cached attachments to the existing message without creating a duplicate, and
skips ingestion when the archived message already holds every cached attachment.
If both `N.emlx` and `N.partial.emlx` exist, the complete `N.emlx` copy wins.
The summary prints how many partial files were read and how many attachments the
run added.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `apple-mail` | Source type recorded in database |
| `--account` | — | Filter to specific account(s) during auto-discover (repeatable) |
| `--accounts-db` | — | Custom path to macOS `Accounts4.sqlite` |
| `--identifier` | — | Manual identifier when auto-discover is not suitable |
| `--no-resume` | `false` | Report totals for this run only, without an interrupted run's counts; completed file receipts still apply |
| `--full-reconcile` | `false` | Invalidate this root's receipts and reconcile every discovered file |
| `--max-message-bytes` | `134217728` (128 MiB) | Maximum EMLX file and merged MIME size in bytes, including MIME encoding; explicit values must be 1–9223372036854775806 |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |
| `--no-default-identity` | `false` | Do not auto-confirm the identifier as this source's "me" identity |

Each completed `.emlx` file receives a receipt. Repeat imports still walk every
mailbox directory and check file metadata, including cached attachments, then
skip reading files whose metadata has not changed and whose message is complete,
reporting them as `Unchanged`. New files import regardless of sent date, a
message found in another mailbox keeps its existing labels, and removing files
from Apple Mail's local cache never deletes archived messages.

A file counts as complete once its message, its attachments under the current
`--no-attachments` setting, and its full-text index entry are stored. Skipping
reads requires Linux or macOS file metadata and a path with no symbolic links;
other files are read on every run. Turning on remote-image archiving also
disables skipping. Receipts trust file metadata without inspecting archived
content. Run `--full-reconcile` if you suspect stale state or edited archived
messages, attachment files, or the search index directly; it re-imports every
discovered file, including files whose content matches the archive.

Problems with one file do not stop the import. An unreadable or oversized file,
an unreadable cached attachment, a merge over the byte limit, or a failed
attachment or search-index write counts as an error. That file stays pending for
the next import while the run finishes and links replies. Database errors fail
the run. If a message's MIME structure cannot be parsed, msgvault archives its
raw bytes with any recoverable headers and does not retry it. Empty files with
no message content are skipped with a warning.

If restoring attachments would exceed the merged message limit, the importer
keeps previously archived attachments and leaves the file pending. Retry with a
higher `--max-message-bytes` value that fits your available memory. This limit
applies in both automatic and manual account modes and affects only the run that
sets it.

See [Importing Local Email](/docs/usage/importing/) for usage examples.

---

## import-pst

Import a Microsoft Outlook PST archive into msgvault.

```bash
msgvault import-pst <identifier> <pst-file>
```

The importer preserves PST folder structure as labels, imports email messages,
and skips non-email items such as calendar entries, contacts, tasks, and notes.

| Flag | Default | Description |
|---|---|---|
| `--source-type` | `pst` | Source type recorded in database |
| `--skip-folder` | — | Folder name to skip, case-insensitive; repeat for multiple folders |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |
| `--no-attachments` | `false` | Skip writing attachments to disk |

See [Importing Local Email](/docs/usage/importing/) for usage examples.

---

## import-whatsapp

Import messages from a decrypted Android `msgstore.db` or Apple
`ChatStorage.sqlite` WhatsApp database. Apple imports include text and URL
messages, participants, and available names. They link text and URL replies to
quoted messages archived from the same Apple source, including messages imported
in the same run. Native Apple attachments are skipped.

```bash
msgvault import-whatsapp <msgstore.db> --phone <your-number>
```

You must pass `--phone` in E.164 format, for example `+447700900000`.

| Flag | Required | Description |
|---|---|---|
| `--phone` | Yes | Your phone number in E.164 format (must start with `+`) |
| `--contacts` | No | Path to contacts `.vcf` file for name resolution |
| `--media-dir` | No | Path to decrypted Media folder for attachments |
| `--limit` | No | Limit number of messages (for testing) |
| `--after` | No | Apple only: import messages on or after this date (`YYYY-MM-DD`, local time) |
| `--before` | No | Apple only: import messages before this date (`YYYY-MM-DD`, local time) |
| `--display-name` | No | Display name for the phone owner |
| `--full` | No | Apple only: compare every message, including chats unchanged since the last import |
| `--no-default-identity` | No | Do not auto-confirm the phone number as this source's "me" identity |

Android `msgstore.db` imports return an error if you set `--after` or
`--before`.

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-imessage

Import messages from the local iMessage database on macOS. You need Full Disk
Access enabled in **System Settings**.

```bash
msgvault import-imessage
```

By default, the command reads `~/Library/Messages/chat.db` in read-only mode.
Eligible attachments referenced by `chat.db` are copied into the archive. See
[Text Messages](/docs/usage/text-messages/#import-imessage) for limits, copied
trees, and voice notes.

| Flag | Default | Description |
|---|---|---|
| `--db-path` | `~/Library/Messages/chat.db` | Path to chat.db |
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--me` | — | Your phone/email for recipient tracking |
| `--contacts` | — | Path to contacts `.vcf` file for display-name backfill |
| `--messages-root` | `~/Library/Messages` | Messages folder to read attachments from; rebase copied Mac paths here |
| `--no-attachments` | `false` | Do not copy attachment files (attachment metadata is still recorded) |
| `--max-media-mb` | `0` | Skip files larger than this many MiB; `0` uses `max_media_mb` from `[imessage]` (default 250) |

The participant limit and the default size cap come from `[imessage]` in
[Configuration](/docs/configuration/#imessage).

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-imazing-csv

Import iMessage and SMS history from an iMazing Messages CSV export. Pass the
export root containing `csv/` and optional `attachments/`, or pass its `csv/`
directory directly.

```bash
msgvault import-imazing-csv ~/Downloads/messages-export \
  --me +14155550100 --timezone America/Los_Angeles
```

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your phone number or email address |
| `--timezone` | local IANA zone (required on Windows) | Timezone used for dates that do not include an offset |
| `--contacts` | — | vCard file used to fill empty participant names |

The importer accepts comma-, tab-, and semicolon-delimited CSV files with named
iMazing headers and produces deterministic results across reruns. Available
referenced files are stored up to 100 MiB each. Larger files are recorded as
skipped; missing files and ambiguous filenames remain visible as missing
attachment occurrences. None of these outcomes fail the import or block contact
enrichment, and a later rerun can fill in missing files. Reply links are added
only when the exported reply text identifies exactly one earlier message.

CSV exports do not share stable message IDs with `chat.db`; importing the same
history through both commands can create cross-source duplicates.

See [Text Messages](/docs/usage/text-messages/#import-imazing-csv) for the full
format and rerun behavior.

---

## import-gvoice

Import texts, calls, and voicemails from a Google Voice Takeout export.

```bash
msgvault import-gvoice <takeout-voice-dir>
```

Pass the `Voice` folder from a Google Takeout export; that folder contains
`Calls/` and `Phones.vcf`.

| Flag | Default | Description |
|---|---|---|
| `--before` | — | Only messages before this date (YYYY-MM-DD) |
| `--after` | — | Only messages after this date (YYYY-MM-DD) |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-default-identity` | `false` | Do not auto-confirm the phone number as this source's "me" identity |

See [Google Voice imports](usage/text-messages.md#import-gvoice) for voicemail audio behavior and usage examples.

---

## import-discord-package

Import an extracted official Discord
sent-message data package on the daemon host:

```bash
msgvault import-discord-package <package-dir>
```

| Flag | Description |
|------|-------------|
| `--no-default-identity` | Skip assigning the exporting account to the default identity |

This importer reads only the exporting account's sent messages, including DMs.
Legacy CSV transcripts are supported, and JSON transcripts are experimental.
Attachment URLs are stored as links. Re-running is safe and resumes an
interrupted import. See
[Discord packages](usage/discord.md#import-a-personal-data-package) for
requirements and limits.

## import-linkedin

Import a LinkedIn member data export ZIP or extracted folder:

```bash
msgvault import-linkedin <identifier> <archive> --me <profile-url>
```

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your LinkedIn member profile URL |
| `--display-name` | | Your display name for posts and comments |
| `--include-drafts` | `false` | Include unsent message drafts |
| `--json` | `false` | Print the import summary as JSON |

This importer archives DMs along with your own posts and comments, while
`Connections.csv` adds people without creating messages. Attachment URLs are
stored as links. Reuse the same identifier when importing newer exports. See
[LinkedIn import](usage/linkedin.md) for supported files and limits.

## import-telegram

Import a Telegram Desktop machine-readable JSON export along with available
media.

```bash
msgvault import-telegram <export-dir>
```

| Flag | Default | Description |
|---|---|---|
| `--me` | Export owner | Numeric Telegram user ID; required for single-chat exports |
| `--limit` | `0` | Maximum messages added; `0` imports all messages |
| `--no-resume` | `false` | Scan every chat, ignoring the resume checkpoint |
| `--no-default-identity` | `false` | Skip automatic account identity creation |

Re-importing adds new messages and fills in missing media while keeping archived
text and edit flags unchanged. See
[Telegram imports](usage/text-messages.md#import-telegram) for export and resume
behavior.

---

## import-messenger

Import Facebook Messenger conversations from a Download Your Information export.

```bash
msgvault import-messenger --me <you@facebook.messenger> <dyi-export-dir>
```

| Flag | Default | Description |
|---|---|---|
| `--me` | (required) | Your synthetic Messenger identifier, e.g. `test.user@facebook.messenger` |
| `--format` | `auto` | Export format: `auto`, `json`, `html`, or `both` |
| `--limit` | `0` | Limit number of messages (for testing) |
| `--no-resume` | `false` | Start fresh, ignoring interrupted progress |
| `--checkpoint-interval` | `200` | Save progress every N messages |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## import-synctech-sms

Import SMS Backup & Restore XML or ZIP backups.

```bash
msgvault import-synctech-sms <path> --owner-phone <your-number>
```

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--sms` | `true` | Import SMS records |
| `--mms` | `true` | Import MMS records |
| `--calls` | `true` | Import call logs |
| `--attachments` | `true` | Import MMS attachments |

See [Text Messages](/docs/usage/text-messages/) for usage examples.

---

## add-synctech-sms-drive

Configure a Google Drive source for SMS Backup & Restore backups.

```bash
msgvault add-synctech-sms-drive <name> --owner-phone <number> --folder-id <id> --google-account <email>
```

| Flag | Default | Description |
|---|---|---|
| `--owner-phone` | (required) | Your phone number in E.164 format |
| `--folder-id` | (required) | Google Drive folder ID containing backups |
| `--google-account` | (required) | Google account used for Drive access |
| `--schedule` | `30 4 * * *` | Cron schedule used by `msgvault serve` |
| `--oauth-app` | — | Named Google OAuth app to use |

---

## sync-synctech-sms

Run one configured SMS Backup & Restore source immediately.

```bash
msgvault sync-synctech-sms <name>
```

---

## backup

Create, list, verify, and restore incremental archive snapshots in a backup
repository.

```bash
msgvault backup init --repo ~/Backups/msgvault
msgvault backup create --repo ~/Backups/msgvault
msgvault backup list --repo ~/Backups/msgvault
msgvault backup verify --repo ~/Backups/msgvault
msgvault backup restore --target ~/msgvault-restored --repo ~/Backups/msgvault
```

Every backup subcommand needs a repository path. Pass `--repo`, or configure
`[backup].repo` in `config.toml` so you can omit the flag. Running `backup init`
initializes the repository directory without modifying `config.toml`.
`backup create` routes through the selected daemon so the daemon can freeze a
consistent SQLite snapshot while scanning pages and attachments. Because
`backup verify` and `backup restore` do not write to the live archive, they run
locally against the repository.

### backup init

```bash
msgvault backup init --repo <dir>
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |

### backup create

```bash
msgvault backup create [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--include-config` | Include `config.toml` verbatim; may contain API keys |
| `--include-tokens` | Include OAuth token files |
| `--allow-plaintext-secrets` | Allow config/tokens in an unencrypted repository |
| `--tag <text>` | Optional label recorded on the snapshot manifest |
| `--force-unlock` | Break a stale exclusive repository lock before creating |
| `--jobs N` | Concurrent attachment capture workers; `0` uses one per CPU |

### backup list

```bash
msgvault backup list [--repo <dir>]
```

Prints snapshot ID, creation time, message count, bytes added, and tag.

### backup verify

```bash
msgvault backup verify [snapshot] [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--all` | Verify every snapshot instead of only the latest |
| `--quick` | Skip reading and hash-verifying content blobs |
| `--force-unlock` | Break a stale exclusive repository lock before verifying |
| `--jobs N` | Concurrent pack readers; `0` uses one per CPU |

### backup restore

```bash
msgvault backup restore [snapshot] --target <dir> [flags]
```

| Flag | Description |
|---|---|
| `--repo <dir>` | Backup repository directory |
| `--target <dir>` | Directory to restore into (required) |
| `--overwrite` | Allow restoring into a non-empty target directory |
| `--permissions-only` | Retry token permissions for an explicit snapshot (Windows only); cannot be combined with `--overwrite`, `--jobs`, `--integrity-check`, or `--loose-attachments` |
| `--integrity-check` | Run SQLite's full integrity check after restoring (slow for large databases) |
| `--loose-attachments` | Restore attachments as individual loose files instead of installing compatible packs |
| `--force-unlock` | Break a stale exclusive repository lock before restoring |
| `--jobs N` | Concurrent pack readers; `0` uses one per CPU |

By default, `backup restore` installs compatible repository packs directly into
the restored attachment store. Every selected attachment is still read and
SHA-256 verified, and entries that exceed the target store's maintenance limits,
or packs that use an incompatible representation, are restored as loose files
instead. The summary prints the resulting packed and loose split along with any
fallback reasons. Every restore verifies database pages and content blobs by
hash and compares the restored database statistics with the snapshot manifest.
Pass `--integrity-check` to also run SQLite's full `PRAGMA integrity_check`
scan; it is optional because it can dominate restore time on large databases.

Use `--loose-attachments` when downgrading or recovering. Restore into a fresh
target directory with `--loose-attachments` to guarantee a fully loose result. Because an overwritten
target can retain uncataloged old pack files and `unpack-attachments` processes
only cataloged packs, overwriting cannot currently make that guarantee.
Restoring into the live archive home of a running daemon is refused. See
[Backup](/docs/usage/backup/) for repository format, scheduling, verification,
and privacy details.

---

## pack-attachments

Move every eligible loose content-addressed attachment into sealed immutable
pack files:

```bash
msgvault pack-attachments
```

The daemon serializes packing against sync and backup operations. Reads stay
available from loose, packed, or mixed storage, and the command is safe to rerun
as new loose content arrives. Attachment-producing commands sent to the daemon
defer packing, including after a partial failure, and scheduled syncs also defer
packing when they write new loose blobs. The daemon's `attachment-pack` job runs
every 6 hours to pack them, and the daily `attachment-maintenance` job packs and
repacks. Automatic maintenance errors show up in scheduled job status. Each
automatic pack pass has a 256 MiB soft raw-byte budget and finishes its current
blob before stopping for that limit. Scheduled packing and daily attachment
maintenance also stop after one minute so queued scheduled work can run. Each
scheduled pack pass verifies at most 128 indexed blobs with canonical loose
copies or missing packs, or 32 MiB of their raw content, allowing one oversized
blob. Its durable hash cursor resumes verification after a restart, while
pending passes and the daily job pack new loose blobs when nothing is queued;
verification-only follow-ups skip them. Manual unpacking reads the full catalog.
Cancellation keeps committed packs. A pass with committed progress resumes
behind waiting work, while runtime expiry without a checkpoint records an error
and waits for the next trigger. The first pack tick after a restart also checks
for blobs left by the previous daemon. Running `pack-attachments` processes the
complete eligible backlog right away.

If you set `[data].loose_attachments = true`, automatic packing turns off and
this command refuses to run.

---

## repack-attachments

Reclaim dead bytes from sparse attachment packs:

```bash
msgvault repack-attachments
```

Repack always runs through the selected daemon so it can atomically replace live
blob mappings, retire shared readers, and remove old pack files. You can safely
retry after an interruption or a Windows file-sharing error. Repack refuses to
run when `[data].loose_attachments = true` because repacking creates replacement
pack files.

---

## unpack-attachments

Restore every cataloged packed attachment to a loose file and remove its pack:

```bash
msgvault daemon stop
msgvault unpack-attachments
```

msgvault verifies each object's SHA-256 hash as it writes the loose file. Use
this command as your downgrade and recovery escape hatch because msgvault
versions before packed attachment support cannot read packs. It runs locally
only and refuses to run while a daemon holds pack readers open. If you
configured `[remote]`, run it on the archive host or pass `--local` there to
select that host's local archive.

---

## find-chat

Find archived chats when a person's name differs across networks.

```bash
msgvault find-chat "Jordan Lee Chen"
msgvault find-chat "Jordan Lee Chen" --json
msgvault find-chat "Lee Chen" --source-id 2 --limit 50
```

| Flag | Default | Description |
|---|---|---|
| `--json` | false | Return `results` and `has_more` as JSON |
| `--limit` | 20 | Maximum results, from 1 to 100 |
| `--source-id` | all sources | Restrict to one positive archive source ID |

`has_more` means additional ranked candidates exist. Each request returns at most
100 results; use a more specific name token or `--source-id` to find further
candidates.

Discovery matches any whole query token in chat titles, participant names,
addresses, identifiers, current contact observations, curated person names, and
archived recipient names. Matching ignores case and normalizes equivalent
Unicode spellings. Punctuation splits tokens, and quotes group shell arguments
rather than acting as a phrase operator. Discovery does not do substring, typo,
or message-body matching. Your query must have 1 to 16 unique tokens and fit
within 256 UTF-8 bytes.

Discovery skips your own participant names and recipient aliases when a
confirmed identity or archived provider attribution identifies you on that
source, while unknown ownership stays searchable. Titles stay searchable too,
including titles of chats with yourself.

A name or title with exactly the query's normalized tokens in the same order
ranks first, followed by the most unique matching tokens within one name or
title, most
recent archived message activity, and then conversation ID. Reversed token order
still matches and is marked `partial`. Because of this, a full-name query can
return both `Jordan Lee Chen` and `Lee Chen`. Treat these as candidates to
inspect; discovery does not establish that they are the same person or create
identity links.

A match on `Lee Chen` ranks above a group where separate members match `Jordan`,
`Lee`, and `Chen`, though match evidence still lists tokens across all matching
names.

Table output prints archive conversation, message, and source IDs, network,
provider chat ID, match kind, title, and bounded matching names in `MATCHED`,
with `…` marking omitted evidence. JSON output also includes source provenance,
matched tokens, and up to eight sorted matching names, titles, or identifiers
(including the name that decided the rank), with `evidence_truncated` reporting
omitted evidence. For Beeper, the network label comes from the source's display
label and shows `unknown` when that label has no network or its trimmed suffix
equals the account ID.

Direct chats, groups, channels, and Google Chat conversations archived through
Gmail are eligible when they have archived messages. Messages deleted at the
provider stay discoverable, while deduplication-hidden messages, email threads,
and empty chats are excluded.

The archive keeps a membership index of each chat's senders and recipient
aliases so searches read chat metadata instead of scanning every message.
Imports and edits queue the chats they change, and the next search applies those
queued updates first, so the first search after a large import takes longer. A
small `--limit` bounds output, not that indexing work. Discovery never reads
message bodies. Owner credentials are required, and delegated agent grants
cannot use discovery. See the [HTTP contract](api-server.md#find-archived-chats)
for result fields.

Each result's `MESSAGE` column shows the chat's newest visible archived message.
Pass that ID to `show-message` to inspect it:

```bash
msgvault show-message 456
```

---

## search

Search the archive with Gmail-like query syntax across keyword (FTS5), semantic,
and hybrid modes.

```bash
msgvault search <query> [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Maximum number of results (default: 50) |
| `--offset N` | Skip first N results (only valid for `--mode fts`) |
| `--json` | Output results as JSON |
| `--account` | Limit results to a specific account |
| `--collection` | Limit results to all member accounts of a collection |
| `--message-type` | Limit results to one or more message types, e.g. `email`, `teams`, `discord`, `calendar_event`, `sms`, or an imported type such as `feed_item` |
| `--deletion-scope` | Source-deletion scope: `active` (default), `deleted`, or `any`. Non-active scopes require `--mode fts`. |
| `--mode` | Search mode: `fts` (default), `vector`, or `hybrid`. `vector` and `hybrid` require vector search to be configured. |
| `--explain` | Include per-signal scores (RRF, BM25, vector) in the output. Only applies to `--mode vector` and `--mode hybrid`. |
| `--snippet` | Add a bounded match-context column. |
| `--sort date` | Display the retrieved vector/hybrid page newest first; requires `--mode vector` or `--mode hybrid`. |

Both `search` and `stats` reject an explicitly empty collection with
`empty_scope` for owners and agents.

Without a message-type filter, `search` returns all matching cached message
types, including meeting transcripts and chats. Ordinary aggregate views and
statistics still default to email only; pass `--message-type` (or put
`message_type:` in your query) when you want a narrower search scope.

You can use `list:` and `list-id:` interchangeably as RFC 2919 List-Id filters.
Values match case-insensitive literal substrings; quote values with spaces. If
you repeat either alias, every value must match.

`--mode vector` and `--mode hybrid` require at least one free-text term in the
query; use `--mode fts` for filter-only queries. Because the vector index covers
active messages only, vector and hybrid modes reject `--offset` and non-active
deletion scopes. Raise `--limit` when you need a larger candidate pool. See
[Searching](/docs/usage/searching/) for the operator reference and
[Vector Search](/docs/usage/vector-search/) for semantic setup.

`filename:` matches a case-insensitive literal substring of an attachment
filename. Quote spaces; repeated values must all match attachments on the same
message. JSON output includes `snippet` (the stored preview) and
`conversation_id` in every mode,
plus `attachment_names` and `attachment_count` when available. Omitted fields
indicate unavailable metadata, while `[]` and `0` mean known empty values.
`attachment_names` lists rows currently available, and `attachment_count` can be
higher while sync is still writing them. FTS adds `match_snippet` when SQLite
can produce a query-specific excerpt. FTS keeps its bare array JSON output,
while vector and hybrid keep an object containing `results`. Every JSON result
also includes the best-effort `hidden_text_present` flag, where `true` means the
stored HTML likely hides readable text and `false` does not guarantee all text
is visible. See `show-message` below for the detector and one-time backfill.

Use `--mode hybrid` for natural-language intent paired with lexical clues, and
`--mode vector` for meaning alone. Date operators filter before ranking, while
`--sort date` sorts only the retrieved page. For example:

```bash
msgvault search 'how did we approve the budget from:sender@example.com after:2026-01-01' --mode hybrid --explain
msgvault search 'release planning newer_than:90d' --mode vector --sort date --snippet
```

When a first-page FTS search with free text returns zero results and a usable
embedding generation exists, `search` suggests hybrid mode on stderr. Non-active
source-deletion searches do not receive that suggestion, and JSON stdout stays
machine-readable.

When more full-text results exist, `search` prints a stderr hint with the next
`--offset`. Vector and hybrid text output print one hint at a time, and their
JSON objects add a `hints` array with each hint's `code` and `message`, while
full-text JSON stays a bare array. Complete pages add no hint. At the semantic
page cap, the hint asks you to raise `vector.search.max_page_size_hybrid`. When
hybrid retrieval measures messages omitted by its per-method limits, the hint
names `vector.search.k_per_signal`. If both limits apply, it names both. Raise
those settings above their reported values on the server and restart the daemon
before raising `--limit`.

Search tables show the subject, or the message snippet when the subject is
blank. Redirected or piped output keeps the full sender and subject or snippet
on one line, stripping terminal control characters and collapsing whitespace.
Terminal tables fit your terminal width by shortening sender and subject text,
including Unicode characters and emoji, while fixed IDs, dates, sizes, and
explanation scores stay complete (very narrow terminals may still wrap). Unknown
message sizes appear as `-`.

With `--json`, each result includes `web_url` when the selected daemon can
provide a browser link for that message.

The daemon checks full-text index completeness in the background. If it finds an
index gap, is rebuilding the index, or has a rebuild waiting behind other daemon
work, the CLI warns that results may be incomplete. An owner completeness check
alone prints no warning, while agent searches report checking uncertainty on
stderr. API clients can still see `index_state="checking"` while that check
runs.

---

## repair-list-ids

Re-derive archived email List-Id values from stored raw MIME without contacting
a provider.

```bash
msgvault repair-list-ids [--apply]
```

By default, the command runs in dry-run mode without modifying the archive. Pass
`--apply` to write changed values and mark derived analytics stale for the
normal rebuild path.

The analytics cache exports invalid UTF-8 List-IDs as `NULL` so mailing-list
filters cannot match unrelated messages through replacement characters. Run
`repair-list-ids --apply` to recover those values when stored raw MIME has an
intact `List-Id` header. This command cannot recover a header that is itself
damaged.

---

## repair-labels

Rebuild an IMAP source's message labels from its stored mailbox memberships
without contacting the provider.

```bash
msgvault repair-labels [identifier] [--apply]
```

An add-only label merge, used when a sync's mailbox snapshot is incomplete or a
dedup match is not fully confirmed, never removes a label. If the message's
stored membership never changes again, nothing revisits it later and a stray
label can linger. `repair-labels` rebuilds a message's labels directly from
`imap_message_memberships`, matching a full mailbox scan without needing a live
IMAP connection.

If you omit the identifier, `repair-labels` repairs every IMAP source; pass an
identifier to scope the repair to one matching source. By default, it runs as a
dry run without modifying the archive. Pass `--apply` to write repaired labels
and mark derived analytics stale for the normal rebuild path.

| Flag | Description |
|---|---|
| `--apply` | Write repaired labels to the archive. |

---

## media search

Search spoken words in recording transcripts through the daemon's configured
Docbank integration:

```bash
msgvault media search "quarterly numbers"
msgvault media search "quarterly numbers" --person 7 --direction from_person --limit 20 --json
```

`--mode lexical` is the default. `--mode semantic` and `--mode hybrid` search
recording transcripts and mirrored attachment content by meaning through
Docbank, using `recording_search_profile` for recordings and
`attachment_search_profile` for attachments (see
[semantic media search](usage/searching.md#search-spoken-words)). `--limit`
accepts 1 to 100 message occurrences. `--direction` accepts `from_person`,
`to_person`, or `group` and requires `--person`.

The recordings table prints message, conversation, and attachment IDs, supplied
or generated origin, a plain excerpt, and recorded timing. An empty recording
result rules out matches only when coverage is complete and `partial=false`. The
coverage line prints the actual recording mode, pending media, unavailable
attribution, partial coverage, and truncated results, followed by any
degradations. JSON includes the same fields plus optional `filename`,
`containing_title`, and `occurred_at`. Recording search requires Docbank's
source-selected search contract. Shared audio searches the currently selected
transcript, and differing supplied captions can leave some messages unavailable
even when no excerpt matches. A recording with more than 64 distinct current caption inputs is left out; its
occurrences count in `unavailable_occurrences` with `partial=true`. See [the API scope limits](api-server.md#media-transcript-search)
for version and source ceilings, and narrow the person scope if you hit a
ceiling. Run `msgvault show-message <message_id>` to read a result in context,
and see [browser recording search](web-ui.md#recording-matches) for transcript
matches in Messages.

`media search` also prints a separate attachment table with message,
conversation, attachment, filename, and excerpt, followed by the actual
attachment mode, coverage, partial status, degradations, truncation, and search
errors. This table searches text Docbank extracted from mirrored document files.
When more than 4,096 such files match the scope, msgvault searches them in
batches of 4,096. Results from each batch are interleaved by their position
within that batch, so ordering across batches is approximate. An empty list does
not rule out matches in files awaiting extraction. `--limit` caps each list
separately, and `--json` adds optional `attachments` and `recordings_error`
fields. Semantic and hybrid attachment search uses `attachment_search_profile`.
See
[attachment search formats](usage/searching.md#search-mirrored-attachment-text)
for the files included in the scope.

## tui

Launch the interactive terminal interface.

```bash
msgvault tui [flags]
```

| Flag | Description |
|---|---|
| `--local` | Use the local daemon instead of the configured remote server |

The daemon manages the analytics engine and cache behavior. Configure
`[analytics].engine` and `[analytics].auto_build_cache` in `config.toml` to
force live SQL, require DuckDB, or disable automatic cache builds. See
[Configuration: analytics](/docs/configuration/#analytics).

Deprecated in 0.17.0: the older TUI-only `--force-sql`, `--no-cache-build`, and
`--no-sqlite-scanner` flags are hidden and no longer affect the foreground CLI.
Set `[analytics].engine = "sql"` for live SQL, set
`[analytics].auto_build_cache = false` to skip daemon cache builds, or run
`msgvault build-cache` to prebuild cache files on the daemon host.

---

## export-messages

Export a bounded, provider-neutral message window as deterministic JSON Lines.
The command reads the archive through the selected daemon and never contacts a
provider.

```bash
msgvault export-messages \
  --start <RFC3339> \
  --end <RFC3339> \
  [--message-type <type>] \
  [--source <type:email>] \
  [--person-id <id>] \
  [--format jsonl]
```

| Flag | Default | Description |
|---|---|---|
| `--start` | required | Inclusive RFC3339 lower bound |
| `--end` | required | Exclusive RFC3339 upper bound |
| `--message-type` | all | Exact message type to include; repeatable |
| `--source` | all | `type:email` from `list-accounts --json`; repeatable |
| `--person-id` | all | Durable person whose bound participants scope the export |
| `--format` | `jsonl` | Output format; v1 accepts only `jsonl` |

Both `--start` and `--end` are required, and missing bounds fail before
contacting the daemon. Against a remote server, the export streams from the
server, requires server schema 5.19.0 or later, and works with
[read-only remote client](guides/remote-deployment.md#read-only-remote-clients)
keys. For `--source`, combine an account's `type` and `email` from
`msgvault list-accounts --json`, for example `--source mbox:you@example.com`.

Pass `--person-id N` to export messages involving a durable person's bound
participants; linked participants outside those bindings contribute no messages.
Exported authors use the sender's curated person name when available while
keeping the address unchanged.

The stream uses schema `msgvault-message-export/1` and writes one manifest, all
sources, all conversations, all messages, and one completion record with counts.
If the completion record is missing, the stream is partial and must be rejected,
and the command exits nonzero. Stdout contains JSONL only, while diagnostics go
to stderr.

See [Exporting Data](/docs/usage/exporting/) for the full record, identity,
ordering, and validation contract.

---

## export-discord

Export a bounded interval from one Discord guild using the older
`msgvault-discord-export/1` JSON envelope.

```bash
msgvault export-discord <guild-id-or-name> \
  --start 2026-01-01T00:00:00Z \
  --end 2026-02-01T00:00:00Z
```

| Flag | Description |
|---|---|
| `--start <RFC3339>` | Inclusive lower bound (required) |
| `--end <RFC3339>` | Exclusive upper bound (required) |
| `--format json` | Output format; `json` is the only supported value |

This compatibility command reads only the archive and never contacts Discord.
For new integrations, use [`export-messages`](#export-messages), which outputs
the provider-neutral `msgvault-message-export/1` JSONL schema. See the
[Discord export guide](usage/discord.md#export-a-bounded-history-window).

---

## export-eml

Export a message as a `.eml` file by passing an internal message ID or a
provider message ID. When you pass a numeric ID, msgvault checks live internal
IDs first and falls back to a provider ID if no internal ID matches. The same
lookup rule applies with `--thread`.

If a provider ID matches more than one account, the command returns an error.
Pass the numeric archive message ID instead to pick one message.

```bash
msgvault export-eml <id> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <path>` | Output file (default: `<source_message_id>.eml`, use `-` for stdout). With `--thread`, the output directory (default: current directory) |
| `--thread` | Write visible archived messages in the conversation that have stored MIME as `<n>-<source_message_id>.eml`, numbered oldest first. Reports skipped messages and when the account last synced |
| `--account <email>` | With `--thread`, the account that holds a provider message ID found in more than one account |

Thread export captures its message list once. Messages added afterward belong to
the next export. If a message disappears or loses its stored MIME before it
is read, the command reports and skips it; other read or write errors stop the
export. Hidden duplicate copies are skipped. The last-sync timestamp reflects
account freshness rather than conversation completeness.

---

## export-attachment

Export an attachment by its SHA-256 content hash.

```bash
msgvault export-attachment <content-hash> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <path>` | Output file path (use `-` for stdout) |
| `--base64` | Output raw base64 to stdout |
| `--json` | Output as JSON with base64-encoded data |

`--json`, `--base64`, and `--output` are mutually exclusive.

Stdout output, including `--json`, streams before download verification
finishes. Consume it only after the command exits cleanly. Pass
`--output <path>` to write a verified file to disk.

See [Exporting Data](/docs/usage/exporting/) for usage examples.

---

## export-attachments

Export all attachments from a message as individual files.

```bash
msgvault export-attachments <message-id> [flags]
```

| Flag | Description |
|---|---|
| `-o`, `--output <dir>` | Output directory (default: current directory) |

You can pass an internal numeric ID or a Gmail message ID. See
[Exporting Data](/docs/usage/exporting/) for usage examples.

---

## create-subset

Create a new SQLite archive containing a chosen number of your most recent
messages and the records they reference. The output directory gets its own
`msgvault.db` and works as a standalone msgvault home.

```bash
msgvault create-subset --output ./subset-vault --rows 1000
MSGVAULT_HOME=./subset-vault msgvault tui
```

| Flag | Description |
|---|---|
| `-o`, `--output <directory>` | Destination directory (required) |
| `--rows <count>` | Number of most recent messages to copy; must be positive (required) |
| `--include-identity` | Copy complete identity clusters for included participants and every confirmed address of each included account |
| `--include-attributes` | Copy current and historical person and organization attribute values |
| `--include-profiles` | Copy profiles, profile history and media, relationships, employment history, and referenced organizations |
| `--include-vcard-resources` | Copy complete native vCards and retired UID aliases; requires `--include-profiles` |

This command works on SQLite archives only. Without `--include-identity`, it
copies only the confirmed addresses used by each account's included messages,
including addresses in stored headers of mail not yet attributed, so each
message keeps its account. The optional identity, attribute, profile, and vCard
flags can include personal records that have no message in the subset; read the
command's warning before sharing the subset.

---

## export-token

Export a browser-created OAuth refresh token to a remote msgvault instance. Use
this for headless hosts such as a NAS, cloud VM, or remote server that cannot
run a browser sign-in flow.

```bash
msgvault export-token <email> [flags]
```

| Flag | Description |
|---|---|
| `--to <url>` | Remote msgvault URL (or `MSGVAULT_REMOTE_URL`) |
| `--api-key <key>` | API key (or `MSGVAULT_REMOTE_API_KEY`) |
| `--allow-insecure` | Allow HTTP for trusted networks (for example Tailscale) |
| `--upload-only` | Upload the token without registering an enabled Gmail sync account |

`export-token` reads from your configured local token backend, uploads the token
to `/api/v1/auth/token/<email>`, and saves it through the daemon's configured
token backend. Each side uses credential commands when configured, or
`~/.msgvault/tokens/<email>.json` otherwise. By default, it also posts account
metadata to `/api/v1/accounts` to enable a daily Gmail sync. Pass
`--upload-only` if you only want to store the token, such as for Calendar-only
access.

---

## diagnose-acquisition

List Gmail message download, processing, and file-saving failures for a source:

```bash
msgvault diagnose-acquisition --source-id 1
msgvault diagnose-acquisition --source-id 1 --all
```

This command reads the local database without starting the daemon or contacting
Gmail; pass `--local` if a remote URL is configured. After merging a source,
pass `--all` with the original source ID to inspect its history, noting that
unconfirmed work is unknown because sync no longer retries that source. By
default, the output lists only failures that sync still retries based on saved
sync records. Pass `--all` to include the other states:

| Text | JSON `state` | Meaning |
|---|---|---|
| `still failing` | `active` | Sync retries the failure. |
| `fixed` | `resolved` | A later sync recorded confirmation that the work succeeded. |
| `retired` | `historical` | Cleanup retired the failure for a message deleted from Gmail. |
| `unknown` | `indeterminate` | No retry or confirmation is recorded, sync skipped a later download, or msgvault does not recognize the failure. |

Each row shows the message ID, state, and failure description, and JSON adds the
saved phase and error-kind codes. A message can appear once per state, such as
an older fixed failure alongside a newer one still failing. This command does
not inspect saved files on disk, and an empty list does not prove the archive is
complete.

---

## diagnose-gmail-inventory

Not in the latest release yet. Compare Gmail message IDs with one archived
account through the selected daemon:

```bash
msgvault diagnose-gmail-inventory --source-id 1
msgvault diagnose-gmail-inventory --source-id 1 --json --limit 1000
```

This command observes IDs only. It does not sync, refetch, repair, delete,
or change archive data or recorded sync results. It uses the source's existing
Gmail credentials. The selected source must be an existing Gmail account that
is neither merged into another account nor kept only as history. Legacy sources
with an empty type use the existing Gmail convention. The authenticated Gmail
address must match the account address saved for that source; aliases or an
outdated address require an explicit account configuration correction before
comparing.

The diagnostic opens the archive read-only. Connecting can start the local
daemon, which retains its normal startup and scheduled work. Authentication
can invoke configured credential commands and refresh stored tokens. The
`--timeout` budget covers diagnostic collection after the daemon connection
is ready; it does not include daemon startup.

The comparison includes every ID returned by Gmail, including Spam and Trash,
without a search query or label filter. Earlier filtered or limited syncs may
explain differences; the diagnostic does not reconstruct their exclusions.
Archived IDs come from the `source_message_id` field for that source, including
hidden messages and messages marked deleted from Gmail. Hidden messages still
count as archived. History retained only from merged accounts may contain IDs
from a different mailbox and is excluded, with the message count reported.
Message content and attachment download exclusions do not remove a message ID.

| Flag | Default | Contract |
|---|---|---|
| `--source-id` | Required | One positive Gmail source ID, listed by `list-accounts`. |
| `--json` | `false` | Emit one JSON object; errors go to stderr. |
| `--limit` | `100` | Maximum IDs in each difference, sorted lexically; `0` shows counts only. |
| `--max-messages` | `1000000` | Bound archived messages examined and Gmail ID observations separately, including excluded historical messages and repeated IDs. Range 1–10000000. |
| `--max-pages` | `10000` | Maximum Gmail response pages, including empty pages. Range 1–1000000. |
| `--timeout` | `30m` | Positive duration for the diagnostic, including archive reads and Gmail retries. |

Each difference has `count`, `ids`, and `omitted`. The display limit never
changes the counts or hides the number of omitted IDs. Increase `--limit` to
retrieve more IDs. `remote_count` is the number of unique IDs actually observed;
`remote_observations` also counts repetitions. `shared_count` counts IDs observed
in both places. No message bodies, original email data, attachment files or
recorded sync outcomes are read for this comparison.

| `status` | Meaning |
|---|---|
| `complete` | All archived IDs were read, Gmail returned its last page, and its nonzero History IDs (mailbox change markers) matched before and after listing. Differences remain observations at different times. |
| `partial` | Gmail listing stopped on an error, cancellation, deadline, invalid response, repeated next-page marker or resource limit. `remote_only` contains observed candidates only. |
| `inconclusive` | A comparison could not be established, or Gmail listing ended but the authenticated account could not be verified or its History ID changed or was unknown. |

`local_complete` and `remote_complete` describe the two collections separately.
`reason` gives a machine-readable explanation; text output describes that reason
in words. An archive message limit or database error prevents comparison rather
than treating an incomplete read as the full archive. Archive read failures can
leave counts unset at zero; consult `local_complete` before interpreting them.
On partial or inconclusive results, `local_not_observed` is `null` and
`local_unassessed` counts archived IDs whose Gmail presence remains unknown.
`remote_only` is `null` when no Gmail observations could be compared. Non-complete
results return an error exit status after writing the available report. Errors
opening or validating the archive, or invalid arguments, produce no report.

The command reads archived IDs from one consistent database view (a SQL read
snapshot), then releases that view before contacting Gmail. Gmail's listing
does **not** promise that all pages describe the mailbox at one instant.
The [Gmail listing API](https://developers.google.com/workspace/gmail/api/reference/rest/v1/users.messages/list)
provides a next-page token to retrieve the following page and an estimated
count. The command follows tokens even through empty pages and ignores the
estimate when deciding completion. History IDs are Gmail's markers of mailbox
changes. The command compares them before and after listing to detect observed
changes. Matching markers do not make Gmail and the archive a single consistent
view. A sync, merge or account change after archived IDs were read may make
differences temporary. Repeat the observation after activity has settled when
investigating a candidate.

Messages found in Gmail but absent from the archive are investigation candidates.
A sync may still be copying them, or earlier filters may have intentionally
excluded them. Archived IDs not observed in a completed Gmail listing may be
legitimate retained deletions.
Neither differences nor equal ID sets verify saved original emails (RAW),
attachments, extracted message data, full-text search (FTS), or stored files.
Use `verify` and the existing targeted audits for those separate questions.

## verify

Verify archive integrity against Gmail through your configured remote server or
local daemon, streaming the daemon's stdout and stderr back to your terminal.
Provider-message counts exclude calendar events extracted from invitations.

```bash
msgvault verify <email> [flags]
```

| Flag | Description |
|---|---|
| `--sample N` | Messages to sample (default: 100) |
| `--skip-db-check` | Skip SQLite integrity check |
| `--json` | Emit machine-readable JSON summary |

Sample verification checks for nonempty raw MIME that can be decoded and parsed.
Empty, missing, unreadable, or unparseable samples count toward `sample_errors`
instead of `sample_verified`. Raw MIME coverage counts stored rows without
certifying their contents. Sample errors show up in the summary and do not by
themselves cause a nonzero exit status.

---

## stats

Print archive totals for messages, threads, attachments, labels, accounts, and
database size. The message total includes both active and source-deleted
messages; when source-deleted messages exist, their count appears separately.
The analytics cache also keeps source-deleted messages.

Text output is the default. Pass `--json` to print one JSON object:

```bash
msgvault stats --json
msgvault stats --collection team --json
```

JSON always includes these integer counts, even when zero:

| Field | Meaning |
|---|---|
| `total_messages` | Active plus source-deleted messages, excluding dedup-hidden rows |
| `active_messages` | Messages still present at the source |
| `source_deleted_messages` | Messages deleted at the source and retained in the archive |
| `total_threads`, `total_attachments`, `total_labels` | Counts associated with active messages |
| `total_accounts` | Source records in the selected scope, including calendar sources and sources with no messages |

The CLI's `total_messages` includes both active and source-deleted messages,
while the daemon HTTP API keeps its legacy active-only `total_messages` for
compatibility.

The `scope` object sets `kind` to `archive`, `account`, `collection`,
`agent_grant`, or `authorized_sources`. Its `source_count` equals
`total_accounts`, and both count source records, including associated calendar
sources and sources with no messages (one account can span multiple source
records). Scoped results include a `label` when available. Without an explicit
account or collection, delegated reads report `agent_grant` and count only
granted sources, including zero when none remain. A remote limited by its host
to specific sources, such as one using a credential helper, reports
`authorized_sources` and counts only those sources.

Owner reads include `database_size_bytes` (even when zero) and
`database_size_scope: "global"`. The reported size remains global even with
`--account` or `--collection`. Delegated and source-limited reads omit both size fields, and
JSON never includes a database path or server URL. Pass `--json=false` to force
text output.

For custom structured counts, run:

```bash
msgvault query "SELECT count(*) AS n FROM messages"
```

```bash
msgvault stats [flags]
```

| Flag | Description |
|---|---|
| `--account` | Show stats for a specific account |
| `--collection` | Show stats for all member accounts of a collection |
| `--json` | Output structured archive counts and scope metadata as JSON |

---

## People command guide

Follow the [people guide](/docs/usage/people/) for the full workflow. Observed
contacts use participant IDs, while saved profiles use person IDs; each command
below takes the ID named in its arguments.

### identity matches

Review archive-derived identity suggestions using a token from the current
review snapshot:

| Command | Purpose |
|---|---|
| `identity matches list [--state candidate] [--limit 100] [--offset 0] [--json]` | List candidate, accepted, rejected, conflict, or all match records |
| `identity matches show <id> [--json]` | Inspect evidence, blockers, and the current review token |
| `identity matches accept <id> --review-token <token> [--notes-file <path> | --notes-stdin] [--json]` | Accept the reviewed suggestion and apply its participant link |
| `identity matches reject <id> --review-token <token> [--notes-file <path> | --notes-stdin] [--json]` | Reject the reviewed suggestion while retaining its record |

Always inspect a match before deciding. If evidence or endpoints change, the
review token becomes stale, and you must fetch the match again to get a fresh
token. A successful acceptance records your decision and applies the link. Review
notes are private data. See
[people and profiles](/docs/usage/people/#review-identity-matches) for safe
review steps.

### person scoring

Run a bounded scoring batch to generate identity review suggestions and journal
the results; scoring never accepts matches. Configure the
disabled-by-default
[`people.identity_scoring`](configuration.md#people-identity-scoring) settings,
review the provider disclosure and data fields, and then consent to its
fingerprint:

```bash
msgvault person scoring status
msgvault person scoring consent <disclosure-fingerprint>
msgvault person scoring run --limit 20
msgvault person scoring history --limit 20
```

| Command | Purpose |
|---|---|
| `person scoring status [--json]` | Show readiness, credential availability, consent, blockers, and the provider disclosure |
| `person scoring consent <disclosure-fingerprint> [--json]` | Consent to the exact current disclosure |
| `person scoring revoke <disclosure-fingerprint> [--json]` | Withdraw that disclosure's consent without changing configuration |
| `person scoring run [--limit <n>] [--json]` | Score and journal one batch; local blockers prevent provider requests for those pairs |
| `person scoring history [--candidate-id <id>] [--limit <n>] [--before-id <id>] [--json]` | Read redacted judgments, optionally for one candidate or below a history cursor |

`run --limit` defaults to the configured batch size and cannot exceed it.
`history` defaults to 100 rows and accepts limits from 1 to 100. Candidate and
cursor IDs must be nonnegative, where zero means no filter; pass the returned
`next_before_id` to `--before-id` to read older history.

Each run result returns the candidate's review token, proposed action, and
blockers. If a batch stops early, completed results stay in the response and
history while the command reports the batch error. Inspect the candidate's
current evidence before accepting a proposal. See the
[people guide](usage/people.md#optional-identity-scoring) for consent and
provider boundaries.

### person notes

Read, replace, or append private Notes while keeping earlier values:

| Command | Purpose |
|---|---|
| `person notes get <person-id>` | Read the current Notes text |
| `person notes set <person-id> --text <text>` | Replace Notes |
| `person notes append <person-id> --text <text>` | Append a fragment atomically on a new line |

Pass inline text, `@path`, or `-` (stdin) to `--text`. Every subcommand accepts
`--json`, and `set --expected-value-id <id>` rejects concurrent changes. See
[private notes](/docs/usage/people/#keep-private-notes) for how Notes work with
search, MCP, and CardDAV.

### person search and files

| Command | Purpose |
|---|---|
| `person search <free-text> [--limit 20] [--json]` | Search curated profiles semantically; requires person embeddings and consent |
| `person files <person-id> [flags]` | Find archived attachments related to the person's linked identities |

`person search` accepts `--limit` from 1 to 100. `person files` defaults to the
`metadata` lane and 100 results, with `--direction`, `--filename`,
`--mime-family`, `--after`, and `--before` as its main filters. `--lane visual`
and `--lane all` require `--query`, and a `--cursor` belongs to one lane and
cannot be used with `all`. See
[person search](/docs/usage/people/#find-a-person-by-what-you-remember) and
[person files](/docs/usage/people/#find-files-related-to-a-person) for index
requirements, supported filters, and pagination.

File results show `unverified` when a content hash is recorded but storage has
not been checked.

#### Typo-tolerant filenames

Pass `--fuzzy-filename` to request typo-tolerant filename suggestions:

```bash
msgvault person files 42 --filename 'quaterly report' --fuzzy-filename
msgvault person files 42 --filename 'quaterly report' --fuzzy-filename --direction to_person --mime-family pdf --limit 20 --json
```

This flag requires a nonempty `--filename` in the `metadata` lane and rejects
`--query` and `--cursor`, including explicitly empty values. Direction,
MIME-family, and date filters still apply. Suggestions default to 20 results,
and an explicit `--limit` must be from 1 to 100. Ordinary file lookup keeps its
existing substring filter, output, pagination, and default limit of 100.

Human-readable output shows `MATCH` (`exact`, `prefix`, or `typo`) and person
provenance, and prints `"Candidate limit reached; suggestions may be incomplete."`
when the bounded lookup cannot prove exhaustion. `--json` returns a separate
object with `suggestions`, `candidate_limit_reached`, and `index_revision`, with
no cursor or total count. Each suggestion keeps the actual filename, attachment
occurrence, and matching person roles.

An unavailable or warming index returns `filename_index_unavailable`, while a
person scope that exceeds the bounds returns `filename_scope_too_large`. The
command reports those errors instead of falling back to an ordinary or unscoped
search. See
[filename suggestion limits](architecture/search-ranking.md#filename-suggestions)
and the [API contract](api-server.md#filename-suggestions).

### person track, tiers, provider, sweep, and facts

Tracking chooses which profiles automatic maintenance can process; it does not
grant provider consent or enroll a person in briefs.

| Command | Purpose |
|---|---|
| `person track <person-id>...` / `person untrack <person-id>...` | Enable or stop future profile maintenance for one or more people, in order. Stops at the first person the daemon rejects; `--json` prints one object per person per line |
| `person tiers preview [--json]` | List saved people in tier 1 or 2 (two-way contact or meetings), whether they are tracked, and the latest day that counted toward the tier. Reads only; `--json` gives the IDs to pass to `person track` |
| `person tiers explain <person-id> [--json]` | Show every count behind one person's tier against its threshold |
| `person provider add <name> [flags]` | Configure and synthetically check a named model provider |
| `person provider list` | List configured model profiles |
| `person provider use <name>` | Select a checked profile and enable sweeps |
| `person provider check [name]` | Run fixed synthetic input without granting consent |
| `person provider consent [name] --yes` | Grant consent to the exact checked policy |
| `person provider revoke [name]` | Revoke that policy's consent; `--all` revokes all stored sweep policies |
| `person provider remove <name>` | Remove a configured profile and its stored key. For a profile no longer in the config, delete its leftover stored key and revoke consent for the policies that used it |
| `person provider history [name] [--person <id>]` | Inspect redacted runs and attempts |
| `person sweep run [--person <id>] [--limit 25]` | Run a bounded maintenance pass for tracked people |
| `person sweep status` | Read redacted progress and usage |
| `person sweep history [--person <id>] [--limit 20]` | Read recent maintenance attempts |
| `person facts catalog [--include-sensitive]` | List eligible fields, their keys, and revisions |
| `person facts evidence <person-id>` | Inspect saved supporting evidence |
| `person facts evidence-status <person-id>` | Inspect changes to evidence support |
| `person facts claims <person-id>` | Inspect proposed values |
| `person facts decisions <person-id>` | Inspect resolver decisions |
| `person facts pins <person-id>` | List fields protected from automatic replacement |
| `person facts pin <person-id> <kind> <key>` | Protect a field's current or empty value |
| `person facts unpin <person-id> <kind> <key>` | Allow automatic resolution for the field again |

All of these commands accept `--json`. Pass catalog keys, not slugs, for fact
pins. Evidence, claims, and decisions accept an exact `--target`, and
fact-history commands accept `--limit` (1 to 200) and `--offset`. Run
`sweep run --backstop` to revisit older evidence in a bounded pass.

Provider configuration edits run on the daemon host. Restart the daemon after
changing its active provider or policy. Profile removal uses the running daemon
to revoke consent and clear checks for both the saved and running policies. If
the daemon is incompatible, stop it before removing a profile; if no daemon is
running, removal updates the local archive directly.

For setup, privacy controls, budgets, and recovery, see
[profile automation](/docs/usage/people-automation/). See
[provider status](#person-provider-status), [reverify](#person-provider-reverify),
[set](#person-provider-set), and [briefs](#person-brief) below for more details.

### person enrichment

Look up public person information through independently configured and
consented Exa or Sixtyfour policies:

| Command | Purpose |
|---|---|
| `person enrichment profiles` | List saved policy fingerprints |
| `person enrichment status [--limit 20]` | Inspect policies, consent, and bounded suppression history |
| `person enrichment consent <fingerprint>` | Grant consent for that exact enrichment policy |
| `person enrichment revoke [fingerprint]` | Revoke an exact policy; `--all` revokes all enrichment policies |
| `person enrichment run --person <id> --provider <name> --idempotency-key <key>` | Request a lookup for one person |
| `person enrichment suppress --person <id> --reason <reason>` | Record an opt-out for the person's current identifiers |
| `person enrichment suppress --provider <name> --identifier-class <class> --reason <reason> < identifier.txt` | Suppress an identifier read from standard input |

`status`, `profiles`, `consent`, `revoke`, and `run` all accept `--json`.
Suppression reasons are `opt_out` and `data_subject_request`. The two
suppression forms are mutually exclusive, so `--person` does not accept
`--provider` or `--identifier-class`.

For the identifier form, `<class>` must be `email`, `phone`,
`public_profile_url`, `provider_person_id`, or `name_company`. Provide one
nonempty line on standard input, except for `name_company`, which requires two
lines: the name, then the company. Never pass identifier values on the command
line.

See [external enrichment](/docs/usage/people-enrichment/) for required identity
details, configuration, request limits, and the persistent suppression key.

## organization and employment

Track organization details and current or historical jobs in structured records.
You can use `org` as a short alias for `organization`.

| Command | Purpose |
|---|---|
| `organization list [--query <text>] [--include-retired]` | Find organizations |
| `organization create <name> [--domain <domain>]` | Create an organization |
| `organization show <id> [--history]` | Read an organization and its profile data |
| `organization set <id> --name <name> [flags]` | Replace mutable organization fields |
| `organization retire <id>` / `organization unretire <id>` | Change active status while keeping the record |
| `organization merge <survivor-id> <losing-id>` | Consolidate duplicate organizations |
| `organization delete <id>` | Permanently delete an organization |
| `organization attribute list <id>` | Read attributes; `--include-superseded` adds history |
| `organization attribute set <id> --definition <slug> --text <value>` | Set a text attribute |
| `organization attribute clear <id> <slug>` | Close an attribute value while retaining history |
| `employment add --person <id> --organization <id> [flags]` | Connect a person to an organization |
| `employment list --person <id>` | List a person's jobs; `--organization <id>` lists an organization's jobs |
| `employment show <id>` / `employment set <id> [flags]` | Inspect or edit one job |
| `employment end <id> [--end <date>]` | End a job while keeping its history |
| `employment set-primary <id>` | Select the primary current job |
| `employment delete <id>` | Permanently delete a job |

All of these commands accept `--json`. `employment add` accepts `--title`,
`--role`, `--department`, `--location`, `--description`, `--start`, `--end`, and
`--primary`, with partial dates allowed (`YYYY`, `YYYY-MM`, or `YYYY-MM-DD`).
Attribute writes support `--dry-run` and `--expected-value-id` for validation
and concurrent-edit checks. See
[employment and relationships](/docs/usage/people/#record-employment-and-relationships).

## relationship-type and person relationship

Record typed relationships between two saved profiles:

| Command | Purpose |
|---|---|
| `relationship-type list` | Read forward and reverse labels for available types |
| `relationship-type create <slug> <forward-label> <reverse-label>` | Create a user-owned type; `--symmetric` requires identical labels |
| `relationship-type update <type-id> [flags]` | Change labels and presentation |
| `relationship-type delete <type-id>` | Delete an eligible user-owned type |
| `person relationship add <source-person-id> <type-slug> <target-person-id>` | Add a relationship; accepts `--from`, `--until`, and `--notes` |
| `person relationship list <person-id> [--include-ended]` | Read relationships from that person's perspective |
| `person relationship end <relationship-id> <until-date>` | End a relationship while keeping history |
| `person relationship delete <relationship-id>` | Permanently delete a relationship |
| `person relationship reviews [--status <status>]` | Inspect relationship review candidates |

All of these commands accept `--json`. The forward label reads as "source is the
___ of target." See
[relationships](/docs/usage/people/#record-employment-and-relationships) for
direction and date examples.

## add-carddav, sync-carddav, and carddav

Connect CardDAV accounts, assign address-book roles, and resolve sync conflicts.
Publishing a profile or resolving a conflict can modify the external address
book; see the [CardDAV guide](/docs/usage/people-carddav/).

| Command | Purpose |
|---|---|
| `add-carddav <base-url> <username> [--connection <name>] [--schedule <cron>] [--disabled]` | Discover and save an account; password is prompted or read from piped stdin |
| `add-carddav --google <email> [--connection <name>] [--oauth-app <name>] [--schedule <cron>] [--disabled]` | Connect Google Contacts using an authorized account token |
| `add-carddav --icloud <apple-id> [--connection <name>] [--schedule <cron>] [--disabled]` | Discover the iCloud contacts partition using an app-specific password |
| `add-carddav --microsoft <email> [--headless] [--connection <name>] [--schedule <cron>] [--disabled]` | Sign in to Microsoft and connect Microsoft 365 or Outlook.com contacts through Microsoft Graph |
| `carddav authorize-google <email> [--oauth-app <name>] [--no-browser]` | Authorize Google Contacts in the browser, preserving existing Google permissions |
| `carddav authorize-microsoft <email> [--headless]` | Sign in to Microsoft for contacts without saving a connection |
| `sync-carddav [--connection <name>] [--full]` | Synchronize all enabled connections, or the selected connection; `--full` reconciles complete books |
| `carddav connections` | List connection names, enablement, runtime availability and orphaned accounts |
| `person publish <person-id>` / `person unpublish <person-id>` | Publish a saved profile or remove its remote card |
| `person publish <person-id> --preview` | Print the exact vCard and approval token as JSON without publishing |
| `person publish <person-id> --approve <token>` | Approve reviewed changes; conflict previews also need explicit `keep_local` resolution |
| `carddav books [--connection <name>]` | List discovered books, owning connection names and roles |
| `carddav books set-role <book-id> [--write-target] [--subscribed] [--lookup-source]` | Replace all three roles; omitted flags become false |
| `carddav bind-participants [--dry-run] [--undo] [--json]` | Reconcile exact archived email/phone matches from retained subscribed cards; preview counts without writes, or undo only automatic memberships |
| `carddav conflicts list` | List unresolved conflicts |
| `carddav conflicts show <conflict-id>` | Compare bounded base, local, and remote summaries |
| `carddav conflicts resolve <conflict-id> <keep_local\|keep_remote>` | Choose the local or remote side explicitly |

Running `add-carddav` without `--connection` saves the connection as `default`.
Passing `--connection` to `sync-carddav` syncs that single connection, including
a disabled one for manual repair. Running `sync-carddav` without `--connection`
when no connection is enabled fails as unavailable. Aggregate sync prints counts and safe failure
codes per connection and exits nonzero on partial or failed runs. Book IDs,
publication commands, and conflict commands are global, while network requests
use the owning connection's credentials.
Sync reports binding conflicts archive-wide at the time of the run, including
conflicts from other connections.

Directory and integrations can also use the
[publication API](/docs/usage/people-carddav/#sync-and-publish-selected-people).
CardDAV commands do not have a general `--json` flag; role changes, publication
previews, and conflict `list` and `show` commands already output JSON.

---

`bind-participants` uses the selected daemon and makes no CardDAV network requests.
Its JSON contains `cards_scanned`, `participants_bound`, `participants_unbound`,
`conflicts`, and `conflict_details` with numeric participant/person IDs and a
`reason`. Conflicting ownership is reported without merging people. `--undo`
releases feature-owned memberships; the next sync may recreate them. See
[CardDAV identity binding](usage/people-carddav.md#connect-cards-to-archived-identities)
for persistent corrections and manual confirmation.

## identity

Manage the confirmed "me" identifiers for each account. These subcommands talk
to your configured remote server or local daemon by default; pass `--local` to
target the local daemon even when a remote is configured.

```bash
msgvault identity list [--account <account> | --collection <name> | --source-id <id>]
msgvault identity show [<account>] [--source-id <id>]
msgvault identity add [<account>] <identifier> [--source-id <id>]
msgvault identity remove [<account>] <identifier> [--source-id <id>]
msgvault identity discover [<account>] [--source-id <id>] [--apply]
msgvault identity import [<account>] [--source-id <id>] (--file <path> | --stdin)
```

| Command | Description |
|---|---|
| `identity list` | List confirmed identifiers across accounts |
| `identity show <account>` | Show one account's identity in detail |
| `identity add <account> <identifier>` | Add a confirmed identifier |
| `identity remove <account> <identifier>` | Remove a confirmed identifier |
| `identity discover <account>` | Preview archived source evidence; `--apply` confirms strong candidates |
| `identity import <account>` | Preview or apply source-scoped identifiers from text or JSON |

| Flag | Applies to | Description |
|---|---|---|
| `--account` | `list` | Restrict to a single account |
| `--collection` | `list` | Restrict to all member accounts of a collection |
| `--source-id` | all subcommands | Select one source unambiguously by numeric ID; mutually exclusive with an account argument or `list` scope |
| `--json` | `list`, `show`, `discover`, `import` | Output structured JSON; discovery also suppresses progress |
| `--signal` | `add` | Evidence signal name (default `manual`) |
| `--apply` | `discover` | After the complete preview scan, confirm strong evidence |
| `--provider` | `discover` | Include the authenticated Gmail profile or the source's configured `[[fastmail]]` alias inventory |
| `--confirm <address>` | `discover` | Explicitly confirm one weak candidate; repeatable and requires `--apply` |
| `--file <path>` / `--stdin` | `import` | Read a text or JSON identity list from exactly one input |
| `--signal` | `import` | Evidence signal recorded for imported identities (default `manual`) |
| `--apply` | `import` | Confirm every validated imported identity; without it the command only previews |

Email sync enriches identities you already confirmed for a source using strong
sender evidence from trusted Sent metadata, without confirming first-time
aliases on its own. Review candidates with `msgvault identity discover`, and
confirm strong candidates with `msgvault identity discover --apply`.
Recipient-only evidence stays review-only. See
[People, Profiles, and Source Identities](/docs/usage/people/) for
classifications, Fastmail inventory, and import formats.

---

## person

Manage durable person profiles and their typed, historized attributes. You can
create a profile directly, promote an observed participant's identity cluster,
or import a subscribed CardDAV contact. A participant is a single address or
handle seen in messages, while a person groups participants into a profile with
notes, attributes, relationships, and briefs.

To find saved people without semantic search or creating a profile, run:

```bash
msgvault person directory --query "Alex Example"
msgvault person directory --address alex@example.test
```

To find an observed contact by address without creating a person, run:

```bash
msgvault query "SELECT id, email_address, display_name FROM participants WHERE email_address ILIKE '%alice%'"
```

Pass the returned participant `id` to `person promote`. Running
`person list --json` returns `id`, `participant_ids`, `revision`, `vcard_uid`,
`created_at`, `updated_at`, and `display_name` when known. Promotion takes the
name from the first named participant, CardDAV import takes the contact's name,
and `create` uses `--name`. The list stays empty until you create a person,
promote a participant, or import a CardDAV contact. Check addresses with
`person identities <person-id>` and saved profiles with `person get`.
`person search` ranks semantic matches; `msgvault setup status` explains how to
enable that lane.

```bash
msgvault person create --name <name> [flags]
msgvault person promote <participant-id>
msgvault person list [--json]
msgvault person directory [flags]
msgvault person get <person-id> [--json]
msgvault person get --vcard-uid <uid> [--json]
msgvault person identities <person-id> [--json]
msgvault person set-display-name <person-id> <display-name> [--json]
msgvault person set-display-name <person-id> --clear [--json]
msgvault person delete <person-id>
msgvault person merge <survivor-id> <absorbed-id> [flags]
msgvault person split <source-person-id> [flags]
msgvault person merge-history <person-id> [--json]
msgvault person merge-show <merge-id> [--snapshot] [--json]
msgvault person merge-candidate <candidate-id> [flags]

msgvault person attributes list <person-id> [--slug <slug>] [--history] [--json]
msgvault person attributes set <person-id> <slug> (--value <scalar> | --value-json <json|@path|-> | --list A|B|C|D) [flags]
msgvault person attributes clear <person-id> <slug> [flags]
```

`person get --json` includes the person's first archived appearance and first direct contact, along with the appearance
reference, `email_intro` or `observed` kind, and introducer when known. See
[first archived appearance](usage/people.md#see-a-persons-first-archived-appearance)
for evidence rules and limits.

`create` creates a profile with a new vCard UID and no participants. Optional
flags include repeatable `--email <value[:type]>`, repeatable
`--phone <value[:type]>`, `--org`, `--title` (which requires `--org`),
`--address`, `--note`, `--json`, `--publish`, and `--if-exists error|return`
(default `error`). The command refuses existing current contact points and
observed identities and reports the matched person or participant.
`--if-exists return` returns a single saved match unchanged, with
`ensure.status` set to `matched_existing` in JSON; a new person reports
`created`. Retry safety requires an email or phone; name-only calls create a
new person each time. Unmatched contacts are reported without being added.
Ambiguous matches and unbound clusters return coded errors with resolution details (`--json`
prints the error envelope). `--publish` publishes only a new person. Without
`--publish`, everything stays local. Publication uses the existing CardDAV
path; if it fails, the new person stays saved and the output prints its ID so
you can retry or review. See [Create a person](usage/people.md#create-a-person).

`promote` seeds a new profile with the first nonblank observed name in its
linked cluster, ordered by participant ID, and leaves the name empty if no
member has one. Promoting again keeps the saved name, including an edited or
cleared value. `set-display-name` keeps the profile's stable ID and vCard UID.
`delete` permanently retires that UID and removes the profile's participant
bindings, though you cannot delete a person with active merge lineage until you
split that lineage completely.

`person get --vcard-uid` looks up a person by their current UID, a retired UID that still aliases them after a
merge, or a UID on a CardDAV card currently mapped to them. JSON responses for
`person` and `person directory` include `vcard_uid` and `carddav_bindings`,
where each binding lists its connection, address book, resource `href`,
`remote_uid`, and `mapping_status`. Importing a subscribed CardDAV card adopts
its UID when that UID is a bare or `urn:uuid:` UUID not used by any current,
merged, or deleted profile, while existing profiles keep their UID.

`identities` lists the email addresses, phone numbers, and chat identifiers used
by the person's current participants in your archive, so a merge or split shows
up on the next call. Email addresses are `supported` draft recipients that you
can pass to [`draft-compose --to`](#draft-to-a-person). A listed value keeps the
quotes that a local part such as `"first last"` needs. Phone numbers and chat identifiers are
`unsupported`, and curated contact points and postal addresses are omitted. An
unknown or merged-away person fails with `Person profile not found`. Only the
owner can list identities; delegated agent tokens are refused. This listing does
not wait for a running sync or import.

`merge` keeps the survivor's ID and vCard UID, moves the absorbed profile into
it, and records a reversible merge packet. Profiles with active CardDAV
publication are rejected. `split` creates a new person and UID; repeat
`--participant` to move multiple absorbed lineages, or omit it when the absorbed
profile had no participants.

| Merge flag | Applies to | Description |
|---|---|---|
| `--survivor-revision <n>` | `merge` | Required expected revision of the surviving person |
| `--absorbed-revision <n>` | `merge` | Required expected revision of the absorbed person |
| `--idempotency-key <key>` | `merge`, `split` | Required retry key; reusing it with a different request is rejected |
| `--merge-id <id>` | `split` | Required merge record to reverse |
| `--participant <id>` | `split` | Absorbed participant lineage to move; repeat as needed |
| `--revision <n>` | `split`, `merge-candidate` | Required expected revision of the current person |
| `--person-id <id>` | `merge-candidate` | Person that owns the review candidate |
| `--decision <accepted\|rejected>` | `merge-candidate` | Resolve a conflicting single-value attribute |
| `--snapshot` | `merge-show` | Read and verify the immutable merge snapshot |
| `--json` | all merge commands | Output structured JSON |

Exact splits restore the pre-merge profiles when their lineage and referenced
rows are still available. For partial splits, pass `--json` to inspect ambiguous
or unrestored rows. Complete merge packets keep merge-time profile values even
after later redaction and require the strongest profile-data options when copied
into a subset. See
[People, Profiles, and Source Identities](/docs/usage/people/#merge-duplicate-profiles-and-reverse-a-merge)
for the workflow and lifecycle boundaries.

| Attribute flag | Applies to | Description |
|---|---|---|
| `--slug <slug>` | `list` | Restrict output to one definition |
| `--history` | `list` | Include superseded values instead of current values only |
| `--value <scalar>` | `set` | Coerce text, integer, real, boolean, date, or timestamp input to the definition's type |
| `--value-json <json\|@path\|->` | `set` | Supply a structured typed-value envelope |
| `--ordinal <n>` | `set`, `clear` | Address one slot of a multi-valued definition |
| `--source <name>` | `set` | Provenance: `user`, `carddav_import`, `vcard_import`, `archive_observation`, `extraction`, `enrichment`, or `system` |
| `--source-ref <value>` | `set` | Resource or message reference that produced the value |
| `--confidence <0..1>` | `set` | Confidence for a derived or suggested value |
| `--actor <value>` | `set` | Actor recorded with the value |
| `--expected-value-id <id>` | `set`, `clear` | Compare-and-swap guard for the current value |
| `--dry-run` | `set`, `clear` | Validate and preview without writing |
| `--json` | `list`, `set`, `clear` | Output structured JSON |

Setting or clearing a value closes the current history row instead of deleting
it. See [People, Profiles, and Source Identities](/docs/usage/people/) for the
shipped definitions and full workflow.

---

## person agenda

Read and organize a person's live Kata tasks. Configure
[`[integrations.kata]`](configuration.md#integrationskata) on the daemon first.
Tasks belong to one person and stay in the configured Kata project.

```bash
msgvault person agenda list <person-id> [--json]
msgvault person agenda create <person-id> --title "Discuss the proposal" [flags]
msgvault person agenda link <person-id> <ref> [--list agenda] [--json]
msgvault person agenda edit <person-id> <ref> --list follow-up [--json]
msgvault person agenda unlink <person-id> <ref> [--json]
```

`<ref>` is a task reference returned by Kata or an agenda command. `create`
creates a task and links it to the person, `link` attaches an existing task,
`edit` changes only its list, and `unlink` removes the person link without
deleting the task. To complete or reopen tasks, or to edit their title, body,
priority, or labels, use Kata directly.

| Flag | Commands | Default | Description |
|---|---|---|---|
| `--title` | `create` | — | Required task title |
| `--body` | `create` | — | Task body |
| `--priority` | `create` | — | Kata priority from 0 through 4 |
| `--label` | `create` | — | Kata label; repeat for multiple labels |
| `--list` | `create`, `link`, `edit` | `agenda` for create/link | List name; required for `edit` |
| `--idempotency-key` | `create` | Generated | Stable key to reuse when retrying the same creation |
| `--json` | All agenda commands | `false` | Print the response as JSON |

`create` prints its generated retry key to stderr before sending the request.
To retry, pass that key with `--idempotency-key` and keep the task details the
same. You can also choose the key in advance:

```bash
msgvault person agenda create 42 --title "Discuss the proposal" --idempotency-key proposal-discussion-1
```

`list` returns up to 100 open tasks and matches the person's current vCard UID
and aliases. JSON sets `truncated` when more items remain; open Kata to view the
rest. Oversized Kata responses return an explicit error. See
[Kata configuration](configuration.md#integrationskata) for metadata and
response limits.

---

## kata

Create Kata issues that quote exact message or file text. Configure
[`[integrations.kata]`](configuration.md#integrationskata) on the daemon first.

```bash
msgvault kata evidence prepare [--input FILE]
msgvault kata create --idempotency-key KEY [--input FILE] [--json]
msgvault kata link <ref> [--input FILE] [--json]
msgvault kata issues --message ID [--attachment ID] [--json]
msgvault kata context <ref> [--offset N] [--json]
```

`prepare`, `create`, and `link` each read a single JSON request from `--input`,
or from stdin by default. `prepare` prints exact excerpts and the references
that `create` and `link` accept. `create` requires `--idempotency-key`, a key
you choose to identify the issue; running it again with the same key and input
returns the existing issue instead of filing a duplicate. `link` adds evidence
to an existing issue in the configured project, given as `project#ref` or a bare
ref. `issues` lists open and closed issues that already cite a message or one of
its files, printing one `ref<TAB>status<TAB>title` line per issue. It checks the
newest 500 evidence-bearing issues (or 20 if the listing exceeds 16 MiB) and
warns on stderr when the list may be incomplete, setting `truncated` in JSON;
check Kata before filing again. `context` shows each passage an issue cites, its
current archive state, and surrounding text, up to 10 passages at a time; pass
`--offset` to read the next page. See [Kata issues](usage/kata-issues.md) for
request shapes and limits.

---

## person directory

Find saved people by curated name or attached address, or browse by last contact
through the selected local or configured remote daemon. Results default to most
recent contact first (`last_contact_desc`). Each page uses the daemon's default
of 50 people. Lookup is
read-only and needs no semantic search.

```bash
msgvault person directory
msgvault person directory --query "Alex Example"
msgvault person directory --address alex@example.test
msgvault person directory --last-contact-after 2026-06-01 --last-contact-before 2026-07-01 --json
msgvault person directory --sort last_contact_asc
msgvault person directory --sort name
msgvault person directory --cadence overdue --sort most_overdue
msgvault person directory --cadence due --due-within-days 7 --sort next_due
msgvault person directory --last-contact-after 2026-06-01 --cursor "<next_cursor>"
```

| Flag | Default | Description |
|---|---|---|
| `--query <text>` | absent | Lexical search over current curated names, contact points, and organizations |
| `--address <text>` | absent | Literal, case-insensitive substring of current bound participant addresses/identifiers or curated contact points |
| `--last-contact-after <date>` | absent | Inclusive lower bound, `YYYY-MM-DD` or RFC3339 |
| `--last-contact-before <date>` | absent | Inclusive upper bound, `YYYY-MM-DD` or RFC3339 |
| `--sort <order>` | `last_contact_desc` | `name`, `last_contact_desc`, `last_contact_asc`, `most_overdue`, or `next_due` |
| `--cadence <filter>` | absent | `overdue`, `due`, `no_list`, or `unknown` |
| `--due-within-days <days>` | `0` | Inclusive UTC horizon 0..3660; requires `--cadence due` |
| `--cursor <cursor>` | absent | Opaque `next_cursor` from the previous page |
| `--json` | `false` | Output the full Directory page envelope |

Date-only bounds mean midnight UTC on that date, while RFC3339 bounds accept
offsets and fractional seconds. Both bounds are inclusive, so
`--last-contact-before 2026-07-01` includes midnight at the start of July 1;
pass a full timestamp if you want a different cutoff.

All filters intersect, and `%`, `_`, and backslash in `--address` match as
literal characters. Address lookup searches archived aliases on currently bound
participants and current curated contact points, skipping superseded values,
unpromoted participants, and postal addresses. Curated display names stay
unchanged, including cleared names. An older daemon that does not support the
address filter returns an upgrade error.

Sparse or no-match address-only lookups can scan the entire saved directory, and
remote PostgreSQL connections can add network latency to those round trips. If
you know a name or organization, combine `--query` with `--address` to narrow
candidates first.

Human-readable output prints `ID`, `DISPLAY NAME`, `LIST`, `LAST CONTACT`,
`DUE`, and `STATUS` in daemon order. The `LIST` column shows a preset letter or
a custom day interval. Timestamps print in UTC RFC3339 at whole-second
precision, while JSON keeps fractional seconds. Missing display names and
timestamps print as `-`. When more results exist, the output prints
`Next cursor`; pass that value unchanged to `--cursor` with the same query,
address, bounds, and sort to read the next page.

JSON returns a `people` array and an optional `next_cursor`, keeping all
Directory fields for each person (categories, organizations, contact state, ID,
revision, `vcard_uid`, and `carddav_bindings`). Optional fields stay omitted
when the daemon omits them, including `last_contact_at` for people without a
contact timestamp. `person list` still returns the full unpaginated profile
collection with its existing human columns and JSON array output.

---

## person provider add

Create and synthetically check a named people inference profile. Pick a built-in
preset to bind its protocol, endpoint, and authentication scheme:

```bash
msgvault person provider add primary --provider openrouter --model <model> \
  --credential-env OPENROUTER_API_KEY --retention-posture <assertion> \
  --training-posture <assertion> --source conversation_text \
  --source-since 2026-01-01 --allow-sensitive=true --yes
msgvault person provider consent primary --yes
msgvault person provider use primary
```

`--provider` accepts `openai`, `openrouter`, or `venice`, requires an explicit
model and privacy policy, and cannot combine with `--custom` or
`--accept-catalog-prices`; conflicting protocol, endpoint, or auth overrides are
rejected. `--credential-env` reads only the named host variable. You can also
pass `--api-key-stdin` to read a key from stdin, or let an interactive terminal
prompt you. Passing the synthetic check does not grant consent or select the
profile. See [profile automation](usage/people-automation.md) for custom
protocol profiles and policy fields.

## person provider enroll-codex

Create, check, consent to, and select a new Codex profile through the daemon.
**Codex enrollment is unavailable in this release.** No Codex build is approved,
and the daemon returns HTTP 503 before changing credentials or consent. This
command requires a terminal and never starts a noninteractive device login.

```bash
msgvault person provider enroll-codex <new-name> \
  --retention-posture <assertion> --training-posture <assertion> \
  --source conversation_text --source-since 2026-01-01 --allow-sensitive=true
```

The gated flow prints a verification URL, user code, and local deadline, waits
for sign-in, and then prompts you for an available model and reasoning effort.
Existing profile names are rejected. After saving, you must pass a synthetic
check and grant separate consent before selecting the profile; declining consent
leaves it unselected. Restart the daemon when the reported running profile
differs.

| Flag | Default | Description |
|---|---|---|
| `--model` | prompt | Codex model ID available to the signed-in account |
| `--reasoning-effort` | prompt | Supported effort for the chosen model |
| `--retention-posture`, `--training-posture` | required | Explicit operator assertions |
| `--source` | required | Repeatable: `conversation_text` or `meeting_text` |
| `--source-since` | required | Earliest disclosed date, `YYYY-MM-DD` |
| `--source-until` | absent | Latest disclosed date, on or after `--source-since` |
| `--allow-sensitive` | explicit decision required | `true` permits archive evidence; `false` permits only synthetic checks |
| `--yes` | `false` | Confirm the displayed check disclosure and select the profile without a consent prompt |

Host-side `person provider login` re-authenticates the selected existing Codex
profile, and `person provider models` lists its models and reasoning efforts.
Both accept `--json` and stay unavailable behind the same release gate. See
[Codex configuration](configuration.md#codex-app-server-profiles).

## person provider status

Show the exact people inference provider policy along with its check and consent
state. JSON output includes the selected profile `name` and adds
`stale_program_check` or `stale_program_consent` when a verified historical
record matches every policy field except the extraction program and supplies a
missing current gate. Human-readable output notes a differing extraction program
and prints the recovery command without showing provider credentials or response
content.

```bash
msgvault person provider status [name] [--json]
```

## person provider reverify

Run the existing synthetic provider check, then grant consent for the same
current exact provider profile. The check must succeed before consent is
granted. The command prints the provider
disclosure and prompts for confirmation unless you pass `--yes`.

```bash
msgvault person provider reverify [name] --yes [--json]
```

Omit `name` to target the active provider, as with `check` and `consent`.
`--yes` confirms the disclosure. Human-readable output still prints the
disclosure even when you pass `--yes`,
while `--json` returns the final exact check and consent status. Even if the
named profile is disabled in configuration, this command still selects it for
the operation without enabling scheduled sweeps.

---

## person brief

Read a short summary before your next conversation with someone, or generate a
fresh one from their recent supported chat and text messages.

```bash
msgvault person brief enroll <person-id> [--track] [--json]
msgvault person brief generate <person-id> [--json]
msgvault person brief show <person-id> [--json]
msgvault person brief history <person-id> [--limit 20] [--json]
msgvault person brief reject <person-id> [--reason "..."] [--json]
msgvault person brief unenroll <person-id> [--json]
```

Enrollment requires tracking; pass `--track` to enable both at once. Generation
uses the consented provider and its budget. `history` accepts limits from 1 to
200. Rejecting a brief hides the current version while keeping it in history,
and unenrolling stops future generation without deleting saved versions.

See [person briefs](usage/people-briefs.md) for provider setup, supported
sources, and a first-run example.

---

## person provider set

Update mutable policy fields on an existing named people inference provider
profile in place while keeping its protocol, endpoint, auth scheme, credential
source, executable, execution boundary, selection, and enablement unchanged.

```bash
msgvault person provider set <name> --model <model> [flags]
```

The command reruns the exact synthetic provider check and revokes consent for
the previous fingerprint. After reviewing the updated policy, grant fresh
consent with `msgvault person provider consent <name> --yes`. If a local daemon
is running, run `msgvault daemon restart` so its scheduled sweeps pick up the
change. Configured remote daemons refuse this mutation; run it on the daemon
host or pass `--local`.

| Flag | Default | Description |
|---|---|---|
| `--model` | unchanged | Provider model identifier |
| `--retention-posture` | unchanged | Provider retention assertion |
| `--training-posture` | unchanged | Provider training assertion |
| `--source` | unchanged | Allowed source class; repeatable, replaces the existing list |
| `--source-since` | unchanged | Earliest disclosed source date |
| `--source-until` | unchanged | Latest disclosed source date |
| `--allow-sensitive` | unchanged | Allow sensitive text in provider packets |
| `--reasoning-effort` | unchanged | Explicit reasoning effort |
| `--reasoning-mode` | unchanged | Explicit reasoning mode |
| `--request-timeout` | unchanged | Provider request timeout |
| `--yes` | `false` | Confirm the final provider and privacy values; required |
| `--json` | `false` | Output structured JSON |

---

## attribute-definition

Manage portable field metadata. Definitions add no runtime database columns, and
their universal IDs and slugs stay stable while you can change their labels and
descriptions.

```bash
msgvault attribute-definition list [--object-type person|organization] [--include-hidden] [--json]
msgvault attribute-definition get <definition-id> [--json]
msgvault attribute-definition create --definition <json|@path|-> [--dry-run] [--json]
msgvault attribute-definition rename <definition-id> [--label <text>] [--description <text> | --clear-description] [--json]
msgvault attribute-definition delete <definition-id>
```

`create --dry-run` validates structure locally but cannot detect conflicts with
definitions already saved by the daemon. `rename` never changes the immutable
slug or universal ID. You can delete only user-created, deletable definitions
that have no stored values.

---

## collection

Manage named groups of accounts. These subcommands talk to your configured
remote server or local daemon by default; pass `--local` to target the local
daemon even when a remote is configured.

```bash
msgvault collection create <name> --accounts <account1,account2,...>
msgvault collection list
msgvault collection show <name>
msgvault collection add <name> --accounts <account1,account2,...>
msgvault collection remove <name> --accounts <account1,account2,...>
msgvault collection delete <name>
```

Deleting a collection leaves its sources and messages intact.

---

## deduplicate

Find and merge duplicate messages within an account or collection.

```bash
msgvault deduplicate [flags]
```

By default, msgvault deduplicates each source on its own. Pass `--collection` to
opt in to cross-source deduplication.

| Flag | Description |
|---|---|
| `--dry-run` | Scan and report only; do not hide duplicates |
| `--account` | Scope dedup to one account |
| `--collection` | Dedup across every member account of a collection |
| `--content-hash` | Also detect duplicates by normalized raw MIME content |
| `--prefer` | Comma-separated source type preference order |
| `--undo <batch-id>` | Restore rows hidden by a previous dedup run; repeatable |
| `--delete-dups-from-source-server` | Stage same-source pruned duplicates for remote deletion |
| `--no-backup` | Skip the database backup before merging |
| `-y`, `--yes` | Skip confirmation prompt |

---

## delete-deduped

Permanently delete dedup-hidden messages from the selected msgvault archive.

```bash
msgvault delete-deduped --batch <batch-id>
msgvault delete-deduped --all-hidden
```

The CLI sends the request to your configured remote daemon, or to the
auto-started local daemon if no remote is configured, instead of opening the
SQLite database directly. You cannot undo this deletion with
`deduplicate --undo`; when backups are enabled, the daemon writes a backup next
to its database before deleting. Repeated `--batch` selections commit as one
transaction; if the request is canceled, none of the selected batches are left
half-deleted.

| Flag | Description |
|---|---|
| `--batch` | Delete rows hidden by this dedup batch ID; repeatable |
| `--all-hidden` | Delete every dedup-hidden row regardless of batch |
| `--no-backup` | Skip database backup before deleting |
| `-y`, `--yes` | Skip confirmation prompt (`--all-hidden` still prompts) |

---

## list-senders

List top senders by message count.

```bash
msgvault list-senders [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

When more rows match, `list-senders`, `list-domains`, and `list-labels` print a
stderr hint suggesting you raise `-n`, including when you pass `--json`. JSON
stdout stays a bare array.

---

## list-domains

List top sender domains by message count.

```bash
msgvault list-domains [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## list-labels

List all labels with message counts.

```bash
msgvault list-labels [flags]
```

| Flag | Description |
|---|---|
| `-n`, `--limit N` | Number of results (default: 50) |
| `--after YYYY-MM-DD` | Only messages after this date |
| `--before YYYY-MM-DD` | Only messages before this date |
| `--json` | Output as JSON |

---

## build-cache

Build or update the Parquet analytics cache through the configured remote server
or the local daemon.

```bash
msgvault build-cache [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Rebuild all cache files from scratch |

The CLI sends the request over HTTP and streams the daemon's stdout and stderr
back to your terminal. A local daemon runs the DuckDB export in an isolated
child process so DuckDB's bundled SQLite library never opens the archive inside
the long-lived daemon process. When `[remote].url` is configured, the remote
daemon builds its own cache; pass `--local` only if you want to target the
local daemon on this machine.

To rebuild the cache automatically after daemon-owned syncs, set
`[analytics].auto_build_cache` in `config.toml`. See
[analytics settings](configuration.md#analytics) for snapshot freshness, builder
memory, threads, and temporary disk limits.

Build warnings count exported text values repaired with `U+FFFD` and identity
values exported as unknown. Invalid source message IDs, source conversation IDs,
and source identifiers export as empty strings while valid keys stay unchanged,
and invalid List-IDs export as `NULL` (see [List-Id recovery](#repair-list-ids)).
These counts cover only datasets written by that build or refresh, on every
platform. A source value written to two output columns counts twice, while
repeated identity comparisons do not increase the count. See
[encoding repair limits](#repair-encoding) for archive recovery.

---

## activity

Refresh contact activity and last-contact dates from archived messages.

```bash
msgvault activity build
msgvault activity build --backstop
```

The normal build resumes from its watermark, while `--backstop` rescans the full
archive. `msgvault serve` also runs this projection on the schedule configured
under `[activity]`.

---

## rebuild-fts

Rebuild the SQLite FTS5 search index.

```bash
msgvault rebuild-fts
```

Run this command if `verify` reports FTS5 shadow-table corruption such as a
malformed inverted index. It rebuilds the search index from the canonical
`messages` table.

---

## embeddings

Manage the vector embedding index used by `--mode vector` and `--mode hybrid`
search. You need a build with a vector backend (`sqlite_vec` for SQLite archives
or `pgvector` for PostgreSQL archives) and configured `[vector.embeddings]`
settings. See [Vector Search](/docs/usage/vector-search/) for prerequisites,
model rotation, and troubleshooting.

```bash
msgvault embeddings <subcommand> [flags]
```

| Subcommand | Description |
|---|---|
| `build` | Build or update the index. Incremental by default; `--full-rebuild` starts a new generation. |
| `resume` | Continue scan-and-fill embedding for the building or active generation. Incremental by default; `--backstop` also scans below the watermark. |
| `list` | List index generations with their state, model, dimension, and pending count. |
| `status` | Read or watch daemon coverage, worker activity, and batch timings. See [embeddings status](#embeddings-status). |
| `optimize [generation-id]` | Build or resume the local SQLite search accelerator, or remove it with `--drop`. |
| `prune` | Remove embeddings for hard-deleted messages. |
| `activate <generation-id>` | Activate a completed building generation, retiring the current active one. |
| `retire <generation-id>` | Retire a generation. |
| `prune` | Remove embeddings whose messages were hard-deleted. |

### embeddings build

```bash
msgvault embeddings build [flags]
```

| Flag | Description |
|---|---|
| `--full-rebuild` | Create a new index generation and rebuild from scratch. The new generation is activated atomically once coverage reaches zero. Same-model rebuilds keep serving the previous active generation in the meantime, but active-generation top-ups are frozen until activation; model or dimension changes return `index_stale` for vector/hybrid search until the new generation activates. If the daemon started before the settings changed, run `msgvault daemon restart` after the rebuild. |
| `--yes` | Skip the confirmation prompt that `--full-rebuild` otherwise requires. |
| `--backstop` | Run a full-scan pass that ignores the per-generation watermark to recover missing coverage skipped by incremental scans. Already-covered messages are skipped. |
| `--account <identifier>` | Limit embedding to this account, by identifier or display name — numeric source IDs are rejected (repeatable). Overrides `[vector.embed.scope] accounts` for this run; configured `message_types` still apply. After activating this one-off scope, add the equivalent accounts to config and restart the daemon before searching. |
| `--collection <name>` | Limit embedding to this collection's accounts (repeatable). Can be combined with `--account`; the scope is the union. This is a one-run override; persist the resolved accounts in config before restarting the daemon. |

Scheduled syncs can run while indexing is in progress. For daemon-wide SQL
logging, set `[log].sql_trace` or `[log].sql_slow_ms` in the daemon
configuration.

Without `--full-rebuild`, the command runs incrementally: it resumes any
in-flight rebuild matching the configured embedding settings, or scans for live
messages still missing coverage in the active generation, and then exits. You
can safely schedule it with cron or let `msgvault serve` run it using
`[vector.embed.schedule]`.

Because the account scope is part of the generation fingerprint, building with a
different `--account` or `--collection` set than the active generation requires
`--full-rebuild`, as when changing the model. See
[Scoped Generations](/docs/usage/vector-search/#scoped-generations).

### embeddings resume

```bash
msgvault embeddings resume [flags]
```

Continue embedding work and finish the current generation. If a generation
matching the configured embedding settings is building, this command embeds its
remaining rows and activates it once coverage reaches zero; otherwise it tops up
the active generation. It behaves like `msgvault embeddings build` with no
flags, except it never starts a full rebuild, and it accepts the same
`--account` and `--collection` scope flags as `embeddings build`.

Scheduled syncs can run while indexing is in progress. For daemon-wide SQL
logging, set `[log].sql_trace` or `[log].sql_slow_ms` in the daemon
configuration.

Pass `--backstop` to also scan below the per-generation watermark for missing
coverage. This fills gaps in the selected generation without starting a full
rebuild, and it activates a generation only if that generation is building.

```bash
msgvault embeddings resume --backstop
```

### embeddings status

```bash
msgvault embeddings status
msgvault embeddings status --json
msgvault embeddings status --watch --interval 5s
msgvault embeddings status --source 3
```

Read message embedding coverage and live worker activity from the daemon. Like
`multimodal status`, the JSON response begins with generation and coverage. It
targets a building generation that matches the configured settings, or the
active generation. A build started with `--account` or `--collection` reports
progress only in its own terminal. `status` never starts a build or acquires
the scheduler slot.

If vector search is disabled, `status` refuses to run until you set `[vector] enabled = true` and
restart the daemon.

`current` counts live messages stamped for this generation, including messages
with no text to embed, and `pending = eligible - current`. Run `embeddings list`
to see the split between embedded and blank messages. Coverage respects the
configured message types and sources. Passing `--source` requires an existing
source inside `[vector.embed.scope]`, including sources with no messages. It
narrows coverage to that source, while batch metrics, throughput, job state, and
failures still describe the full run across sources. ETA is `null` with
`--source`. The
daemon caches coverage counts for 15 seconds and runs one count per scope at a
time. When counts are older, `status` waits up to two seconds for a fresh count;
if counting takes longer or fails, it returns the previous counts (up to five
minutes old) while counting continues in the background. `coverage_sampled_at`
records when counting started, text output displays its age, and ETA is `null`
whenever counts are older than 15 seconds. `failed` counts failures in the
latest message-embedding pass across all sources, regardless of the selected
generation or `--source`. A successful pass with no work clears `failed` while
keeping prior batch timings. Batch error codes describe the sampled pass. Four
conditions end a pass without counting as errors: yielding the slot to a waiting
operation, cancellation such as daemon shutdown, retirement of the generation,
and a size rejection that the worker recovers from by splitting or truncating a
document. Messages can change while the sample is read.

Job state is `idle`, `queued`, or `running`. `queued` means a scheduled run is
waiting for the slot, including a post-sync pass waiting for sync to release it,
whereas a manual build waiting for the slot shows as `idle` along with the
current holder. Scheduler state reports the configured schedule, whether
embeddings run after sync, and the current slot holder (`none`, `sync`,
`embeddings`, or `other`). `sync` covers account syncs and scheduled source
syncs such as Slack or CardDAV, and slot labels never include source names.

The latest 20 finished batches and the current batch report attempted and newly
covered messages, input characters accepted by the provider, provider-call time,
HTTP request time, database-write time, total time, request attempts, retries,
and HTTP 429 responses. Provider time includes retry waits, while HTTP request
time excludes them. After an HTTP 429, the live phase becomes `retry_wait` with
elapsed seconds until the next request starts. Database-write time covers vector
publication and coverage stamps, excluding telemetry writes. These timings apply
to the message lane only; people work and activation can keep running after
`pending` reaches zero. Batches that call no provider, cover nothing, and fail
nothing are omitted. While a pass runs, the daemon saves the timing sample at
most once per second and tries to save at least every 15 seconds. Errors use
fixed public codes with no provider payloads. The final snapshot retries up to
five times, one second apart, on transient write failures, while cancellation
stops retries after one initial terminal write.

Throughput in both `status` and foreground progress divides newly committed
coverage by the summed durations of the last `eta_window` completed batches.
`status` uses at most its 20 retained batches, while foreground progress uses
the configured window, including values above 20. Time since the last completion
is excluded; live phase and elapsed seconds show current work instead. ETA
estimates pending message coverage, not activation, and is `null` before the
first finished batch, at zero throughput, after five minutes without a finished
batch, when idle, or when filtering coverage by `--source`. Earlier passes keep
their timing sample but cannot provide an ETA for a new job. A sample counts as
live only when it belongs to the active run, that run started during the current
job, and the sample was saved within the last minute. Interrupted invocations
show no current batch, rate, or ETA. An active run whose saves fall behind
reports phase `diagnostics_stale` and `diagnostics_stale=true`, with no current
batch, rate, or ETA until saves resume, and `diagnostics.updated_at` gives the
last save time. If diagnostics cannot be read or decoded, `status` still returns
coverage and job state with `diagnostics=null`.

`--watch` polls every five seconds by default; `--interval` must be at least one
second and is ignored for a single read. `--watch --json` writes one JSON object
per line until you interrupt it. If a poll fails, for example during a daemon
restart, the CLI prints the error on stderr and keeps watching. After a failed
poll, watch mode reconnects if the local daemon restarts on a new port, though
it never starts a daemon and stops if the restarted daemon is incompatible with
this client. The daemon CLI runner rejects `--watch`; remote callers poll
`GET /api/v1/embeddings/status` instead. Older generations have no timing sample
until a new embedding pass runs.

### embeddings list

```bash
msgvault embeddings list
```

Print one row per index generation showing its ID, generation state, model,
dimension, coverage, accelerator state and row count, accelerator timestamps and
last error, fingerprint, and generation timestamps.

Because listing reads existing metadata only, it works while other jobs hold the
archive. If the metadata still needs an upgrade, the command fails and prompts
you to run `msgvault daemon restart`, which applies the upgrade when vector
search is enabled.

### embeddings optimize

```bash
msgvault embeddings optimize [generation-id] [--drop]
```

Build or resume the SQLite approximate-search accelerator from vectors already
stored for a generation, defaulting to the active generation when you omit the
ID. This command never calls the embedding provider, and you can safely
interrupt and rerun it. Search does not use the accelerator until verification
and atomic publication finish. While it runs, the daemon's
operation gate pauses scheduled embedding and other gated writes, while searches
stay available. PostgreSQL archives do not need this command.

Pass `--drop` to remove the accelerator and its build state, including for a
retired generation, leaving exact vectors intact. Freed database pages become
reusable without shrinking the database file. Run optimization again after
substantial archive growth to retrain its fixed bucket count.

### embeddings prune

```bash
msgvault embeddings prune
```

Remove stored message embeddings whose source messages were hard-deleted.

### embeddings activate

```bash
msgvault embeddings activate <generation-id> [flags]
```

Activate a completed building generation and retire the current active one. By
default, this refuses to activate a generation that still has messages missing
coverage or a fingerprint that does not match your current config.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force` | Activate even with missing coverage or a fingerprint mismatch. |

### embeddings retire

```bash
msgvault embeddings retire <generation-id> [flags]
```

Mark a generation as retired. Because retiring the active generation leaves no
generation serving vector or hybrid search, doing so requires `--force-active`.

| Flag | Description |
|---|---|
| `--yes` | Skip the confirmation prompt. |
| `--force-active` | Allow retiring the generation that is currently active. |

### embeddings prune

```bash
msgvault embeddings prune
```

Remove vector rows whose source messages no longer exist. Your configured vector
backend must support orphan pruning.

`msgvault build-embeddings` stays available as a deprecated alias for
`msgvault embeddings build` with the same `--full-rebuild` and `--yes` flags.

---

## multimodal

Build, inspect, and search the optional visual attachment index. The workflow
requires `[vector.multimodal]` configuration and explicit consent for hosted
processing. Voyage also requires a probed capability manifest. See
[Visual attachment search](usage/vector-search.md#visual-attachment-search).

| Subcommand | Purpose |
|---|---|
| `probe --seeds <dir> --out <file> --yes` | Send synthetic fixtures to Voyage and write a capability manifest. `--fixtures` keeps the generated fixtures instead of using a temporary directory. |
| `build --yes` | Record consent for the configured generation and build the visual index. |
| `resume` | Continue a consented build. |
| `status [--json]` | Report generation state and coverage. |
| `retry --message <id> --hash <sha256>` | Retry one attachment occurrence. |
| `retire <generation-id> --yes` | Retire one generation and delete its vectors. Original attachments remain archived. |

Search by text or by one local JPEG, PNG, or WebP image:

```bash
msgvault multimodal search "a whiteboard timeline"
msgvault multimodal search --image ./reference.png
```

| Search flag | Default | Description |
|---|---|---|
| `--image <path>` | — | Use a local query image of at most 20 MiB instead of text |
| `--limit <count>` | `20` | Results to return; must be 1–100 |
| `--cursor <value>` | — | Continue from an opaque result cursor |
| `--sender-person <id>` | — | Attachments sent by one durable person |
| `--person <id>` | — | Attachments related to one durable person |
| `--participant <id>` | — | Attachments related to one observed participant |
| `--direction <value>` | — | `from_person`, `to_person`, or `group`; requires `--person` or `--participant` |
| `--source <id>` | — | Restrict to one source |
| `--message <id>` | — | Restrict to one owning message |
| `--filename <text>` | — | Case-insensitive filename substring |
| `--mime-prefix <text>` | — | Case-insensitive MIME prefix |
| `--after`, `--before` | — | `YYYY-MM-DD` sent-date bounds |
| `--json` | `false` | Emit JSON |

Pass either one text query or `--image`, not both. `--person` and
`--participant` are mutually exclusive, and `--sender-person` cannot be combined
with either of them or with `--direction`.

---

## eval

Measure how well search modes find messages you have rated. `eval` runs the same
full-text, vector, and hybrid retrieval paths used by production search, then
reports ranking quality, recall, latency, configuration, and input diagnostics.

```text
# topics.tsv: qid<TAB>query<TAB>optional-category
q1	quarterly planning	pointed
```

```text
# qrels.txt: qid iteration document-id relevance (1 or greater means relevant)
# Replace example-message-001 with a real source_message_id, not a local numeric ID.
q1 0 example-message-001 2
```

```bash
msgvault eval \
  --topics topics.tsv \
  --qrels qrels.txt \
  --modes fts,vector,hybrid \
  --limit 100
```

| Flag | Default | Description |
|---|---|---|
| `--topics <path>` | required | Tab-separated topics: `qid`, query, and optional category |
| `--qrels <path>` | required | Whitespace-separated judgments: `qid iteration docid relevance`; relevance of 1 or greater means relevant |
| `--modes <list>` | `fts,vector,hybrid` | Comma-separated modes to evaluate |
| `--doc-key <kind>` | `message` | Match judgments to `message` source IDs or `conversation` source IDs |
| `-n`, `--limit <count>` | `100` | Distinct documents retrieved per query |
| `--json` | `false` | Emit the report as JSON |
| `--rerank-jev <shapes>` | disabled | Opt in to `per-candidate`, `batched`, or both Jev shapes |
| `--rerank-top <count>` | `30` | Between 2 and 30 candidates; cannot exceed `--limit` |
| `--rerank-max-requests <count>` | `1000` | Maximum provider requests for the whole invocation |
| `--rerank-input-usd-per-million <amount>` | required when enabled | Input price supplied for this run |
| `--rerank-output-usd-per-million <amount>` | required when enabled | Output price supplied for this run |

`eval` opens the locally configured archive directly and does not use `[remote]`.
Vector and hybrid evaluation currently require a SQLite archive, an `sqlite_vec`
build, enabled vector configuration, and a compatible active generation. On
PostgreSQL, run `--modes fts`.

### Optional Jev reranking

`--rerank-jev` adds an evaluation arm after retrieval that sends the topic query
and up to 30 message candidates to TypeSafe's fixed Jev endpoint using
`TYPESAFE_API_KEY` from your environment. Selecting a shape acts as your
per-invocation consent to send that archived text. Because TREC judgments
identify messages, reranked arms accept only `--doc-key=message`, while
conversation judgments stay available for the baseline run.

Candidate text goes through the same body selection and cleanup used for
embeddings, sending only the query and cleaned subject and body (capped at 2048
UTF-8 bytes per candidate and 4096 bytes for the query). msgvault calls TypeSafe
through Docbank's `document/typesafe` client, which enforces request and
response size limits and runs at most eight calls at once. Each ranking gets a
40-second budget across all its calls, enough for four waves of 10-second calls
at 30 candidates, and its full elapsed time counts toward reported latency. Both
`per-candidate` and `batched` shapes evaluate the same retrieved messages in a
single invocation.

Cost equals returned input and output tokens multiplied by the prices you pass
on the command line. Before opening the archive, `eval` rejects any run whose
worst-case request count (judged topics times modes times requests per ranking)
exceeds `--rerank-max-requests`. Because msgvault has no local spend limit,
verify an enforced TypeSafe account or order cap that rejects charges above the
limit before running a live study; a displayed balance or alert does not prove
that behavior. A response missing token usage fails that arm, and the first
provider failure stops reranking for the run. The report keeps the baseline and
token subtotals collected before the failure, sets `usage_complete=false` to
mark them partial, prints `unknown` in the table (`null` in JSON) for unknown
cost, counts requests and tokens for completed rankings only, and exits nonzero.

Example with placeholder prices:

```bash
TYPESAFE_API_KEY=replace-me msgvault eval \
  --topics topics.tsv --qrels qrels.txt --modes fts,vector,hybrid \
  --limit 100 --rerank-jev per-candidate,batched --rerank-top 30 \
  --rerank-input-usd-per-million 1 \
  --rerank-output-usd-per-million 2
```

Reranked p95 covers retrieval, shared candidate preparation, and that shape's
provider calls. A live TREC Legal 2010 study also needs a permitted matching
message collection, topics, qrels, source-ID mapping, and the enforced account
cap. Consider a search integration only if a complete hybrid shape improves
Hit@10 by 0.05 absolute and keeps end-to-end p95 at or below 2000 ms on
the same corpus, topics, qrels, retrieval settings, and top N of 30. Report the
paired topic denominator and changes for both shapes. The local threaded fixture
checks wiring only.

---

## cache-stats

Show statistics about the analytics cache.

```bash
msgvault cache-stats
```

This command queries your configured msgvault server over HTTP. With a local
configuration, the CLI auto-starts or reuses the local daemon to read the
analytics cache files; with a remote configuration, the remote server reports
its own cache state.

---

## query

Run arbitrary SQL against the Parquet analytics cache using an in-memory DuckDB
engine.

```bash
msgvault query <sql> [flags]
```

A usable stale cache stays queryable while refresh work runs. JSON output
includes `cache.published_at`, plus `stale_reason` and `pending_additions` when
known. If a fresh publication is required before rows can be returned, the
command reports the build job on stderr, waits for it, and then returns the
results, exiting with an error if the build fails or the wait is interrupted.
Pass `--fresh` to wait for a freshness check covering archive writes committed
before your request, rebuilding if needed, before returning rows.

| Flag | Default | Description |
|---|---|---|
| `--format` | `json` | Output format: `json`, `csv`, or `table` |
| `--fresh` | `false` | Wait for a freshness check and any required rebuild before returning rows |

See [SQL Queries](/docs/usage/querying/) for available views and example queries.

---

## credentials

Manage provider keys on the daemon host without the Web UI:

```sh
msgvault credentials set <id> --from-file PATH [--endpoint URL]
msgvault credentials set <id> --stdin [--endpoint URL]
msgvault credentials list [--json]
msgvault credentials import-env
```

Pass either `--from-file` or `--stdin`, not both. Supported IDs are
`vector.embeddings`, `vector.multimodal`, `people.enrichment/<name>`, and
`people.enrichment/suppression`. The configured provider supplies the default
endpoint, while suppression keys have no endpoint. `list` prints IDs and bound
origins, never key values, and `import-env` copies present configured
environment keys without overwriting keys you already stored. See
[stored credentials](configuration.md#stored-provider-credentials) for input
security, runtime precedence, and sweep-provider commands.

## mcp

### Discover running HTTP listeners

Run `msgvault mcp status --json` to list HTTP MCP listeners started by this
version. The command reads local runtime records without starting a server. Each entry includes `transport`, `url`,
`pid`, `backend_url` when known, and `token_path` when the listener requires a
bearer token. Read the token from that private file; status output never prints
it.

Each listener publishes its actual bound port after startup (including when
started on port zero) and removes its record on orderly shutdown. `status` skips
records whose process has exited or whose process-start identity cannot be
verified, including when another process reuses the PID. Stdio sessions do not
listen on a port and do not appear. An empty JSON list means no HTTP listeners
were found in this application's configured data directory.

Start the Model Context Protocol server to connect AI assistants to your
archive.

Draft tools prepare and manage drafts through the selected daemon with the same
commands and permissions as the CLI, and msgvault never sends messages. The
owner gets eight draft tools.

When you pass `--agent-url` and `--agent-token-file`, `msgvault mcp` serves
scoped read tools, up to six delegated draft tools, and calendar tools over
stdio. Available tools follow the token's permissions and granted sources, and
the daemon enforces those checks on every call. Delegated sessions refuse `--http`.

```bash
msgvault mcp [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--force-sql` | `false` | Deprecated in 0.17.0; use `[analytics].engine = "sql"` in `config.toml` instead. See [Configuration: analytics](/docs/configuration/#analytics). |
| `--no-sqlite-scanner` | `false` | Deprecated in 0.17.0; cache engine selection is daemon-managed. Use `[analytics].engine = "sql"` for live SQL. |
| `--http` | — | Serve MCP over StreamableHTTP on this address instead of stdio. Bare ports bind to loopback, e.g. `8080` becomes `127.0.0.1:8080`. Non-loopback addresses require an effective inbound key or `--http-allow-insecure`. |
| `--http-token-file` | — | Read an independent inbound bearer key from an owner-only file; takes priority over `--http-token-env`. Requires `--http`. |
| `--http-token-env` | — | Name the environment variable holding an independent inbound bearer key. Requires `--http`. |
| `--http-allow-insecure` | `false` | Allow non-loopback HTTP binding without an effective inbound key. A configured key is still enforced; without one, use only behind a trusted network boundary or authenticated reverse proxy. |
| `--http-allow-writes` | `false` | Expose native email tag edits, Saved View management, attachment export, deletion staging, and managed draft writes over StreamableHTTP. Profile, source, calendar, identity, CardDAV, and Kata writes also need their separate opt-ins; see [write controls](usage/chat.md#write-controls). Enable only for trusted, authenticated clients. |
| `--allow-profile-writes` | `false` | Expose person promotion, display-name edits, and private Notes writes. HTTP also requires `--http-allow-writes`. |
| `--allow-curated-writes` | `false` | Expose atomic structured person profile patches. Each call requires explicit client confirmation; the client is responsible for obtaining user approval. HTTP also requires `--http-allow-writes` and protocol `2026-07-28` or newer. |
| `--allow-attribute-writes` | `false` | On unreleased `main`, expose person attribute and definition writes that require client confirmation. HTTP also requires `--http-allow-writes` and protocol `2026-07-28` or newer. |
| `--allow-loop-writes` | `false` | Expose open-loop done, snooze, and reopen decisions. Each call requires approval. HTTP also requires `--http-allow-writes` and protocol `2026-07-28` or newer. |
| `--confirm-sensitive-drafts` | `false` | Require client confirmation before creating or editing drafts with detected sensitive content. Grants no tools; HTTP writes still require `--http-allow-writes`. |
| `--allow-import-writes` | `false` | Expose bounded historical import creation with per-call confirmation. HTTP also requires `--http-allow-writes` and protocol `2026-07-28` or newer. |
| `--allow-source-writes` | `false` | Expose source sync and Slack selection writes. Each call requires confirmation. HTTP also requires `--http-allow-writes` and protocol `2026-07-28` or newer. |
| `--allow-calendar-writes` | `false` | Expose calendar event mutation tools. HTTP also requires `--http-allow-writes`; only enable for sessions where the user explicitly authorizes calendar writes. |
| `--allow-kata-writes` | `false` | Expose `create_kata_issue` and `link_kata_evidence`; `find_kata_issues` and `get_kata_issue_context` need no flag. HTTP also requires `--http-allow-writes`; archive text is untrusted input, so only enable for sessions where the user explicitly authorizes Kata issue writes. See [Kata issues](usage/kata-issues.md). |

See [MCP Server](/docs/usage/chat/) for configuration and the full tool
reference.

For Kata person agendas, `get_person_agenda` reads live open tasks and returns
the person's canonical vCard UID and aliases. msgvault's agenda tool is
read-only. Use Kata's MCP tools to create or update tasks, setting the scalar
`msgvault.person` metadata value to the returned canonical UID and
`msgvault.list` to the list name (`agenda` by default). See
[Kata configuration](configuration.md#integrationskata) for setup and limits.

---

## agent setup

Run `msgvault agent setup` to install skills and connect the MCP server for Claude Code and Codex. Pass
`--agent claude,codex` to choose agents, `--check` to check readiness, `--json`
for structured output, or `--print` to print manual MCP commands.

Setup registers `msgvault mcp`, and the agent starts it with its own
environment.

## skills

Install or remove the bundled msgvault agent skills:

```bash
msgvault skills install
msgvault skills uninstall
```

`skills install` detects Claude Code and Codex from their agent directories and
writes four bundled skills (`msgvault-analytics`, `msgvault-attachments`,
`msgvault-feedback`, and `msgvault-search`) to their user-level skill
directories. Existing generated copies update in place, while files without
msgvault's generation marker stay untouched unless you pass `--force`. Set
`CLAUDE_CONFIG_DIR` or `CODEX_HOME` to override the default agent directories.

| Install flag | Description |
|---|---|
| `--agent claude` / `--agent codex` | Restrict installation to one or more detected agents; repeat or comma-separate values |
| `--dir <path>` | Install into an explicit skill directory instead of detected agents |
| `--force` | Overwrite skill files that no longer carry the msgvault generation marker |

`skills uninstall` accepts `--agent` and `--dir` with the same targeting rules
and deletes only generated copies that still carry the marker. See
[Agent Skills](/docs/guides/agent-skills/) for the workflow and safety model.

---

## openapi

Print the checked-in msgvault OpenAPI contract without starting the daemon or
opening the archive database.

```bash
msgvault openapi [flags]
```

| Flag | Default | Description |
|---|---|---|
| `--version` | `3.1` | OpenAPI version to emit: `3.1` or `3.0` |
| `--format` | `yaml` | Output format: `yaml` or `json` |

The OpenAPI `info.version` is the API schema version rather than the msgvault
binary version, so you can check compatibility between a local or remote CLI and
a server. Running daemons also serve this contract at `/openapi.json`, and
generated client artifacts build from the OpenAPI 3.0 output for tool
compatibility.

---

## daemon

Manage the local background daemon used by HTTP-backed CLI commands.

```bash
msgvault daemon start
msgvault daemon status
msgvault daemon stop
msgvault daemon restart
```

`start` launches the daemon in the background, `status` shows its recorded URL,
PID, version, API schema, uptime, and lifetime, `stop` shuts it down, and
`restart` stops and starts it again. When `[server].daemon_auto_restart = "newer"`,
starting a newer compatible binary replaces an older recorded daemon. A
source-build CLI also replaces the release daemon it was built from or an
earlier release, while a daemon from another source build of the same release
keeps running and the command prints the restart command. If an incompatible
daemon is running, the CLI reports it and asks you to stop it first.

These lifecycle commands take no command-specific flags; they read `config.toml`
and any [runtime environment overrides](configuration.md#environment-variables).
See [Web UI & API Server](/docs/api-server/) for endpoints, or inspect the
generated OpenAPI contract with `msgvault openapi` or `/openapi.json` on a
running server. See [Configuration](/docs/configuration/#server) for server
options. When vector search is enabled, the daemon can also run the embed worker
on a cron schedule, after every successful sync, or both; see
[Configuration: vector.embed.schedule](/docs/configuration/#vectorembedschedule).

Daemons auto-started by a CLI command shut down after
`[server].daemon_idle_timeout` with no requests (`20m` by default; set it to
`"0s"` to disable idle shutdown, or override it for auto-started daemons with
`MSGVAULT_DAEMON_IDLE_TIMEOUT`). Daemons launched by `daemon start` and
`daemon restart` stay up until you stop them. If `daemon start` reuses an
auto-started daemon, it keeps that daemon's idle lifetime and prints
`msgvault daemon restart` if you want it to stay running.

`[server].daemon_auto_restart` controls how local commands handle a version
mismatch between the CLI and the recorded daemon. The default `newer` restarts
older compatible daemons (and a source-build CLI replaces the release daemon it
was built from or an earlier release, while leaving another source build of the
same release running and printing the restart command). `never` leaves restarts
to you or your supervisor, and `always` restarts on any version mismatch that is
safe for the current API schema. Whenever automatic replacement is skipped for
an eligible daemon, the CLI prints both versions and `msgvault daemon restart`
on stderr.

For compatibility with existing scripts, `msgvault serve start|status|stop|restart`
still works without warnings, though those aliases stay hidden from help and
shell completion.

---

## serve

Start the Web UI and API server in the foreground, with optional background sync
scheduling.

```bash
msgvault serve
```

`msgvault serve` runs in the foreground until you stop it and never shuts down
for being idle. Use `serve` for externally supervised, Docker, and NAS
deployments, and `msgvault daemon` for local background management.

`--bind ADDRESS` and `--port PORT` override environment variables, `config.toml`,
and defaults, and port `0` picks an open port. `--bind iface:NAME` resolves a
named network interface at startup and fails before binding if no usable address
is found. See [runtime configuration](configuration.md#environment-variables)
for environment-only deployments and persisted API keys.

```sh
msgvault serve --bind 0.0.0.0 --port 8080
```

`/mcp` serves the read-only MCP catalog on this listener using the configured
owner bearer key; native MCP never allows keyless access.

| Flag | Default | Effect |
|---|---|---|
| `--mcp-allow-writes` | `false` | Enable the independent `/mcp/writes` catalog; `/mcp` stays read-only. |
| `--mcp-allow-profile-writes` | `false` | Enable profile promotion, display-name and Notes writes. |
| `--mcp-allow-curated-writes` | `false` | Enable structured person profile patches requiring explicit client confirmation; the client is responsible for obtaining user approval. |
| `--mcp-allow-identity-decisions` | `false` | Enable confirmed identity match accept/reject calls. |
| `--mcp-allow-identity-scoring` | `false` | Enable confirmed manual scoring using the fixed Jev provider. |
| `--mcp-allow-person-merges` | `false` | Enable confirmed local person merges. |
| `--mcp-allow-carddav-writes` | `false` | Enable confirmed CardDAV publication and sync. |
| `--mcp-allow-calendar-writes` | `false` | Enable calendar mutations. |
| `--mcp-allow-kata-writes` | `false` | Enable Kata issue creation and evidence linking. |
| `--mcp-allow-attribute-writes` | `false` | On unreleased `main`, enable person attribute and definition writes that require client confirmation. |
| `--mcp-allow-loop-writes` | `false` | Enable approved open-loop done, snooze, and reopen decisions. |
| `--mcp-confirm-sensitive-drafts` | `false` | Require client confirmation before creating or editing drafts with detected sensitive content. Writes still require `--mcp-allow-writes`. |
| `--mcp-allow-import-writes` | `false` | Enable confirmed bounded historical import creation. |
| `--mcp-allow-source-writes` | `false` | Enable confirmed source synchronization and policy writes. |

Every family flag also requires `--mcp-allow-writes`. These are process-level
flags, and auto-started local daemons default to read-only. See the
[MCP guide](usage/chat.md#native-daemon-endpoints) for client setup and
confirmation.

---

## setup

Run the first-run setup wizard. It writes `config.toml`, offers the detected
host IANA timezone for activity day boundaries, and includes the effective zone
in any NAS bundle it creates. If a daemon is already running, restart it to apply
the change; later projection passes re-date existing activity rows
automatically. Setup can also save a Google OAuth credential (needed only for
Gmail and Google Calendar; press Enter to skip) and configure a remote
deployment.

```bash
msgvault setup
```

When configured for a remote server, `setup` creates `<MSGVAULT_HOME>/nas-bundle`
containing:

- `config.toml` ready for container deployment
- `client_secret.json`, if you configured a Google OAuth credential
- `docker-compose.yml`

The wizard also saves the remote URL and API key in the `remote` config block so
`export-token` can use them without extra flags.

### setup providers

Configure optional search and people features based on your available API keys.
The command checks `VOYAGE_API_KEY` and `OPENAI_API_KEY`. For each unset text
feature, it picks Voyage contextual embeddings first, then OpenAI embeddings,
then an available Ollama `nomic-embed-text` model, and finally the shared local
server. Hosted people inference requires an explicit `--provider` and `--model`;
an OpenAI key alone does not select it. Without an OpenAI key or explicit
provider choice, setup can offer the configured local Ollama chat model. Once
running, the daemon never falls back to another provider after a failure.

Setup prints a plan, prompts once per hosted provider, writes `config.toml`, and
prints the remaining commands to run. It onboards a new people-sweep provider
through the same check and consent gates as `person provider`, while other
feature-specific consents stay separate. Visual search requires a valid probe
manifest before setup enables it, and Docbank processes mirrored documents. See
[Recommended Configuration](usage/recommended-configuration.md).

Local people-sweep setup stays pending until you pass `--allow-sensitive`. An
explicit `--provider` requires `--allow-sensitive=true|false` along with
explicit `--retention-posture` and `--training-posture` assertions. Only `true`
allows sending sensitive archive excerpts and inferring sensitive personal
attributes; `false` allows the synthetic check, while real sweeps cannot process
evidence. Passing `--yes` alone does not grant this permission. If a sweep is
already enabled, setup rejects `--provider`; run `person provider add` and
`person provider use` to switch profiles. Vector lanes also stay pending if your
binary lacks the backend required by the configured database, in which case
setup prints rebuild instructions.

Saved retention and training postures on disabled lanes stay intact unless you
explicitly pass their posture flags. Custom hosted text endpoints require
explicit configuration of dependent lanes. Search mirrored document text with
`media search`, `GET /api/v1/media/search`, MCP `search_media`, or the Web
**Everything** panel; run `media search --person <id> --direction from_person`
to scope document search to a person. See
[Docbank configuration](https://msgvault.io/docs/configuration/#search-document-text-through-docbank).

Passing `--local-embeddings` switches text search to the shared local server
even when Ollama is running. Setup prompts once before downloading missing model
and runtime files, or accepts that download automatically with `--yes`.
`--dry-run` leaves downloads and configuration untouched. A matching Nomic
message recipe keeps its index, while a different model requires
`msgvault embeddings build --full-rebuild --yes`.

Without `--local-embeddings`, setup preserves any existing text model or
endpoint even if the feature is disabled, and does not replace it when a
different key appears. Explicit text and visual schedule keys, including an
empty cron and `run_after_sync = false`, also stay untouched. Setup does not
configure source syncs, track people, enroll briefs, or set provider prices and
monetary caps.

```bash
msgvault setup providers --dry-run
msgvault setup providers
msgvault setup providers --allow-sensitive
msgvault setup providers --yes
```

| Flag                   | Default             | Description                                                                                                                                                                                                       |
| ---------------------- | ------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `--provider` | absent | Explicit people inference preset: `openai`, `openrouter`, or `venice`; requires model and privacy decisions |
| `--model` | absent | Required model ID for the selected preset |
| `--credential-env` | absent | Host environment-variable name for the preset key; mutually exclusive with `--api-key-stdin` |
| `--api-key-stdin` | `false` | Read the preset API key from standard input; otherwise a terminal can prompt for it |
| `--yes`                | `false`             | Accept every provider disclosure without prompting. Required to apply a plan with hosted-provider prompts when stdin is not a terminal or `--json` is used. It does not grant the separate sensitive-data opt-in. |
| `--allow-sensitive`    | `false`             | Allow the people sweep to send sensitive archive excerpts and infer sensitive personal attributes                                                                                                                 |
| `--dry-run`            | `false`             | Print the plan, the disclosures, and the current lane report without writing                                                                                                                                      |
| `--retention-posture`  | `provider-declared` | Retention assertion recorded for embedding and inference providers                                                                                                                                                |
| `--training-posture`   | `provider-declared` | Training assertion recorded for embedding and inference providers                                                                                                                                                 |
| `--json`               | `false`             | Output the plan, applied flag, follow-ups, and report as JSON                                                                                                                                                     |

This command edits the config file on your local machine and refuses to run
against a configured remote daemon; run it on the daemon host or pass `--local`.

### setup status

Report the setup state of each feature (text search, semantic people search,
visual attachments, people sweep, activity projection, and media policy): its
state, provider, model, recorded consent, schedule, why a lane is off, and the
command to turn it on. `setup status` also checks Claude Code and Codex for
msgvault, and Codex may start `msgvault mcp` and the daemon, which can create
files under the msgvault home.

`setup status` reads `config.toml`, your environment, stored provider
credentials, and the local archive's consent records without contacting any
provider. An `on` state means setup checks passed, not that an index is already
built or that a provider responds. The MCP summary is a partial list inferred
from enabled config flags rather than a live `tools/list` response. To inspect
index progress and coverage, run the feature's status command or check
[Settings Overview](web-ui.md#settings-and-restart-behavior).

```bash
msgvault setup status
msgvault setup status --json
```

---

## docbank attachments

Manage the optional Docbank folder mirror through the selected msgvault daemon.

```bash
msgvault docbank attachments status
msgvault docbank attachments backfill
```

`status` reports the mirror state and destination folder, delivered, sending,
and failed file counts, failure reasons, and all distinct eligible content
hashes that have no delivery record for that destination. Pass `--json` for
structured output. When `document_profile` is set, a running mirror also prints
`Last error` (if set), `Document profile`, `Text extracted`, `Processing`,
`Waiting`, and `Without text (<code>): N` for each failure or refusal code,
reflecting local job outcomes for the profile fingerprint last read by the
worker.

`backfill` restarts discovery and retries failed uploads, and requires
attachment upload consent plus a running attachment mirror job. It resets
discovery and requeues failed files with reset attempt counts in one
transaction. Backfill requests processing again for files that failed due to
missing consent, stale authority, a missing PDF password, an unreadable password
file, a quarantined job, or a malformed Docbank answer. Backfill clears only
settled recoverable failures. A job that fails afterward needs another backfill,
and other outcomes wait for a profile change. The daemon drains the backlog at
upload speed. The mirror includes files on dedup-hidden messages, skips files
whose attachment rows were removed, and names uploads
`<first 12 hex of hash>-<sanitized basename>`. See
[attachment mirror configuration](configuration.md#mirror-stored-attachments-to-docbank)
for processing behavior.

## show-message

Show full details for a single message (`show` is an alias).

```bash
msgvault show-message <id> [flags]
msgvault show <id> --body-only --strip-quoted
```

| Flag | Description |
|---|---|
| `--json` | Output as JSON; cannot be combined with `--body-only`. |
| `--body-only` | Print plain body text without metadata or borders; fall back to the stored snippet. |
| `--strip-quoted` | Remove quoted reply history and conventional signatures; preserve complete URLs and uncertain wrapped headers. |

Default output prints the full message. `--strip-quoted` is a display-only
heuristic that never alters the archive: it strips `>` lines, complete
single-line dated `On ... wrote:` headers before quoted lines, and `-- `
signature blocks, while leaving Outlook `-----Original Message-----` blocks,
non-English headers, unquoted inline and bottom-posted replies, complete URLs,
uncertain wrapped headers, and ordinary prose intact. In JSON output, cleaning
updates `body_text` while keeping the original `body_html`.

JSON output includes `web_url` when the selected daemon can provide a browser
link. JSON output also includes the best-effort `hidden_text_present` flag. A
`true` value means stored HTML likely hides readable text. The check uses
inline-style heuristics plus `hidden` and legacy color attributes; separately,
non-blank HTML that fails to parse also sets the flag. When a message is
flagged, text output writes
`Warning: this message's HTML may hide text.` to stderr (including with
`--body-only`) without changing the message body. The check skips stylesheets
and browser layout. `false` does not guarantee all text is visible. Many
newsletters hide a preview line (a preheader) that triggers the flag. `true`
marks hidden text rather than malicious intent. MCP
`get_message` and `search_in_message` return the same stored flag, and CLI
`search --json` and MCP `search_message_bodies` include it on every result row.
Archive schema 16 backfills older mail once at archive initialization, and new
HTML writes refresh the flag automatically.

## show-thread

Read a conversation in chronological order with quoted reply history stripped,
using any message ID in the conversation as the anchor.

```bash
msgvault show-thread <message-id> [--limit N] [--offset N] [--json] [--strip-quoted=false]
```

Pages default to 100 messages (up to 500), ordered by timestamp instant and ID
with undated messages last. SQLite compares instants to the millisecond and
breaks ties by message ID. Pass `--offset` to read later pages; a notice prints
on stderr when more messages exist. Archived source-deleted messages stay
included, while dedup-hidden rows are excluded. The command talks to your
selected daemon and returns an upgrade error against unsupported older daemons.

A positive numeric argument checks internal message IDs first and falls back to
a provider ID if no internal match exists; ambiguous provider IDs require an
internal message ID from `search` output. An offset past the end of the thread
returns an empty page. JSON output includes `conversation_id`, `total`,
`offset`, `has_more`, and `messages` with cleaned `body_text`. Text and JSON
share the same `--strip-quoted` behavior; pass `--strip-quoted=false` to keep
archived body text unchanged.

Each message in `show-thread --json` carries `hidden_text_present` using the
same check as `show-message`, and text output prints the stderr warning once if
any message on the page is flagged.

## family show

Show other archived copies of an email, along with the replies and forwards that
reference it, across accounts and conversations.

```bash
msgvault family show <message-id> [--json]
```

Each row prints a message ID, account, role (sent, received, or draft), date,
and the header evidence tying it to the anchor email: the same message, a reply,
a reply it answers, a forward, a forwarded original, shared references, a
relation through another copy, or only the same provider thread. Rows appear in
ascending message ID order, with the anchor email marked `"this message"`.
Relations come from `Message-ID`, `In-Reply-To`, `References`, and
attached-original headers, plus the rest of the provider threads of a Gmail or
Google Groups email and its exact copies, so a copy in another account without
any of those stays unlinked. Because other sources derive conversations from
headers, their conversations do not count as separate evidence.

Copies can carry status notes explaining missing links:

- **not indexed yet.** Relations may be missing; sync or run
  [`repair-derived`](#repair-derived) to index it.
- **headers not fully read.** The message had an oversized payload, unreadable
  headers, or too many identifiers, and the copy may also have hidden relations;
  re-indexing does not change that.
- **original message not stored** or **stored format not readable.** No raw
  message is available to read; storing the message again clears this state.
- **no stored Message-ID.** The message was indexed, but replies that name it
  cannot match it.

If any email in the archive is still waiting to be indexed, stderr notes that
related emails may be missing because that email could be one the lookup cannot
see yet. Lookup stops at 500 emails or 5,000 identifiers and prints which cap it hit
on stderr.

JSON output includes `anchor_message_id`, `members`, `truncated`,
`truncated_reason`, and `pending_linkage`, with `linkage_outcome` and `evidence`
on each member. This command uses your selected daemon and requires API schema
5.12.0 or newer. Only the owner can run it; delegated grants are refused.

---

## list-accounts

List every archived email, chat, meeting, and file-import source. JSON rows
include `id`, `email`, `type`, `display_name`, and `last_sync`; pass `email` to
`--account` or `id` to `--source-id` on other commands, or `--source <type>:<email>`
to `export-messages`.

Table and JSON output also show the archive alias, history-only state,
retirement destination, and Beeper re-anchor marker. JSON rows also include
`message_count` and `source_deleted_count`; both are always current.

```bash
msgvault list-accounts [flags]
```

| Flag | Description |
|---|---|
| `--json` | Output as JSON |

---

## update-account

Update account settings through your configured remote server or local daemon;
pass `--local` to target the local daemon even when a remote is configured.

```bash
msgvault update-account [account] [flags]
```

| Flag | Description |
|---|---|
| `--display-name` | Set a display name for the account |
| `--alias ALIAS` | Set a unique archive alias without changing the provider identifier, credentials, or sync cursors |
| `--history-only=true` | Keep a Beeper source in the archive and exclude it from scheduled sync |
| `--history-only=false` | Make a Beeper source eligible for sync again |
| `--accept-reanchor` | Clear a Beeper re-anchor marker after manually verifying the replacement account's anchors |
| `--source-id ID` | Update exactly one source by numeric ID; mutually exclusive with the account argument |

Pass either an account argument (an identifier, alias, or unique display name)
or `--source-id`. Aliases must be nonempty, contain no leading or trailing
whitespace or control characters, and stay unique across all source identifiers
and aliases. All settings in one command apply atomically.

To keep history for a Beeper account that no longer exists and suppress
scheduled sync warnings, run:

```bash
msgvault update-account --source-id 42 --history-only=true
```

History-only accounts still work with `backfill-beeper-media` to download
attachments for already archived messages, keep any re-anchor marker for
inspection, and reject manual sync until you set `--history-only=false`. After
verifying the live account and its anchors, clear the marker explicitly:

```bash
msgvault update-account --source-id 42 --history-only=false --accept-reanchor
```

Accepting the marker records your decision by clearing it without verifying
anchors or changing provider cursors. You cannot reactivate or update a retired
source.

---

## merge-account

Move one source's archived messages into a compatible destination and retire
the original source.

```bash
msgvault merge-account --from historical-alias --into live-account --dry-run --json
msgvault merge-account --from-source-id 42 --into-source-id 43 --yes
```

| Flag | Description |
|---|---|
| `--from ACCOUNT` | Historical source identifier, alias, or unique display name |
| `--into ACCOUNT` | Destination identifier, alias, or unique display name |
| `--from-source-id ID` | Exact historical source ID; mutually exclusive with `--from` |
| `--into-source-id ID` | Exact destination source ID; mutually exclusive with `--into` |
| `--dry-run` | Run the compatibility and duplicate checks and report counts without changing archive data |
| `--yes` | Confirm and perform the source-retiring merge; required unless `--dry-run` is set |
| `--json` | Output the merge report as JSON |

Both sources must share the same source type and be idle, and known conflicting
Beeper networks cannot merge. Pending provider drafts on the historical source
must finish first. The destination cannot be retired, though it may be
history-only. Because batches targeting a retired source cannot execute, cancel
or finish staged deletions before merging and restage them against the
destination if needed.

The merge namespaces incoming provider message and conversation IDs and hides
only unambiguous duplicate pairs matched by an email `Message-ID` or a full
payload fingerprint; ambiguous matches stay visible and appear in
`ambiguous_matches`. Original message bodies, raw payloads, attachments,
deletions, and provider receipts remain intact as evidence, though historical
attachments cannot fetch from the destination's provider endpoints.

The daemon recalculates conversation counts and previews, identity direction,
and derived-data revisions, and invalidates embeddings for rebuilding. Provider cursors, checkpoints, and completed sync runs stay on the
original source rather than transferring resume state, destination re-anchor
markers stay unchanged, and the retired source can no longer sync or accept
imports.

Retrying a committed merge into the same destination returns the saved report,
while retrying into a different destination fails. A later Beeper history
backfill may fetch a new live copy of a namespaced historical message; those
copies stay visible because msgvault cannot infer provider identity from content
alone.

These maintenance commands require owner access; delegated API keys cannot
preview or run them.

---

## remove-account

Permanently remove an account or source and all of its archived data (messages,
labels, sync state, unshared credentials, and unique attachment files) from the
selected archive. This cannot be undone, though it never touches the remote
provider.

```bash
msgvault remove-account [account] [flags]
msgvault remove-account 123456789012345678 --type discord
msgvault remove-account --source-id 42
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip the confirmation prompt (and allow removal when an active sync is in progress) |
| `--dry-run` | Check removal eligibility without deleting data |
| `--type` | Source type to remove when the same identifier exists across source types (`gmail`, `imap`, `mbox`, `discord`, etc.) |
| `--source-id ID` | Remove exactly one source by numeric ID; mutually exclusive with the account argument and `--type` |

The account argument accepts an identifier or a unique display name.

`remove-account` refuses any source that participated in a merge, including the
live destination, before asking for confirmation. The source records preserve
merge provenance and prevent later imports from recreating a retired account.

Attachment files are removed only when no other account references the same
content hash. A Discord bot token stays stored while another registered guild
uses it and is deleted once its last source is removed. The daemon rebuilds the
shared Parquet analytics cache automatically.

---

## stage-delete

Stage active messages as a pending deletion batch using either Gmail-like search
criteria or a comma-separated list of internal message IDs. Query mode runs the
daemon's search and preflight checks before staging, while ID mode sends
`message_ids` directly to the daemon. Staging never deletes messages from a
provider.

In query mode, staging refuses to run while the daemon is verifying or
rebuilding its full-text search index, or while the analytical cache is
unavailable, because an incomplete index could silently miss matching messages;
both resolve in the background, and you can retry once they finish. Like deletion staging
in the TUI and Web UI, query mode also resolves against the committed analytical
snapshot, so messages synced after the last cache build do not appear until the
daemon refreshes the cache (which happens automatically after each sync). Run
the command again after the refresh to stage newly synced matches.

```bash
msgvault stage-delete <query>
msgvault stage-delete --ids 123,456,789
```

Pass either `<query>` or `--ids IDS`, not both; `--ids` is also mutually
exclusive with `--source-id`.

| Flag | Description |
|---|---|
| `--dry-run` | Show the staged subset and skipped counts without creating a deletion batch |
| `--source-id ID` | Restrict staging to one exact source ID |
| `--ids IDS` | Stage positive, unique, comma-separated internal message IDs instead of a query |

Query staging preflight reports the exact deletable subset. Queries follow the
same search rules as `msgvault search`, and `--dry-run` previews the set staging would create.

Deletion staging applies only to email from Gmail and Microsoft Graph mail
(`msmail`) sources. If a query also matches chats, meetings, calendar entries,
or mail from other sources such as IMAP or Apple Mail imports, staging stages
the deletable subset and reports how many items it skipped, failing only when
nothing deletable matches. Legacy Gmail messages imported before message types
existed carry a blank type and count as email, so `message_type:email` stages
them too.

In ID mode, the daemon resolves live targets and source boundaries for the
requested IDs directly. IDs that do not map to live deletable messages with
provider message IDs are omitted, so the reported count reflects the targets the
daemon resolved. The CLI does not search, check FTS readiness, call Explore or
preflight, or require the analytical cache to be ready, though a newly started
local daemon may initialize its cache in the background for later queries.

If a query resolves to a single source, `--source-id` is optional. For a
multi-source query, specify `--source-id` for each source:

```bash
msgvault stage-delete --source-id 42 "from:newsletter@example.com older_than:1y"
```

Review the staged batch with `msgvault show-deletion <batch-id>`, then run
`msgvault delete-staged <batch-id>` to execute it.

---

## list-deletions

List pending and recent deletion batches.

```bash
msgvault list-deletions
```

---

## show-deletion

Show details of a deletion batch.

```bash
msgvault show-deletion <batch-id>
```

---

## cancel-deletion

Cancel pending or in-progress deletion batches. Run it without a batch ID to
list available batches.

```bash
msgvault cancel-deletion [batch-id]
msgvault cancel-deletion --all
```

| Flag | Description |
|---|---|
| `--all` | Cancel all pending and in-progress batches |

---

## delete-staged

Execute staged remote deletions. By default, Gmail and IMAP move messages to
Trash and Microsoft Graph mail moves them to Deleted Items. Passing
`--permanent` uses Gmail batch deletion, IMAP `UID EXPUNGE` (which requires
`UIDPLUS`), or Graph `permanentDelete`. The first deletion on a Graph mail
account prompts to upgrade its token to `Mail.ReadWrite`. Trash recovery depends
on the provider; see [deletion behavior](usage/deletion.md).

```bash
msgvault delete-staged [batch-id] [flags]
```

| Flag | Description |
|---|---|
| `-y`, `--yes` | Skip confirmation prompt |
| `--permanent` | Permanently delete through Gmail batch deletion, IMAP UID EXPUNGE, or Graph `permanentDelete` instead of moving to Trash |
| `--headless` | Use device-code sign-in for Microsoft Graph permission upgrades; open the printed URL on another device |
| `--dry-run` | Show what would be deleted without deleting |
| `-l`, `--list` | List staged deletion batches |
| `--account` | Filter to one source by identifier or unique display name |
| `--source-id ID` | Filter to exactly one source by numeric ID; mutually exclusive with `--account` |

Starting in v0.20.0, remote deletion is permanently opt-in. Enable it on the
invoking CLI either durably with `[deletion] remote_enabled = true` or for a
single command with `MSGVAULT_ENABLE_REMOTE_DELETE=1`; both guardrails are
permanent with no planned automatic removal. A remote daemon's `[deletion]`
setting does not act as server policy for commands invoked elsewhere. Staging,
`--list`, and `--dry-run` stay ungated. Because permanent deletion always
requires the destructive confirmation prompt, `--permanent` and `--yes` are
mutually exclusive.

---

## repair-identity

Confirm a source's own email address and recompute `is_from_me` for matching
archived messages. This command writes immediately with no dry-run flag, and
skips sources whose account address is already confirmed.

```bash
msgvault repair-identity user@example.com
msgvault repair-identity --type imap
```

Pass an optional identifier to narrow the repair, and `--type` to pick a source
type (`gmail` by default). Only plain, valid email addresses are confirmed; to
review other aliases, use [identity discovery](#identity). Any other email
account types with an unconfirmed account address print with a `--type` repair
hint (reuse your identifier if you passed one). Activity and contacts refresh on
the daemon's next pass, or immediately with `msgvault activity build`.

## repair-senders

Recover missing sender metadata from archived email MIME without contacting a
provider. By default, this prints a read-only report; pass `--apply` to write
repairs and refresh analytics. It repairs only messages missing both `sender_id`
and a From-recipient snapshot, without replacing existing sender evidence.

```bash
msgvault repair-senders
msgvault repair-senders --apply
```

## repair-message

Repair one Gmail message snapshot by fetching it from Gmail, or run a local
audit of stored Gmail MIME without changing the archive.

```bash
msgvault repair-message --audit
msgvault repair-message --audit --source-id 42 --json
msgvault repair-message 123 --source-id 42
```

| Flag | Description |
|---|---|
| `--audit` | Inspect stored MIME; accepts no message argument |
| `--source-id` | Limit to one positive source ID |
| `--json` | Emit newline-delimited audit results; requires `--audit` |

Without `--audit`, pass a single internal numeric message ID or Gmail ID. Repair
fetches a fresh snapshot from Gmail and replaces that message's stored MIME,
headers, bodies, recipients, labels, and MIME attachment rows while keeping its
internal message ID and provider-owned attachments intact.

`--audit` parses stored raw MIME and compares it against archived fields on
every run without contacting Gmail, modifying the archive, or reading saved
mismatch flags. Messages are omitted only when every comparison matches. Any
demonstrated difference reports `status=mismatch` even if other comparisons are
`inconclusive`, while any inconclusive comparison with no mismatch reports
`status=inconclusive` even if other fields match. `inconclusive` means the audit
could not prove consistency, not that the message is corrupt.

For optional headers and bodies, a stored SQL `NULL` matches an empty
recalculated value, whereas a nonempty recalculated value makes `NULL`
`inconclusive`. Stored non-`NULL` values, including empty strings, are compared
directly. Missing legacy recipient addresses or attachment part keys can also
produce `inconclusive` results without identifying why evidence is missing.

Both human-readable and `--json` output follow these rules. JSON fields and
status values stay the same, though consumers can receive additional
`inconclusive` rows. Count `mismatch` and `inconclusive` separately instead of
treating every emitted row as corruption. Finishing an audit without an
operational error does not imply every comparison matched.

After a repair succeeds, run the same audit again to check any remaining
uncertainty; you do not need to re-sync or rebuild the cache. When stored part
keys exist, attachments are compared by physical MIME occurrence so separate
parts with identical bytes still count separately, whereas older rows without
part keys cannot verify duplicate occurrence counts.

An `inconclusive` raw MIME result means stored MIME is missing, empty,
undecodable, unparseable, or larger than the 64 MiB decompression limit. Repair
fetches fresh MIME from Gmail and requires it to parse cleanly; if fetching,
parsing, writing attachment bytes, or replacing the snapshot fails, the existing
snapshot stays unchanged. A later cache-refresh error can occur after the
snapshot has been committed; re-audit to verify that snapshot. If a message
stays flagged after a successful repair, keep the new audit output and the
repair's error output for diagnosis.

## repair-derived

Recompute derived text and metadata from stored provider payloads without
contacting the provider. This command writes immediately, refreshes analytics,
extracts calendar invites from archived email MIME, and leaves raw payloads,
downloaded media, and sync cursors untouched.

```bash
msgvault repair-derived
msgvault repair-derived --source-type imap
msgvault repair-derived --source-type beeper
```

Repeat `--source-type` or `--identifier` to narrow which sources run; requesting
a type with no re-derivation pass and no matching source in the archive returns
an error. Sources with stored MIME (including custom email labels such as
`outlook`) rebuild body text, HTML, body-derived snippets, and search while
keeping provider snippets. Beeper and Discord sources also recompute their
provider-derived text and metadata, and any email stored under those labels
repairs from its MIME like other sources. Every source also derives pending
account attribution for email and calendar rows so `received:` and `account:`
find them, and indexes pending `Message-ID`, `In-Reply-To`, `References`, and
attached-original identifiers read by [`family show`](#family-show). Missing or
unreadable payloads are marked incomplete and do not block later messages.
Because source syncs and imports run this work first, use `repair-derived` for
on-demand repairs, file imports you do not run again, or retrying an interrupted
pass.

To restore missing nonempty MIME attachments for a single Gmail message from
stored raw MIME without contacting Gmail, run:

```bash
msgvault repair-derived --attachment-message <internal-id-or-gmail-id> --source-id <source-id>
```

This mode keeps original payloads, message fields, existing attachment rows, and
valid loose or packed blobs intact, adds missing MIME occurrences, and rebuilds
missing loose files while skipping empty MIME parts (as in normal sync) and
preserving provider-owned attachments. Pass `--source-id` to disambiguate
references, omitting it only when the reference is unique; you cannot combine
source filters with this mode.

Missing, empty, undecodable, or diagnostically incomplete raw MIME, conflicting
metadata, corrupt files, and missing packs all fail without replacing existing
evidence. Fix the reported issue and rerun the same command. Regular Gmail syncs
also retry recorded materialization failures locally, though a successful full
scan does not inspect every previously archived attachment. Note that
`repair-message --audit` checks MIME occurrences rather than file contents, and
regular `repair-derived` and automatic `Heal` text passes do not reconstruct
files.

## repair-attachment-roles

Classify email attachments archived before msgvault tracked attachment roles,
using the raw MIME stored with each message without contacting any provider.

```bash
msgvault repair-attachment-roles
```

Older email attachments carry role `unknown`, so filters cannot tell an inline
image (such as a signature logo) from a standalone file attachment. This command
sets `inline` or `standalone` whenever an attachment matches a single MIME part,
leaving attachments without stored MIME or with ambiguous evidence as `unknown`.

`repair-attachment-roles` writes immediately, prints the counts of scanned
messages and updated attachments, and saves progress after each batch so an
interrupted run picks up where it stopped and a run after completion returns
right away. Because new email gets its role on import, one completed pass is
enough.

## gc

Permanently purge **all source-deleted messages** from a SQLite archive, compact
the database, and remove loose attachment blobs that no remaining message
references. Active messages and messages hidden only by deduplication remain. This command
never contacts providers and does not run on PostgreSQL.

```bash
msgvault gc
```

| Flag | Default | Description |
|---|---|---|
| `--yes`, `-y` | `false` | Skip interactive confirmation |
| `--no-backup` | `false` | Skip the automatic SQLite database backup |

`gc` has no source or date filter and no dry-run flag. The automatic backup
covers only the database and not attachment files deleted afterward. Take a full
[backup](usage/backup.md) first. After the purge finishes, follow the
printed commands to rebuild each enabled analytics or embedding cache. See
[local purge](usage/deletion.md) for how this differs from remote deletion.

## purge-excluded-media

Remove locally stored provider media excluded by your current media policy,
replacing its occurrences with typed skip markers while keeping the messages
themselves. Shared blobs stay live as long as a retained occurrence references
them, and the daemon's normal repack maintenance reclaims packed dead bytes.

```bash
msgvault purge-excluded-media --dry-run
msgvault purge-excluded-media
```

Pass `--dry-run` to preview occurrence, blob, and logical-byte counts without
changing the archive; applying requires confirmation or `--yes` (`-y`). This
never removes media from a provider. Review
[media policy](configuration.md#media-policy) and [backup](usage/backup.md)
before applying.

---

## repair-encoding

Fix UTF-8 encoding issues in existing messages through your configured remote
server or local daemon. The command streams daemon stdout and stderr to your
terminal, serializes the repair with other archive writes, and rebuilds the
analytics cache for SQLite archives. Invalid RFC 822 `Message-ID` values are
reported and left byte-for-byte unchanged so distinct IDs cannot collide; the
analytics cache exports them as `NULL`. Recover the original values separately
from a verified source before rebuilding the cache.

This command does not repair source message IDs, source identifiers, recorded
sender or recipient envelope addresses, account identity addresses, or
participant identifiers. Those fields require separate recovery from verified
original values; rebuilding the analytics cache alone cannot recover them.

```bash
msgvault repair-encoding
```

---

## repair-dates

Report email messages whose canonical `sent_at` is missing, earlier than 1990,
or more than 30 days in the future. Replacements are chosen in order from a
plausible `Date` header, the oldest plausible `Received` timestamp, or stored
source metadata.

By default, this runs as a read-only report. Pass `--apply` to write repaired
dates to the archive and record a JSON audit ledger under your data directory.
SQLite archives also rebuild the Parquet analytics cache (PostgreSQL archives do
not use that cache). Original source files and remote servers are never
modified.

```bash
msgvault repair-dates
msgvault repair-dates --apply
```

| Flag | Description |
|---|---|
| `--apply` | Write repaired dates to the archive |

---

## update

Update msgvault to the latest version.

```bash
msgvault update [flags]
```

`update` restarts only a background daemon started by msgvault, leaving a daemon
run with `msgvault serve` to its supervisor or whoever started it. Pass
`--no-restart` to leave even a background daemon stopped.

Before replacing the installed binary, `update` runs `msgvault version` on the
new binary. If that check fails, times out after a minute, or is canceled, the
update aborts, keeps your installed binary, and prints the new binary's output
(for example, a missing glibc version).

Homebrew installs check and report whether a newer release exists without
replacing themselves; run `brew upgrade msgvault` instead.

| Flag | Description |
|---|---|
| `--no-restart` | Leave the daemon stopped after the update |
| `--check` | Check for updates without installing |
| `-y`, `--yes` | Skip confirmation prompt |
| `-f`, `--force` | Force update even if already on the latest version |

---

## version

Print version, commit, build date, and platform information.

```bash
msgvault version
```

---

## completion

Generate a shell completion script.

```bash
msgvault completion [bash|zsh|fish|powershell]
```

To load completions:

**Bash:**
```bash
source <(msgvault completion bash)

# Permanent (Linux):
msgvault completion bash > /etc/bash_completion.d/msgvault

# Permanent (macOS with Homebrew):
msgvault completion bash > $(brew --prefix)/etc/bash_completion.d/msgvault
```

**Zsh:**
```bash
msgvault completion zsh > "${fpath[1]}/_msgvault"
```

If shell completion is not already enabled, add `autoload -U compinit; compinit` to your `~/.zshrc` first.

**Fish:**
```bash
msgvault completion fish > ~/.config/fish/completions/msgvault.fish
```

**PowerShell:**
```powershell
msgvault completion powershell | Out-String | Invoke-Expression
```

---

## logs

View and follow structured log files from the selected daemon. When
`[remote].url` is configured, this reads remote daemon logs; otherwise it starts
or contacts your local daemon. Enable file logging first (see
[Configuration: Log](/docs/configuration/#log)).

```bash
msgvault logs [flags]
```

| Flag | Default | Description |
|---|---|---|
| `-f`, `--follow` | `false` | Follow today's log file as new lines are written |
| `-n`, `--lines` | `50` | Number of trailing lines to show before following |
| `--run-id <id>` | — | Filter to a single run (matches on prefix) |
| `--level <level>` | — | Filter by log level: `debug`, `info`, `warn`, `error` |
| `--grep <string>` | — | Substring filter applied to the raw JSON record |
| `--all` | `false` | Read every log file in the logs directory, not just today's |
| `--path` | `false` | Print the selected daemon's log directory path and exit |

Examples:

```bash
# Last 50 lines of today's log
msgvault logs

# Follow live
msgvault logs -n 200 -f

# Filter to a single run by its correlation ID
msgvault logs --run-id a1b2c3

# Only errors
msgvault logs --level error

# Substring search across all log files
msgvault logs --all --grep deduplicate
```

---

## quickstart

Print a quickstart guide for AI agents.

```bash
msgvault quickstart
msgvault quickstart --mcp
```

The `--mcp` flag prints the [MCP guide](guides/agent-quickstart.md) offline.

---

## agent-token

Manage restricted agent grants. Your daemon must run with
`[server] agent_access = true` and a nonempty `[server] api_key`. All three
subcommands require the owner API key (keyless loopback is not enough), and a
delegated caller cannot issue or modify grants.

The daemon keeps grants in memory; restarting it invalidates their tokens.

### What a token does and does not defend against

A restricted agent token scopes and revokes access for an agent process, not
for an agent that already has shell access as the daemon owner (that agent can
read provider credentials, edit `config.toml`, or replace the daemon binary).
Tokens give you scoped, revocable access for an agent already constrained by its
deployment environment, plus a usable way to run the daemon somewhere the agent
cannot reach.

A token does not protect against a local agent running under the same user
account as the daemon. Keep owner credentials, provider credentials, daemon
configuration, and the daemon binary outside the agent's control. A remote
user-managed daemon or an isolated local agent environment achieves that, while
a second daemon or data directory under the same unrestricted user does not.

A grant stays valid until you revoke it, it reaches its optional expiry, or the
daemon restarts. Revoking a grant blocks new authentication attempts, while
requests already authenticated may finish.

### Scoped archive reads

Issue a scoped grant when an agent needs read access to your archive, and keep
the unrestricted owner key on the daemon host for administration.

```bash
msgvault agent-token issue --label researcher \
  --permissions search.read,message.read --source-ids 1,2 --expires 24h
msgvault --agent-url https://archive.example.test \
  --agent-token-file ./reader.token search 'subject:meeting' --json
msgvault --agent-url https://archive.example.test \
  --agent-token-file ./reader.token show-message 42 --json
```

| Permission | Granted reads |
|---|---|
| `search.read` | Keyword search with snippets and body-match excerpts, metadata lists, fast search, aggregate counts, and domain search |
| `message.read` | Message bodies and metadata, and containing threads |
| `attachment.read` | Attachment metadata and stored bytes; message access alone does not grant bytes |
| `stats.read` | Statistics within the granted sources; global database size is omitted |
| `verification.read` | Reveals detected verification content alongside an existing read permission; grants no reads alone |

See [detection rules and limits](api-server.md#verification-content).

Unscoped reads cover all live sources in the grant, and any explicit account,
collection, or source set must sit entirely inside it. Collection membership
resolves on every request. Adding an ungranted source makes that collection
unavailable to the grant. CLI `search` and `stats` return
`400 empty_scope` for an explicitly empty collection, while other supported
reads return no matches and discovery hides empty collections. Source matching
uses source type and identifier, so removing and re-adding the same source keeps
authority intact. Requests missing the required permission, or targeting
accounts outside the grant, return `403 permission_denied` naming the missing
permission, while messages, threads, or attachments outside the grant return the
same `404` as missing items.

`--agent-url` and `--agent-token-file` also work with `stats` and an `mcp` stdio
server without local owner configuration. Supported MCP tools cover keyword
search, message and thread reads, attachment reads, lists, aggregate, and stats.
Multi-source metadata lists, fast search, aggregates, and stats keep exact
per-call scopes, whereas deep body search requires a single source (pass
`account` when the grant spans multiple accounts). Vector and hybrid reads stay
unavailable to scoped grants.

Agent searches start a background index check and clear its status once the
index is complete. If the index is incomplete, the owner is prompted to run
`rebuild-fts` or run a search once to repair it; read grants can check the index
but cannot rebuild it. With `--json`, the CLI keeps its bare result array and
prints index advice on stderr. Admitted HTTP search, aggregate, filter, and
total-statistics responses with search text, along with MCP keyword search
responses, include `index_state`.

Scoped archive reads return a consistent authorized view, reading live SQL
tables for scoped aggregates (which can take longer on large archives). Unknown
or ungranted account and collection selectors return `403 permission_denied`. If
an agent-started check takes longer than two minutes, agents see `unverified`
until the owner searches, runs `rebuild-fts`, or the daemon restarts. Owner
searches check without that timeout and repair automatically; if an owner check
fails, it also reports `unverified` and the next owner search checks again. Each
MCP attachment chunk is re-authorized on every call, including cached
continuations.

The delegated MCP catalog exposes a read tool only when the grant holds its
permission. Draft and calendar tools are listed for every grant. The daemon
checks each call against the token, and a denial names the permission the call
needs.

Sync, deletion, configuration, account and token administration, SQL, exports,
people administration, and all other ungranted routes are rejected. Existing
`draft.*` and `calendar.*` permissions keep their separate authority.

### agent-token issue

Issue a restricted grant for one agent. The daemon prints the secret once and
keeps only its SHA-256 digest in memory; save the secret to a file the agent can
read, and never pass it as a flag or environment variable.

```bash
msgvault agent-token issue --label <name> \
  --permissions draft.create \
  --source-ids <id>[,<id>...] \
  --sender <source-id>=<address>
```

| Flag | Description |
|---|---|
| `--label <name>` | (required) Human-readable name for the grant |
| `--permissions <perms>` | Comma-separated permissions: `search.read`, `message.read`, `attachment.read`, `stats.read`, and the `verification.read` reveal modifier (see [scoped archive reads](#scoped-archive-reads)); `calendar.read` for availability, `calendar.event.read` for provider-derived event details in delegated plans and write receipts, `calendar.write` for calendar mutations, and additional `calendar.invite` for guest changes; `draft.create` for `draft-reply`, `draft-compose`, and `draft-get`; `draft.edit` for `draft-get`, `draft-edit`, and `draft-recover`; `draft.delete` for `draft-get`, `draft-delete`, and `draft-recover` (see [managed drafts](#draft-get-draft-edit-draft-delete-and-draft-recover)) |
| `--source-ids <ids>` | Comma-separated source IDs that the permissions apply to |
| `--expires <expiry>` | Optional future RFC3339 timestamp or positive duration such as `24h`; the CLI clock converts a duration to absolute expiry |
| `--sender <source-id>=<address>` | Restrict a source to one confirmed sender identity; repeat for multiple choices |

The grant stays valid until you revoke it, it reaches its optional expiry, or
the daemon restarts.

If you omit `--sender` for a selected source, issuance snapshots every currently
confirmed valid mailbox identity. All sender selections, whether explicit or
snapshotted, are stored as canonical mailbox keys and remain fixed until the
token is revoked. Adding an alias later does not expand an existing grant. A source
with no selected sender has no delegated draft sender authority. Calendar grants
use exact calendar source identities and need no draft sender selection.
Delegated callers can run `calendar` commands and `mcp` over stdio, where the
delegated MCP bridge combines scoped read, draft, and calendar tools.

The response prints the daemon address, the secret, and the granted source
references. Pass `--agent-url <address>` and `--agent-token-file <path>` to
delegated commands, including `msgvault mcp` (which serves the admitted draft tools
over stdio under that grant). The printed address comes from the issuing
request: on a default local install, it is an HTTP loopback URL that works only
on that machine and requires `--agent-allow-insecure`. For an agent on another
machine, use a reachable HTTPS address instead. Plain HTTP requires
`--agent-allow-insecure` and sends the token without transport encryption; use
it only on a trusted network.

### agent-token list

List all grants, including expired ones, without exposing secrets or digests.
Agent-token commands return an error when `server.agent_access` is disabled.

```bash
msgvault agent-token list
```

Each row shows the grant ID, label, permissions, sources (`id/type/identifier`),
frozen sender keys, creation time, and optional expiry. Expired grants stay in
the list with an `(expired)` marker so you can revoke them, though they can no
longer authenticate.

### agent-token revoke

Revoke a grant by ID. This always returns `204` whether the ID existed or not,
preventing grant ID enumeration.

```bash
msgvault agent-token revoke <id>
```

Revocation removes the grant from the in-memory registry right away. In-flight
requests that already authenticated can finish, while the next request using the
revoked secret fails with no fallback.

## policy

Inspect staged conversation archive tiers and source-qualified overrides. See
[conversation archive tiers](configuration.md#conversation-archive-tiers-staged)
for defaults and the staged-release limit.

```sh
msgvault policy list
msgvault policy set SOURCE_ID CONVERSATION_ID full
msgvault policy reset SOURCE_ID CONVERSATION_ID
```

`list` always prints JSON. Saved CLI overrides take precedence over config, and
`reset` restores config or automatic selection. Effective tiers stay `full` in
this release (`stub` and `skip` are rejected), and no messages are removed.

## import-tiktok

Import a TikTok "Download your data" ZIP or extracted folder:

```bash
msgvault import-tiktok <identifier> <archive>
```

| Flag | Default | Description |
|---|---|---|
| `--json` | `false` | Print the import summary as JSON |

This imports DMs and your own comments, saving shared post and media URLs as
links. Include Profile Information in the export so your own messages are marked
as sent by you. See [TikTok export import](usage/tiktok-export.md) for accepted
layouts and limits.

## import-x

Import direct and group DMs along with supplied local media from an extracted X
account archive.

```bash
msgvault import-x <archive-dir>
```

| Flag | Default | Description |
|------|---------|-------------|
| `--me` | From `account.js` | Numeric X account ID; required without account metadata; conflicting overrides fail |
| `--no-default-identity` | `false` | Skip recording the source account as a default identity |

Re-imports skip already stored messages and retry missing media. The source
files must sit on the daemon host; if you use a remote daemon, run the import on
that host with `--local`. See [X DM imports](usage/text-messages.md#import-x)
for supported layouts, evidence, limits, and replay behavior.

## address

Request Google Places suggestions for a partial address with
`msgvault address autocomplete "12 Example"`. Set
`MSGVAULT_GOOGLE_PLACES_API_KEY` in your environment before running it. The
command returns JSON containing a session token, candidates, and attribution; it
sends the text you enter and any location hints to Google without reading or
changing your archive.

Pass `--session-token` to reuse the returned token while refining the same
query, and optionally narrow results with `--language`, `--region`, `--limit`
(1 to 5), and the combination `--lat`, `--lng`, and `--radius` (meters). Once you
pick a candidate, run
`msgvault address select <place-id> --session-token <token>` to fetch
address-only Place Details as JSON and close the session; start a fresh session
for another selection. Neither suggestions nor selected details validate an
address or write to your archive. Display Google Maps attribution alongside
suggestions and follow Google's Places display and attribution policies.

Google bills Places separately from the free GoPlaces software. Autocomplete and
Details requests have separate free monthly caps, and incomplete or abandoned
sessions bill per request. Check current
[Google pricing](https://developers.google.com/maps/billing-and-pricing/pricing)
and
[session pricing](https://developers.google.com/maps/documentation/places/web-service/session-pricing).
