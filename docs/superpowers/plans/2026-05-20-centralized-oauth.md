# Centralized Verified Google OAuth Client — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate the bring-your-own (BYO) Google Cloud OAuth setup as a first-run prerequisite by shipping a project-owned, Google-verified OAuth client baked into the msgvault binary. BYO + named-apps + service-account paths all remain as escape valves.

**Architecture:** A new `internal/oauth/embedded.go` exposes package-level OAuth client credentials (overridable at build time via `-ldflags -X`) plus an `EmbeddedConfig` builder and `NewEmbeddedManager` factory. A new `cmd/msgvault/cmd/oauth_resolve.go` collapses today's BYO-only resolution into a single three-way helper (named BYO, global BYO, embedded). Service-account resolution remains at call sites since the SA manager has a different type. The `oauthManagerCache()` factory in `root.go` becomes the canonical refactor target; downstream consumers like `syncfull.go` inherit the new behavior automatically. Verification-window failures (100-user lifetime cap, unlisted test users) surface as a typed `access_denied` error in the OAuth callback handler; the Manager prints a fallback message when it sees this on the embedded path.

**Tech Stack:** Go 1.x, `golang.org/x/oauth2`, `golang.org/x/oauth2/google`, Cobra CLI, stdlib `testing` (the existing msgvault test suite uses `t.Errorf`/`t.Fatalf`/`t.Run` patterns rather than testify, so plan tests follow that convention for consistency), GNU Make, GitHub Actions.

---

## Spec reference

The full design is at `docs/superpowers/specs/2026-05-20-centralized-oauth-design.md`. Read it before implementing.

## Prerequisites (out of band, before merging)

The dev and production Cloud projects need to exist before this work can ship usefully:

- **Dev Google Cloud project** ("msgvault-dev"): create the project, register a Desktop OAuth client, list current contributors as test users, low API quota. Its `client_id` and `client_secret` become the source defaults in `internal/oauth/embedded.go` (Task 2).
- **Production Google Cloud project**: owned by the project maintainer. Its `client_id` and `client_secret` are injected at release time via GitHub Actions Secrets (Task 17, Task 18). Production verification (consent screen submission, brand verification, CASA Tier 2 assessment) is an out-of-band process tracked in the spec.

The code in this plan compiles and tests pass with the placeholder string `"TBD-msgvault-dev-client-id"`. The dev project's real values must be substituted before the change is useful at runtime in source builds.

## File structure

New files:
- `internal/oauth/embedded.go` — package vars `oauthClientID` / `oauthClientSecret`, `EmbeddedConfig`, `NewEmbeddedManager`, `HasEmbeddedCredentials`
- `internal/oauth/embedded_test.go` — unit tests for the above
- `cmd/msgvault/cmd/oauth_resolve.go` — `resolveOAuthManager` three-way helper
- `cmd/msgvault/cmd/oauth_resolve_test.go` — unit tests for the helper

Modified files:
- `internal/oauth/oauth.go` — add `ScopesEmbedded`, add `errAccessDenied` typed error, add `isEmbedded` field to `Manager`, modify `newCallbackHandler` to detect `error=access_denied`, modify `authorize` to print the embedded fallback message on access_denied
- `internal/oauth/oauth_test.go` — add `TestScopesEmbedded`, `TestCallbackHandlerAccessDenied`, `TestAuthorizeEmbeddedFallbackMessage`, `TestAuthorizeNonEmbeddedNoFallback`
- `internal/config/config.go` — remove `HasAnyConfig` method
- `internal/config/config_test.go` — remove `TestOAuthConfig_HasAnyConfig` and inline `HasAnyConfig` assertions
- `cmd/msgvault/cmd/root.go` — refactor `oauthManagerCache()` to call `resolveOAuthManager`; remove `errOAuthNotConfigured`, `tryFindClientSecrets`, `oauthSetupHint`, `wrapOAuthError`
- `cmd/msgvault/cmd/root_test.go` — remove tests for the deleted symbols
- `cmd/msgvault/cmd/addaccount.go` — replace BYO branch (`ClientSecretsFor` + `NewManager` + `errOAuthNotConfigured`/`wrapOAuthError`) with `resolveOAuthManager` call; remove the local `clientSecretsPath` variable
- `cmd/msgvault/cmd/addaccount_test.go` — add cases for the three resolver branches plus the named-app-not-found error
- `cmd/msgvault/cmd/deletions.go` — remove the `HasAnyConfig` gate, refactor the `!isServiceAccount` block, refactor the `getOAuthMgr` lambda, change `promptScopeEscalation`'s parameter from `clientSecretsPath` to `appName`, drop the local `clientSecretsPath` variable
- `cmd/msgvault/cmd/verify.go` — remove the `HasAnyConfig` gate; replace the OAuth fallback arm with `resolveOAuthManager`
- `cmd/msgvault/cmd/serve.go` — remove the `HasAnyConfig` startup gate
- `cmd/msgvault/cmd/syncfull.go` — remove the per-source `HasAnyConfig` skip
- `cmd/msgvault/cmd/sync.go` — remove the per-source `HasAnyConfig` skip
- `cmd/msgvault/cmd/setup.go` — remove `setupOAuthSecrets` and the call from `runSetup`; make the bundle's `[oauth] client_secrets` line conditional
- `cmd/msgvault/cmd/setup_test.go` — drop tests of the removed interactive prompt; update bundle tests to reflect the conditional `[oauth]` section
- `Makefile` — extend `LDFLAGS` with two more `-X` entries for the OAuth credentials
- `.github/workflows/release.yml` — inject `MSGVAULT_OAUTH_CLIENT_ID` and `MSGVAULT_OAUTH_CLIENT_SECRET` from GitHub Secrets into the release build step
- `README.md` — drop the "Follow the OAuth Setup Guide" prerequisite from Quick Start; add an "Advanced: bring your own OAuth client" subsection
- `cmd/msgvault/cmd/quickstart.md` — same shape as README updates

---

## Task 1: Add `ScopesEmbedded` scope set

**Files:**
- Modify: `internal/oauth/oauth.go` (around line 35, after `ScopesDeletion`)
- Test: `internal/oauth/oauth_test.go` (add a new test function)

- [ ] **Step 1: Write the failing test**

Add this test to `internal/oauth/oauth_test.go`:

```go
func TestScopesEmbedded(t *testing.T) {
	want := []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		"https://mail.google.com/",
	}
	if len(ScopesEmbedded) != len(want) {
		t.Fatalf("ScopesEmbedded has %d entries, want %d", len(ScopesEmbedded), len(want))
	}
	for i, scope := range want {
		if ScopesEmbedded[i] != scope {
			t.Errorf("ScopesEmbedded[%d] = %q, want %q", i, ScopesEmbedded[i], scope)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" -run TestScopesEmbedded ./internal/oauth/`

Expected: FAIL with `undefined: ScopesEmbedded`.

- [ ] **Step 3: Add the variable**

In `internal/oauth/oauth.go`, right after the `ScopesDeletion` block (around line 37), insert:

```go
// ScopesEmbedded is the scope set requested by the centralized verified
// msgvault OAuth client. It is the union of Scopes and ScopesDeletion so
// users on the embedded path never need scope escalation for permanent
// delete.
var ScopesEmbedded = []string{
	"https://www.googleapis.com/auth/gmail.readonly",
	"https://www.googleapis.com/auth/gmail.modify",
	"https://mail.google.com/",
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags "fts5 sqlite_vec" -run TestScopesEmbedded ./internal/oauth/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/oauth/oauth.go internal/oauth/oauth_test.go
git commit -m "feat(oauth): add ScopesEmbedded for the verified client"
```

---

## Task 2: Create embedded credentials module

**Files:**
- Create: `internal/oauth/embedded.go`
- Create: `internal/oauth/embedded_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/oauth/embedded_test.go`:

```go
package oauth

import (
	"log/slog"
	"testing"

	"golang.org/x/oauth2/google"
)

func TestHasEmbeddedCredentials(t *testing.T) {
	// Save and restore package vars around the test
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()

	tests := []struct {
		name   string
		id     string
		secret string
		want   bool
	}{
		{"both set", "id", "secret", true},
		{"id only", "id", "", false},
		{"secret only", "", "secret", false},
		{"neither", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			oauthClientID = tc.id
			oauthClientSecret = tc.secret
			if got := HasEmbeddedCredentials(); got != tc.want {
				t.Errorf("HasEmbeddedCredentials() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEmbeddedConfig(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = "test-client-id"
	oauthClientSecret = "test-client-secret"

	scopes := []string{"scope-a", "scope-b"}
	cfg := EmbeddedConfig(scopes)

	if cfg.ClientID != "test-client-id" {
		t.Errorf("ClientID = %q, want %q", cfg.ClientID, "test-client-id")
	}
	if cfg.ClientSecret != "test-client-secret" {
		t.Errorf("ClientSecret = %q, want %q", cfg.ClientSecret, "test-client-secret")
	}
	if len(cfg.Scopes) != 2 || cfg.Scopes[0] != "scope-a" || cfg.Scopes[1] != "scope-b" {
		t.Errorf("Scopes = %v, want %v", cfg.Scopes, scopes)
	}
	if cfg.Endpoint != google.Endpoint {
		t.Errorf("Endpoint = %v, want google.Endpoint", cfg.Endpoint)
	}
}

func TestNewEmbeddedManager(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = "test-client-id"
	oauthClientSecret = "test-client-secret"

	tokensDir := t.TempDir()
	mgr, err := NewEmbeddedManager(tokensDir, slog.Default(), ScopesEmbedded)
	if err != nil {
		t.Fatalf("NewEmbeddedManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("NewEmbeddedManager returned nil manager")
	}
	if mgr.tokensDir != tokensDir {
		t.Errorf("tokensDir = %q, want %q", mgr.tokensDir, tokensDir)
	}
	if !mgr.isEmbedded {
		t.Error("isEmbedded = false, want true")
	}
}

func TestNewEmbeddedManagerWithoutCredentials(t *testing.T) {
	origID, origSecret := oauthClientID, oauthClientSecret
	defer func() {
		oauthClientID = origID
		oauthClientSecret = origSecret
	}()
	oauthClientID = ""
	oauthClientSecret = ""

	_, err := NewEmbeddedManager(t.TempDir(), slog.Default(), ScopesEmbedded)
	if err == nil {
		t.Fatal("NewEmbeddedManager: want error when credentials are empty, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags "fts5 sqlite_vec" -run 'TestHasEmbeddedCredentials|TestEmbeddedConfig|TestNewEmbeddedManager' ./internal/oauth/`

Expected: FAIL with `undefined: oauthClientID`, `undefined: HasEmbeddedCredentials`, `undefined: EmbeddedConfig`, `undefined: NewEmbeddedManager`, and `undefined: isEmbedded`.

- [ ] **Step 3: Add the `isEmbedded` field to the Manager struct**

In `internal/oauth/oauth.go`, modify the `Manager` struct (around lines 57-67) to add `isEmbedded bool` at the end:

```go
type Manager struct {
	config     *oauth2.Config
	tokensDir  string
	logger     *slog.Logger
	profileURL string

	browserFlowFn func(ctx context.Context, email string, launchBrowser bool) (*oauth2.Token, error)

	// isEmbedded is true when this manager uses the centralized verified
	// OAuth client (via NewEmbeddedManager) rather than a BYO
	// client_secrets file. Used to enable the verification-window
	// fallback message on access_denied.
	isEmbedded bool
}
```

- [ ] **Step 4: Create the embedded module**

Create `internal/oauth/embedded.go`:

```go
package oauth

import (
	"fmt"
	"log/slog"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

// oauthClientID and oauthClientSecret hold the centralized verified
// msgvault OAuth client credentials. They are package vars (not consts)
// so release builds can override them via:
//
//   go build -ldflags "-X github.com/wesm/msgvault/internal/oauth.oauthClientID=..."
//
// Per https://developers.google.com/identity/protocols/oauth2 the desktop
// client secret is "obviously not treated as a secret"; PKCE provides the
// flow security. The values below are the dev project's credentials,
// suitable for contributor builds. Production binaries override both.
var (
	oauthClientID     = "TBD-msgvault-dev-client-id"
	oauthClientSecret = "TBD-msgvault-dev-client-secret"
)

// HasEmbeddedCredentials reports whether the package vars are non-empty.
// Forks that strip the values out (or contributors testing the fallback)
// will see this return false, in which case NewEmbeddedManager refuses
// to construct an embedded manager.
func HasEmbeddedCredentials() bool {
	return oauthClientID != "" && oauthClientSecret != ""
}

// EmbeddedConfig returns the oauth2.Config built from the embedded
// credentials. RedirectURL is set later inside Manager.browserFlow when
// the loopback port is known; the rest of the config is fixed here.
func EmbeddedConfig(scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     oauthClientID,
		ClientSecret: oauthClientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       scopes,
	}
}

// NewEmbeddedManager constructs a Manager backed by the centralized
// verified OAuth client. Returns an error when credentials are missing
// (forks, stripped builds, or contributors who blanked the vars
// locally).
func NewEmbeddedManager(tokensDir string, logger *slog.Logger, scopes []string) (*Manager, error) {
	if !HasEmbeddedCredentials() {
		return nil, fmt.Errorf("no embedded OAuth credentials in this build")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		config:     EmbeddedConfig(scopes),
		tokensDir:  tokensDir,
		logger:     logger,
		isEmbedded: true,
	}, nil
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -tags "fts5 sqlite_vec" -run 'TestHasEmbeddedCredentials|TestEmbeddedConfig|TestNewEmbeddedManager' ./internal/oauth/`

Expected: PASS.

- [ ] **Step 6: Run the full oauth package tests to confirm no regressions**

Run: `go test -tags "fts5 sqlite_vec" ./internal/oauth/`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/oauth/embedded.go internal/oauth/embedded_test.go internal/oauth/oauth.go
git commit -m "feat(oauth): add embedded credentials module"
```

---

## Task 3: Add `errAccessDenied` typed error and detect it in the OAuth callback

**Files:**
- Modify: `internal/oauth/oauth.go` (add `errAccessDenied`, modify `newCallbackHandler`)
- Test: `internal/oauth/oauth_test.go` (add callback test)

- [ ] **Step 1: Write the failing test**

Add to `internal/oauth/oauth_test.go`:

```go
func TestCallbackHandlerAccessDenied(t *testing.T) {
	mgr := &Manager{logger: slog.Default()}
	codeChan := make(chan string, 1)
	errChan := make(chan error, 1)
	handler := mgr.newCallbackHandler("expected-state", codeChan, errChan)

	req := httptest.NewRequest("GET", "/callback?error=access_denied&state=expected-state", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	select {
	case err := <-errChan:
		if !errors.Is(err, errAccessDenied) {
			t.Errorf("callback error = %v, want errAccessDenied", err)
		}
	default:
		t.Fatal("callback handler did not send an error")
	}
}
```

You may need to add `"errors"`, `"log/slog"`, `"net/http/httptest"` imports to the test file if not already present.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -tags "fts5 sqlite_vec" -run TestCallbackHandlerAccessDenied ./internal/oauth/`

Expected: FAIL with `undefined: errAccessDenied`.

- [ ] **Step 3: Add the typed error and modify the callback handler**

In `internal/oauth/oauth.go`, near the existing error definitions (e.g., right after `TokenMismatchError`), add:

```go
// errAccessDenied is returned by the OAuth callback when Google
// signals that the authorization was rejected. On the embedded path
// this is the failure mode for "caller is not on the test-user list"
// and "100-user lifetime cap reached" during the verification window;
// on the BYO path it usually means the user clicked Deny.
var errAccessDenied = errors.New("oauth: access_denied")
```

Make sure `"errors"` is imported.

Now modify `newCallbackHandler` (around lines 210-226). Insert the access_denied check before the state check (since Google sends the error param when consent is denied and the state may still be present). The updated handler:

```go
func (m *Manager) newCallbackHandler(expectedState string, codeChan chan<- string, errChan chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if errParam := r.URL.Query().Get("error"); errParam != "" {
			if errParam == "access_denied" {
				errChan <- errAccessDenied
				_, _ = fmt.Fprintf(w, "Authorization denied. You can close this window.")
				return
			}
			errChan <- fmt.Errorf("oauth callback error: %s", errParam)
			_, _ = fmt.Fprintf(w, "Error: %s", errParam)
			return
		}
		if r.URL.Query().Get("state") != expectedState {
			errChan <- fmt.Errorf("state mismatch: possible CSRF attack")
			_, _ = fmt.Fprintf(w, "Error: state mismatch")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errChan <- fmt.Errorf("no code in callback")
			_, _ = fmt.Fprintf(w, "Error: no authorization code received")
			return
		}
		codeChan <- code
		_, _ = fmt.Fprintf(w, "Authorization successful! You can close this window.")
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -tags "fts5 sqlite_vec" -run TestCallbackHandlerAccessDenied ./internal/oauth/`

Expected: PASS.

- [ ] **Step 5: Run the full oauth package tests to confirm no regressions**

Run: `go test -tags "fts5 sqlite_vec" ./internal/oauth/`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/oauth/oauth.go internal/oauth/oauth_test.go
git commit -m "feat(oauth): detect access_denied in OAuth callback"
```

---

## Task 4: Print embedded-path fallback message on access_denied

**Files:**
- Modify: `internal/oauth/oauth.go` (modify `authorize`)
- Test: `internal/oauth/oauth_test.go` (add test using `browserFlowFn` override)

- [ ] **Step 1: Write the failing test**

Add to `internal/oauth/oauth_test.go`:

```go
func TestAuthorizeEmbeddedFallbackMessage(t *testing.T) {
	tokensDir := t.TempDir()
	mgr := &Manager{
		config:    &oauth2.Config{ClientID: "x", ClientSecret: "y", Scopes: []string{"s"}},
		tokensDir: tokensDir,
		logger:    slog.Default(),
		isEmbedded: true,
		browserFlowFn: func(ctx context.Context, email string, launchBrowser bool) (*oauth2.Token, error) {
			return nil, errAccessDenied
		},
	}

	var buf bytes.Buffer
	origStdout := stdout
	stdout = &buf
	defer func() { stdout = origStdout }()

	err := mgr.Authorize(context.Background(), "u@example.com")
	if !errors.Is(err, errAccessDenied) {
		t.Fatalf("Authorize error = %v, want errAccessDenied", err)
	}
	if !strings.Contains(buf.String(), "still in Google's verification") {
		t.Errorf("expected fallback message, got: %q", buf.String())
	}
}

func TestAuthorizeNonEmbeddedNoFallback(t *testing.T) {
	mgr := &Manager{
		config:    &oauth2.Config{ClientID: "x", ClientSecret: "y", Scopes: []string{"s"}},
		tokensDir: t.TempDir(),
		logger:    slog.Default(),
		// isEmbedded: false (default)
		browserFlowFn: func(ctx context.Context, email string, launchBrowser bool) (*oauth2.Token, error) {
			return nil, errAccessDenied
		},
	}

	var buf bytes.Buffer
	origStdout := stdout
	stdout = &buf
	defer func() { stdout = origStdout }()

	err := mgr.Authorize(context.Background(), "u@example.com")
	if !errors.Is(err, errAccessDenied) {
		t.Fatalf("Authorize error = %v, want errAccessDenied", err)
	}
	if strings.Contains(buf.String(), "still in Google's verification") {
		t.Errorf("did not expect fallback message for non-embedded, got: %q", buf.String())
	}
}
```

You may need to add `"bytes"`, `"strings"`, `"context"`, `"io"` imports if not already present.

- [ ] **Step 2: Add a package-level `stdout io.Writer` so tests can capture output**

In `internal/oauth/oauth.go`, near the top of the file (after the imports and existing package vars), add:

```go
// stdout is the destination for user-facing messages printed during
// the OAuth flow. Replaceable in tests via the var to capture output.
var stdout io.Writer = os.Stdout
```

Add `"io"` to the imports if not already present.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test -tags "fts5 sqlite_vec" -run 'TestAuthorizeEmbedded|TestAuthorizeNonEmbedded' ./internal/oauth/`

Expected: FAIL (the fallback message is not yet printed).

- [ ] **Step 4: Modify `authorize` to print the fallback on embedded access_denied**

In `internal/oauth/oauth.go`, modify the `authorize` method (around lines 180-202) to detect `errAccessDenied` on the embedded path and print the fallback message before returning. Replace the function body with:

```go
func (m *Manager) authorize(
	ctx context.Context, email string, launchBrowser bool,
) error {
	flow := m.browserFlow
	if m.browserFlowFn != nil {
		flow = m.browserFlowFn
	}
	token, err := flow(ctx, email, launchBrowser)
	if err != nil {
		if m.isEmbedded && errors.Is(err, errAccessDenied) {
			fmt.Fprint(stdout, embeddedFallbackMessage)
		}
		return err
	}

	if _, err := m.resolveTokenEmail(ctx, email, token); err != nil {
		return err
	}

	return m.saveToken(email, token, m.config.Scopes)
}
```

And add the fallback message constant near `errAccessDenied`:

```go
const embeddedFallbackMessage = `
msgvault's centralized OAuth client is still in Google's verification
queue. Two options:

  1. Use the bring-your-own setup (one-time, ~5 minutes):
     https://msgvault.io/guides/oauth-setup/

  2. Request beta access (open a GitHub issue with your Gmail address):
     https://github.com/wesm/msgvault/issues/new?template=beta-oauth.md

`
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test -tags "fts5 sqlite_vec" -run 'TestAuthorizeEmbedded|TestAuthorizeNonEmbedded' ./internal/oauth/`

Expected: PASS.

- [ ] **Step 6: Run the full oauth package tests to confirm no regressions**

Run: `go test -tags "fts5 sqlite_vec" ./internal/oauth/`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/oauth/oauth.go internal/oauth/oauth_test.go
git commit -m "feat(oauth): print fallback message on embedded access_denied"
```

---

## Task 5: Create `resolveOAuthManager` helper

**Files:**
- Create: `cmd/msgvault/cmd/oauth_resolve.go`
- Create: `cmd/msgvault/cmd/oauth_resolve_test.go`

- [ ] **Step 1: Write the failing tests**

Create `cmd/msgvault/cmd/oauth_resolve_test.go`:

```go
package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/oauth"
)

// writeStubClientSecrets writes a minimal valid client_secret.json that
// parseClientSecrets will accept. We only need this to verify the BYO
// path returns a non-nil manager — we don't run any OAuth flow.
func writeStubClientSecrets(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	const stub = `{"installed":{"client_id":"abc","client_secret":"xyz","redirect_uris":["http://localhost"]}}`
	if err := os.WriteFile(path, []byte(stub), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// newTestConfig returns a Config with Data.DataDir set to a fresh temp
// directory. TokensDir() returns <tmp>/tokens, which is what the
// resolver passes to the OAuth manager constructors.
func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	return &config.Config{
		Data: config.DataConfig{DataDir: t.TempDir()},
	}
}

func TestResolveOAuthManager_NamedBYO(t *testing.T) {
	cfg := newTestConfig(t)
	secrets := writeStubClientSecrets(t, cfg.Data.DataDir, "acme.json")
	cfg.OAuth.Apps = map[string]config.OAuthApp{"acme": {ClientSecrets: secrets}}
	mgr, err := resolveOAuthManager(cfg, "acme", oauth.Scopes, slog.Default())
	if err != nil {
		t.Fatalf("resolveOAuthManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("manager is nil")
	}
}

func TestResolveOAuthManager_NamedNotConfigured(t *testing.T) {
	cfg := newTestConfig(t)
	_, err := resolveOAuthManager(cfg, "nonexistent", oauth.Scopes, slog.Default())
	if err == nil {
		t.Fatal("expected error for unknown app name")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error %q should mention the app name", err.Error())
	}
}

func TestResolveOAuthManager_GlobalBYO(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.OAuth.ClientSecrets = writeStubClientSecrets(t, cfg.Data.DataDir, "default.json")
	mgr, err := resolveOAuthManager(cfg, "", oauth.Scopes, slog.Default())
	if err != nil {
		t.Fatalf("resolveOAuthManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("manager is nil")
	}
}

func TestResolveOAuthManager_Embedded(t *testing.T) {
	// Embedded credentials must be non-empty in this test (they are by
	// default — the source has the dev placeholder strings).
	cfg := newTestConfig(t)
	mgr, err := resolveOAuthManager(cfg, "", oauth.Scopes, slog.Default())
	if err != nil {
		t.Fatalf("resolveOAuthManager: %v", err)
	}
	if mgr == nil {
		t.Fatal("manager is nil")
	}
}
```

You may need to check the exact name of the Data config struct in `internal/config/config.go` — the field on Config is called `Data` and its `DataDir` field holds the path. If the struct type name differs (e.g., `DataConfig` vs another name), adjust the import or the literal accordingly.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -tags "fts5 sqlite_vec" -run TestResolveOAuthManager ./cmd/msgvault/cmd/`

Expected: FAIL with `undefined: resolveOAuthManager`.

- [ ] **Step 3: Create the resolver helper**

Create `cmd/msgvault/cmd/oauth_resolve.go`:

```go
package cmd

import (
	"fmt"
	"log/slog"

	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/oauth"
)

// resolveOAuthManager builds the *oauth.Manager appropriate for the
// account+config+scopes triple. Resolution order:
//
//  1. Named BYO: appName is non-empty and cfg.OAuth.Apps[appName] has
//     client_secrets set — use that BYO OAuth client.
//  2. (If appName is non-empty but no client_secrets is registered for
//     it) — return an error rather than falling through to embedded,
//     because the user explicitly named a binding that doesn't exist.
//  3. Global BYO: appName is empty and cfg.OAuth.ClientSecrets is set —
//     use the global BYO client.
//  4. Embedded: otherwise, use the centralized verified client. On the
//     embedded path the manager is always built with oauth.ScopesEmbedded,
//     ignoring the caller's per-call scope choice, because the embedded
//     grant is broader than any per-call need.
//
// Callers handle service-account resolution themselves (via
// cfg.OAuth.ServiceAccountKeyFor(appName)) before calling this helper,
// because *oauth.Manager and the service-account manager have
// different interfaces.
func resolveOAuthManager(
	cfg *config.Config,
	appName string,
	scopes []string,
	logger *slog.Logger,
) (*oauth.Manager, error) {
	if appName != "" {
		app, ok := cfg.OAuth.Apps[appName]
		if !ok || app.ClientSecrets == "" {
			return nil, fmt.Errorf("OAuth app %q not configured (add [oauth.apps.%s] client_secrets to config.toml, or omit --oauth-app to use the embedded client)", appName, appName)
		}
		return oauth.NewManagerWithScopes(app.ClientSecrets, cfg.TokensDir(), logger, scopes)
	}

	if cfg.OAuth.ClientSecrets != "" {
		return oauth.NewManagerWithScopes(cfg.OAuth.ClientSecrets, cfg.TokensDir(), logger, scopes)
	}

	return oauth.NewEmbeddedManager(cfg.TokensDir(), logger, oauth.ScopesEmbedded)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestResolveOAuthManager ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/msgvault/cmd/oauth_resolve.go cmd/msgvault/cmd/oauth_resolve_test.go
git commit -m "feat(oauth): add resolveOAuthManager three-way helper"
```

---

## Task 6: Wire `oauthManagerCache` to use `resolveOAuthManager`

**Files:**
- Modify: `cmd/msgvault/cmd/root.go` (the `oauthManagerCache` function at lines 419-442)

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Refactor `oauthManagerCache` to delegate to the resolver**

In `cmd/msgvault/cmd/root.go`, replace the existing `oauthManagerCache` (around line 419) with:

```go
// oauthManagerCache returns a resolver function that lazily creates and
// caches oauth.Manager instances keyed by app name. The cache is safe
// for concurrent use (serve runs scheduled syncs in goroutines). The
// underlying resolution is delegated to resolveOAuthManager.
func oauthManagerCache() func(appName string) (*oauth.Manager, error) {
	var mu sync.Mutex
	managers := map[string]*oauth.Manager{}
	return func(appName string) (*oauth.Manager, error) {
		mu.Lock()
		defer mu.Unlock()
		if mgr, ok := managers[appName]; ok {
			return mgr, nil
		}
		mgr, err := resolveOAuthManager(cfg, appName, oauth.Scopes, logger)
		if err != nil {
			return nil, err
		}
		managers[appName] = mgr
		return mgr, nil
	}
}
```

(`wrapOAuthError` is no longer needed here; we'll remove it in Task 13.)

- [ ] **Step 3: Run tests to verify behavior is preserved**

Run: `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cmd/msgvault/cmd/root.go
git commit -m "refactor(oauth): route oauthManagerCache through resolveOAuthManager"
```

---

## Task 7: Refactor `addaccount.go` BYO branch

**Files:**
- Modify: `cmd/msgvault/cmd/addaccount.go` (around lines 164-176)
- Test: `cmd/msgvault/cmd/addaccount_test.go`

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestAddAccount ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Replace the BYO branch and remove the unused `clientSecretsPath` variable**

`clientSecretsPath` in `addaccount.go` is declared at line 51 and only used at lines 165 and 174 (both in the branch we're replacing). After the refactor it has no callers, so delete the declaration too.

a. Delete the `var clientSecretsPath string` line (around line 51).

b. Replace the BYO branch (around lines 164-176):

```go
// Resolve client secrets path (standard OAuth flow)
clientSecretsPath, err = cfg.OAuth.ClientSecretsFor(resolvedApp)
if err != nil {
	if !cfg.OAuth.HasAnyConfig() {
		return errOAuthNotConfigured()
	}
	return err
}

// Create OAuth manager
oauthMgr, err := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
if err != nil {
	return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
}
```

with:

```go
// Build the OAuth manager. resolveOAuthManager handles named BYO,
// global BYO, and the embedded fallback automatically.
oauthMgr, err := resolveOAuthManager(cfg, resolvedApp, oauth.Scopes, logger)
if err != nil {
	return err
}
```

- [ ] **Step 3: Add resolution-branch tests to addaccount_test.go**

Add to `cmd/msgvault/cmd/addaccount_test.go`:

```go
func TestAddAccount_ResolverBranches(t *testing.T) {
	tests := []struct {
		name        string
		appName     string
		setup       func(cfg *config.Config, t *testing.T)
		wantErr     bool
		errContains string
	}{
		{
			name:    "named BYO with client_secrets",
			appName: "acme",
			setup: func(cfg *config.Config, t *testing.T) {
				path := writeStubClientSecrets(t, cfg.Data.DataDir, "acme.json")
				cfg.OAuth.Apps = map[string]config.OAuthApp{"acme": {ClientSecrets: path}}
			},
			wantErr: false,
		},
		{
			name:        "named app without client_secrets",
			appName:     "missing",
			setup:       func(cfg *config.Config, t *testing.T) {},
			wantErr:     true,
			errContains: "missing",
		},
		{
			name:    "global BYO",
			appName: "",
			setup: func(cfg *config.Config, t *testing.T) {
				cfg.OAuth.ClientSecrets = writeStubClientSecrets(t, cfg.Data.DataDir, "default.json")
			},
			wantErr: false,
		},
		{
			name:    "no config falls through to embedded",
			appName: "",
			setup:   func(cfg *config.Config, t *testing.T) {},
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := newTestConfig(t)
			tc.setup(cfg, t)
			_, err := resolveOAuthManager(cfg, tc.appName, oauth.Scopes, slog.Default())
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q should contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
```

(`writeStubClientSecrets` and `newTestConfig` were added in Task 5. Both test files can share them because they're in the same package.)

- [ ] **Step 4: Run tests to verify everything passes**

Run: `go test -tags "fts5 sqlite_vec" -run 'TestAddAccount|TestResolveOAuthManager' ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 5: Build the project to catch unused-variable errors**

Run: `go build -tags "fts5 sqlite_vec" ./...`

Expected: success.

- [ ] **Step 6: Commit**

```bash
git add cmd/msgvault/cmd/addaccount.go cmd/msgvault/cmd/addaccount_test.go
git commit -m "refactor(oauth): route addaccount BYO branch through resolveOAuthManager"
```

---

## Task 8: Refactor `deletions.go` BYO branches

**Files:**
- Modify: `cmd/msgvault/cmd/deletions.go` (HasAnyConfig gate ~line 428, BYO branch ~lines 434-456, `getOAuthMgr` lambda ~lines 460-474, `promptScopeEscalation` body ~lines 680+)

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestDeletion ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Refactor the `!isServiceAccount` block**

In `cmd/msgvault/cmd/deletions.go`, the relevant section today reads (around lines 426-457):

```go
var clientSecretsPath string
if src.SourceType == "gmail" {
	if !cfg.OAuth.HasAnyConfig() {
		return errOAuthNotConfigured()
	}
	appName := sourceOAuthApp(src)
	isServiceAccount := cfg.OAuth.ServiceAccountKeyFor(appName) != ""

	if !isServiceAccount {
		clientSecretsPath, err = cfg.OAuth.ClientSecretsFor(appName)
		if err != nil {
			return err
		}

		needsBatchDelete := !deleteTrash
		if needsBatchDelete {
			requiredScopes := oauth.ScopesDeletion
			oauthMgr, err := oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, requiredScopes)
			if err != nil {
				return wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
			}
			if !oauthMgr.HasScope(account, "https://mail.google.com/") && oauthMgr.HasScopeMetadata(account) {
				if err := promptScopeEscalation(ctx, oauthMgr, account, needsBatchDelete, clientSecretsPath); err != nil {
					if errors.Is(err, errUserCanceled) {
						return nil
					}
					return err
				}
			}
		}
	}
}
```

Replace with (deleting both the `HasAnyConfig` gate AND the `clientSecretsPath` declaration, and updating the `promptScopeEscalation` call to pass `appName`):

```go
if src.SourceType == "gmail" {
	appName := sourceOAuthApp(src)
	isServiceAccount := cfg.OAuth.ServiceAccountKeyFor(appName) != ""

	if !isServiceAccount {
		needsBatchDelete := !deleteTrash
		if needsBatchDelete {
			oauthMgr, err := resolveOAuthManager(cfg, appName, oauth.ScopesDeletion, logger)
			if err != nil {
				return err
			}
			if !oauthMgr.HasScope(account, "https://mail.google.com/") && oauthMgr.HasScopeMetadata(account) {
				if err := promptScopeEscalation(ctx, oauthMgr, account, needsBatchDelete, appName); err != nil {
					if errors.Is(err, errUserCanceled) {
						return nil
					}
					return err
				}
			}
		}
	}
}
```

The outer `var clientSecretsPath string` declaration at line 426 goes away. The `if !cfg.OAuth.HasAnyConfig() { return errOAuthNotConfigured() }` gate goes away. The `appName` variable still lives inside the `if src.SourceType == "gmail"` block.

- [ ] **Step 3: Refactor the `getOAuthMgr` lambda**

The lambda at lines 460-474 captures the outer `clientSecretsPath` (which no longer exists after Step 2). Replace the lambda body:

```go
getOAuthMgr := func(appName string) (*oauth.Manager, error) {
	secretsPath := clientSecretsPath
	if secretsPath == "" {
		var err error
		secretsPath, err = cfg.OAuth.ClientSecretsFor(appName)
		if err != nil {
			return nil, err
		}
	}
	scopes := oauth.Scopes
	if !deleteTrash {
		scopes = oauth.ScopesDeletion
	}
	return oauth.NewManagerWithScopes(secretsPath, cfg.TokensDir(), logger, scopes)
}
```

with:

```go
getOAuthMgr := func(appName string) (*oauth.Manager, error) {
	scopes := oauth.Scopes
	if !deleteTrash {
		scopes = oauth.ScopesDeletion
	}
	return resolveOAuthManager(cfg, appName, scopes, logger)
}
```

- [ ] **Step 4: Change `promptScopeEscalation` signature from `clientSecretsPath` to `appName`**

In `cmd/msgvault/cmd/deletions.go`, find `func promptScopeEscalation` (around line 680). Today its signature ends with `, clientSecretsPath string`:

```go
func promptScopeEscalation(ctx context.Context, oauthMgr *oauth.Manager, account string, batchDelete bool, clientSecretsPath string) error {
```

Change to:

```go
func promptScopeEscalation(ctx context.Context, oauthMgr *oauth.Manager, account string, batchDelete bool, appName string) error {
```

Inside the function body, find the manager re-creation after the user opts into elevated scopes (it calls `oauth.NewManagerWithScopes(clientSecretsPath, cfg.TokensDir(), logger, requiredScopes)`). Replace it with:

```go
newMgr, err := resolveOAuthManager(cfg, appName, requiredScopes, logger)
if err != nil {
	return err
}
```

(Adapt the variable name `newMgr` to whatever the function uses internally.)

Both callers of `promptScopeEscalation` already have `appName` available (the first one as a local var in the `if src.SourceType == "gmail"` block, the second one as `sourceOAuthApp(src)` computed inline). The first caller updated in Step 2 already passes `appName`; the second caller (around line 537) currently passes `clientSecretsPath` and needs updating:

```go
// Before
if err := promptScopeEscalation(ctx, oauthMgr, account, !useTrash, clientSecretsPath); err != nil {
// After
if err := promptScopeEscalation(ctx, oauthMgr, account, !useTrash, sourceOAuthApp(src)); err != nil {
```

- [ ] **Step 5: Build to catch any remaining references**

Run: `go build -tags "fts5 sqlite_vec" ./...`

Expected: success. Any leftover references to `clientSecretsPath` or `wrapOAuthError` in `deletions.go` mean a step was skipped.

- [ ] **Step 6: Run all deletions tests**

Run: `go test -tags "fts5 sqlite_vec" -run TestDeletion ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/msgvault/cmd/deletions.go
git commit -m "refactor(oauth): route deletions through resolveOAuthManager"
```

---

## Task 9: Refactor `verify.go` — remove HasAnyConfig gate and route OAuth fallback through resolver

**Files:**
- Modify: `cmd/msgvault/cmd/verify.go` (HasAnyConfig gate ~line 95, OAuth fallback ~lines 126-132)

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestVerify ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Remove the HasAnyConfig gate**

In `cmd/msgvault/cmd/verify.go`, find and delete the block (around lines 94-96):

```go
if !cfg.OAuth.HasAnyConfig() {
	return errOAuthNotConfigured()
}
```

This gate prevented `verify` from running without OAuth config; with the embedded fallthrough every Gmail source can be verified.

- [ ] **Step 3: Replace the OAuth fallback arm**

Find the block (around lines 126-132):

```go
} else {
	clientSecretsPath, secretsErr := cfg.OAuth.ClientSecretsFor(appName)
	if secretsErr != nil {
		return secretsErr
	}
	oauthMgr, mgrErr := oauth.NewManager(clientSecretsPath, cfg.TokensDir(), logger)
	if mgrErr != nil {
		return wrapOAuthError(fmt.Errorf("create oauth manager: %w", mgrErr))
	}
	// ... rest of the OAuth path
}
```

Replace the resolution lines with:

```go
} else {
	oauthMgr, mgrErr := resolveOAuthManager(cfg, appName, oauth.Scopes, logger)
	if mgrErr != nil {
		return mgrErr
	}
	// ... rest of the OAuth path
}
```

- [ ] **Step 4: Build to catch unused-variable errors**

Run: `go build -tags "fts5 sqlite_vec" ./...`

Expected: success.

- [ ] **Step 5: Run verify tests**

Run: `go test -tags "fts5 sqlite_vec" -run TestVerify ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/msgvault/cmd/verify.go
git commit -m "refactor(oauth): route verify through resolveOAuthManager"
```

---

## Task 10: Remove `HasAnyConfig` startup gate in `serve.go`

**Files:**
- Modify: `cmd/msgvault/cmd/serve.go` (around line 68)

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestServe ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Delete the gate**

In `cmd/msgvault/cmd/serve.go`, find and delete the block (around lines 68-70):

```go
if !cfg.OAuth.HasAnyConfig() {
	return errOAuthNotConfigured()
}
```

- [ ] **Step 3: Build and test**

Run: `go build -tags "fts5 sqlite_vec" ./...`
Run: `go test -tags "fts5 sqlite_vec" -run TestServe ./cmd/msgvault/cmd/`

Expected: both succeed.

- [ ] **Step 4: Commit**

```bash
git add cmd/msgvault/cmd/serve.go
git commit -m "refactor(oauth): remove HasAnyConfig gate from serve"
```

---

## Task 11: Remove per-source `HasAnyConfig` skip in `syncfull.go`

**Files:**
- Modify: `cmd/msgvault/cmd/syncfull.go` (around line 118)

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestSyncFull ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Delete the skip block**

In `cmd/msgvault/cmd/syncfull.go`, find the multi-source loop and delete the block (around lines 118-121):

```go
if !cfg.OAuth.HasAnyConfig() {
	fmt.Printf("Skipping %s (OAuth not configured)\n", src.Identifier)
	continue
}
```

The loop continues with the `appName := sourceOAuthApp(src)` line and downstream calls to `getOAuthMgr(appName)`.

- [ ] **Step 3: Build and test**

Run: `go build -tags "fts5 sqlite_vec" ./...`
Run: `go test -tags "fts5 sqlite_vec" -run TestSyncFull ./cmd/msgvault/cmd/`

Expected: both succeed.

- [ ] **Step 4: Commit**

```bash
git add cmd/msgvault/cmd/syncfull.go
git commit -m "refactor(oauth): drop per-source skip in syncfull"
```

---

## Task 12: Remove per-source `HasAnyConfig` skip in `sync.go`

**Files:**
- Modify: `cmd/msgvault/cmd/sync.go` (around line 128)

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestSync ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Delete the skip block**

In `cmd/msgvault/cmd/sync.go`, find the multi-source loop and delete the block (around lines 128-131):

```go
if !cfg.OAuth.HasAnyConfig() {
	fmt.Printf("Skipping %s (OAuth not configured)\n", src.Identifier)
	continue
}
```

- [ ] **Step 3: Build and test**

Run: `go build -tags "fts5 sqlite_vec" ./...`
Run: `go test -tags "fts5 sqlite_vec" -run TestSync ./cmd/msgvault/cmd/`

Expected: both succeed.

- [ ] **Step 4: Commit**

```bash
git add cmd/msgvault/cmd/sync.go
git commit -m "refactor(oauth): drop per-source skip in sync-incremental"
```

---

## Task 13: Remove dead OAuth-setup helpers from `root.go`

**Files:**
- Modify: `cmd/msgvault/cmd/root.go` (delete `errOAuthNotConfigured`, `tryFindClientSecrets`, `oauthSetupHint`, `wrapOAuthError`)
- Modify: `cmd/msgvault/cmd/root_test.go` (delete tests for the deleted symbols)

- [ ] **Step 1: Verify there are no remaining callers**

Run: `grep -rn 'errOAuthNotConfigured\|tryFindClientSecrets\|oauthSetupHint\|wrapOAuthError' cmd/ internal/`

Expected output: only matches inside `cmd/msgvault/cmd/root.go` (the definitions) and `cmd/msgvault/cmd/root_test.go` (their tests). If anything else shows up, refactor those sites before continuing.

- [ ] **Step 2: Delete the helpers and their tests**

In `cmd/msgvault/cmd/root.go`, delete the following functions (the line ranges from the previous tasks may have shifted; locate by symbol):

- `errOAuthNotConfigured` (around line 277)
- `tryFindClientSecrets` (around line 288)
- `oauthSetupHint` (around line 256)
- `wrapOAuthError` (around line 322)

In `cmd/msgvault/cmd/root_test.go`, delete the corresponding tests:

- `TestErrOAuthNotConfigured`
- Any tests that call `wrapOAuthError` (search for the name)

- [ ] **Step 3: Build and test**

Run: `go build -tags "fts5 sqlite_vec" ./...`
Run: `go test -tags "fts5 sqlite_vec" ./...`

Expected: both succeed.

- [ ] **Step 4: Commit**

```bash
git add cmd/msgvault/cmd/root.go cmd/msgvault/cmd/root_test.go
git commit -m "refactor(oauth): remove dead OAuth-setup error helpers"
```

---

## Task 14: Remove `OAuthConfig.HasAnyConfig`

**Files:**
- Modify: `internal/config/config.go` (delete the `HasAnyConfig` method)
- Modify: `internal/config/config_test.go` (delete `TestOAuthConfig_HasAnyConfig` and inline assertions)

- [ ] **Step 1: Verify there are no remaining callers**

Run: `grep -rn 'HasAnyConfig' --include='*.go'`

Expected: only matches inside `internal/config/config.go` (definition) and `internal/config/config_test.go` (tests, plus comments).

- [ ] **Step 2: Delete the method and its tests**

In `internal/config/config.go`, delete the `HasAnyConfig` method (around lines 178-188).

In `internal/config/config_test.go`:

- Delete `TestOAuthConfig_HasAnyConfig` (lines around 1240-1308).
- In `TestLoadWithNamedOAuthApps` (around line 1310), remove the block (around lines 1363-1367) that asserts on `HasAnyConfig`.
- In `TestLoadWithNamedOAuthApps_RelativePaths` (around line 1484), remove the block (around lines 1604-1608) that asserts on `HasAnyConfig`.

- [ ] **Step 3: Build and test**

Run: `go build -tags "fts5 sqlite_vec" ./...`
Run: `go test -tags "fts5 sqlite_vec" ./internal/config/`

Expected: both succeed.

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "refactor(oauth): remove OAuthConfig.HasAnyConfig"
```

---

## Task 15: Remove interactive OAuth prompt from `setup` and adjust bundle behavior

**Files:**
- Modify: `cmd/msgvault/cmd/setup.go`
- Modify: `cmd/msgvault/cmd/setup_test.go`

- [ ] **Step 1: Verify existing tests pass**

Run: `go test -tags "fts5 sqlite_vec" -run TestSetup ./cmd/msgvault/cmd/`

Expected: PASS.

- [ ] **Step 2: Remove `setupOAuthSecrets` and update `runSetup`**

In `cmd/msgvault/cmd/setup.go`:

a. Delete `func setupOAuthSecrets(reader *bufio.Reader) (string, error)` (lines 100-143).

b. Update `runSetup` to remove the OAuth step. Replace the body with:

```go
func runSetup(cmd *cobra.Command, args []string) error {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("Welcome to msgvault setup!")
	fmt.Println()

	if err := cfg.EnsureHomeDir(); err != nil {
		return fmt.Errorf("create home directory: %w", err)
	}

	// Configure remote NAS (optional). msgvault now ships with an
	// embedded verified OAuth client, so the old "Step 1: OAuth
	// credentials" prompt is gone. Operators who want their own OAuth
	// client can still set [oauth] client_secrets in config.toml
	// manually.
	remoteURL, remoteAPIKey, err := setupRemoteServer(reader)
	if err != nil {
		return err
	}

	if remoteURL != "" {
		cfg.Remote.URL = remoteURL
		cfg.Remote.APIKey = remoteAPIKey
		if strings.HasPrefix(remoteURL, "http://") {
			cfg.Remote.AllowInsecure = true
		}
	}

	if remoteURL != "" {
		if err := cfg.Save(); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Printf("\nConfiguration saved to %s\n", cfg.ConfigFilePath())
	}

	fmt.Println()
	fmt.Println("Setup complete! Next steps:")
	fmt.Println()
	fmt.Println("  1. Add a Gmail account:")
	fmt.Println("     msgvault add-account you@gmail.com")
	fmt.Println()
	fmt.Println("  2. Sync your emails:")
	fmt.Println("     msgvault sync-full you@gmail.com")
	fmt.Println()
	if remoteURL != "" {
		fmt.Println("  3. Export token to your NAS (after add-account):")
		fmt.Println("     msgvault export-token you@gmail.com")
		fmt.Println()
	}
	fmt.Println("For more help: msgvault --help")

	return nil
}
```

c. Update `setupRemoteServer` signature to drop the unused `oauthSecretsPath` parameter:

```go
func setupRemoteServer(reader *bufio.Reader) (string, string, error) {
	// ... same body but reference cfg.OAuth.ClientSecrets directly
}
```

Inside `setupRemoteServer`, replace the `effectiveSecrets` lines (around 203-205) with:

```go
effectiveSecrets := cfg.OAuth.ClientSecrets // empty when operator uses embedded
```

d. Update `createNASBundle` so the generated `config.toml` only includes the `[oauth]` section when `oauthSecretsPath != ""`. Replace the `nasConfig` string construction with:

```go
nasConfig := fmt.Sprintf(`[server]
bind_addr = "0.0.0.0"
api_port = 8080
api_key = %q

[sync]
rate_limit_qps = 5

# Accounts will be added automatically when you export tokens.
# You can also add them manually:
# [[accounts]]
# email = "you@gmail.com"
# schedule = "0 2 * * *"
# enabled = true
`, apiKey)

if oauthSecretsPath != "" {
	nasConfig += `
[oauth]
client_secrets = "/data/client_secret.json"
`
}
```

Update the printed instructions inside `setupRemoteServer` to only mention `client_secret.json` when one was bundled:

```go
fmt.Printf("\nNAS deployment files created in: %s\n", bundleDir)
fmt.Println("  - config.toml (ready for NAS)")
if effectiveSecrets != "" {
	fmt.Println("  - client_secret.json (copy of OAuth credentials)")
}
fmt.Println("  - docker-compose.yml (ready to deploy)")
```

(That last bit is already the existing structure; verify it still reads correctly after the surrounding edits.)

- [ ] **Step 3: Update `setup_test.go`**

In `cmd/msgvault/cmd/setup_test.go`:

- Delete any tests that directly exercise `setupOAuthSecrets`.
- Update the bundle tests so the "no secrets path given" case verifies the generated `config.toml` does NOT contain a `[oauth]` section.
- Update the "secrets path given" case to verify the bundle contains both `client_secret.json` AND a `config.toml` with `[oauth] client_secrets = "/data/client_secret.json"`.

Concretely, for the "no secrets" case, add an assertion:

```go
configBytes, err := os.ReadFile(filepath.Join(bundleDir, "config.toml"))
if err != nil {
	t.Fatalf("read bundle config: %v", err)
}
if strings.Contains(string(configBytes), "[oauth]") {
	t.Error("config.toml should not contain [oauth] section when no secrets provided")
}
```

- [ ] **Step 4: Build and test**

Run: `go build -tags "fts5 sqlite_vec" ./...`
Run: `go test -tags "fts5 sqlite_vec" -run TestSetup ./cmd/msgvault/cmd/`

Expected: both succeed.

- [ ] **Step 5: Commit**

```bash
git add cmd/msgvault/cmd/setup.go cmd/msgvault/cmd/setup_test.go
git commit -m "refactor(setup): remove interactive OAuth step; bundle [oauth] section is now opt-in"
```

---

## Task 16: Wire `-ldflags` injection in Makefile

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Extend `LDFLAGS`**

In `Makefile`, find the `LDFLAGS` definition (lines 9-11):

```make
LDFLAGS := -X github.com/wesm/msgvault/cmd/msgvault/cmd.Version=$(VERSION) \
           -X github.com/wesm/msgvault/cmd/msgvault/cmd.Commit=$(COMMIT) \
           -X github.com/wesm/msgvault/cmd/msgvault/cmd.BuildDate=$(BUILD_DATE)
```

Replace with:

```make
LDFLAGS := -X github.com/wesm/msgvault/cmd/msgvault/cmd.Version=$(VERSION) \
           -X github.com/wesm/msgvault/cmd/msgvault/cmd.Commit=$(COMMIT) \
           -X github.com/wesm/msgvault/cmd/msgvault/cmd.BuildDate=$(BUILD_DATE) \
           -X github.com/wesm/msgvault/internal/oauth.oauthClientID=$(MSGVAULT_OAUTH_CLIENT_ID) \
           -X github.com/wesm/msgvault/internal/oauth.oauthClientSecret=$(MSGVAULT_OAUTH_CLIENT_SECRET)
```

When `MSGVAULT_OAUTH_CLIENT_ID` and `MSGVAULT_OAUTH_CLIENT_SECRET` are unset (contributor builds via `make build`), Make expands them to empty strings and the `-X` flags become effectively no-ops; the package vars keep their compiled-in dev defaults.

- [ ] **Step 2: Verify contributor build still works**

Run: `make build`

Expected: success. The resulting binary uses the source-default (dev) OAuth credentials.

- [ ] **Step 3: Verify release-style build works**

Run: `MSGVAULT_OAUTH_CLIENT_ID=test-prod-id MSGVAULT_OAUTH_CLIENT_SECRET=test-prod-secret make build`

Expected: success.

- [ ] **Step 4: Commit**

```bash
git add Makefile
git commit -m "build: inject embedded OAuth credentials via ldflags"
```

---

## Task 17: Wire production credential injection in release workflow

**Files:**
- Modify: `.github/workflows/release.yml`

- [ ] **Step 1: Find the build step(s)**

Open `.github/workflows/release.yml`. The Linux build step runs `go build` directly (not `make build-release`). Locate the step (it's the one with the `go build` command) and also any other platform build steps.

Each `go build` invocation that produces a release binary needs `MSGVAULT_OAUTH_CLIENT_ID` and `MSGVAULT_OAUTH_CLIENT_SECRET` set in its environment. The simplest pattern is to set them as job-level env vars so every step inherits them.

- [ ] **Step 2: Add the env injection at the job level**

For each release-producing job (Linux amd64, Linux arm64, macOS, Windows), add an `env:` block at the job level (or at each build step) that pulls from GitHub Secrets:

```yaml
env:
  MSGVAULT_OAUTH_CLIENT_ID: ${{ secrets.MSGVAULT_OAUTH_CLIENT_ID }}
  MSGVAULT_OAUTH_CLIENT_SECRET: ${{ secrets.MSGVAULT_OAUTH_CLIENT_SECRET }}
```

If the build step currently runs `go build` with an explicit `-ldflags` argument (not `make build-release`), update the `-ldflags` string to include the two new `-X` entries:

```yaml
go build \
  -ldflags "-s -w \
    -X github.com/wesm/msgvault/cmd/msgvault/cmd.Version=$VERSION \
    -X github.com/wesm/msgvault/cmd/msgvault/cmd.Commit=$COMMIT \
    -X github.com/wesm/msgvault/cmd/msgvault/cmd.BuildDate=$BUILD_DATE \
    -X github.com/wesm/msgvault/internal/oauth.oauthClientID=$MSGVAULT_OAUTH_CLIENT_ID \
    -X github.com/wesm/msgvault/internal/oauth.oauthClientSecret=$MSGVAULT_OAUTH_CLIENT_SECRET" \
  ...
```

Preserve the existing build flags. The simplest pattern is to switch the workflow to call `make build-release` (which already picks up the env vars after Task 16). Use whichever is closer to the existing workflow style.

- [ ] **Step 3: Confirm CI doesn't try to use the secrets**

Open `.github/workflows/ci.yml`. CI should NOT set `MSGVAULT_OAUTH_CLIENT_ID` or `MSGVAULT_OAUTH_CLIENT_SECRET`. CI builds use the source defaults (dev client), which is fine for unit tests since they stub the OAuth manager.

- [ ] **Step 4: Local sanity check (workflow not actually run here)**

You cannot run the release workflow without a tag push. As a smoke test, run the equivalent command locally:

Run: `MSGVAULT_OAUTH_CLIENT_ID=fake-prod-id MSGVAULT_OAUTH_CLIENT_SECRET=fake-prod-secret make build-release`

Expected: success.

- [ ] **Step 5: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "ci: inject embedded OAuth credentials into release builds"
```

---

## Task 18: Update README and quickstart docs

**Files:**
- Modify: `README.md`
- Modify: `cmd/msgvault/cmd/quickstart.md`

- [ ] **Step 1: Update the Quick Start section in `README.md`**

In `README.md`, find the section that today instructs users to "Follow the OAuth Setup Guide" before running `add-account`. Drop that prerequisite and replace with the embedded-default flow.

Replace the existing Quick Start lines (the ones beginning with "Follow the OAuth Setup Guide…") with:

```markdown
### Quick Start

```sh
# Initialize the database
msgvault init-db

# Add a Gmail account — opens your browser for consent
msgvault add-account you@gmail.com

# Sync mail
msgvault sync-full you@gmail.com

# Browse the archive
msgvault tui
```

No Google Cloud Console setup required: msgvault ships with a verified OAuth client.
```

Add a new subsection later in the README (after the basic usage block) titled "Advanced: bring your own OAuth client":

```markdown
### Advanced: bring your own OAuth client

The default flow uses msgvault's centralized verified OAuth client. You only need your own Cloud project if:

- Your Workspace organization prohibits authorizing third-party OAuth apps
- You prefer your own Cloud project's third-party-access listing to show
- You need your own Gmail API quota for very large mailboxes
- You want a fallback before msgvault's centralized client finishes Google verification

Follow the [OAuth setup guide](https://msgvault.io/guides/oauth-setup/) to create a Desktop OAuth client, then add it to `~/.msgvault/config.toml`:

```toml
[oauth]
client_secrets = "/path/to/client_secret.json"
```

Use `--oauth-app NAME` for per-account named-app routing — see the OAuth setup guide for details.
```

- [ ] **Step 2: Update `quickstart.md`**

In `cmd/msgvault/cmd/quickstart.md`, apply the same shape: drop the "Follow OAuth Setup Guide" prerequisite, mention the centralized client as default, and note BYO as advanced.

- [ ] **Step 3: Confirm no broken doc links**

Run: `grep -n 'oauth-setup\|client_secret.json' README.md cmd/msgvault/cmd/quickstart.md`

Expected: only matches inside the "Advanced" subsection, not the Quick Start.

- [ ] **Step 4: Commit**

```bash
git add README.md cmd/msgvault/cmd/quickstart.md
git commit -m "docs: drop OAuth setup prerequisite from Quick Start"
```

---

## Task 19: Final integration check

**Files:** none (verification only)

- [ ] **Step 1: Run the full test suite**

Run: `make test`

Expected: PASS.

- [ ] **Step 2: Run linters**

Run: `make lint-ci`

Expected: clean.

- [ ] **Step 3: Run go vet**

Run: `go vet -tags "fts5 sqlite_vec" ./...`

Expected: clean.

- [ ] **Step 4: Confirm the dead symbols are truly gone**

Run: `grep -rn 'errOAuthNotConfigured\|tryFindClientSecrets\|oauthSetupHint\|wrapOAuthError\|HasAnyConfig\|setupOAuthSecrets' --include='*.go'`

Expected: no matches in any production or test file.

- [ ] **Step 5: Confirm the new symbols are in place**

Run: `grep -rn 'ScopesEmbedded\|EmbeddedConfig\|NewEmbeddedManager\|HasEmbeddedCredentials\|resolveOAuthManager\|errAccessDenied\|embeddedFallbackMessage' --include='*.go'`

Expected: matches in `internal/oauth/embedded.go`, `internal/oauth/oauth.go`, `cmd/msgvault/cmd/oauth_resolve.go`, and their respective test files.

- [ ] **Step 6: Manual walkthrough (smoke test)**

This step does not pass/fail automatically; it's a sanity check against a real Gmail account.

1. Build: `make build`.
2. Run `./msgvault init-db` in an isolated `MSGVAULT_HOME=/tmp/mvtest`.
3. Run `./msgvault --home /tmp/mvtest add-account you@gmail.com`.
4. The browser should open to Google's OAuth consent screen. If your Gmail account is on the dev project's test-user list, consent should succeed. If not, you should see the verification-window fallback message in the terminal.
5. Confirm a token file appears under `/tmp/mvtest/tokens/`.
6. Run `./msgvault --home /tmp/mvtest sync-full you@gmail.com --limit 5` and confirm five messages sync.

Document any deviations in the PR description.

- [ ] **Step 7: Mark plan complete in the worktree**

No commit needed for this step. The plan is fully executed once Steps 1-6 pass.

---

## Self-review checklist

After implementing every task, run through this checklist before declaring done:

1. **Embedded client works in source builds?** `make build` then `./msgvault add-account` should reach the consent screen (assuming a non-empty dev `oauthClientID`).
2. **BYO still works?** Set `[oauth] client_secrets = "..."` in `config.toml` and confirm `add-account` uses it.
3. **Named BYO still works?** Add `[oauth.apps.acme]` and confirm `add-account --oauth-app acme` uses it.
4. **Service account still works?** Configure `[oauth] service_account_key`, confirm `add-account` short-circuits to the SA path.
5. **`--oauth-app nonexistent` errors?** Confirm the error mentions the app name.
6. **Token-refresh access_denied prints the fallback?** Manual test only — temporarily revoke consent in your Google account dashboard and re-run a sync; the fallback message should appear.
7. **`make test` clean?** No failing tests, no skipped tests that previously passed.
8. **`make lint-ci` clean?** No new lints.
9. **Spec coverage?** Every code change in the spec maps to a task above. The out-of-band verification prep (privacy policy, demo video, CASA assessment) is handled separately from this plan.
10. **No references to deleted symbols?** Grep across the repo.
