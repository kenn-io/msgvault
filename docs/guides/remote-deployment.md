---
last_edited: "2026-10-03"
title: Remote Deployment
description: Run msgvault in Docker on a remote host and provision it from a machine with a browser.
---

Run your archive on an always-on server and use it from your own computer.
Set up source credentials on a machine with a browser, then copy a deployment
bundle to a Linux host with Docker, such as a NAS, cloud VM, or Raspberry Pi.

The flow is built on three capabilities:

- `msgvault setup` interactive wizard that can generate a deployment bundle
- `msgvault export-token` to upload OAuth tokens over API
- `[remote]` config for running local CLI commands against a remote server

!!! note "Version Requirement"
    Remote deployment requires msgvault with NAS/Docker support (`setup`, `export-token`, and the token upload API). Check that your installed version includes these commands before proceeding.

## Signed remote CLI access

Native request signing is unreleased; use a release that includes it
before exposing this listener. The daemon keeps the archive and provider
credentials. A normal CLI uses HTTPS and dedicated API and signing credentials.
Responses and exported attachments still reveal archive content to that client.
Signing does not protect data from a compromised client holding both credentials.

The main API retains its private deployment defaults and rejects signing claims
and dedicated ingress credentials. The opt-in restricted
listener always requires a valid native `X-Api-Key` and request signature. It
never enables a public route, changes a firewall, or configures a reverse proxy.
The signing configuration does not grant account isolation: allowed readers can
query the shared archive, including attachments and derived metadata.

### Prepare credentials and replay state

Create a dedicated API credential and a separate signing secret on the daemon
host. Store both in private regular files. The signing secret is base64 encoding
of exactly 64 random bytes. The API credential must contain 32 to 512 visible
ASCII characters. Credential files must be owned by the daemon user
and readable only by that user. Do not reuse the main API credential. Deliver
the two client files through an existing secure channel; do not put their values
in command arguments, repository files, or logs.

Initialize the replay state once, before enabling ingress:

```sh
msgvault signing init-state --file /srv/msgvault/signing-replay.json
```

This command creates an exclusive, private state file and does not open an
archive. Starting a verifier requires an existing valid file; it never silently
creates or replaces missing state. Keep the state and its `.lock` file on a
local filesystem with reliable atomic rename and fsync semantics. Its parent
directory must be owned by the daemon user and deny writes by other users.
On Windows, use an owner-only DACL for the files and state directory.

### Configure the daemon

```toml
[server.remote_ingress]
enabled = true
listen = "127.0.0.1:8081"
external_url = "https://archive.example.test/msgvault"
trusted_proxies = ["127.0.0.1", "::1"]
replay_state_file = "/srv/msgvault/signing-replay.json"
max_request_bytes = 16777216
max_concurrent = 4

[[server.remote_ingress.clients]]
client_id = "archive-reader"
api_key_file = "/srv/msgvault/reader-api-key"
grants = []

[[server.remote_ingress.clients.keys]]
key_id = "reader-1"
secret_file = "/srv/msgvault/reader-signing-secret"
```

The listener defaults to loopback. An explicitly selected bind address must be
loopback or private. Only connections from `trusted_proxies` can reach its
verified route surface. Configure the existing HTTPS proxy to strip exactly
`/msgvault` and preserve the escaped remainder, raw query, covered headers and
body bytes. Forwarded host, scheme and prefix headers never select the signed
authority or target. Keep the main API private and route only this listener.

The external URL must use HTTPS, contain no user information, query or fragment,
and name exactly the public origin and prefix. The prefix must agree with the
client configuration. Do not normalize ambiguous paths, decompress request
bodies, inject covered headers, or follow redirects at the proxy.

### Configure the normal CLI

```toml
[remote]
url = "https://archive.example.test/msgvault"
api_key_file = "/home/example/.config/msgvault/reader-api-key"
signing_key_id = "reader-1"
signing_secret_file = "/home/example/.config/msgvault/reader-signing-secret"
max_request_bytes = 16777216
```

The existing remote command selection applies. `--local` explicitly selects the
local daemon. Unsupported operations fail at the restricted listener; they never
fall back to a local archive. A signed client requires the complete signing
configuration and a native API key. It pins credentials to this HTTPS origin
and prefix, refuses redirects and signs every retry with a fresh nonce. There is
no discovery negotiation or unsigned retry fallback. Unsigned private clients
continue to use the existing `api_key` configuration.
If a connection loses its response, the client returns an error rather than
letting the HTTP transport resend the same signed attempt. Check the result of
a mutation before retrying it.

### Allowed operations

| CLI or API operation | Restricted ingress default |
| --- | --- |
| Schema and operation status (`GET /api/v1/health`) | Allowed |
| `stats`, `list-accounts`, `cache-stats`, FTS `search` | Allowed |
| `show-message`, raw/original message reads, thread export | Allowed with bounded reads |
| `export-attachment`, attachment reads used by exports | Allowed with size and integrity checks |
| `collection list/show`, `identity list` | Allowed |
| `collection create/add/remove/delete` | Requires `collections-write` |
| SQL queries, vector/hybrid routes, TUI browse/mutations | Denied |
| Sync, verification repair, imports, cache/FTS rebuild, uploads | Denied |
| OAuth/provider setup, tokens, administrator routes, UI, OpenAPI, pprof | Denied |
| Local files, configuration and signing state administration | Local only |

`collections-write` changes collection metadata and membership; it never permits
provider sends/deletes, archive deletion, arbitrary CLI execution or filesystem
access. Add it deliberately to one client's `grants`, then restart the daemon.
This version has no finer account or per-message access policy. Keep different
owners' archives and credentials separate.

Every native daemonclient transport is signed: typed generated requests, direct
HTTP, streaming/download requests and busy-operation retries. The restricted
listener intentionally excludes streaming mutation and upload endpoints. The
main private API retains its existing authentication and rejects signing claims.
Point signed clients at the restricted listener.
Provider configuration remains local administration. Run those commands on the
daemon host; this ingress provides no remote provider setup. `export-token` is
unavailable when request signing is configured.

### Limits and failure behavior

The restricted listener bounds headers at 16 KiB, request targets at 8 KiB and
request bodies at 16 MiB by default (configurable up to 64 MiB). Bodies are hashed
into private temporary files; large files are not buffered in memory. Its default
concurrency is four requests (maximum 32). A global and per-client budget allows
10 requests per second with a burst of 20, independent of caller-supplied IP
headers. Headers have a five-second read budget; body reads have 30 seconds.
Other requests and response writes have a five-minute budget. Attachment
streams have a five-minute first-response and write-idle budget, refreshed as
data is written; their total duration is uncapped. Reverse proxies should
enforce matching connection and header limits.

Restricted search reports existing or unconfirmed index state without starting
backfill. The server owner runs index maintenance privately. Search returns at
most 500 results per request. Thread reads return at most 500
members; an entire-thread export exceeding this limit fails without truncation.
Message details, raw MIME and original MIME are limited to 16 MiB before database
allocation or decompression. Attachment downloads stream up to 1 GiB. The CLI's
`export-attachments` and binary or `--base64` output from `export-attachment`
use that stream. The JSON mode of `export-attachment` buffers the attachment and
is limited to 32 MiB in signed mode. Other buffered signed-client responses are
bounded at 32 MiB, and error bodies at 64 KiB.
Exceeding a limit returns an error; a truncated attachment never passes its
existing content-hash verification. Cancellation releases request resources and
removes temporary body files.

Missing, invalid, expired, future-dated, revoked or replayed signatures fail with
401. A valid client calling an ungranted route receives 403. Size limits return
413 or 431; saturated concurrency/rate budgets return 429. Startup quarantine or
failed replay persistence returns 503. Errors never disclose credential values
or message bodies.
For an explicit 429 rejection, a signed native request waits one second and
retries at most twice with fresh signatures. Nonempty bodies must have a native
rewind function; otherwise the client returns the rejection. Cancellation stops
the wait. Unsigned clients keep their existing retry behavior.

### Rotation, revocation and recovery

A client can have one or two signing keys. Each key has a globally unique
`key_id`; `not_before` and `not_after` are Unix seconds. For a rotation, give the
old key a finite expiry, give the new key an activation time and limit their
overlap to 24 hours. Restart the daemon, deliver the new secret and key ID, then
remove the old key after clients have switched. Requests must expire within the
accepted key's validity window. Remove a client or key and restart to revoke it;
there is no live reload or revocation discovery.

All listeners in one process use one verifier. Its bounded nonce map admits a
nonce atomically, and fsyncs the maximum admitted expiry before executing a
request. A restart enforces both a 36-second monotonic quarantine and wall time
past the persisted expiry. A backward or frozen wall clock fences verification
until time passes the retained expiry and resumes progressing. Persistence errors
poison the verifier until recovery. The process
holds an exclusive file lock; a second verifier using that state cannot start.
Do not restore an older replay-state snapshot with the same accepted keys. Use
separate signing keys for active replicas with different state files.

If state is missing or corrupt, stop ingress, rotate every accepted signing key,
archive the broken state and initialize a new file explicitly. Reinitialize
state only after revoking the old keys. Clients may need to wait through startup
quarantine before requests succeed. Signing cannot make an old state backup safe
under reused credentials.

### Fixed wire profile

The profile uses RFC 9421 label `sig1`, HMAC-SHA256 and the RFC 9530 `sha-256`
Content-Digest, including for an empty body. It covers these components in order:
`@method`, `@target-uri`, `content-digest`, `content-type`, `x-api-key`. The required
parameters are `created`, `expires`, `nonce`, `alg="hmac-sha256"`, and `keyid`.
The client emits them in that order; the verifier accepts canonical transmitted
order with exactly those five parameters. Creation and expiry are integer Unix
seconds, with exactly a 30-second lifetime and at most five seconds of future
clock skew. A request is
expired when its expiry is reached. Each attempt uses a cryptographically random
24-byte base64url nonce. Key IDs are restricted ASCII Structured Field strings.

The target includes the configured external origin and prefix, the exact escaped
path and the raw query in its original order. Covered headers must appear exactly
once. Trailers, content encodings and ambiguous escaped paths are rejected. The
parser deliberately accepts only this fixed profile, rather than arbitrary
RFC signature dictionaries or alternate component orders. Signature parameters
are authenticated in their transmitted order. `Content-Type` is always present;
the client supplies `application/octet-stream` when absent.

For example, an empty synthetic health request has this signature base (lines
are separated by LF, with no final LF):

```text
"@method": GET
"@target-uri": https://archive.example.test/msgvault/api/v1/health
"content-digest": sha-256=:47DEQpj8HBSa+/TImW+5JCeuQeRkm5NMpJWZG3hSuFU=:
"content-type": application/octet-stream
"x-api-key": example-key-012345678901234567890123
"@signature-params": ("@method" "@target-uri" "content-digest" "content-type" "x-api-key");created=1791000000;expires=1791000030;nonce="abcdefghijklmnopqrstuvwx01234567";alg="hmac-sha256";keyid="reader-1"
```

`Signature-Input` contains `sig1=` followed by that final parameter value.
`Signature` contains `sig1=:` followed by the base64 HMAC of these bytes and a
closing `:`. The example time, nonce, and API credential are illustrative;
provision fresh random credentials and let the native CLI generate live requests.

See [RFC 9421](https://www.rfc-editor.org/rfc/rfc9421.html) and
[RFC 9530](https://www.rfc-editor.org/rfc/rfc9530.html) for the signature-base and
digest formats. The native implementation has no shared signing service.

### Before publishing ingress

Use released client and server versions that include tested native signing.
Route HTTPS only to the restricted listener. Check the final proxy's prefix and
query handling, unsigned rejection, signed read access and signed denial of UI,
provider and administrative routes. Provision dedicated client credentials
through secure delivery and keep the main API private. Keep a rollback that
removes the public route. Starting msgvault performs none of these publication
steps.

## Setup Flow Overview

1. Configure OAuth credentials and choose the remote target in `msgvault setup`
2. Copy the generated `nas-bundle` directory to your remote host
3. Run `docker-compose up -d` on the remote host
4. Add a Gmail account locally and export the token to the remote host
5. Run the initial full sync on the remote host
6. Use the `msgvault` API or CLI in remote mode

## Docker Image

Choose a published image from the [container package](https://github.com/kenn-io/msgvault/pkgs/container/msgvault)
and set its full name in your Compose file. Version tags omit the leading `v`:
for example, release 0.19.3 uses `ghcr.io/kenn-io/msgvault:0.19.3`.

`latest` is not a stable-release guarantee and can refer to an older development
snapshot. Repository-owned image publishing has been removed, so a new commit
or release tag does not automatically update GHCR. Check that the image you
choose contains the version you need. To deploy current source before a matching
image is published, follow [Container builds](../development.md#container-builds).

**Architectures:** `linux/amd64` (Intel/AMD NAS, standard servers) and `linux/arm64` (Raspberry Pi 4/5, newer NAS). Docker selects the correct one automatically.

## 1) Interactive Setup and Bundle Generation

Run the wizard once after installing msgvault:

```bash
msgvault setup
```

If you already have OAuth configured, you can skip that step during the flow.
If you choose to configure a remote server, the wizard:

- prompts for remote hostname/IP and port
- generates a random API key
- creates `<MSGVAULT_HOME>/nas-bundle`
- writes a server-ready `config.toml`
- copies `client_secret.json` into the bundle
- writes a `docker-compose.yml` for deployment
- prints the command for uploading the OAuth token once the account is added

### Bundle Contents

From the local machine, the wizard creates:

```bash
ls -la ~/.msgvault/nas-bundle
```

- `config.toml` — preconfigured server config for the remote container
- `client_secret.json` — copied OAuth credentials
- `docker-compose.yml` — ready-to-run Compose service

### Example `config.toml` Generated by Setup

```toml
[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = "<32-byte-hex-key>"

[oauth]
client_secrets = "/data/client_secret.json"

[sync]
rate_limit_qps = 5

# Accounts will be added automatically when you export tokens.
# [[accounts]] can be added manually if needed.
```

### Example `docker-compose.yml` Generated by Setup

```yaml
services:
  msgvault:
    image: ghcr.io/kenn-io/msgvault:latest
    pull_policy: always
    container_name: msgvault
    user: root
    restart: unless-stopped
    ports:
      - "8080:8080"
    volumes:
      - ./:/data
    environment:
      - TZ=America/Los_Angeles
      - MSGVAULT_HOME=/data
    command: ["serve"]
    healthcheck:
      test: ["CMD", "wget", "-qO/dev/null", "http://localhost:8080/health"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 10s
```

The wizard currently writes `:latest` as shown above. Before deployment, change
`image` to your chosen published version or digest. For an image built and
loaded locally, use its local tag and remove `pull_policy: always`.

## 2) Deploy to Remote Host

Copy the bundle and start services via SSH:

```bash
# Copy the generated bundle
scp -r ~/.msgvault/nas-bundle user@remote-host:/opt/msgvault

# Start services on the remote host
ssh user@remote-host "cd /opt/msgvault && docker-compose up -d"
```

Verify the service:

```bash
curl http://remote-host:8080/health
curl http://remote-host:8080/openapi.json
```

See [Platform Notes](#platform-notes) for Synology, QNAP, and Raspberry Pi-specific paths.

## 3) Provision Gmail Tokens

For each mailbox:

1. Add account locally (requires browser):

```bash
msgvault add-account you@gmail.com
```

2. Export the token to the remote endpoint:

```bash
msgvault export-token you@gmail.com \
  --to http://nas-ip:8080 --api-key YOUR_API_KEY --allow-insecure
```

!!! note "Why `--allow-insecure`?"
    `export-token` requires HTTPS by default. Most deployments use Tailscale or a home LAN where HTTP is fine — use `--allow-insecure` in these cases.

    If you ran `msgvault setup` and configured a remote server, the wizard already set `allow_insecure = true` in your local config, so `--allow-insecure` is not needed on the command line.

The command uploads to `POST /api/v1/auth/token/{email}` and also posts to `POST /api/v1/accounts` to register:

- default sync schedule `0 2 * * *`
- account enabled

If you did not configure remote details during setup, you can also set:

```bash
export MSGVAULT_REMOTE_URL=http://nas-ip:8080
export MSGVAULT_REMOTE_API_KEY=YOUR_API_KEY
msgvault export-token you@gmail.com --allow-insecure
```

## 4) Run Initial Full Sync

Scheduled syncs are incremental, which require a completed full sync to work.
Run the initial full sync through the daemon-backed CLI so the serving process
continues to own and serialize archive writes:

```bash
# Required — scheduled sync will not work without this
docker exec msgvault msgvault sync-full you@gmail.com

# Optional: test with a small batch first
docker exec msgvault msgvault sync-full you@gmail.com --limit 100
```

After the full sync completes, scheduled syncs run automatically on the cron schedule registered during token export (`0 2 * * *` by default). The daemon runs these itself, so there is no contention. You can also trigger manual syncs through the CLI or the incremental sync API:

```bash
# Full or incremental sync through the daemon-backed CLI
docker exec msgvault msgvault sync-full you@gmail.com
docker exec msgvault msgvault sync you@gmail.com

# Trigger incremental sync via API (only works after full sync)
curl -X POST -H "X-API-Key: YOUR_API_KEY" http://remote-host:8080/api/v1/sync/you@gmail.com

# Check schedule status
curl -H "X-API-Key: YOUR_API_KEY" http://remote-host:8080/api/v1/scheduler/status
```

Maintenance commands that mutate the archive also go through the daemon-backed
CLI. For example, `docker exec msgvault msgvault repair-encoding`,
`docker exec msgvault msgvault build-cache`, and
`docker exec msgvault msgvault rebuild-fts` send HTTP requests to the serving
process and stream its output back to the terminal instead of opening SQLite in
a second writer process.

## 5) Verify Setup

```bash
# Check token was saved
docker exec msgvault ls -la /data/tokens/

# Check daemon logs
docker logs msgvault

# Verify scheduled sync is registered
curl -H "X-API-Key: YOUR_API_KEY" http://remote-host:8080/api/v1/scheduler/status
```

!!! tip "Archive Google Calendar too"
    The same headless-token workflow archives Google Calendar. Authorize with
    `msgvault add-calendar you@gmail.com` on a machine with a browser, copy the
    token to the server (it now carries Gmail + Calendar), then add a `[[gcal]]`
    entry with a cron `schedule` so the daemon syncs it. See
    [Google Calendar](/docs/usage/calendar/).

After setup, your data directory contains:

```
/opt/msgvault/          # (or wherever you deployed the bundle)
├── config.toml           # Server configuration
├── client_secret.json    # Google OAuth credentials
├── docker-compose.yml    # Compose service definition
├── msgvault.db           # SQLite database (created on first run)
├── tokens/               # OAuth tokens (one per account)
│   └── you@gmail.com.json
├── attachments/          # Content-addressed attachment storage
└── analytics/            # Parquet cache for fast queries
```

## Open the Remote Web UI

Open the daemon URL in a browser and sign in with the configured API key. For
regular remote use, terminate HTTPS at a reverse proxy and add only that
proxy's address or CIDR to `server.trusted_proxies`; the daemon uses trusted
forwarding information to mark its browser session cookie `Secure`. Plain HTTP
on a private network is supported as an explicit tradeoff and produces a UI
warning because the cookie is not encrypted in transit. See [Web UI](/docs/web-ui/)
for the complete session and proxy model.

## Using the Local CLI Against Remote

When your local machine config has:

```toml
[remote]
url = "http://remote-host:8080"
api_key = "YOUR_API_KEY"
allow_insecure = true
```

HTTP-backed CLI commands automatically use the remote API. This includes `sync`, `sync-full`, `verify`, `search` in FTS mode, `query`, `stats`, `list-accounts`, `list-senders`, `list-domains`, `list-labels`, `identity` subcommands, `collection` subcommands, `show-message`, `export-eml`, `export-attachment`, `export-attachments`, `rebuild-fts`, `build-cache`, `cache-stats`, and `tui`.

Use `--local` only when you explicitly want the command to talk to this machine's local background daemon instead of the configured remote.

!!! tip "Remote TUI"
    The interactive TUI (`msgvault tui`) connects to the remote server automatically when `[remote]` is configured. All views, drill-downs, search, filtering, deletion staging, and attachment export work through the selected daemon. Staged deletion manifests are saved on the daemon host; attachment export streams bytes from the daemon and writes the zip file on the CLI machine. Use `--local` to force the local daemon instead of the configured remote server.

## Platform Notes

### Synology DSM

1. Install **Container Manager** (Docker) from Package Center
2. Create a shared folder for data (e.g., `/volume1/docker/msgvault`)
3. Use Container Manager UI or SSH to run docker-compose

Synology uses ACLs that can override standard Unix permissions. The generated bundle already includes `user: root` in docker-compose.yml to handle this. If you're writing your own Compose file, add `user: root` to the service.

Via SSH:

```bash
cd /volume1/docker/msgvault
docker-compose up -d
```

### QNAP

1. Install **Container Station** from App Center
2. Create a folder for data (e.g., `/share/Container/msgvault`)
3. Use Container Station or SSH to run docker-compose

### Raspberry Pi

Works on Pi 4 and Pi 5 with a 64-bit OS:

```bash
# Verify 64-bit OS
uname -m  # Should show aarch64

# Standard docker-compose setup
docker-compose up -d
```

Initial sync of large mailboxes will be slower on Pi hardware. Use `--limit` to test with a small batch first.

## Security Notes

- **Use Tailscale.** The recommended way to access your NAS remotely is via [Tailscale](https://tailscale.com/). It encrypts all traffic and avoids the need for TLS certificates, port forwarding, or reverse proxies. Use your Tailscale hostname (e.g., `http://nas.tail12345.ts.net:8080`) with `--allow-insecure`.
- **API key protects all API access.** The server requires `api_key` for non-loopback addresses. Anyone with the key can read your entire archive, so treat it like a password.
- **Don't expose port 8080 to the internet.** msgvault is designed for trusted networks. If you need internet access, use Tailscale rather than opening ports on your router.
- The generated bundle sets `user: root` in Docker Compose, which works around common NAS ACL quirks (for example Synology). On a standard Linux server you can change this to a non-root user.

## Container Management

```bash
# View logs
docker logs msgvault
docker logs -f msgvault  # Follow

# Run msgvault commands inside the container
docker exec msgvault msgvault stats
docker exec -it msgvault msgvault tui  # Interactive TUI

# Restart using the currently installed image
docker-compose restart

# Reconcile the service with the image configured in Compose
docker-compose up -d

# Pull the configured image after choosing the version to deploy
docker-compose pull
docker-compose up -d

# Stop
docker-compose down
```

`restart` does not check the registry or replace the image. Generated bundles
set `pull_policy: always`, so `up -d` checks the configured tag in GHCR. A
version-pinned service stays on that version until you edit `image`. Back up
before upgrading and keep the client and daemon versions compatible. Then use
`pull` followed by `up -d` to deploy the selected image.

Bundles generated before `pull_policy: always` was added are not rewritten
automatically. Existing installations should either regenerate the bundle,
add `pull_policy: always` to the service manually, or keep updating explicitly
with `docker compose pull` followed by `docker compose up -d`.

### Health Checks

The container includes a health check that polls `/health` every 30 seconds.

```bash
docker inspect --format='{{.State.Health.Status}}' msgvault
# Returns: healthy, unhealthy, or starting
```

### Backups

Back up the data directory regularly:

```bash
# Stop container for consistent backup
docker-compose stop
tar -czf msgvault-backup-$(date +%Y%m%d).tar.gz ./data
docker-compose start
```

Critical files:
- `msgvault.db` — email metadata and bodies
- `tokens/` — OAuth tokens (re-auth required if lost)
- `config.toml` — configuration
- `attachments/` — email attachments (large, optional if you can re-sync)

## Cron Schedule Reference

The `schedule` field in `[[accounts]]` uses standard cron format (5 fields):

```
┌───────────── minute (0-59)
│ ┌───────────── hour (0-23)
│ │ ┌───────────── day of month (1-31)
│ │ │ ┌───────────── month (1-12)
│ │ │ │ ┌───────────── day of week (0-6, 0=Sunday)
│ │ │ │ │
* * * * *
```

| Schedule | Description |
|----------|-------------|
| `0 2 * * *` | Daily at 2:00 AM |
| `0 */6 * * *` | Every 6 hours |
| `*/30 * * * *` | Every 30 minutes |
| `0 8,18 * * *` | Twice daily at 8 AM and 6 PM |
| `0 2 * * 0` | Weekly on Sunday at 2 AM |
| `0 2 1 * *` | Monthly on the 1st at 2 AM |

## Troubleshooting

### Export fails with HTTPS required

`msgvault export-token` requires HTTPS by default. If your endpoint is `http://`, add `--allow-insecure`.

### 401/authorization errors from export

Check that `X-API-Key` matches the server's `[server] api_key` and that `/api/v1/auth/token/{email}` is reachable.

### Sync fails with "no history ID" or "run full sync first"

Scheduled syncs and the public sync API run incremental syncs. You must run a full sync first through the daemon-backed CLI (see [Run Initial Full Sync](#4-run-initial-full-sync)):

```bash
docker exec msgvault msgvault sync-full you@gmail.com
```

### Account not syncing after import

Verify the account exists in the server config:

```bash
curl -H "X-API-Key: YOUR_API_KEY" http://remote-host:8080/api/v1/accounts
```

If missing, re-run `export-token`, which also posts account metadata.
