---
title: "Microsoft IMAP PR — Remaining Fix Plan"
description: "Remaining fixes and verification steps recorded for the Microsoft IMAP pull request."
last_edited: "2026-04-02"
---

# Microsoft IMAP PR — Remaining Fix Plan

**Branch:** `microsoft-imap`
**Created:** 2026-04-01 by Opus, for Sonnet to execute
**Source:** Combined findings from Opus deep review, Sonnet security review, and 12 roborev CI review cycles on PR #228

## Already fixed (committed as `28f3e1a`)

- **H3** — Token refresh timeout (30s goroutine-based timeout in `TokenSource` closure)
- **M2** — Auth URL redaction (`redactAuthURL` helper strips state/nonce/code_challenge)
- **H4** — Token revocation on `DeleteToken` (best-effort POST to Microsoft logout endpoint)

## Remaining fixes

Each fix includes the exact file, line numbers, what to change, and why. Fixes are grouped into commits by logical area. After all fixes, run `go fmt ./... && go vet ./... && make lint && make test`.

**Important:** Line numbers below are approximate — the H3/M2/H4 commit shifted lines in `oauth.go`. Read the file fresh before editing. Use the surrounding code context (function names, comments) to locate the right spot.

---

## Commit 1: Fix `remove-account` source lookup for O365 IMAP sources (C1)

### Problem
`remove-account user@outlook.com` fails with "account not found" because `resolveSource()` in `cmd/msgvault/cmd/remove_account.go:189` uses `GetSourcesByIdentifier()`, which only matches the `identifier` column. For O365 IMAP sources, the identifier is the full IMAP URL (e.g. `imaps://user%40outlook.com@outlook.office365.com:993`), not the email. The `sync-full` and `sync` commands were already fixed to use `GetSourcesByIdentifierOrDisplayName`, but `remove-account` was not updated.

### Fix
In `cmd/msgvault/cmd/remove_account.go`, change `resolveSource()` to use `GetSourcesByIdentifierOrDisplayName` instead of `GetSourcesByIdentifier`:

```go
// BEFORE:
sources, err := s.GetSourcesByIdentifier(identifier)

// AFTER:
sources, err := s.GetSourcesByIdentifierOrDisplayName(identifier)
```

The rest of `resolveSource` already handles disambiguation (multiple matches require `--type`), so no other changes needed.

### Test
Add a test case in `cmd/msgvault/cmd/remove_account_test.go` that:
1. Creates an IMAP source with identifier `imaps://user%40outlook.com@outlook.office365.com:993` and display_name `user@outlook.com`
2. Calls `resolveSource(s, "user@outlook.com", "")`
3. Asserts it finds the source

---

## Commit 2: Token save failure on refresh must be treated as an error (H1)

### Problem
In `internal/microsoft/oauth.go`, inside the `TokenSource` closure, when `saveToken` fails after a successful refresh, only a `Warn` log is emitted. Microsoft refresh tokens are single-use (rotated on each exchange). If the save fails and the process exits, the on-disk token has a revoked refresh token, requiring full re-authorization.

### Fix
Find the `if changed {` block inside the `TokenSource` closure (inside the `case res := <-ch:` arm of the select statement). It currently looks like:

```go
if changed {
    if saveErr := m.saveToken(email, tok, scopes, tf.TenantID); saveErr != nil {
        m.logger.Warn("failed to save refreshed token", "email", email, "error", saveErr)
    }
}
```

Change to:

```go
if changed {
    if saveErr := m.saveToken(email, tok, scopes, tf.TenantID); saveErr != nil {
        return "", fmt.Errorf("save refreshed microsoft token for %s: %w (token refreshed but not persisted — re-run may require re-authorization)", email, saveErr)
    }
}
```

### Test
Add `TestTokenSource_SaveFailureReturnsError` in `internal/microsoft/oauth_test.go`. The most practical approach: test `saveToken` directly with a read-only directory and verify it returns an error, confirming the error path works. Testing the full closure requires a mock token endpoint, which is complex — a unit test of `saveToken` with a bad path is sufficient to validate the mechanism.

---

## Commit 3: Migrate pre-migration tokens by binding tenant ID on refresh (H2)

### Problem
In `internal/microsoft/oauth.go`, the `TokenSource` function validates persisted scopes against `tf.TenantID`, but this check is gated on `tf.TenantID != ""`. Tokens saved before the scope-correction feature have `TenantID == ""`, permanently bypassing validation. On refresh, the token is re-saved with the same empty `TenantID`, so the migration never happens.

### Fix
In `TokenSource`, after the scopes fallback (`if len(scopes) == 0 { scopes = scopesForEmail(email) }`) and **before** the scope validation block (`if tf.TenantID != "" && len(tf.Scopes) > 0 {`), add:

```go
// Migrate pre-migration tokens: if the token file has no tenant_id but the
// Manager was constructed with a specific tenant (not "common"), bind it.
// This allows scope validation to kick in on next load.
if tf.TenantID == "" && m.tenantID != "" && m.tenantID != DefaultTenant {
    tf.TenantID = m.tenantID
    m.logger.Info("migrating pre-scope-correction token: binding tenant ID",
        "email", email, "tenant", m.tenantID)
}
```

Verify that the save call later in the closure uses `tf.TenantID` (it already does).

### Test
Add `TestTokenSource_PreMigrationTokenGetsTenantBinding` in `internal/microsoft/oauth_test.go`:
1. Save a token with empty TenantID and org scopes
2. Create a Manager with `tenantID: "my-org-tenant"` (not "common")
3. Call `TokenSource` — should succeed (tenant gets bound internally)
4. The tenant binding is verified by the fact that `TokenSource` doesn't error — if scope validation now runs with the bound tenant and finds a mismatch, it would error

---

## Commit 4: Add Content-Type headers to OAuth callback responses (M1)

### Problem
In `internal/microsoft/oauth.go`, the `browserFlow` callback handler writes `error_description` (attacker-controllable via crafted URL) and other text into the HTTP response without setting `Content-Type: text/plain`. Browser MIME-sniffing could interpret injected HTML/JS.

### Fix
In the `HandleFunc` for `callbackPath` inside `browserFlow`, add two headers at the very top of the handler function:

```go
mux.HandleFunc(callbackPath, func(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/plain; charset=utf-8")
    w.Header().Set("X-Content-Type-Options", "nosniff")
    // ... rest of handler unchanged
```

This covers all response paths (state mismatch, error, success) with a single addition.

---

## Commit 5: Add timeout and increase shutdown grace period for browser flow (M3 + M4)

### Problem
- M3: No timeout on browser OAuth flow; port 8089 stays bound if the user abandons.
- M4: 1-second shutdown timeout may abort the callback success response.

### Fix
In `internal/microsoft/oauth.go`, in `browserFlow`:

**M3 — Add a 5-minute deadline** at the very start of `browserFlow`, before the listener bind:

```go
func (m *Manager) browserFlow(ctx context.Context, email string, scopes []string) (*oauth2.Token, string, error) {
    // Bound the entire browser flow to 5 minutes. This prevents port 8089
    // from staying bound indefinitely if the user abandons authorization.
    ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
    defer cancel()

    // Bind the listener before constructing the auth URL ...
```

**M4 — Increase shutdown timeout** from 1 second to 5 seconds. Find:

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
```

Change to:

```go
shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
```

---

## Commit 6: Restrict `openBrowser` to HTTPS only (L3) + remove mutable ScopeIMAP (M6)

### Problem
- L3: `openBrowser` permits `http://` scheme; auth URLs are always `https://`.
- M6: `var ScopeIMAP = ScopeIMAPOrg` is exported and mutable but unused.

### Fix

**L3** — In `openBrowser`, change the scheme check:

```go
// BEFORE:
if scheme != "http" && scheme != "https" {
    return fmt.Errorf("refused to open URL with scheme %q", parsed.Scheme)
}

// AFTER:
if scheme != "https" {
    return fmt.Errorf("refused to open URL with scheme %q (only https is allowed)", parsed.Scheme)
}
```

**M6** — Delete these two lines entirely:

```go
// ScopeIMAP is the organizational IMAP scope (kept for backward compatibility).
var ScopeIMAP = ScopeIMAPOrg
```

Then grep the entire codebase for `\bScopeIMAP\b` (not `ScopeIMAPOrg` or `ScopeIMAPPersonal`) to confirm nothing references it. If something does, make it unexported or a const instead of deleting.

---

## Commit 7: Log XOAUTH2 server error challenge at debug level (L2)

### Problem
`internal/imap/xoauth2.go:30-37` — On IMAP auth failure, Microsoft sends a JSON challenge with diagnostic details. The `Next()` implementation correctly returns an empty response per SASL protocol, but never logs the challenge content, making auth failures opaque.

### Fix
In `internal/imap/xoauth2.go`, add `"log/slog"` to the imports and update `Next()`:

```go
func (c *xoauth2Client) Next(challenge []byte) ([]byte, error) {
    if len(challenge) > 0 {
        slog.Debug("XOAUTH2 authentication failed", "server_challenge", string(challenge))
    }
    return []byte{}, nil
}
```

The go-imap library base64-decodes the challenge before passing it to `Next()`, so logging the raw bytes as a string is correct.

---

## Commit 8: Remove tenant ID from user-facing error message (L1)

### Problem
In `internal/microsoft/oauth.go`, the stale scope error message includes the Azure AD tenant ID (a UUID that uniquely identifies organizations). This leaks in terminal output and bug reports.

### Fix
Find the stale scope error in `TokenSource`:

```go
// BEFORE:
return nil, fmt.Errorf(
    "token for %s has stale IMAP scope %q (expected %q for tenant %s) — run 'msgvault add-o365 %s' to re-authorize",
    email, tf.Scopes[0], correctScope, tf.TenantID, email,
)

// AFTER:
m.logger.Debug("stale IMAP scope detected",
    "email", email,
    "current_scope", tf.Scopes[0],
    "expected_scope", correctScope,
    "tenant_id", tf.TenantID,
)
return nil, fmt.Errorf(
    "token for %s has stale IMAP scope — run 'msgvault add-o365 %s' to re-authorize",
    email, email,
)
```

Update `TestTokenSource_StaleScopeReturnsError` to match the new shorter error string (check for "stale IMAP scope" instead of the full message).

---

## Commit 9: Fix remove-account credential cleanup for XOAUTH2 sources (L6)

### Problem
`cmd/msgvault/cmd/remove_account.go` — For IMAP sources, the code always tries to delete the password credential file first, then also deletes the Microsoft token if XOAUTH2. XOAUTH2 sources never have an IMAP credential file.

### Fix
Replace the `case "imap":` block in `runRemoveAccount` with auth-method-aware cleanup:

```go
case "imap":
    if source.SyncConfig.Valid && source.SyncConfig.String != "" {
        imapCfg, parseErr := imaplib.ConfigFromJSON(source.SyncConfig.String)
        if parseErr == nil {
            switch imapCfg.EffectiveAuthMethod() {
            case imaplib.AuthXOAuth2:
                msMgr := microsoft.NewManager(
                    cfg.Microsoft.ClientID,
                    cfg.Microsoft.EffectiveTenantID(),
                    cfg.TokensDir(),
                    logger,
                )
                if err := msMgr.DeleteToken(imapCfg.Username); err != nil {
                    fmt.Fprintf(os.Stderr,
                        "Warning: could not remove Microsoft token: %v\n", err,
                    )
                }
            default:
                credPath := imaplib.CredentialsPath(
                    cfg.TokensDir(), source.Identifier,
                )
                if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
                    fmt.Fprintf(os.Stderr,
                        "Warning: could not remove credentials file %s: %v\n",
                        credPath, err,
                    )
                }
            }
        }
    } else {
        // No sync_config — try removing credential file as fallback
        credPath := imaplib.CredentialsPath(
            cfg.TokensDir(), source.Identifier,
        )
        if err := os.Remove(credPath); err != nil && !os.IsNotExist(err) {
            fmt.Fprintf(os.Stderr,
                "Warning: could not remove credentials file %s: %v\n",
                credPath, err,
            )
        }
    }
```

---

## Commit 10: Add sanitizeEmail null byte protection (L5)

### Problem
`internal/microsoft/oauth.go` — `sanitizeEmail` doesn't strip null bytes, which could truncate filenames on some systems.

### Fix
Add a null byte replacement at the start of `sanitizeEmail`:

```go
func sanitizeEmail(email string) string {
    safe := strings.ReplaceAll(email, "\x00", "_")
    safe = strings.ReplaceAll(safe, "/", "_")
    safe = strings.ReplaceAll(safe, "\\", "_")
    safe = strings.ReplaceAll(safe, "..", "_.._")
    return filepath.Base(safe)
}
```

Add a test case in the `TestSanitizeEmail` table:
```go
{"user\x00@evil.com", "user_@evil.com"},
```

And in `TestSanitizeEmail_NoPathTraversal`, add to the inputs:
```go
"user\x00@evil.com",
```

---

## Items NOT being fixed (by design)

| # | Issue | Reason |
|---|-------|--------|
| M5 | Concurrent double-write on token save | Idempotent writes via atomic rename; no correctness impact. Not worth the complexity of a write-coalescing lock. |
| L4 | Unverified tid peek before OIDC validation | Fully mitigated by subsequent OIDC signature/issuer/audience validation. |
| L7 | Plaintext token storage | Known gap, tracked in CLAUDE.md as "not yet implemented" (app-level encryption). |
| L8 | HTTP callback URI | Standard for desktop OAuth per Azure AD docs. |

---

## Execution checklist

After all commits:
1. `go fmt ./...`
2. `go vet ./...`
3. `make lint` (only pre-existing lint issues should remain — no new ones)
4. `make test`
5. Verify no regressions in existing tests, especially:
   - `go test ./internal/microsoft/...`
   - `go test ./internal/imap/...`
   - `go test ./cmd/msgvault/cmd/...`
