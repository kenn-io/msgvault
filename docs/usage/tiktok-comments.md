---
title: TikTok Comments
description: Archive captions, comments, and replies on your own TikTok videos through an approved company app.
last_edited: 2026-11-02
---

Archive comments on your own TikTok videos as searchable conversations. Each
video caption becomes the thread root. Comments and separately paginated replies
keep their parent links. This integration is unreleased and has been exercised
with synthetic HTTP fixtures. It has not been verified with an approved live app.

## Get API access

Use **TikTok API for Business v1.3**, rather than Login Kit or the Display API.
Bring your own approved company app; msgvault does not provide a shared app or
hosted token exchange.

1. [Register as a developer](https://business-api.tiktok.com/portal/docs/register-as-a-developer/v1.3)
   with a company-domain email address and a public company website. TikTok does
   not onboard individual developers through this program.
2. Submit the **Accounts API Access Application Form** and request app review
   for the TikTok Accounts permissions. Request `comment.list`, `video.list`,
   and `user.info.basic`. Read-only archiving does not need comment-write scopes.
3. Authorize the account using your app's OAuth flow and registered redirect URI.
   Keep the returned `open_id` as the account selector. Exchange authorization
   codes outside msgvault using TikTok's supported app flow; do not put an app
   secret in a shared client or on the command line.
4. Put the access token in a named environment variable on the **daemon host**.
   Restart the daemon after changing its environment.

See TikTok's [Accounts API authentication](https://business-api.tiktok.com/portal/docs/accounts-api-authentication/v1.3)
and [comment access documentation](https://business-api.tiktok.com/portal/docs/manage-comments-on-owned-tiktok-videos/v1.3).
Personal-account eligibility for comments has not been confirmed live.

## Connect and sync

Add a [`[[tiktok]]` profile](../configuration.md#tiktok). All values below are
synthetic. Environment-variable fields contain names, never token values.

```toml
[[tiktok]]
identifier = "example-account"
app_id = "100001"
account_id = "example-open-id"
token_env = "EXAMPLE_TIKTOK_TOKEN"
enabled = true
schedule = "*/30 * * * *"
```

```bash
msgvault source tiktok-comments add example-account
msgvault source tiktok-comments sync example-account --max-requests 1000
```

With no identifier, commands select every enabled profile. Disabled profiles
are skipped, and selecting one explicitly fails. Registration calls
`/business/get/` for the configured account. It checks the returned profile;
it does not attest to all scopes or discover other authorized accounts.

`sync` requires registration. The default budget is 1,000 API reads per account,
including the profile check. `--max-requests` must be positive. Budget pauses
save pending pages and resume on the next run. Provider errors also preserve
accepted rows and checkpoints, but return failure. Completed scans start again
at the newest video page, so new comments on older videos can be discovered.

Use `--json` for JSON results on stdout and JSON command errors on stderr.
The daemon owns archive access and credentials even when a remote CLI requests
the command. Use `--build-cache` or `--no-build-cache` to control the normal
post-sync analytics refresh.

If a cursor expires or the checkpoint is incompatible, restart that traversal:

```bash
msgvault source tiktok-comments sync example-account --full
```

`--full` preserves existing messages. Comment IDs deduplicate repeated records,
including duplicates in TikTok's mixed sort order above 500 comments per video.
Empty pages with `has_more = true` continue to the next cursor. The importer
requests `status=ALL` to retain returned hidden comments.

## Keep authorization working

TikTok access tokens last 24 hours. You can rotate a plain access token through
your own credential process and restart the daemon. For automatic refresh, set
`app_secret_env` to a second daemon environment-variable name. Set `token_env`'s
value to a JSON object with these fields:

```json
{
  "access_token": "example-access-token",
  "refresh_token": "example-refresh-token",
  "expires_at": "2026-11-03T12:00:00Z",
  "refresh_expires_at": "2027-11-02T12:00:00Z"
}
```

Use the actual absolute expiry times from your authorization process. msgvault
refreshes within five minutes of access-token expiry and atomically stores both
rotated tokens in private files under `<data_dir>/tiktok/`. Refresh tokens rotate
and last up to one year. Reauthorize when they expire. Changing the bootstrap
environment value replaces the cached authorization on its next use. Back up
rotated token files securely; the original environment value becomes stale.
An ambiguous failed refresh is not retried automatically with the same token.

Requests are paced across processes and profiles sharing an app at no more
than 10 per second and 600 per minute. Provider throttling records a five-minute
cooldown and fails the current sync; a later sync resumes. Lower account or
endpoint limits may still apply.

## Understand the archive

Messages use source and message type `tiktok_comments`. The archive keeps returned
text and raw JSON, including visibility and link fields. It does not download
video or comment media. The caption root is from the account; comments are
self-authored only when TikTok marks them as the owner. Missing authors remain
unknown. Display names never establish identity.

Commenter identities use the app, account, and `unique_identifier`. Use the
[people tools](people.md) to link those observations to your maintained people.
Matching usernames do not merge people.

Pull sync cannot infer deletions from absent comments, and never deletes prior
history. It does not subscribe to optional `comment.update` webhooks or promise
complete provider coverage. App approval, granted scopes, privacy filtering,
cursor validity, and the service's history access determine what can be read.
