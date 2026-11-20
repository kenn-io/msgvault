---
last_edited: "2026-11-10"
title: Facebook Page Comments
description: Archive comments on a selected Facebook Page and link commenters to existing people.
---

Connect a selected Facebook Page to search its returned posts, comments, and
nested replies in msgvault. This integration reads public comment conversations
and does not send private messages. You can also
[hide, unhide, or delete](#hide-or-delete-comments) archived comments and
[reply to them publicly](#reply-to-comments).

## Connect a Page

1. Get a Page access token for the selected Page through your Meta app's
   supported authorization flow, and store it in a named environment variable
   on the **daemon host**. A personal User token does not work.
2. Add a [`[[facebook_comments]]` profile](../configuration.md#facebook-comments)
   with your app ID, Page ID, and that environment variable's name. The IDs
   in this example are synthetic.

   ```toml
   [[facebook_comments]]
   identifier = "example-page"
   app_id = "9"
   page_id = "100"
   token_env = "EXAMPLE_FACEBOOK_PAGE_TOKEN"
   enabled = true
   schedule = "*/30 * * * *"
   ```

3. Restart the daemon after changing its environment or scheduled profiles.
4. Register and sync the Page:

   ```bash
   msgvault source facebook-comments add example-page
   msgvault source facebook-comments sync example-page --max-requests 1000
   ```

During registration, msgvault checks `/me?fields=id,name,category` against the
selected Page. Registration fails if the identity does not match, Page-only
evidence is missing, or Meta rejects a field. Category is a Page-only field in
Meta's pinned SDK, and this check does not attest to the token's app or prove
its permission grants. For a stronger way to get a token bound to a selected
Page, use Meta's
[`/me/accounts` flow](https://www.postman.com/meta/facebook/request/bqfxwbp/get-access-tokens-of-pages-you-manage).
msgvault does not discover other Pages or ask for expanded permissions.

The client uses Graph API v26.0. What Meta returns depends on the token's
granted permissions, app access level, Page tasks, and object access. Existing
app permissions do not establish access to every comment or ad placement.
Failures stay visible, and msgvault never treats a denied surface as complete
coverage.

## Understand coverage and resuming

Each scan visits the selected Page's fixed `posts`, `visitor_posts`, and
`ads_posts` edges, and ad discovery includes returned inline-created posts.
Each post becomes one conversation, while comments and separately paginated
nested replies keep their exact parent relationships. When organic, visitor,
and ad discovery return the same object, msgvault updates the existing archive
row.

Each surface tracks its own progress. The next run starts after the last
surface that attempted a collection request, even if Meta denied that request.
This rotation gives accessible surfaces turns during persistent denial
elsewhere. Completed surfaces rescan after real provider failures so later runs
can find new comments on old organic posts. Missing or inaccessible objects
never delete archived data. If a run hits the request budget, msgvault pauses
and keeps the unfinished work for the next run. If a provider error happens,
msgvault keeps partial writes and reports a failure.

The default budget is **1,000 API reads per Page per run**, which includes
identity validation and nested reply reads. `--max-requests` must be positive:
a budget of 1 covers identity validation only, and collection needs at least 2.
After a complete scan, each surface uses fresh cursors to find new comments on
old posts, so large histories can require many runs and repeated reads. This is
bounded polling. To archive changes as they happen, add
[Meta webhooks](meta-webhooks.md).

If a saved cursor expires, or if a checkpoint is incompatible or invalid,
restart that selected source explicitly. msgvault rejects checkpoints from the
earlier private version 1 format instead of upgrading them silently:

```bash
msgvault source facebook-comments sync example-page --full --max-requests 1000
```

With `--full`, msgvault uses fresh cursors under the same request budget. It
keeps existing rows and updates them in place instead of erasing the archive.
Other provider errors do not trigger an automatic reset or a retry with fewer
requested fields.

## Search and link people

Facebook comments appear as text conversations with `facebook_comments` as both
the source type and message type. A post returned by a Page is not always
written by it; self attribution requires the returned author's exact ID to
match the selected Page. Missing authors stay unknown, and if a later sync
omits an author field, msgvault keeps the stronger evidence already archived.

Commenter identities include the app, Page, and native actor ID, and matching
display names or usernames never merge customers. To link an observed identity
to a person you already maintain, use the existing
[participant linking and people tools](people.md). msgvault does not create a
separate customer database.

## Hide or delete comments

You can hide, unhide, or delete an archived comment on Meta from msgvault. This
works for Facebook Page comments and
[Instagram comments](instagram-comments.md).

1. Allow the actions on the comment's profile. Adding `"hide"` also allows
   unhiding.

   ```toml
   [[facebook_comments]]
   identifier = "example-page"
   # ...
   actions = ["hide", "delete"]
   ```

2. Find the comment's message ID with `msgvault search` or in the Web UI.
3. Preview the change with `--dry-run`, which shows the comment and its author
   without calling Meta, then hide or unhide it:

   ```bash
   msgvault comment moderate 42 --hide --dry-run
   msgvault comment moderate 42 --hide
   msgvault comment moderate 42 --unhide
   ```

4. To delete a comment, pass `--yes` as well, because Meta cannot undo a
   deletion:

   ```bash
   msgvault comment moderate 42 --delete --dry-run
   msgvault comment moderate 42 --delete --yes
   ```

Once Meta accepts the change, msgvault records it. msgvault remembers any hide
made with this command and shows it as `[hidden]` in `--dry-run`, while the
comment still appears in search and the Web UI. Polling sync does not pick up
hides and unhides made on Facebook or Instagram; with
[Meta webhooks](meta-webhooks.md), hides and unhides of archived Facebook Page
comments are applied. A deleted comment stays in the archive, marked as deleted
from its source like other messages removed from a provider, and its archived
replies are marked deleted with it. You cannot change a comment that is already
deleted from Meta, and this command does not change posts or media captions.

Your token needs Meta's `pages_manage_engagement` permission for Facebook
comments, or `instagram_manage_comments` for Instagram comments. If Meta
refuses a change, msgvault shows Meta's error code and leaves the archive
unchanged. If the comment's source is syncing, the command fails and you can
run it again after the sync finishes. If a resumed comment sync fails on a
comment that was deleted and then purged by `msgvault gc`, run the sync once
with `--full`.

For all flags, see [`comment moderate`](../cli-reference.md#comment-moderate).

## Reply to comments

You can post a public reply to an archived comment as the Page, or as the
Instagram account for [Instagram comments](instagram-comments.md). The reply
usually appears in the archive straight away, marked as sent by you. If
msgvault cannot read the new reply back from Meta, or if Meta does not say
which comment a reply to a reply belongs to, the command tells you and the next
sync archives the reply.

1. Add `"reply"` to the profile's `actions`.
2. Preview the reply, then send it:

   ```bash
   msgvault comment reply 42 --body "Thanks, we have fixed it." --dry-run
   msgvault comment reply 42 --body "Thanks, we have fixed it."
   ```

msgvault records each reply before sending it, so it refuses to post the same
text to the same comment twice. Trying to send the same reply again fails;
change the text to post another reply.

Sometimes msgvault cannot tell whether Meta posted a reply, for example when
the connection drops. When that happens, msgvault refuses the same reply until
you check:

1. Open the comment on Facebook or Instagram.
2. If the reply is there, do nothing. The next sync archives it, though
   msgvault cannot prove you sent it. On Instagram it is not marked as sent
   by you, and on Facebook it is marked as sent by you only when Meta names the
   Page as its author.
3. If the reply is not there, clear the record and send it again:

   ```bash
   msgvault comment reply 42 --body "Thanks, we have fixed it." --clear-unknown
   msgvault comment reply 42 --body "Thanks, we have fixed it."
   ```

If Meta refuses a reply, nothing was posted and you can send it again. Replies
need the same permissions as hiding and deleting. For all flags, see
[`comment reply`](../cli-reference.md#comment-reply).

## Limits

This integration archives returned text along with identity and context
metadata. It does not download post or comment media, manage Facebook
personal-profile comments, or promise complete coverage of every ad variant. To
archive comment changes between syncs, set up
[Meta webhooks](meta-webhooks.md). For linked professional accounts, use
[Instagram comments](instagram-comments.md). Ad-creative discovery and private
replies remain separate work. Threads uses its own authorization and source,
and a Page token does not authorize Threads.
