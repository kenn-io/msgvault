---
last_edited: "2026-11-06"
title: YouTube comments
description: Archive videos and comments from one explicitly selected owned channel.
---

Keep the comments on your own YouTube channel searchable in msgvault. Each
video becomes a conversation, with its description and watch URL as the root.
Comments and replies retain their parent links and author identities. A later
sync revisits old videos and appends new comments without duplicating earlier
ones. Missing comments are retained.

## Connect a channel

Enable YouTube Data API v3 in your own Google Cloud project. Authorize the
channel with OAuth scope
`https://www.googleapis.com/auth/youtube.force-ssl` and offline access to obtain
a refresh token. msgvault consumes that token; it does not run an OAuth login
server.

An external OAuth app in **Testing** can have refresh tokens expire after seven
days. Publishing changes that condition; Google's verification requirements
are separate. Reauthorize an expired token rather than storing an access token
in configuration.

Add a profile to `config.toml`:

```toml
[[youtube]]
identifier = "example-channel"
channel_id = "UCexample000000000000000"
token_env = "EXAMPLE_YOUTUBE_REFRESH_TOKEN"
client_id_env = "EXAMPLE_YOUTUBE_CLIENT_ID"
client_secret_env = "EXAMPLE_YOUTUBE_CLIENT_SECRET"
enabled = true
schedule = "0 2 * * *"
```

Set the three named variables in the daemon's environment and restart the
daemon. Configuration holds variable names, never credential values. The
channel ID is immutable; do not use a display name or handle. One profile
selects one channel, and duplicate channel profiles are rejected.

```bash
msgvault source youtube add example-channel
msgvault source youtube sync example-channel --max-requests 1000
```

Registration verifies that the credentials own the configured channel before
creating its source. Sync requires registration and verifies ownership again.
Renaming the local profile keeps the same archive source. With no identifier,
the commands process all configured profiles. Scheduled polling uses enabled
profiles with a cron schedule.

## Understand progress and visibility

Sync lists the uploads playlist, including accessible private and unlisted
videos. It polls published, held-for-review and likely-spam thread filters and
fetches remaining reply pages when an inline thread response is incomplete.
Each comment's recognized moderation status supplies its current label; the
filter that found its thread does not. Missing or unknown status preserves an earlier
observation. Commenters with equal names remain separate people when their
channel IDs differ. A previously known author is retained when a later response omits the channel ID or name. When a comment has a name but no channel ID, its author name is shown without linking it to other comments.

The default request budget is 1,000 Data API calls, including channel
verification. Every ownership page counts toward the budget. The minimum is 2. A partial run saves page-level progress and
continues on the next manual or scheduled run. Pending pages take turns, so a
large thread cannot continually restart ahead of older work. A completed cycle
starts another uploads walk on the next invocation, finding late comments on
old videos.

Quota, authentication and transient errors stop the run without declaring
coverage complete. Deleted videos and comments leave their archived rows in place. Videos, threads, and replies that return forbidden are skipped until the next uploads cycle. Disabled comments on one video do not abort
the channel. An invalid page token restarts only its affected listing; stable
IDs make replay safe.

A complete cycle needs one request per ownership page, one per uploads page, one per batch of up to 50 videos, and three comment-thread requests per returned video, before extra thread or reply pages. A forbidden or missing batch is retried one video at a time. Scheduled runs use a budget of 1,000 requests.

The usual default Google project quota is 10,000 units per day. These list
calls cost one unit each; a local request budget is not a quota grant. OAuth
refresh requests do not count against the Data API request budget.
