---
last_edited: 2026-11-03
title: Facebook Page Comments
description: Archive comments on a selected Facebook Page and link commenters to existing people.
---

Connect a selected Facebook Page to search its returned posts, comments, and
nested replies in msgvault. This unreleased integration archives public comment
conversations and can moderate or reply to archived Facebook comments after
explicit opt-in.

## Connect a Page

1. Obtain a Page access token for the selected Page through your Meta app's
   supported authorization flow. A personal User token is not sufficient.
   Keep the token in a named environment variable on the **daemon host**.
2. Add a [`[[facebook_comments]]` profile](../configuration.md#facebook-comments)
   with your app ID, Page ID, and that environment variable's name. The IDs
   below are synthetic examples.

   ```toml
   [[facebook_comments]]
   identifier = "example-page"
   app_id = "9"
   page_id = "100"
   token_env = "EXAMPLE_FACEBOOK_PAGE_TOKEN"
   enabled = true
   schedule = "*/30 * * * *"
   ```

3. Restart the daemon after changing its environment or scheduled profiles,
   then register and sync the Page:

   ```bash
   msgvault source facebook-comments add example-page
   msgvault source facebook-comments sync example-page --max-requests 1000
   ```

Registration checks `/me?fields=id,name,category` against the selected Page.
An identity mismatch, missing Page-only evidence, or rejected field fails
registration. Category is a Page-only field in Meta's pinned SDK; this check
does not attest to the token's app or prove its permission grants. Meta's
[`/me/accounts` flow](https://www.postman.com/meta/facebook/request/bqfxwbp/get-access-tokens-of-pages-you-manage)
provides the stronger route for obtaining a token bound to a selected Page.
msgvault does not discover other Pages or request expanded permissions.

The client uses Graph API v26.0. The token's granted permissions, app access
level, Page tasks, and object access determine the returned data. Existing app
permissions do not establish access to every comment or ad placement. Failures
remain visible; msgvault never treats a denied surface as complete coverage.

## Understand coverage and resuming

Each scan visits the selected Page's fixed `posts`, `visitor_posts`, and
`ads_posts` edges. Ad discovery includes returned inline-created posts. A post
becomes one conversation; comments and separately paginated nested replies
retain their exact parent relationships. Organic, visitor, and ad discovery
of the same object updates the existing archive row.

Each surface keeps independent progress. The next run starts after the last
surface that attempted a collection request, including a denied request. This
rotation gives accessible surfaces turns during persistent denial elsewhere.
Completed surfaces rescan after real provider failures so later runs can find
new comments on old organic posts. Missing or inaccessible objects never
delete archived data. A budget pause keeps unfinished work for
the next run. Provider errors retain partial writes and report a failure.

The default budget is **1,000 API reads per Page per run**, including identity
validation and nested reply reads. `--max-requests` must be positive. A budget
of 1 permits identity validation only; collection needs at least 2. After a
complete scan, each surface uses fresh cursors to find new comments on old posts.
Large histories can therefore require many runs and repeated reads. This is
bounded polling, not a webhook service or a guarantee of immediate capture.

If a saved cursor expires or the checkpoint is incompatible or invalid,
restart that selected source explicitly. Checkpoints from the earlier private
version 1 format are rejected rather than silently upgraded:

```bash
msgvault source facebook-comments sync example-page --full --max-requests 1000
```

`--full` uses fresh cursors under the same request budget. Existing rows are
preserved and updated in place; it does not erase the archive. Other provider
errors do not cause an automatic reset or a retry with fewer requested fields.

## Search and link people

Facebook comments appear as text conversations with source type and message
type `facebook_comments`. A post returned by a Page is not necessarily written
by it: self attribution requires the returned author's exact ID to match the
selected Page. Missing authors remain unknown; later omitted author fields
preserve stronger evidence already archived.

Commenter identities include the app, Page, and native actor ID. Matching
display names or usernames never merge customers. Use the existing
[participant linking and people tools](people.md) to link the observed identity
to a person you already maintain. No separate customer database is created.

## Comment actions

Reply to, hide, unhide, or delete an archived Facebook comment through the owner CLI,
HTTP API, or MCP tools. These actions require daemon API schema 3.39.0 or newer.
Delegated agent grants and remote-client keys are refused for actions, triage,
queue reads, and receipt reads. Delegated MCP sessions do not expose these tools.

Add explicit opt-ins to the selected `[[facebook_comments]]` profile and restart
the daemon:

```toml
enabled = true
actions = ["hide", "delete", "public_reply", "private_reply"]
```

`hide` also permits unhide. An omitted or empty `actions` list permits no
provider writes. Moderation requires `pages_manage_engagement` and
`pages_read_user_content` to be granted to the app and token. Live permission
and app-access checks are still pending; enabling this configuration does not
grant permissions.

Use msgvault's archived **message ID**, not a free-form Facebook comment ID.
A dry run uses local archive evidence, reads no token, makes no Graph request,
and creates no receipt. It prints the exact target and a confirmation token:

```bash
msgvault comment moderate 42 --delete --dry-run --json
msgvault comment moderate 42 --delete --confirm CONFIRM_TOKEN --json
msgvault comment moderate 43 --hide --json
msgvault comment moderate 43 --unhide --json
```

Copy `confirm` from the preview into `--confirm`. Delete requires this token
and keeps the archived history. Hide and unhide need no confirmation token.
The daemon checks the target's source identity and verifies that the configured
token belongs to the selected Page before a new provider action. Previews and
receipt resolution stay local. MCP
`moderate_comment` previews and requests client confirmation for delete;
hide and unhide run without that prompt.

### Public and private replies

Choose the visibility explicitly. Public replies appear under the comment as
the selected Page. A private reply sends one private message about the comment:

```bash
msgvault comment reply 42 --visibility public --body "Example reply" --dry-run --json
msgvault comment reply 42 --visibility public --body "Example reply" --confirm CONFIRM_TOKEN --json
```

Use `--visibility private` for a private reply. Each send requires its own
`public_reply` or `private_reply` opt-in. Public replies require
`pages_manage_engagement`; private replies require `pages_messaging`.
Confirmation binds the exact target, visibility, and UTF-8 body (1–8,000 bytes,
no NUL). MCP `reply_to_comment` previews first, then shows `PUBLIC` or
`ONE PRIVATE`, the Page, author, comment, and full body for client confirmation.
Declining sends nothing. A failure never switches visibility.

Private replies are limited to one message per comment within seven days of
the comment's timestamp. The Page's own comments are refused. A prior private
reply to the same archived author on another comment produces a warning; it
does not establish consent for more messages. Follow-up DMs belong to the
separate messaging workflow. `thread_owned_by_other_app` means another app owns
the conversation; change routing outside msgvault before trying again.

A repeated public reply with the same body returns its receipt without sending
again. A different body is a new reply. A second private reply returns
`already_sent`. Opaque private recipient IDs remain receipt evidence and never
link people. Public replies are archived immediately only when the provider
returns complete parent and timestamp evidence; otherwise a later sync must
observe them. `archive_complete` and `reply_message_id` report that evidence.

### Receipts and recovery

Read receipts with `msgvault comment actions 42 --json`. Each receipt identifies
the action, state, error code, and creation and update times so you can distinguish
attempts after a lost response. An `outcome_unknown`
error means the provider may have accepted the action. msgvault does not retry
it automatically. Inspect the provider and receipts first, then resolve the
same action with `--resolve-unknown sent` or `--resolve-unknown not-sent`.
Resolution records your observation without another provider call; `not-sent`
permits a later explicit attempt. Delete and reply resolution still require
`--confirm`. Resolve a reply with the original visibility and exact body.
An existing private reply receipt can be resolved after the seven-day send
window closes; resolution does not send another message.
Archive cleanup retains an unresolved action's comment and parents until you
resolve the receipt, even if the comment has since been deleted from Facebook.
A `remote_accepted_local_failed` error includes the receipt: the provider
accepted the action, but local evidence could not be saved or repaired. Repeating
a succeeded delete repairs its archive marker without deleting again.

Garbage collection preserves the parent links needed by surviving replies.
See [`gc`](../cli-reference.md#gc) for retention and purge rules.

### Local triage

Track work locally without changing Facebook:

```bash
msgvault comment triage 42 --state handled --expect open
msgvault comment queue --state open --limit 50 --json
```

Triage states are `open`, `handled`, and `skipped`. `--expect` refuses an update
if another caller changed the state. Later imports preserve these local labels.
The queue includes comment context, moderation evidence, archived Page replies,
and receipt summaries. It excludes the Page's own comments and normally excludes
comments deleted from Facebook; `--state all` includes deleted comments too.
Results sort by sent time, then message ID, ascending. Continue with
`--before-id` set to the last-seen message ID; the page limit is 1–200, default 50.

See the [CLI reference](../cli-reference.md#comment-moderate) for all flags.

## Limits

This integration archives returned text and identity/context metadata. It does
not download post/comment media, manage Facebook personal-profile comments,
or promise complete coverage of every ad variant. Use [Instagram comments](instagram-comments.md)
for linked professional accounts. Ad-creative discovery remains separate work.
Threads uses its own authorization and source; a Page token does not authorize
Threads.
