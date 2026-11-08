---
last_edited: "2026-11-08"
title: Google Reviews
description: Archive and search Google Business Profile reviews, backfill API-available history, and explicitly confirm owner responses.
---

Use your own approved Google project to read live Business Profile reviews,
archive the review pages Google returns, search those records, and submit an exact
owner response through the CLI or the Web UI's **Google Reviews** workspace.
Facebook Page reviews and recommendations are unavailable because the current
official read eligibility and review-specific owner reply contract remain
unverified. Generic Page post comments are a different feature.

Backfill stores review content and reviewer attribution in the main archive and
its dedicated full-text index. It does not create synthetic messages, message
bodies, embeddings, or operation-history entries. Backfill is owner-triggered and
uses GET requests to the reviews API; it does not submit or edit provider replies.

## Use your own approved Google project

Use this integration only for your own business through your own approved Google
Cloud project. It does not provide hosted access to another business through your
project. `own_project = true` records your attestation; it cannot prove that Google
has approved your project or verified your location.

Before setup, follow Google's [prerequisites](https://developers.google.com/my-business/content/prereqs)
and [basic setup](https://developers.google.com/my-business/content/basic-setup):

1. Manage a verified Business Profile that has been active for at least 60 days
   and has a website listed on the profile.
2. Request Business Profile API approval for the intended Cloud project. Google's
   prerequisites identify 0 QPM as unapproved and 300 QPM as approved. Check the
   actual quota for your project and API; another API's quota is not a guarantee
   for the v4 reviews endpoint.
3. Enable the required Business Profile APIs and create a Google OAuth client in
   that project. Use a separate named OAuth app in msgvault. Existing Gmail or
   Calendar authorization is not reused or silently broadened.

The authorization requests `business.manage` and `userinfo.email`. Google documents
no narrower review-read-only grant. Setting `replies_enabled = false` prevents
responses in msgvault while the provider authorization still has write authority.
See Google's [review guide](https://developers.google.com/my-business/content/review-data)
and [API usage limits](https://developers.google.com/my-business/content/limits).

## Configure and discover a location

Add a named app and a disabled setup entry to your
[configuration](../configuration.md). Replace the example paths and account with
your own values when you choose to connect:

```toml
[oauth.apps.business]
client_secrets = "/path/to/your-own-project-client.json"

[[google_reviews]]
name = "shop"
provider = "google"
email = "owner@example.com"
oauth_app = "business"
own_project = true
enabled = false
replies_enabled = false
```

Configure [owner daemon authentication](../api-server.md#authentication) and
keep that daemon available before authorization. Run authorization and disconnect
on the daemon host with owner access. They first invalidate outstanding previews
through the authenticated daemon, then change the dedicated token. If invalidation
fails, credentials are not changed.

When you are ready to authorize that account, run:

```bash
msgvault google-reviews authorize shop
# For a browser on a different machine:
msgvault google-reviews authorize shop --headless
```

Authorization is an account action. A successful sign-in does not establish project
approval, location verification, or consent to a particular response.

Discover the account and location using the official API Explorer in your own
project and account:

1. Open [Account Management `accounts.list`](https://developers.google.com/my-business/reference/accountmanagement/rest/v1/accounts/list).
   Execute `GET https://mybusinessaccountmanagement.googleapis.com/v1/accounts`.
   Follow `nextPageToken` with `pageToken` if needed. Choose the owned or managed
   account and retain its resource name, such as `accounts/123`.
2. Open [Business Information `accounts.locations.list`](https://developers.google.com/my-business/reference/businessinformation/rest/v1/accounts.locations/list).
   Set `parent` to that account name and the required `readMask` to `name,title`.
   Execute `GET https://mybusinessbusinessinformation.googleapis.com/v1/accounts/123/locations?readMask=name,title`.
   Follow pagination and choose the intended location, such as `locations/456`.
3. Construct the reviews v4 parent from both IDs: `accounts/123/locations/456`.
   Add `location = "accounts/123/locations/456"` to the setup entry, then set
   `enabled = true`. Keep `replies_enabled = false` for read-only use.

The numeric IDs above are synthetic examples. Reviews still use v4, while account
and location discovery use the separate v1 APIs above. Do not use legacy v4
account/location listing endpoints for discovery. Repeat configuration entries
with distinct names for additional locations.

## Read current reviews

```bash
msgvault google-reviews capabilities
msgvault google-reviews list shop --page-size 20
msgvault google-reviews list shop --page-token NEXT_PAGE_TOKEN
msgvault google-reviews get shop REVIEW_ID
```

Commands return JSON. List reads use `updateTime desc`, default to 20 reviews, and
allow at most 50 per page. Pagination is a live sequence, not a stable snapshot.
Refresh to see edits or deleted reviews; a missing item on one page is not proof
of deletion. Access and quota errors are not empty review lists.

In the Web UI, open **Google Reviews**, choose a configured target, and select a
review. The detail shows reviewer attribution, rating, text when present, and the
current owner response. Rating-only and anonymous reviews are supported. Profile
pictures and remote media are not downloaded. Returned moderation information is
shown when available; Google's `reviews.get` currently omits some moderation
fields that `reviews.list` may return.

## Backfill and search the archive

Start or resume backfill for an enabled target with an owner API key or owner
browser session. Keyless loopback, delegated, and remote-client access cannot
start a backfill:

```bash
msgvault google-reviews backfill shop
msgvault google-reviews archive-status shop
msgvault google-reviews search "delivery helpful" --target shop
```

Backfill follows each `nextPageToken` until the API returns no next token. Google
allows up to 50 reviews per page. Every page and its resume checkpoint commit in
one archive transaction. Re-run the backfill command after a provider error or
daemon restart to resume. If Google repeats a page cursor, the next explicit run
restarts from the first page and upserts existing rows. Repeated pages upsert by
configured target, location, and review ID. A later traversal never deletes
older archived rows just because an inconsistent page omits them.

Status reports pages fetched, review records returned by those pages, the API's
reported total, the number of unique archived reviews, and a stable error code
when backfill is partial. `api_exhausted` means Google returned no next page
token. Google's documentation notes that later pages can omit reviews, so this
status does not prove lifetime completeness.

Archive search uses review text, reviewer name, rating, and owner response. It is
a review-specific API and CLI search and does not mix reviews into the generic
message search results. The Web workspace offers the same search and displays
backfill status. Google review records are part of the main archive database and
its normal archive backups.

Search queries are limited to 256 UTF-8 bytes.

## Preview and explicitly confirm a response

Set `replies_enabled = true` for the intended target when you choose to allow
responses. Actionable previews and submission require an owner API key; the Web UI
also uses its normal owner session and CSRF checks. Delegated and remote-client
identities cannot use this feature. All holders of one owner key share authority;
the key does not distinguish individual people or agents.

Provide the exact public response on standard input, never as a command-line body
argument:

```bash
printf '%s' 'Thank you for visiting.' | msgvault google-reviews preview-reply shop REVIEW_ID
```

Inspect the returned review, target, exact `comment`, `expires_at`,
`confirmation_token`, and pre-issued `receipt_id`. Whitespace is preserved. A
response must be nonempty and at most 4096 UTF-8 bytes, which is not the same as
4096 characters. Google requires prior specific and express consent for each
response; OAuth authorization alone does not provide it.

After inspecting that exact preview, make a separate confirmation:

```bash
msgvault google-reviews confirm-reply --confirmation-token TOKEN --confirmed
msgvault google-reviews receipt RECEIPT_ID
```

In the Web UI, enter the response, choose **Preview response**, inspect the exact
text and target, then choose **Submit**. Closing the preview does not
submit it. Each token expires after five minutes and can be consumed once. The
daemon checks the current account, configuration, review, and existing response
again before submission. If any binding changes, refresh and create a new preview.

Google does not document a conditional reply update. Another editor can change the
review or response between the final read and the write. The freshness check
reduces that window but cannot eliminate it.

## Reconcile a lost or uncertain response

A receipt is reserved during preview, before submission. Keep its random
`receipt_id` so you can check the outcome if the confirmation response is lost.
The Web UI offers **Check receipt** without repeating the submission.

| Receipt status | What to do |
|---|---|
| `pending_confirmation` | No confirmation has started for that receipt. Inspect the preview before any confirmation. |
| `submitting` | Check the receipt again. Do not repeat the submission. |
| `submitted` | Google accepted the response. Public visibility may depend on moderation. |
| `failed` | Inspect the live review before deciding whether to create another preview. |
| `unknown` | Inspect the live review and existing response, then obtain fresh consent if another response is needed. Never replay the old confirmation. |

Writes are attempted once. Transport errors, cancellation, some provider failures,
and malformed success responses can leave the outcome unknown. The daemon does
not retry the write automatically. Reusing a consumed token cannot repeat it.

Receipts contain only a random local ID, status, and timestamps. They remain in
memory for ten minutes and disappear on daemon restart. Previews and receipts are
bounded; a full store refuses new state before a provider write. If a receipt is
expired or unavailable, inspect the live review. There is no durable success or
idempotency guarantee.

## Disconnect and retention boundaries

```bash
msgvault google-reviews disconnect shop
```

Disconnect removes the dedicated app-bound Business Profile token and invalidates
pending previews. It stops active backfills and prevents new ones for targets
sharing that account and app until you authorize again. Previously archived
reviews remain in the archive.
It does not remove unrelated Gmail or Calendar tokens. To revoke the provider grant
as well, use your Google account's connected-app controls.

Backfill makes Google review content a permanent part of the local main archive.
Google's [Content storage policy](https://developers.google.com/my-business/content/policies#content_storage)
currently limits stored API content to 30 calendar days. That published limit
conflicts with permanent retention; this feature does not claim compliance with
that policy. The live control and archive search API responses use
`Cache-Control: no-store`; that HTTP header does not change the durable archive.
The Web UI clears live previews and receipts when leaving the workspace and does
not save them in browser storage.

Your configured account/location IDs are intentionally durable user-entered
configuration. Use the feature only for your own business through your own
approved project; it does not provide hosted access to another business.

CLI output can be retained by terminal recordings, shell tools, or other clients.
Those external systems are outside msgvault's retention controls.

MCP exposes live capability discovery by default. Live content reads and
non-actionable preview assistance require a separate explicit non-retaining-client opt-in:
`msgvault mcp --allow-google-review-content` for standalone MCP, or
`msgvault serve --mcp-allow-google-review-content` for native daemon endpoints.
See [the CLI reference](../cli-reference.md) for transport options. Use it only with a
client that does not persist, index, embed, export, or forward live provider
content outside your own project. MCP tools do not expose archived search or
backfill. Ordinary chat transcripts may retain live content and are outside this
boundary. MCP never returns an executable confirmation token or exposes
submission or receipt tools. To respond, obtain a fresh actionable preview in
the CLI or Web UI and confirm there.
