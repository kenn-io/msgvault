# Change-feed request guard

## Problem

`GET /api/v1/messages/changes` briefly takes SQLite's writer lock to prove a
safe commit bound. In keyless loopback mode, safe methods currently accept
cross-origin browser requests and trusted loopback requests bypass the shared
rate limiter. A malicious page can therefore poll the predictable endpoint
fast enough to interfere with imports and syncs.

## Design

Protect only the change-feed route, before cursor parsing or storage access:

- Give the route a dedicated per-client-IP limiter at two requests per second
  with a burst of four. No authentication mode or loopback address bypasses
  this limiter. A rejected request returns the endpoint's existing structured
  `429 rate_limit_exceeded` response and `Retry-After` header.
- In keyless loopback mode, reject an explicit `Origin` that does not match the
  effective request origin.
- Also reject Fetch Metadata whose `Sec-Fetch-Site` value is `cross-site` or
  `same-site`. Accept `same-origin`, `none`, and an absent header so the bundled
  UI, direct navigation, curl, the CLI, and older non-browser clients continue
  to work.
- API-key requests retain cross-origin support because the key is an explicit
  credential, but they still pass through the dedicated limiter to protect the
  database lock.
- Keep the endpoint as GET and leave its generated API contract unchanged.
  The operation already declares 429; the shared security layer's 403 remains
  outside individual operation schemas, consistent with the rest of the API.

## Placement and lifecycle

Wrap `handleMessageChanges` when registering the route. The wrapper owns the
browser-origin check and dedicated limiter, so rejected requests cannot enter
the handler or touch the store. Store the limiter on `Server`, initialize it
with the shared limiter, and close both during shutdown.

## Tests

Exercise the real router with a counting change-feed store:

- cross-origin `Origin`, `Sec-Fetch-Site: cross-site`, and
  `Sec-Fetch-Site: same-site` are rejected in keyless mode before the store is
  called;
- same-origin, `Sec-Fetch-Site: same-origin`, `Sec-Fetch-Site: none`, and
  headerless non-browser requests reach the handler;
- an API-key-authenticated cross-origin request remains allowed;
- the fifth immediate request from the same loopback IP returns 429 and does
  not call the store, proving loopback cannot bypass the dedicated limiter.

## Scope

This change does not alter global loopback rate-limit exemptions, other safe
GET routes, cursor semantics, or SQLite's commit-bound algorithm.
