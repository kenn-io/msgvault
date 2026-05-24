package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.kenn.io/msgvault/internal/config"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
)

func TestServeConfigParsing(t *testing.T) {
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
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	// Verify server config
	if cfg.Server.APIPort != 9090 {
		t.Errorf("APIPort = %d, want 9090", cfg.Server.APIPort)
	}
	if cfg.Server.APIKey != "test-key" {
		t.Errorf("APIKey = %q, want test-key", cfg.Server.APIKey)
	}

	// Verify scheduled accounts
	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) != 2 {
		t.Errorf("len(ScheduledAccounts()) = %d, want 2", len(scheduled))
	}

	// Verify specific accounts
	acc := cfg.GetAccountSchedule("user1@gmail.com")
	if acc == nil {
		t.Fatal("GetAccountSchedule(user1) = nil")
	}
	if acc.Schedule != "0 2 * * *" {
		t.Errorf("user1 schedule = %q, want '0 2 * * *'", acc.Schedule)
	}

	// Disabled account should still be retrievable but not in scheduled list
	disabled := cfg.GetAccountSchedule("disabled@gmail.com")
	if disabled == nil {
		t.Error("GetAccountSchedule(disabled) = nil, want non-nil")
	}
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
	if count != 2 {
		t.Errorf("scheduled = %d, want 2", count)
	}

	// Should have 1 error for invalid cron
	if len(errs) != 1 {
		t.Errorf("len(errs) = %d, want 1", len(errs))
	}

	// Verify status
	statuses := sched.Status()
	if len(statuses) != 2 {
		t.Errorf("len(Status()) = %d, want 2", len(statuses))
	}
}

func TestServeCmdNoAccounts(t *testing.T) {
	// Create temp config without accounts
	tmpDir := t.TempDir()
	configContent := `
[oauth]
client_secrets = "/path/to/secrets.json"
`
	configPath := filepath.Join(tmpDir, "config.toml")
	if err := os.WriteFile(configPath, []byte(configContent), 0644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(configPath, "")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	scheduled := cfg.ScheduledAccounts()
	if len(scheduled) != 0 {
		t.Errorf("expected no scheduled accounts, got %d", len(scheduled))
	}
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
	if err != nil {
		t.Fatalf("setupVectorFeatures error = %v, want nil", err)
	}
	if vf != nil {
		t.Errorf("setupVectorFeatures = %v, want nil when disabled", vf)
	}
}

// TestFindScheduledSyncSource verifies that the scheduler's
// source-resolution helper picks gmail over imap and ignores rows of
// non-syncable source types (mbox, apple-mail, etc.). Regression for
// the daemon-mode IMAP dispatch (#329).
func TestFindScheduledSyncSource(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(filepath.Join(tmpDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// No rows: returns nil, allowing the Gmail token-first fallback.
	got, err := findScheduledSyncSource(s, "missing@example.com")
	if err != nil {
		t.Fatalf("findScheduledSyncSource(missing) error: %v", err)
	}
	if got != nil {
		t.Errorf("findScheduledSyncSource(missing) = %v, want nil", got)
	}

	// IMAP source created by add-imap: identifier is the imaps:// URL,
	// display_name is the user-facing email. Both lookups must resolve
	// to the same row.
	const imapID = "imaps://user@example.com@imap.example.com:993"
	const imapEmail = "user@example.com"
	imapSrc, err := s.GetOrCreateSource("imap", imapID)
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	if err := s.UpdateSourceDisplayName(imapSrc.ID, imapEmail); err != nil {
		t.Fatalf("set imap display_name: %v", err)
	}

	got, err = findScheduledSyncSource(s, imapID)
	if err != nil {
		t.Fatalf("findScheduledSyncSource(imap by identifier) error: %v", err)
	}
	if got == nil || got.SourceType != "imap" {
		t.Fatalf("findScheduledSyncSource(imap by identifier) = %v, want imap source", got)
	}

	// Lookup by display_name (the typical config.toml `email = "..."`
	// shape) must also resolve the IMAP source — otherwise the daemon
	// falls back to Gmail and produces a misleading token error.
	got, err = findScheduledSyncSource(s, imapEmail)
	if err != nil {
		t.Fatalf("findScheduledSyncSource(imap by display_name) error: %v", err)
	}
	if got == nil || got.SourceType != "imap" {
		t.Fatalf("findScheduledSyncSource(imap by display_name) = %v, want imap source", got)
	}

	// Identifier shared by an unsyncable mbox row + a gmail row:
	// gmail wins, the unsyncable row is ignored.
	const sharedID = "shared@example.com"
	if _, err := s.GetOrCreateSource("mbox", sharedID); err != nil {
		t.Fatalf("create mbox source: %v", err)
	}
	if _, err := s.GetOrCreateSource("gmail", sharedID); err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	got, err = findScheduledSyncSource(s, sharedID)
	if err != nil {
		t.Fatalf("findScheduledSyncSource(shared) error: %v", err)
	}
	if got == nil || got.SourceType != "gmail" {
		t.Fatalf("findScheduledSyncSource(shared) = %v, want gmail source", got)
	}

	// Identifier with only an unsyncable row: returns nil so the
	// dispatcher's Gmail fallback fires and produces a Gmail-shaped
	// error (rather than misclassifying as imap).
	const mboxID = "mbox-only@example.com"
	if _, err := s.GetOrCreateSource("mbox", mboxID); err != nil {
		t.Fatalf("create mbox source: %v", err)
	}
	got, err = findScheduledSyncSource(s, mboxID)
	if err != nil {
		t.Fatalf("findScheduledSyncSource(mbox-only) error: %v", err)
	}
	if got != nil {
		t.Errorf("findScheduledSyncSource(mbox-only) = %v, want nil", got)
	}
}

// TestRunScheduledIMAPSync_NoCredentials verifies that the IMAP path
// in runScheduledSync is reachable — i.e. an IMAP source row makes the
// dispatcher build an IMAP client and surface a credentials error,
// rather than the misleading "oauth2: token expired and refresh token
// is not set" message reported in #329.
func TestRunScheduledIMAPSync_NoCredentials(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	const imapID = "imaps://user@example.com@imap.example.com:993"
	if _, err := s.GetOrCreateSource("imap", imapID); err != nil {
		t.Fatalf("create imap source: %v", err)
	}

	// getOAuthMgr is only invoked on the Gmail path; fail loudly so
	// any wrong-path dispatch is obvious.
	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		t.Fatalf("Gmail OAuth manager unexpectedly requested for IMAP source (app=%q)", app)
		return nil, nil
	}

	err = runScheduledSync(context.Background(), imapID, s, getOAuthMgr, nil)
	if err == nil {
		t.Fatal("runScheduledSync(imap, no creds) = nil error, want credentials error")
	}
	msg := err.Error()
	if strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired") {
		t.Errorf("IMAP path produced Gmail-flavoured error %q — dispatch is still Gmail-only", msg)
	}
	if !strings.Contains(msg, "no credentials") && !strings.Contains(msg, "IMAP") {
		t.Errorf("error %q does not mention IMAP credentials", msg)
	}
}

// TestRunScheduledIMAPSync_DispatchByDisplayName verifies the daemon
// resolves IMAP sources when config.toml lists the account as a plain
// email — i.e. the lookup key matches the source's display_name rather
// than its imaps:// identifier. Regression: a previous version only
// matched against identifier, so config-driven scheduled syncs fell
// through to the Gmail OAuth path (#329).
func TestRunScheduledIMAPSync_DispatchByDisplayName(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	const (
		imapID    = "imaps://user@example.com@imap.example.com:993"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	if err := s.UpdateSourceDisplayName(src.ID, imapEmail); err != nil {
		t.Fatalf("set display_name: %v", err)
	}

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		t.Fatalf("Gmail OAuth manager unexpectedly requested for IMAP source (app=%q)", app)
		return nil, nil
	}

	// Pass the email (as config.toml `email = "..."` would supply it),
	// not the imaps:// identifier. Dispatch must still land on the
	// IMAP path; absence of credentials produces an IMAP-shaped error.
	err = runScheduledSync(context.Background(), imapEmail, s, getOAuthMgr, nil)
	if err == nil {
		t.Fatal("runScheduledSync(email, no creds) = nil error, want IMAP credentials error")
	}
	msg := err.Error()
	if strings.Contains(msg, "refresh token") || strings.Contains(msg, "token may be expired") {
		t.Errorf("dispatch fell through to Gmail path: %q", msg)
	}
	if !strings.Contains(msg, "IMAP") {
		t.Errorf("error %q does not mention IMAP — dispatch likely missed the source", msg)
	}
}

// TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName verifies the
// IMAP dispatch path writes the source's display_name (the email) as
// the default account identity — never the raw imaps:// identifier
// URL. Regression: a previous version passed src.Identifier, which
// would inject e.g. "imaps://user@host:993" into account_identities
// when the user had cleared their identities.
func TestRunScheduledIMAPSync_DefaultIdentityIsDisplayName(t *testing.T) {
	savedCfg := cfg
	defer func() { cfg = savedCfg }()
	cfg = &config.Config{}
	cfg.Data.DataDir = t.TempDir()

	s, err := store.Open(filepath.Join(cfg.Data.DataDir, "msgvault.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Use a closed port on loopback so buildAPIClient succeeds (the
	// client doesn't dial in its constructor) and confirmDefaultIdentity
	// fires before syncer.Full hits ECONNREFUSED.
	const (
		imapID    = "imaps://user@example.com@127.0.0.1:1"
		imapEmail = "user@example.com"
	)
	src, err := s.GetOrCreateSource("imap", imapID)
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	if err := s.UpdateSourceDisplayName(src.ID, imapEmail); err != nil {
		t.Fatalf("set display_name: %v", err)
	}
	if err := s.UpdateSourceSyncConfig(src.ID,
		`{"host":"127.0.0.1","port":1,"username":"user@example.com","tls":true}`,
	); err != nil {
		t.Fatalf("set sync_config: %v", err)
	}
	if err := imaplib.SaveCredentials(cfg.TokensDir(), imapID, "unused"); err != nil {
		t.Fatalf("save credentials: %v", err)
	}

	getOAuthMgr := func(app string) (*oauth.Manager, error) {
		t.Fatalf("Gmail OAuth manager unexpectedly requested (app=%q)", app)
		return nil, nil
	}

	// Expected to fail at the IMAP connection; what matters is that
	// confirmDefaultIdentity ran first with the display_name.
	_ = runScheduledSync(context.Background(), imapID, s, getOAuthMgr, nil)

	identities, err := s.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	if len(identities) == 0 {
		t.Fatal("no identities written — confirmDefaultIdentity did not fire on the IMAP path")
	}
	for _, id := range identities {
		if strings.HasPrefix(id.Address, "imaps://") ||
			strings.HasPrefix(id.Address, "imap://") ||
			strings.HasPrefix(id.Address, "imap+starttls://") {
			t.Errorf("identity %q is an IMAP URL — daemon polluted account_identities", id.Address)
		}
	}
	var foundEmail bool
	for _, id := range identities {
		if id.Address == imapEmail {
			foundEmail = true
			break
		}
	}
	if !foundEmail {
		t.Errorf("identities = %+v, want one with Address=%q", identities, imapEmail)
	}
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
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCronExpr(%q) error = %v, wantErr = %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}
