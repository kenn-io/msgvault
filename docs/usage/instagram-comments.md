---
last_edited: "2026-11-10"
title: Instagram Comments
description: Archive a linked professional Instagram account's media comments and replies.
---

Search returned Instagram media captions, comments, and replies beside your
other communications, and link commenters with existing person tools. This
unreleased integration reads comments; it does not publish replies or send
messages.

## Connect a professional account

Use Facebook Login and a Page token for an explicitly selected Page linked to
the professional Business or Creator Instagram account. Consumer accounts and
Instagram Login are unsupported by this integration. Keep the token in a named
environment variable on the **daemon host**; msgvault does not acquire tokens
or expand their permissions.

Add a [`[[instagram_comments]]` profile](../configuration.md#instagram-comments),
restart the daemon with that environment, then register and sync:

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

```bash
msgvault source instagram-comments add example-account
msgvault source instagram-comments sync example-account --max-requests 1000
```

Registration and each sync verify both the selected Page and its linked
`instagram_business_account` against the configured account ID. A changed or
missing link fails explicitly. App grants alone do not guarantee access to
all media or comments.

## Resume and search

The default budget is 1,000 API reads per account per run. The two verification
reads count toward it, so collection needs at least three reads. Media,
comments, and replies each paginate through bounded checkpoints. Partial
writes remain searchable; unfinished work resumes next time. New complete
cycles revisit old media to find late comments, without a creation-date cutoff.
Use `--full` to restart selected progress with fresh cursors under the same
budget, preserving and updating existing rows. Provider failures never reset
progress automatically or delete archived history.

Each media item forms one Conversation, titled from its caption when present.
The source and message type are `instagram_comments`. Identities include the
app, Page, linked account, and native identifier family. Equal usernames or
numeric IDs across families do not merge people. Use the existing
[participant linking tools](people.md) to link an observed commenter explicitly.
Missing authors stay unknown; later omissions preserve stronger stored evidence.
Comment self attribution is unproven when Meta returns only a non-comparable
commenter identity; equal digits do not establish account ownership.

Only media returned by the verified account's media edge is discovered here,
unless you [include ad media](#include-ad-media). Missing media owners remain
unknown; explicit foreign owners are refused. Media downloads and public
replies are separate work.

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
`ads_read` permission for that ad account. Restart the daemon so it sees the
new variable.

Each sync then also reads the ad account's creatives with the ad token and
looks up the Instagram media each creative uses with the Page token. msgvault
archives that media and its comments only when the media's owner is the
configured `instagram_account_id`. Media owned by another account, media with
no owner, and media the Page token cannot read are skipped and logged by the
daemon. Media that is also in the account's media list stays one conversation,
and its comments are marked as ad comments in their metadata.

Creative pages and media lookups count toward the same request budget. Adding,
changing, or removing `ad_account_id` restarts that profile's sync cycle from
the beginning; archived rows are kept.
