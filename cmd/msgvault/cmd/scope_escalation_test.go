package cmd

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/oauth"
	"google.golang.org/api/drive/v3"
)

const scopeEscalationAccount = "user@example.com"

// gmailOnlyTokenJSON is a parseable token file with Gmail scopes but NOT
// Calendar — i.e. the exact shape of the live server's existing token. No real
// credentials are present.
const gmailOnlyTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://www.googleapis.com/auth/gmail.modify"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

const gmailDriveTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://www.googleapis.com/auth/gmail.modify",
    "https://www.googleapis.com/auth/drive.readonly"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

const driveOnlyTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/drive.readonly"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

const legacyTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z"
}`

const gmailCalendarTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://www.googleapis.com/auth/gmail.modify",
    "https://www.googleapis.com/auth/calendar.readonly"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

const gmailCalendarDriveTokenJSON = `{
  "access_token": "fake-access-token",
  "token_type": "Bearer",
  "refresh_token": "fake-refresh-token",
  "expiry": "2099-01-01T00:00:00Z",
  "scopes": [
    "https://www.googleapis.com/auth/gmail.readonly",
    "https://www.googleapis.com/auth/gmail.modify",
    "https://www.googleapis.com/auth/calendar.readonly",
    "https://www.googleapis.com/auth/drive.readonly"
  ],
  "client_id": "test.apps.googleusercontent.com"
}`

// seedTokenEnv points the package globals at a temp home, writes a fake
// client_secret.json plus a pre-existing token for account, and returns the
// token path and a restore func. It mirrors the OAuth scaffolding used by
// sync_test.go so the tests exercise the real production path.
func seedTokenEnv(t *testing.T, tokenJSON string) (tokenPath string, restore func()) {
	t.Helper()
	tmpDir := t.TempDir()

	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(t, os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write client secrets")

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(t, os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	tokenPath = filepath.Join(tokensDir, scopeEscalationAccount+".json")
	require.NoError(t, os.WriteFile(tokenPath, []byte(tokenJSON), 0600), "write token")

	savedCfg, savedLogger := cfg, logger
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	return tokenPath, func() {
		cfg, logger = savedCfg, savedLogger
	}
}

// TestPromptScopeEscalation_PreservesTokenOnFailedReauth is the regression for
// the data-availability bug where the escalation flow deleted the existing
// token BEFORE re-authorizing. On a headless host the browser re-auth can never
// complete, so deleting first left the account with no token — breaking its
// scheduled (e.g. Gmail) sync. The flow must leave the old token intact when
// re-auth does not succeed.
func TestPromptScopeEscalation_PreservesTokenOnFailedReauth(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tokenPath, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
	defer restore()

	before, err := os.ReadFile(tokenPath)
	require.NoError(err, "read seeded token")

	// Answer "y" to the upgrade prompt (promptScopeEscalation reads os.Stdin).
	stdinFile := filepath.Join(t.TempDir(), "stdin")
	require.NoError(os.WriteFile(stdinFile, []byte("y\n"), 0600), "write stdin")
	f, err := os.Open(stdinFile)
	require.NoError(err, "open stdin")
	defer func() { _ = f.Close() }()
	savedStdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = savedStdin }()

	// A cancelled context makes the browser flow fail immediately (no network,
	// no hang) — standing in for "re-auth cannot complete on a headless host".
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	getOutput := captureStdout(t)
	escErr := promptScopeEscalation(ctx, scopeEscalationAccount, oauth.ScopesGmailCalendar,
		"PERMISSION UPGRADE REQUIRED", []string{"upgrade needed"},
		"Cancelled.", cfg.OAuth.ClientSecrets)
	out := getOutput()

	require.Error(escErr, "a re-auth that cannot complete must surface as an error")
	assert.NotContains(out, "Deleting old token", "must not announce deleting the token up front")

	after, err := os.ReadFile(tokenPath)
	require.NoError(err, "the existing token file must survive a failed re-auth")
	assert.Equal(string(before), string(after), "token content must be unchanged")
}

func TestDeletionEscalationScopesForAccountPreservesCalendarGrant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, gmailCalendarTokenJSON)
	defer restore()

	scopes, err := deletionEscalationScopesForAccount(scopeEscalationAccount, true, cfg.OAuth.ClientSecrets)

	require.NoError(err)
	assert.ElementsMatch([]string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		"https://mail.google.com/",
		oauth.ScopeCalendarReadonly,
	}, scopes)
}

func TestCalendarEscalationScopesForAccountPreservesDriveGrant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, gmailDriveTokenJSON)
	defer restore()

	scopes, err := calendarEscalationScopesForAccount(scopeEscalationAccount, cfg.OAuth.ClientSecrets)

	require.NoError(err)
	assert.ElementsMatch([]string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		oauth.ScopeCalendarReadonly,
		drive.DriveReadonlyScope,
	}, scopes)
}

func TestCalendarEscalationScopesForAccountDoesNotAddGmailToDriveOnlyToken(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, driveOnlyTokenJSON)
	defer restore()

	scopes, err := calendarEscalationScopesForAccount(scopeEscalationAccount, cfg.OAuth.ClientSecrets)

	require.NoError(err)
	assert.ElementsMatch([]string{
		oauth.ScopeCalendarReadonly,
		drive.DriveReadonlyScope,
	}, scopes)
}

func TestCalendarEscalationScopesForAccountPreservesLegacyTokenAsGmail(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, legacyTokenJSON)
	defer restore()

	scopes, err := calendarEscalationScopesForAccount(scopeEscalationAccount, cfg.OAuth.ClientSecrets)

	require.NoError(err)
	assert.ElementsMatch([]string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		oauth.ScopeCalendarReadonly,
	}, scopes)
}

func TestCalendarOAuthScopesForAccountPreservesExistingGrants(t *testing.T) {
	assert := assert.New(t)

	assert.ElementsMatch(oauth.ScopesCalendar,
		calendarOAuthScopesForAccount(false, false, nil),
		"new Calendar accounts should not request Gmail")

	assert.ElementsMatch([]string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		oauth.ScopeCalendarReadonly,
		drive.DriveReadonlyScope,
	}, calendarOAuthScopesForAccount(true, true, []string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		drive.DriveReadonlyScope,
	}), "Gmail accounts should preserve Drive during Calendar reauth")

	assert.ElementsMatch([]string{
		oauth.ScopeCalendarReadonly,
		drive.DriveReadonlyScope,
	}, calendarOAuthScopesForAccount(true, true, []string{
		drive.DriveReadonlyScope,
	}), "Drive-only accounts should keep Drive without adding Gmail")

	assert.ElementsMatch(oauth.ScopesGmailCalendar,
		calendarOAuthScopesForAccount(true, false, nil),
		"legacy tokens should preserve Gmail")
}

func TestDeletionEscalationScopesForAccountPreservesDriveGrant(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, gmailCalendarDriveTokenJSON)
	defer restore()

	scopes, err := deletionEscalationScopesForAccount(scopeEscalationAccount, true, cfg.OAuth.ClientSecrets)

	require.NoError(err)
	assert.ElementsMatch([]string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		"https://mail.google.com/",
		oauth.ScopeCalendarReadonly,
		drive.DriveReadonlyScope,
	}, scopes)
}

func TestDeletionEscalationScopesForAccountPreservesGmailScopesWithoutCalendar(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	_, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
	defer restore()

	scopes, err := deletionEscalationScopesForAccount(scopeEscalationAccount, true, cfg.OAuth.ClientSecrets)

	require.NoError(err)
	assert.ElementsMatch([]string{
		"https://www.googleapis.com/auth/gmail.readonly",
		"https://www.googleapis.com/auth/gmail.modify",
		"https://mail.google.com/",
	}, scopes)
}

// TestAddCalendarHeadless_PrintsInstructionsAndPreservesToken is the regression
// for add-calendar --headless on a host whose account has a Gmail-only token
// (the live server's state). It must NOT launch a browser flow or delete the
// token; it prints copy-the-token instructions and returns cleanly.
func TestAddCalendarHeadless_PrintsInstructionsAndPreservesToken(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tokenPath, restore := seedTokenEnv(t, gmailOnlyTokenJSON)
	defer restore()

	before, err := os.ReadFile(tokenPath)
	require.NoError(err, "read seeded token")

	addCmd := newAddCalendarCmd()
	addCmd.SetContext(context.Background())
	addCmd.SetArgs([]string{"--headless", scopeEscalationAccount})
	defer func() { calAddHeadless = false }()

	getOutput := captureStdout(t)
	execErr := addCmd.Execute()
	out := getOutput()

	require.NoError(execErr, "headless add-calendar must not error or hang")
	assert.Contains(out, "Headless Server Calendar Setup", "must print copy-the-token instructions")
	assert.Contains(out, "copy", "instructions must tell the user to copy the existing token to the browser machine first")
	assert.Contains(out, "previously granted scopes", "instructions must preserve non-Calendar OAuth grants")
	assert.Contains(out, "scp", "instructions must include the token copy step")

	after, err := os.ReadFile(tokenPath)
	require.NoError(err, "the existing Gmail token must survive")
	assert.Equal(string(before), string(after), "token content must be unchanged")
}
