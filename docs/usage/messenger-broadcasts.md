---
last_edited: 2026-11-03
title: Messenger Broadcasts
description: Plan and send paid Messenger marketing messages to explicitly selected, opted-in subscribers.
---

Send a paid Messenger marketing message to selected subscribers, with a saved
consent decision and a receipt for each recipient. This integration is
unreleased. Your agent chooses the audience and message; msgvault checks consent
and spend limits. Sending is disabled until you add `send` to the profile's
`actions`.

## Confirm eligibility first

Configuration does not grant access to Meta's Marketing Messages API. Before
enabling sends, check Meta's current [getting started requirements](https://developers.facebook.com/documentation/business-messaging/messenger-platform/marketing-messages-on-messenger/get-started.md)
and [FAQ](https://developers.facebook.com/documentation/business-messaging/messenger-platform/marketing-messages-on-messenger/faq.md).
Confirm Tech Provider and App Review requirements, supported business regions,
and an eligible ad account with a payment method.

Live use needs the relevant Advanced Access grants: `ads_management`,
`pages_messaging`, and the marketing permission for your onboarding flow
(`marketing_messages_messenger` or `paid_marketing_messages`). Use only the
permission appropriate to that flow. The daemon checks permissions before
sending when the token supports the permission-list endpoint; Meta's send
response remains authoritative. These checks do not establish business or
regional eligibility.

The implementation is tested with synthetic fixtures. Live token pagination,
webhook envelopes and field subscriptions, campaign requests, numeric error
codes, stopped-token disappearance, and permission listing with system-user
tokens still need an owner-run check before relying on live coverage.

## Register the inbox and configure limits

Register the same app/Page pair through [Messenger inbox capture](facebook-messages.md).
Broadcast consent uses that `facebook_messages` source and its Page-scoped
participants. Keep Page and ads credentials in separate named environment
variables on the daemon host. No credential fallback is used.

```toml
[[messenger_broadcasts]]
identifier = "example-broadcasts"
app_id = "9"
page_id = "100"
ad_account_id = "700"
token_env = "EXAMPLE_PAGE_TOKEN"
ad_token_env = "EXAMPLE_ADS_TOKEN"
enabled = true
actions = []
max_daily_budget_cents = 1000
max_recipients = 100
min_interval_hours = 24
consent_max_age_hours = 24
campaign_days = 7
schedule = "23 */6 * * *"
```

The schedule refreshes subscriptions only. It never sends a broadcast and does
not change the inbox's backfill schedule. See the
[configuration reference](../configuration.md#messenger-broadcasts) for defaults
and accepted values.

## Refresh consent and select recipients

```bash
msgvault broadcast sync-subscriptions example-broadcasts --max-requests 1000 --json
msgvault broadcast subscriptions example-broadcasts --status active --json
```

Each refresh starts a new token-list walk. A complete walk refreshes consent
freshness and marks previously listed but now missing tokens absent in one
transaction. An absent token needs another complete walk before a listing can
restore its eligibility. Partial, failed, or cancelled walks retain their
observations but cannot establish fresh list-based consent or prove absence. Inspect the completion result before
planning. The CLI, HTTP API and MCP expose token fingerprints, never raw tokens.

A recipient is eligible only when its subscription is active, its expiry is
known and in the future, and a complete token-list walk confirmed it within
the configured consent age. A signed opt-in or resume updates consent status;
a complete list walk is still required because opt-out webhooks may be missing.
Token refreshes preserve the later of Meta's next eligible time and any locally
recorded cooldown. The planner also checks that time,
the configured minimum interval since an accepted send, and the requested
conversation gap. It selects one eligible token per participant or Page-scoped
ID, then applies the recipient cap. Refused recipients retain a reason.

Stop notifications and local unsubscribe take precedence over token listings.
A listing cannot reactivate either status; newer opt-in or resume evidence is
required. Message content and audience selection stay with the caller's agent.

## Create, inspect, and confirm a plan

Choose either subscription IDs or participant IDs explicitly. The following
IDs are placeholders:

```bash
msgvault broadcast create example-broadcasts \
  --name "Example update" --message-json '{"text":"Example subscriber update"}' \
  --daily-budget-cents 1000 --subscription 1 --subscription 2 --json
msgvault broadcast plan <broadcast-id> --revision <revision> --json
msgvault broadcast show <broadcast-id> --json
```

Create and plan are local operations. Planning freezes the recipient decisions
and returns a revision and `plan_digest`. Review the eligible and refused rows,
Page, message, budget and campaign duration before sending. `preview` requests
a Meta preview of the saved message; it makes a provider call.

The daily budget is an **average**, not a daily spending ceiling. For a budget
of 1,000 cents, allow up to 1,750 cents in a day and 7,000 cents in a week.
Meta bills delivered marketing messages through the ad account. A campaign
ends after `campaign_days` from creation. The daemon saves the requested end
before attempting campaign creation, including when its result is uncertain.

After eligibility and grants are confirmed, enable `actions = ["send"]` and
restart the daemon. Sending requires the exact reviewed revision and digest:

```bash
msgvault broadcast send <broadcast-id> \
  --revision <revision> --plan-digest <plan_digest> --limit 100 --json
```

The daemon rechecks consent and limits. A changed plan is refused. Campaign
creation has a 60-minute warm-up: the first send can create the campaign and
return `campaign_warming` without dispatching recipients. Retry the reviewed
send after `retry_after_seconds`. The daemon can refuse recipients whose
consent is no longer eligible. Create a new broadcast for a changed message
or audience after campaign creation. No background job sends remaining rows.

## Handle uncertain results and opt-outs

If campaign creation times out after it may have reached Meta, the broadcast
becomes `campaign_unknown`. msgvault does not create another campaign on a
retry. Find the campaign in Meta, then use
`broadcast adopt-campaign <broadcast-id> --campaign-id <campaign-id>`; the
daemon verifies its Page, matching daily budget, absence of a lifetime budget,
and an end no later than the originally requested end. Adoption preserves
Meta's actual end and applies a fresh warm-up. Missing spend settings or an
end too soon for warm-up are refused. Cancel instead if you cannot establish
which campaign was created.

A recipient whose send result is ambiguous becomes `remote_unknown` and is
never automatically sent again. A later matching echo or delivery event can
resolve its receipt. Clear provider refusals retain a category; load caps can
skip a recipient, while spend limits and circuit breakers stop the run. Inspect
`broadcast show` before taking another action.

```bash
msgvault broadcast cancel <broadcast-id> --json
msgvault broadcast unsubscribe example-broadcasts --subscription 1 --json
msgvault broadcast unsubscribe example-broadcasts --participant 1 --json
```

Cancel stops remaining local dispatch. It leaves the Meta campaign in place
until its end time; manage any further provider-side cancellation in Meta.
Unsubscribe records local suppression before requesting provider unsubscribe.
Participant selection suppresses all that participant's tokens. Local
suppression still applies when sends are disabled or the provider call fails.

## Receive events and protect stored tokens

Use `msgvault meta-webhooks subscribe` to explicitly request
`messaging_optins`, `marketing_message_echoes`, `marketing_message_deliveries`,
`marketing_message_reads`, `marketing_message_clicks`, and
`marketing_message_delivery_failed`. Configure the broadcast profile and
receiver routing separately; enabling a profile does not subscribe the app.
Consent evidence identifies the durable delivery and item index. Matching
receipts retain delivery, read, click and failure progress.
Tracking receipts that arrive before the send response are retried from the
durable spool. After 12 unsuccessful attempts, they remain failed for owner
review and replay.

Marketing echoes with a Page-scoped recipient and message ID converge with
the inbox archive and retain broadcast and campaign attribution. Custom
Audience recipients without a Page-scoped ID are not archived as messages.
Their broadcast receipts still remain available.

Raw subscription tokens are stored in the local consent tables for provider
calls. Marketing webhooks also put raw tokens in the receiver's filesystem
spool, which is outside `msgvault backup`. Protect and back up that directory
separately. Fingerprint-only output does not remove tokens from these stores.

## Use HTTP or MCP

Owner-authenticated HTTP reads are `GET /api/v1/meta/broadcasts?profile=…`,
`GET /api/v1/meta/broadcasts/{id}`, and
`GET /api/v1/meta/broadcast-subscriptions?profile=…`. Subscription reads accept
`participant` and `status` filters. Delegated tokens receive 403.

MCP exposes `list_broadcast_subscriptions`, `create_broadcast`,
`plan_broadcast`, `get_broadcast`, `send_broadcast`, `cancel_broadcast`, and
`unsubscribe_broadcast_subscription` with daemon API schema 3.38.0 or newer.
They are hidden in delegated sessions. `send_broadcast` requires client
confirmation of recipient count, Page, revision, digest and spend bounds;
clients without confirmation support fail closed.

This release does not send opt-in requests. It uses subscriptions obtained
through an existing approved opt-in flow. See the
[CLI reference](../cli-reference.md#broadcast) for every available command.
