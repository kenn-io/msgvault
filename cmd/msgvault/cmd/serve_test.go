package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func TestServeConfigParsing(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	// Create temp config with scheduled accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"

[server]
api_port = 9090
api_key = "test-key"

[[accounts]]
email = "user1@gmail.com"
schedule = "0 2 * * *"
enabled = true

[[accounts]]
email = "user2@gmail.com"
schedule = "0 3 * * *"
enabled = true

[[accounts]]
email = "disabled@gmail.com"
schedule = "0 4 * * *"
enabled = false
`
	configPath := filepath.Join(tmpDir, "config.toml")
	require.NoError(os.WriteFile(configPath, []byte(configContent), 0644), "write config")

	cfg, err := config.Load(configPath, "")
	require.NoError(err, "Load")

	// Verify server config
	assert.Equal(9090, cfg.Server.APIPort, "APIPort")
	assert.Equal("test-key", cfg.Server.APIKey, "APIKey")

	// Verify scheduled accounts
	scheduled := cfg.ScheduledAccounts()
	assert.Len(scheduled, 2, "len(ScheduledAccounts())")

	// Verify specific accounts
	acc := cfg.GetAccountSchedule("user1@gmail.com")
	require.NotNil(acc, "GetAccountSchedule(user1)")
	assert.Equal("0 2 * * *", acc.Schedule, "user1 schedule")

	// Disabled account should still be retrievable but not in scheduled list
	disabled := cfg.GetAccountSchedule("disabled@gmail.com")
	assert.NotNil(disabled, "GetAccountSchedule(disabled)")
}

func TestSchedulerWithConfig(t *testing.T) {
	cfg := &config.Config{
		Accounts: []config.AccountSchedule{
			{Email: "test1@gmail.com", Schedule: "0 2 * * *", Enabled: true},
			{Email: "test2@gmail.com", Schedule: "0 3 * * *", Enabled: true},
			{Email: "test3@gmail.com", Schedule: "invalid", Enabled: true},
		},
	}

	var syncCalls []string
	sched := scheduler.New(func(ctx context.Context, email string) error {
		syncCalls = append(syncCalls, email)
		return nil
	})

	count, errs := sched.AddAccountsFromConfig(cfg)

	// Should schedule 2 valid accounts
	assertpkg.Equal(t, 2, count, "scheduled count")

	// Should have 1 error for invalid cron
	assertpkg.Len(t, errs, 1, "len(errs)")

	// Verify status
	statuses := sched.Status()
	assertpkg.Len(t, statuses, 2, "len(Status())")
}

func TestServeCmdNoAccounts(t *testing.T) {
	// Create temp config without accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	requirepkg.NoError(t, os.WriteFile(configPath, []byte(configContent), 0644), "write config")

	cfg, err := config.Load(configPath, "")
	requirepkg.NoError(t, err, "Load")

	scheduled := cfg.ScheduledAccounts()
	assertpkg.Empty(t, scheduled, "expected no scheduled accounts")
}

// TestSetupVectorFeatures_Disabled verifies that when
// cfg.Vector.Enabled is false, setupVectorFeatures returns (nil, nil)
// regardless of build tag. Runs under both tagged and untagged builds.
func TestSetupVectorFeatures_Disabled(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Vector.Enabled = false

	vf, err := setupVectorFeatures(context.Background(), nil, "")
	requirepkg.NoError(t, err, "setupVectorFeatures")
	assertpkg.Nil(t, vf, "setupVectorFeatures should be nil when disabled")
}

// TestSetupVectorFeatures_RefusesPostgres verifies setupVectorFeatures
// returns a clear error when invoked against a postgres:// DSN. The
// underlying backend (sqlite-vec + ATTACH DATABASE) cannot work on PG;
// fail closed at the entry point rather than crashing downstream when
// sql.Open("sqlite3", "postgres://...") gets a non-sqlite DSN.
func TestSetupVectorFeatures_RefusesPostgres(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Vector.Enabled = true

	_, err := setupVectorFeatures(context.Background(), nil, "postgres://user@host/db")
	requirepkg.Error(t, err, "setupVectorFeatures with postgres DSN")
	assertpkg.ErrorContains(t, err, "SQLite-only")
}

// TestFindScheduledSyncSource verifies that the scheduler's
// source-resolution helper picks gmail over imap and ignores rows of
// non-syncable source types (mbox, apple-mail, etc.). Regression for
// the daemon-mode IMAP dispatch (#329).
func TestFindScheduledSyncSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	// No rows: returns nil, allowing the Gmail token-first fallback.
	got, err := findScheduledSyncSource(s, "missing@example.com")
	require.NoError(err, "findScheduledSyncSource(missing)")
	assert.Nil(got, "findScheduledSyncSource(missing) should be nil")

	// IMAP source created by add-imap: identifier is the imaps:// URL,
	// display_name is the user-facing email. Both lookups must resolve
	// to the same row.
	const imapID = "imaps://user@example.com@imap.example.com:993"
	const imapEmail = "user@example.com"
	imapSrc, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(imapSrc.ID, imapEmail), "set imap display_name")

	got, err = findScheduledSyncSource(s, imapID)
	require.NoError(err, "findScheduledSyncSource(imap by identifier)")
	require.NotNil(got, "findScheduledSyncSource(imap by identifier) should not be nil")
	require.Equal("imap", got.SourceType, "findScheduledSyncSource(imap by identifier) SourceType")

	// Lookup by display_name (the typical config.toml `email = "..."`
	// shape) must also resolve the IMAP source — otherwise the daemon
	// falls back to Gmail and produces a misleading token error.
	got, err = findScheduledSyncSource(s, imapEmail)
	require.NoError(err, "findScheduledSyncSource(imap by display_name)")
	require.NotNil(got, "findScheduledSyncSource(imap by display_name) should not be nil")
	require.Equal("imap", got.SourceType, "findScheduledSyncSource(imap by display_name) SourceType")

	// Identifier shared by an unsyncable mbox row + a gmail row:
	// gmail wins, the unsyncable row is ignored.
	const sharedID = "shared@example.com"
	_, err = s.GetOrCreateSource("mbox", sharedID)
	require.NoError(err, "create mbox source")
	_, err = s.GetOrCreateSource("gmail", sharedID)
	require.NoError(err, "create gmail source")
	got, err = findScheduledSyncSource(s, sharedID)
	require.NoError(err, "findScheduledSyncSource(shared)")
	require.NotNil(got, "findScheduledSyncSource(shared) should not be nil")
	require.Equal("gmail", got.SourceType, "findScheduledSyncSource(shared) SourceType")

	// Identifier with only an unsyncable row: returns nil so the
	// dispatcher's Gmail fallback fires and produces a Gmail-shaped
	// error (rather than misclassifying as imap).
	const mboxID = "mbox-only@example.com"
	_, err = s.GetOrCreateSource("mbox", mboxID)
	require.NoError(err, "create mbox source")
	got, err = findScheduledSyncSource(s, mboxID)
	require.NoError(err, "findScheduledSyncSource(mbox-only)")
	assert.Nil(got, "findScheduledSyncSource(mbox-only) should be nil")
}

// TestRunScheduledIMAPSync_NoCredentials verifies that the IMAP path
// in runScheduledSync is reachable — i.e. an IMAP source row makes the
// dispatcher build an IMAP client and surface a credentials error,
// rather than the misleading "oauth2: token expired and refresh token
// is not set" message reported in #329.
func TestRunScheduledIMAPSync_NoCredentials(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const imapID = "imaps://user@example.com@imap.example.com:993"
	_, err = s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")

	// getOAuthMgr is only invoked on the Gmail path; fail loudly so
	// any wrong-path dispatch is obvious.
	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested for IMAP source", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	err = runScheduledSync(context.Background(), imapID, s, getOAuthMgr, nil)
	require.Error(err, "runScheduledSync(imap, no creds) want credentials error")
	msg := err.Error()
	assert.False(strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired"),
		"IMAP path produced Gmail-flavoured error %q — dispatch is still Gmail-only", msg)
	assert.True(strings.Contains(msg, "no credentials") || strings.Contains(msg, "IMAP"),
		"error %q does not mention IMAP credentials", msg)
}

// TestRunScheduledIMAPSync_DispatchByDisplayName verifies the daemon
// resolves IMAP sources when config.toml lists the account as a plain
// email — i.e. the lookup key matches the source's display_name rather
// than its imaps:// identifier. Regression: a previous version only
// matched against identifier, so config-driven scheduled syncs fell
// through to the Gmail OAuth path (#329).
func TestRunScheduledIMAPSync_DispatchByDisplayName(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const (
		imapID    = "imaps://user@example.com@imap.example.com:993"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, imapEmail), "set display_name")

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested for IMAP source", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	// Pass the email (as config.toml `email = "..."` would supply it),
	// not the imaps:// identifier. Dispatch must still land on the
	// IMAP path; absence of credentials produces an IMAP-shaped error.
	err = runScheduledSync(context.Background(), imapEmail, s, getOAuthMgr, nil)
	require.Error(err, "runScheduledSync(email, no creds) want IMAP credentials error")
	msg := err.Error()
	assert.False(strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired"),
		"dispatch fell through to Gmail path: %q", msg)
	assert.Contains(msg, "IMAP", "error %q does not mention IMAP — dispatch likely missed the source", msg)
}

// TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName verifies the
// IMAP dispatch path writes the source's display_name (the email) as
// the default account identity — never the raw imaps:// identifier
// URL. Regression: a previous version passed src.Identifier, which
// would inject e.g. "imaps://user@host:993" into account_identities
// when the user had cleared their identities.
func TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	// Use a closed port on loopback so buildAPIClient succeeds (the
	// client doesn't dial in its constructor) and confirmDefaultIdentity
	// fires before syncer.Full hits ECONNREFUSED.
	const (
		imapID    = "imaps://user@example.com@127.0.0.1:1"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	require.NoError(err, "create imap source")
	require.NoError(s.UpdateSourceDisplayName(src.ID, imapEmail), "set display_name")
	require.NoError(s.UpdateSourceSyncConfig(src.ID,
		`{"host":"127.0.0.1","port":1,"username":"user@example.com","tls":true}`,
	), "set sync_config")
	require.NoError(imaplib.SaveCredentials(cfg.TokensDir(), imapID, "unused"), "save credentials")

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		assert.Fail("Gmail OAuth manager unexpectedly requested", "app=%q", app)
		// Unreachable: the assert.Fail above already failed the test; the
		// return only satisfies the signature.
		return nil, nil //nolint:nilnil // unreachable guard, see comment above
	}

	// Expected to fail at the IMAP connection; what matters is that
	// confirmDefaultIdentity ran first with the display_name.
	_ = runScheduledSync(context.Background(), imapID, s, getOAuthMgr, nil)

	identities, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.NotEmpty(identities, "no identities written — confirmDefaultIdentity did not fire on the IMAP path")
	for _, id := range identities {
		if strings.HasPrefix(id.Address, "imaps://") ||
			strings.HasPrefix(id.Address, "imap://") ||
			strings.HasPrefix(id.Address, "imap+starttls://") {
			assert.Fail("identity is an IMAP URL — daemon polluted account_identities",
				"address=%q", id.Address)
		}
	}
	var foundEmail bool
	for _, id := range identities {
		if id.Address == imapEmail {
			foundEmail = true
			break
		}
	}
	assert.True(foundEmail, "identities = %+v, want one with Address=%q", identities, imapEmail)
}

func TestCronExpressionValidation(t *testing.T) {
	tests := []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"daily at 2am", "0 2 * * *", false},
		{"every 15 min", "*/15 * * * *", false},
		{"weekly sunday", "0 0 * * 0", false},
		{"monthly first", "0 0 1 * *", false},
		{"twice daily", "0 8,18 * * *", false},
		{"invalid", "not a cron", true},
		{"empty", "", true},
		{"too many fields", "* * * * * *", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scheduler.ValidateCronExpr(tt.expr)
			if tt.wantErr {
				assertpkg.Error(t, err, "ValidateCronExpr(%q)", tt.expr)
			} else {
				assertpkg.NoError(t, err, "ValidateCronExpr(%q)", tt.expr)
			}
		})
	}
}
