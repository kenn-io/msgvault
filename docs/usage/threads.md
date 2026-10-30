---
title: Threads
description: Archive your Threads posts and available replies, then publish reviewed replies through your own daemon.
---

# Threads

On unreleased `main`, msgvault can archive your own Threads posts, the available
public replies under them, and your own replies elsewhere. Search and read them
with the rest of your archive. Sending is a separate owner-only action on a
reviewed local draft, disabled until you opt in for that source.

## Connect your account

1. Create your own Meta app with the Threads use case. Add your account as a
    Threads tester and accept the invitation in Threads Website permissions.
    Use the **Threads app ID and secret**, not another product's credentials.
    Follow Meta's [app setup](https://developers.facebook.com/documentation/threads/get-started/create-an-app.md)
    and [tester setup](https://developers.facebook.com/documentation/threads/get-started.md).

2. Register an HTTPS callback on a domain you control. Use a minimal page
    without third-party scripts or query logging: its URL receives a short-lived
    authorization code. The daemon does not need a public callback listener.

3. Run this on your workstation, with your usual daemon connection:

    ```bash
    msgvault add-threads --app-id 123 --redirect-uri https://example.com/threads-callback
    ```

    Enter the secret at the masked terminal prompt, or supply it through
    `MSGVAULT_THREADS_APP_SECRET`. Never put it in a command argument. Open the
    printed authorization URL, complete consent, and paste the **full returned
    callback URL**. A bare code is rejected. The CLI checks state, callback
    origin/path and fixed query parameters before contacting the daemon.

4. The selected daemon exchanges and verifies the credential, saves its token
    in protected storage, and prints an exact identifier such as
    `threads:123:456`. Keep that identifier; a username is not an account selector.

The browser stays on your workstation. Exchange, refresh and archive writes run
on the daemon host. Use authenticated HTTPS remotely, or an SSH tunnel with a
literal loopback URL such as `http://127.0.0.1:8080`. Plain HTTP to a remote host
is rejected for this handoff. The saved credential does not retain the app secret
or authorization code. Reconnecting asks for them again.

Default consent requests `threads_basic` and `threads_read_replies`. To request
`threads_content_publish` and `threads_manage_replies` too, rerun the same
command with `--allow-send`. This requests permission; it does not enable sends.
See Meta's [permissions guide](https://developers.facebook.com/documentation/threads/get-started/get-access-tokens-and-permissions.md).

## Read now or on a schedule

```bash
msgvault sync-threads threads:123:456 --json
msgvault sync-threads threads:123:456 --after 2026-01-01T00:00:00Z --json
msgvault sync-threads threads:123:456 --full --json
msgvault sync-threads threads:123:456 --post 789 --json
```

`--after` sets an inclusive RFC3339 history floor. Changing it starts a distinct
read epoch. The floor limits discovery of your posts and own replies. Previously
archived own roots stay eligible for rolling conversation refresh, even below a
raised floor, so late replies can still be collected. `--full` starts or resumes
a full read epoch. `--post` refreshes one
already archived, verified own root; it cannot be combined with `--after` or
`--full`, does not advance source-wide history, and reports `history_complete=false`.

Connection alone does not schedule reads. Add the printed identifier to
[`[[accounts]]`](../configuration.md#accounts), then restart the daemon:

```toml
[[accounts]]
email = "threads:123:456"
schedule = "*/15 * * * *"
enabled = true
```

That is an example schedule, not a Threads-specific default. Each scheduled
run permits at most 200 HTTP attempts; a manual run permits 1,000. Credential
refreshes and retries count. A run lasts at most ten minutes and starts at most
two requests per second for that source. A high provider usage signal pauses
further work. Repeated runs resume persisted walks and rotate through recent
and older posts; a busy conversation does not monopolize every run.

JSON reports `status`, `mode`, `requested_floor`, `oldest_observed`,
`history_complete`, `sweep_boundary`, `sweep_position`, `pending_walks`,
`pending_pages`, `unavailable`, `incomplete`, and message counters. A paused
run may already have saved messages. Reaching the end of available pagination
is a statement about that read epoch, not proof that every historical reply
was visible. Backlog and provider limits determine how soon old or late replies
appear.

Use the source with existing archive commands:

```bash
msgvault list-accounts --json
msgvault search 'project update' --account threads:123:456 --message-type threads --json
msgvault show-message 42 --json
msgvault show-thread 42 --json
```

Each own post is a conversation. Nested reply links are resolved when their
parents are available. Your replies under someone else's post retain partial
external-root context; they do not authorize sending into that conversation.
Media is metadata only: the importer does not download Threads CDN files.

## Review authors and identities

The verified owner's app-scoped ID identifies the account within its source.
Third-party reply authors are reviewable occurrences, with observed usernames
and names as evidence. The API may omit a stable author ID. Msgvault does not
invent one or automatically merge people by handle. Renames and recycled handles
therefore do not silently join different people. Confirm a person association
through the existing people workflow when you have independent evidence.

## Publish a reviewed reply

First reconnect with `--allow-send` if needed. Find the numeric archive source
ID with `list-accounts --json`, then enable only that source in the daemon's
configuration and restart it:

```toml
[[threads.sends]]
source_id = 42
```

The default is empty. Scopes and this opt-in are both required for a new send.
Create a local draft against an archived parent in your own post's conversation:

```bash
msgvault draft-reply 123 --body 'Thanks for the details.' --json
msgvault draft-get chat-draft-example --json
msgvault send-reply --draft chat-draft-example --revision 1 --json
msgvault send-reply 123 --body 'Thanks!' --json
msgvault send-reply 123 --quota --json
```

Replace the example message and draft IDs and revision with the values you
reviewed. `draft-compose --conversation <id> --reply-to <message-id> --body
<text>` is another way to prepare the same kind of draft. Draft creation and
editing never publish. The archived message or local draft determines the
source for the shared `send-reply` command. This release registers the Threads
sender. Use a positional archived message ID with `--body` for a direct send,
or `--draft` with `--revision` to publish a reviewed local draft. A direct send
is pinned to a local draft before provider work. A changed draft revision or
destination is rejected. Sending text must be nonblank UTF-8, contain no NUL,
and fit within **500 UTF-8 bytes**. Draft bodies can be longer and remain
editable. Results report `body_bytes` without echoing the text.

Quota mode inspects that exact source without claiming a draft or publishing.
It is owner-only and cannot be combined with draft, revision or adoption flags.
Delegated tokens cannot connect accounts, enable sending, inspect send receipts
through this command, query quota, or send.

The owner MCP tool `send_reply` takes either `message_id` plus `body`, or
`draft_id` plus `revision`; `adopt_media_id` is only for draft recovery. The
archived message or draft determines the source. Each send requires explicit
client confirmation of that tool and those arguments. A `confirmed` argument
cannot authorize it. Clients unable to complete confirmation cannot send.
Delegated sessions do not expose this tool. See [MCP draft and send
tools](chat.md#draft-and-send-tools).

## Inspect and recover an interrupted send

Read both `remote_status` and `local_status` in the result. A nonzero command
exit does not mean the provider failed to publish.

| `status` | Meaning and next action |
|---|---|
| `complete` | Known remote media ID archived locally; the reviewed draft was consumed. |
| `container_saved` or `ready` | Resumable processing before publication. Repeat the same draft ID and revision. |
| `rate_limited`, `reply_not_permitted`, `invalid_body` | The attempt was rejected or paused before publishing. Inspect the result and fix the stated condition. |
| `send_failed` | A proven unpublished terminal container outcome; the draft remains available. |
| `remote_unknown` | A publish may have happened. Repeating inspects the receipt and never republishes it. |
| `sent_archive_pending` | The remote ID is known; local archive or cache completion needs repair. Repeat the same ID and revision. |

A container is saved before polling. Polling begins no earlier than 30 seconds
after creation, then at most once per minute, with at most five minutes of
waiting per invocation. An unfinished invocation leaves resumable state.

The daemon durably records a publish barrier before its single publish request.
A dropped response, timeout or crash after that barrier remains uncertain.
There is no resend override. Inspect Threads itself. If you identify the exact
published reply, explicitly adopt its media ID:

```bash
msgvault send-reply --draft chat-draft-example --revision 1 --adopt-media-id 987 --json
```

Adoption requires current read permission and proof of your ownership, exact
text, root and parent. Candidate hints are not proof and are never automatically
adopted. Once a media ID is known, local completion works after send opt-out or
credential loss and never publishes again. A canonical provider snapshot wins
over a provisional local copy. A cache failure preserves the known remote ID;
repeat to repair the cache. An interrupted/truncated output stream proves no
remote outcome: inspect the same draft and revision before doing anything else.

## Disable, reconnect or remove

Remove the source's `[[threads.sends]]` entry and restart to stop new sends.
Disable its `[[accounts]]` schedule to stop scheduled reads. Revoking the app in
Threads stops provider access; reconnect explicitly to replace a revoked or
expired credential. These actions do not delete the archive.

`msgvault remove-account --source-id 42 --yes` removes that source, its archived
data, local drafts and send receipts, and its bound credential. It is destructive.
Follow [archive deletion](deletion.md) for removal requests and review derived
exports, model outputs and backups separately; removing the source does not
recall copies outside the archive.

## Coverage and release limits

The importer records what the authorized API returns. Hidden, private, pending,
deleted, inaccessible or provider-omitted replies may be missing. It is not a
real-time deletion-event mirror, a permanent identity directory, or a guarantee
of free access or perpetual retention rights. Optional profile, model and export
processing retains its existing consent controls.

The implementation is tested with synthetic providers. Disposable live checks
of tester OAuth, a **non-tester** reply author, nested and late replies, oldest
available history, terminal cursors, available hidden/private/pending behavior,
rename, revocation and one reviewed send remain **unverified release gates**.
Release requires authorized live checks and platform review. Owner-operated
display and retention requirements
under the [Meta Platform Terms](https://developers.facebook.com/terms/) also need
release review; this guide does not establish an entitlement to retain restricted
third-party content.
