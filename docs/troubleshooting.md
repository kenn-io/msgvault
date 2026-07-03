---
title: Troubleshooting
description: Common issues and solutions.
---

## Windows Path Errors in config.toml

### "invalid escape" or "expected eight hexadecimal digits after '\U'"

TOML treats backslashes inside double-quoted strings as escape characters. A native Windows path like `"C:\Users\you\Downloads\client_secret.json"` triggers a parse error because `\U` is interpreted as a Unicode escape sequence and `\D` is an invalid escape.

**Fix:** use forward slashes or single-quoted strings in your config file:

```toml
# Option 1: forward slashes (recommended)
[oauth]
client_secrets = "C:/Users/you/Downloads/client_secret.json"

# Option 2: single-quoted string (backslashes are literal)
[oauth]
client_secrets = 'C:\Users\you\Downloads\client_secret.json'
```

See [Configuration: Windows paths](/configuration/#windows-paths) for more detail.

## OAuth Errors

### "Error 403: access_denied"

Your email isn't added as a test user. Go to **OAuth consent screen > Test users** and add your Gmail address. For Workspace accounts using a named OAuth app, add the test user in that org's Google Cloud project.

### "Access blocked: This app's request is invalid"

The Gmail scope isn't configured. Verify you added `gmail.modify` on the **Data Access** page and that Gmail API is enabled in your Google Cloud project.

### "Access blocked" or "app not approved" for Workspace accounts

The Workspace organization restricts OAuth to apps created within their org. Create a separate Google Cloud project inside the org and configure it as a named OAuth app. See [OAuth Setup: Google Workspace Accounts](/guides/oauth-setup/#google-workspace-accounts).

### "redirect_uri_mismatch"

Wrong application type. Ensure you selected **Desktop app** when creating OAuth credentials.

### "OAuth client is missing redirect_uris"

You created a "TVs and Limited Input devices" OAuth client, which doesn't work with Gmail. Google's device code flow does not support Gmail scopes. Create a new OAuth client with **Desktop app** type instead.

### General OAuth Issues

1. Remove old tokens: `rm ~/.msgvault/tokens/you@gmail.com.json`
2. Re-add account: `msgvault add-account you@gmail.com`
3. Revoke and retry: [myaccount.google.com/permissions](https://myaccount.google.com/permissions)

## Headless Server Issues

### "No browser available"

The standard OAuth flow requires a browser. For headless servers, use the copy-token workflow:

1. Authorize on a machine with a browser: `msgvault add-account you@gmail.com`
2. Copy the token file to your server: `scp ~/.msgvault/tokens/you@gmail.com.json user@server:~/.msgvault/tokens/`
3. Register on the server: `msgvault add-account you@gmail.com`

Run `msgvault add-account you@gmail.com --headless` to see detailed instructions with the exact paths for your configuration.

### Token copied but "account not found"

After copying the token, you must run `msgvault add-account you@gmail.com` on the headless server to register the account in the database. This command detects the existing token and registers the account without opening a browser.

## Remote Deployment Issues

### `export-token` says HTTPS is required

`msgvault export-token` enforces HTTPS by default. Most deployments use Tailscale or a home LAN where HTTP is fine — add `--allow-insecure`:

```bash
msgvault export-token you@gmail.com --to http://nas-ip:8080 --api-key KEY --allow-insecure
```

To avoid passing the flag every time, run `msgvault setup` with a remote server configured (it sets `allow_insecure = true` in your local config automatically), or add it manually:

```toml
[remote]
allow_insecure = true
```

### `export-token` returns 401/Forbidden

1. Verify your remote server has an `api_key` in `[server]`.
2. Confirm `MSGVAULT_REMOTE_API_KEY` / `--api-key` matches that value.
3. Ensure the remote endpoint is reachable from the local machine.

### Remote command behaves unexpectedly

Remote-aware CLI commands use `[remote].url` automatically when it is set. This includes `sync`, `sync-full`, `verify`, `search`, `query`, `show-message`, `stats`, aggregate list commands, identity commands, collection commands, attachment exports, `update-account`, `repair-encoding`, `rebuild-fts`, `build-cache`, `cache-stats`, and `tui`.

For archive-access commands, use `--local` when you want the same CLI command to talk to the local background daemon instead of the configured remote:

```bash
msgvault list-accounts --local
msgvault rebuild-fts --local
```

## Docker Container Issues

### Container won't start

Check logs first:

```bash
docker logs msgvault
```

Common causes:
- Missing `config.toml` with `[oauth] client_secrets` and `[server] bind_addr = "0.0.0.0"` + `api_key`
- Port 8080 already in use — change `api_port` in config or the port mapping in docker-compose.yml
- Volume mount permissions — on Synology, add `user: root` to docker-compose.yml (see [Platform Notes](/guides/remote-deployment/#platform-notes))

### `permission denied` on Synology

Synology ACLs override standard Unix permissions. Add `user: root` to your docker-compose.yml:

```yaml
services:
  msgvault:
    image: ghcr.io/kenn-io/msgvault:latest
    user: root
    # ... rest of config
```

### Scheduled sync not running

1. Verify accounts are configured in `config.toml`:
   ```toml
   [[accounts]]
   email = "you@gmail.com"
   schedule = "0 2 * * *"
   enabled = true
   ```

2. Verify the token exists:
   ```bash
   docker exec msgvault ls -la /data/tokens/
   ```

3. Check scheduler status:
   ```bash
   curl -H "X-API-Key: KEY" http://localhost:8080/api/v1/scheduler/status
   ```

4. Make sure you've run the initial full sync — the scheduler only runs incremental syncs.
   Run `sync-full` through the container's CLI; it will connect to the running
   daemon over HTTP and stream progress back to your terminal:
   ```bash
   docker exec msgvault msgvault sync-full you@gmail.com
   ```

### "No source found for email"

The account isn't registered in the database. Re-export the token from your local machine:

```bash
msgvault export-token you@gmail.com --to http://nas-ip:8080 --api-key KEY --allow-insecure
```

This uploads the token and registers the account. If the token file already exists on the server, register it directly:

```bash
docker exec msgvault msgvault add-account you@gmail.com
```

## Rate Limiting

If you hit Gmail API rate limits during large syncs:

1. Reduce `rate_limit_qps` in config (default: 5)
2. Use `--limit` during initial testing
3. Wait and retry. Rate limits reset over time

## Database Corruption

If the database is corrupted:

1. Back up your database: `cp ~/.msgvault/msgvault.db ~/.msgvault/msgvault.db.bak`
2. Delete and re-sync:
   ```bash
   rm ~/.msgvault/msgvault.db
   msgvault init-db
   msgvault sync-full you@gmail.com
   ```

## Interrupted Syncs

Run the same sync command again. It resumes from the last checkpoint:

```bash
msgvault sync-full you@gmail.com --after 2024-01-01 --before 2024-02-01
```

To force a fresh start:

```bash
msgvault sync-full you@gmail.com --noresume
```

## CGO / Build Errors

If you get errors about missing C compiler:

- **macOS**: Install Xcode Command Line Tools: `xcode-select --install`
- **Linux**: Install GCC: `apt install gcc` or `dnf install gcc`

CGO is required for `mattn/go-sqlite3` (FTS5 support).

## TUI Not Showing Data

If the TUI launches but shows no data:

1. Verify sync completed: `msgvault stats`
2. Rebuild the Parquet cache: `msgvault build-cache --full-rebuild`
3. Check the in-TUI account filter (shown in the title bar): press `A` to open the account selector and switch to "All Accounts"

## Web Server Issues

### "api_key is required for non-loopback bind address"

You set `bind_addr` to a non-loopback address (e.g., `0.0.0.0`) without configuring an API key. Either add an `api_key` to your `[server]` config, or set `allow_insecure = true` if you understand the security implications. See [Web Server: Security Model](/api-server/#security-model).

### Port already in use

Another process is using the configured port. Either stop the other process, or change `api_port` in your `[server]` config:

```toml
[server]
api_port = 9090
```

### HTTP 429 Too Many Requests

The API server enforces a rate limit of 10 requests per second per client IP. If you are hitting this limit, space out your requests or check the `Retry-After` response header for the wait duration.

## Using Logs for Troubleshooting

Enable file logging to capture detailed diagnostics. Add this to your `config.toml`:

```toml
[log]
enabled = true
```

Then reproduce the issue. Each run gets a unique correlation ID (`run_id`) so you can isolate its log lines:

```bash
# View today's log
msgvault logs

# Follow live while reproducing an issue
msgvault logs -f

# Filter to a specific run
msgvault logs --run-id <id>

# Show only errors
msgvault logs --level error
```

`msgvault logs` reads from the selected daemon. If `[remote].url` is configured, it shows the remote daemon's logs; otherwise it starts or contacts the local daemon.

For one-off debugging without changing config, use the `--log-file` flag:

```bash
msgvault sync --log-file /tmp/debug.log --log-level debug
```

To trace slow SQL queries, lower the threshold or enable full SQL tracing:

```bash
# Log queries slower than 50ms
msgvault tui --log-sql-slow-ms 50

# Log every SQL query (verbose)
msgvault tui --log-sql
```

Log files are stored in `<data_dir>/logs/` by default. Run `msgvault logs --path` to print the selected daemon's directory. See [Configuration: Log](/configuration/#log) for all options.

## Still Stuck?

Ask for help on the [msgvault Discord server](https://discord.gg/fDnmxB8Wkq) or [open an issue on GitHub](https://github.com/kenn-io/msgvault/issues).
