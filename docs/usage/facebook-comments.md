---
last_edited: 2026-11-03
title: Facebook Page Comments
description: Archive comments on a selected Facebook Page and link commenters to existing people.
---

Connect a selected Facebook Page to search its returned posts, comments, and
nested replies in msgvault. This unreleased integration reads public comment
conversations. It does not publish replies or send private messages.

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

## Limits

This integration archives returned text and identity/context metadata. It does
not download post/comment media, manage Facebook personal-profile comments,
or promise complete coverage of every ad variant. Instagram professional
comments, ad-creative discovery, and explicit public replies are separate
work. Threads uses its own authorization and source; a Page token does not
authorize Threads.
