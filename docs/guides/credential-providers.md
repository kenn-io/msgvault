---
title: External credential providers
description: Obtain scoped message MCP access through a trusted local helper.
---

Applications can use `go.kenn.io/msgvault/pkg/credentialprovider` to ask a
trusted local executable for access to a hosted message MCP endpoint. The
application does not need an archive-owner token or its own login flow.

This package supplies the exchange and validation contract. It does not add a
credential issuer to `msgvault serve`, change the standalone CLI's HTTP client,
or configure the MCP listener. The hosting service must implement approval,
issuance, revocation, and scoped reads. Its access checks remain authoritative.

## Configure a helper

The consuming application owns configuration and saved operation identities.
Use a command and argument array from trusted local configuration, for example:

```json
{
  "endpoint": "https://archive.example.org/messages/mcp",
  "credential_provider": {
    "command": "example-credential-provider",
    "args": ["--connection", "personal-archive"]
  }
}
```

Call `Exchange(ctx, argv, request)` with that executable and its arguments. No
shell is used. The helper inherits the application's environment; it is trusted
code running as the same operating-system user, not a sandbox. Never accept a
helper command from a message, remote response, or repository content. A
provider-backed connection must not fall back to an owner token on failure.

The helper receives one JSON document on stdin. It reads it with `DecodeRequest`
and writes its decision with `WriteResponse`. Each document is limited to 16 KiB,
including whitespace: it contains operation metadata, not message content. An
exchange lasts at most 60 seconds or the caller's shorter context deadline.
Cancellation stops the helper and its descendants. Stderr is discarded.

Exit 0 for a valid decision (including denial), 2 for invalid input before
performing a remote operation, and 1 for other failures. The client discards all
stdout on a nonzero exit. Errors omit helper output and parser details so a
credential cannot appear in a diagnostic.

## Request exactly the approved access

Save the complete request before calling the helper:

```json
{
  "version": 1,
  "operation": "authorize",
  "request_id": "11111111-1111-4111-8111-111111111111",
  "endpoint": "https://archive.example.org/messages/mcp",
  "archive_id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "client_id": "22222222-2222-4222-8222-222222222222",
  "delegation_id": "33333333-3333-4333-8333-333333333333",
  "source_ids": ["17", "9007199254740993"],
  "tools": ["get_message", "get_stats"]
}
```

The endpoint is an exact HTTPS URL, including its mount path, without user
information, query, or fragment. `archive_id` is the native archive UID, treated
as an opaque string. The client and delegation UUIDs identify the approved
installation and permission record; their possession does not authenticate a
caller. The helper must independently establish current authority.

Source IDs are positive int64 values written as ascending, unique decimal
strings. Empty scope and wildcards are invalid. Tool names are an explicit,
duplicate-free subset of `get_message`, `list_messages`, `list_thread`,
`search_metadata`, `search_message_bodies`, and `get_stats`. New upstream tools
do not join an existing approval. This profile excludes attachment bytes and
the native HTTP API.

## Accept a credential

A first success echoes `version`, `operation`, `request_id` and all six scope
fields exactly, and adds:

```json
{
  "status": "ready",
  "credential_id": "44444444-4444-4444-8444-444444444444",
  "credential_generation": 1,
  "issued_at": "2026-10-24T15:00:00Z",
  "expires_at": "2026-10-24T15:05:00Z",
  "credential": {"type": "bearer", "token": "synthetic-example-token"}
}
```

This fragment illustrates the added fields; the token is synthetic. The hosting
service chooses its token format and lifetime. `DecodeResponse(reader, request)`
checks a full response before it can be forwarded or used. Unknown fields,
duplicate JSON keys, null fields, extra JSON documents, and mismatched targets or
scope are rejected. A refusal must not carry scope or credentials.

Keep credentials only in process memory. Never log a credential-bearing response
or store it in configuration or an operation journal. Send it only to the exact
validated endpoint; disable redirects. Check the credential before each MCP
request, including requests during a long agent turn. Calculate its remaining
lifetime from `expires_at - issued_at`, subtracting monotonic elapsed time since
the helper started. Do not compare server expiry to the client's wall clock or
restart the clock when replaying metadata. After suspension or an uncertain
timer, obtain fresh authority before sending another request.

## Renew and recover an interrupted exchange

Save each new request before invocation. Do not retry with a different identity
when the outcome is unknown; a failed exchange may have committed remotely.

| Operation | Required fields in addition to the original scope | Result |
| --- | --- | --- |
| `authorize` | New `request_id`; no predecessor fields | First success is `ready`; exact replay is `already_issued`. |
| `renew` or `replace` | New `request_id`, `previous_request_id`, `credential_id`, `expected_generation` | First success is `ready` with a different credential ID and higher generation; exact replay is `replacement_required`. |
| `release` | New `request_id`, `authorization_request_id` naming the original authorize | `released` after the service ends that authorization and its successors. |

`already_issued` and `replacement_required` echo the same scope and credential
metadata as a success, but never a secret. A client with a matching cached
credential may keep it on `already_issued`, without extending its lifetime.
Otherwise, save and send `replace` using the returned ID and generation. A client
restart follows this same path. Renewal and replacement require current
authority; token possession alone is insufficient.

Serialize exchanges for each client installation. A generation conflict is an
explicit refusal, not permission to guess a new generation. Retain a pending
release through failures, stop using its credential, and do not report success
until `released`. Release before authorize must remain terminal in the provider.
These are responsibilities of the consumer and hosting service, not a database
implemented by this package.

Refusals contain only the request's `version`, `operation`, and `request_id`, plus
one of these status/reason pairs. Clients translate the codes into their own text.

| Status | Reason |
| --- | --- |
| `sign_in_required` | `no_current_sign_in` |
| `approval_required` | `delegation_approval_required` |
| `denied` | `delegation_inactive`, `delegation_expired` |
| `conflict` | `request_mismatch`, `generation_changed`, `binding_ended` |
| `unavailable` | `authority_unavailable` |

`released` contains only the request envelope and status, with no reason, scope,
or credential. A missing or inaccessible approval uses the same
`approval_required` response. No helper decision grants more access than the
service's current permission checks allow.
