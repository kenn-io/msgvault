---
last_edited: "2026-11-12"
title: Instagram Comments
description: Archive a linked professional Instagram account's media comments and replies.
---

Search returned Instagram media captions, comments, and replies alongside your
other communications, and link commenters with existing person tools. This
integration reads comments and does not send messages. You can also
[hide, unhide, delete, and publicly reply to archived comments](#hide-delete-or-reply-to-comments).

## Connect a professional account

This integration works with a professional Business or Creator Instagram
account linked to an explicitly selected Facebook Page, using Facebook Login
and a Page token. Consumer accounts and Instagram Login are not supported.
msgvault does not acquire tokens or expand their permissions.

1. Store the Page token in a named environment variable on the **daemon host**.
2. Add an [`[[instagram_comments]]` profile](../configuration.md#instagram-comments)
    to your configuration:

    ```toml
    [[instagram_comments]]
    identifier = "example-account"
    app_id = "9"
    page_id = "100"
    instagram_account_id = "200"
    token_env = "EXAMPLE_INSTAGRAM_PAGE_TOKEN"
    enabled = true
    schedule = "*/30 * * * *"
    ```

3. Restart the daemon with that environment.
4. Register and sync the account:

    ```bash
    msgvault source instagram-comments add example-account
    msgvault source instagram-comments sync example-account --max-requests 1000
    ```

During registration and each sync, msgvault verifies both the selected Page and
its linked `instagram_business_account` against the configured account ID. If
the link is missing or changed, registration or sync fails explicitly. App
grants alone do not guarantee access to all media or comments.

## Resume and search

Each run defaults to a budget of 1,000 API reads per account. The two
verification reads count toward it, so collection needs at least three reads.
Media, comments, and replies each paginate through bounded checkpoints. Partial
writes stay searchable, and unfinished work resumes on the next run. When a new
complete cycle starts, it revisits old media with no creation-date cutoff to
find late comments. To restart selected progress with fresh cursors under the
same budget, run sync with `--full`; msgvault keeps and updates existing rows.
Provider failures never reset progress automatically or delete archived history.

Each media item becomes one Conversation, titled from its caption when one is
present, with `instagram_comments` as both the source and message type.
Identities include the app, Page, linked account, and native identifier family,
and equal usernames or numeric IDs across families do not merge people. To link
an observed commenter explicitly, use the existing
[participant linking tools](people.md). Missing authors stay unknown, and later
omissions keep stronger stored evidence. When Meta returns only a
non-comparable commenter identity, comment self attribution is unproven, and
equal digits do not establish account ownership.

## Include ad media

The account's media list can leave out media made only for ads, so comments on
those ads are missed. To archive them, add the ad account that runs the ads and
the environment variable holding a token that can read it:

```toml
[[instagram_comments]]
identifier = "example-account"
# ...the settings above...
ad_account_id = "700"
ad_token_env = "EXAMPLE_META_AD_TOKEN"
```

Use the decimal ad account ID without the `act_` prefix. The ad token needs the
`ads_read` permission for that ad account. After saving your changes, restart
the daemon so it sees the new variable.

On each sync, msgvault reads the ad account's ads with the ad token and looks
up the Instagram media each one uses with the Page token. It archives that
media and its comments only when the media's owner matches the configured
`instagram_account_id`. If the media is also in the account's media list, it
stays one conversation, and its comments are marked as ad comments in their
metadata.

msgvault skips ad media owned by another account, media with no owner, and
media the Page token cannot read. The sync summary line ends with the number
skipped, and the daemon log names each one. Any other failed lookup, such as a
rate limit, fails the sync with Meta's error code or HTTP status, and the next
sync tries again.

Ad account reads and media lookups count toward the same request budget. If you
add, change, or remove `ad_account_id`, msgvault restarts that profile's sync
cycle from the beginning and keeps archived rows.

## Hide, delete, or reply to comments

Add `actions = ["hide", "delete", "reply"]` to your `[[instagram_comments]]`
profile, then run `msgvault comment moderate` or `msgvault comment reply`. The
steps are the same as for Facebook comments; see
[Hide or delete comments](facebook-comments.md#hide-or-delete-comments) and
[Reply to comments](facebook-comments.md#reply-to-comments). Instagram accepts
replies to top-level comments only.

## Limits

This integration discovers only media returned by the verified account's media
edge, unless you [include ad media](#include-ad-media). Missing media owners
stay unknown, and explicit foreign owners are refused. Media downloads are
separate work. To archive new comments as they arrive, set up
[Meta webhooks](meta-webhooks.md).

## Archive media that tags the account

You can archive media where other accounts tag your linked account.

1. Set `mentions = true` on the profile.
2. Restart the daemon.
3. Register and sync the separate mentions source:

    ```bash
    msgvault source instagram-mentions add example-account
    msgvault source instagram-mentions sync example-account
    ```

The source is `instagram_mentions`, so this media stays apart from the
account's own comments. The author's `@username` is the subject so you can
search for it, and Meta returns no author ID for this media, so the author is
not linked to a person. Media the account owns is skipped here.

Captions and comments by other accounts that @-mention the account are not on
any edge that sync can read. With [Meta webhooks](meta-webhooks.md), subscribe
the Instagram object to the `mentions` field and msgvault archives each
mentioned caption or comment into the same source, storing a mentioned comment
as a reply to its media.

An unfinished run resumes from its saved position, and a finished run starts
again from the newest tagged media. The profile's `schedule` also schedules
this source.
