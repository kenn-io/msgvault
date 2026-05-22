package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

// fakeClientSecrets is a minimal Google OAuth client_secret.json that
// oauth.NewManager can parse. No real credentials are exposed.
const fakeClientSecrets = `{
  "installed": {
    "client_id": "test.apps.googleusercontent.com",
    "client_secret": "test-secret",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "redirect_uris": ["http://localhost"]
  }
}`

// TestSyncCmd_DuplicateIdentifierRoutesCorrectly verifies that when
// Gmail and IMAP sources share the same identifier, the single-arg
// sync path resolves both and routes each to the correct backend.
//
// Regression test: before the fix, GetSourceByIdentifier returned
// an arbitrary single row, so one source type would be lost.
// The Gmail source is seeded with a SyncCursor and valid OAuth
// scaffolding so the test exercises runIncrementalSync, not just
// the OAuth manager setup.
func TestSyncCmd_DuplicateIdentifierRoutesCorrectly(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Insert IMAP *before* Gmail so that an ambiguous single-row
	// lookup (the old GetSourceByIdentifier bug) would return the
	// IMAP row, not the Gmail one. This ensures the test only
	// passes when the resolved Gmail source is actually used.
	_, err = s.GetOrCreateSource("imap", "shared@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}

	gmailSrc, err := s.GetOrCreateSource(
		"gmail", "shared@example.com",
	)
	if err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	// Set a history cursor so runIncrementalSync proceeds past
	// the "no history ID" guard and into getTokenSourceWithReauth.
	if err := s.UpdateSourceSyncCursor(gmailSrc.ID, "99999"); err != nil {
		t.Fatalf("set sync cursor: %v", err)
	}
	_ = s.Close()

	// Write a minimal client_secret.json so the OAuth manager
	// can be created without error.
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	err = os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	)
	if err != nil {
		t.Fatalf("write client secrets: %v", err)
	}

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "sync [email]",
		Args: cobra.MaximumNArgs(1),
		RunE: syncIncrementalCmd.RunE,
	}

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"sync", "shared@example.com"})

	// Capture stdout: the sync command prints per-source errors
	// to stdout while the returned error is just the count.
	getOutput := captureStdout(t)
	execErr := root.Execute()
	output := getOutput()

	if execErr == nil {
		t.Fatal("expected error (no credentials/token)")
	}

	errMsg := execErr.Error()

	// Should NOT hit the legacy Gmail-only fallback, which sets
	// source to nil and produces "no source found".
	if strings.Contains(output, "no source found") {
		t.Error("should not hit legacy Gmail-only fallback path")
	}

	// Both sources should be resolved and attempted, producing
	// 2 failures (IMAP: missing config, Gmail: missing token).
	if !strings.Contains(errMsg, "2 account(s) failed") {
		t.Errorf(
			"expected both sources resolved; got: %s",
			errMsg,
		)
	}

	// The Gmail error should come from inside runIncrementalSync
	// (reaching getTokenSourceWithReauth), not from OAuth manager
	// creation. "add-account" appears only in the token-missing
	// error produced by getTokenSourceWithReauth.
	if !strings.Contains(output, "add-account") {
		t.Errorf(
			"Gmail error should originate from "+
				"runIncrementalSync; output:\n%s",
			output,
		)
	}
}

// TestSyncCmd_SingleSourceNoAmbiguity verifies that a single
// source for an identifier works without the legacy fallback.
func TestSyncCmd_SingleSourceNoAmbiguity(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = s.GetOrCreateSource("imap", "solo@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "sync [email]",
		Args: cobra.MaximumNArgs(1),
		RunE: syncIncrementalCmd.RunE,
	}

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"sync", "solo@example.com"})

	err = root.Execute()
	if err == nil {
		t.Fatal("expected error (no IMAP config)")
	}

	errMsg := err.Error()

	// Exactly 1 source should fail (IMAP with missing config).
	if !strings.Contains(errMsg, "1 account(s) failed") {
		t.Errorf(
			"expected 1 failed account; got: %s",
			errMsg,
		)
	}

	// Should NOT hit legacy fallback (source exists in DB).
	if strings.Contains(errMsg, "no source found") {
		t.Error("should not hit legacy fallback path")
	}
}

// TestSyncCmd_MboxIdentifierDoesNotFallback verifies that an
// identifier that exists only as a non-syncable source type (mbox)
// returns a clear error instead of falling back to the legacy
// Gmail path.
func TestSyncCmd_MboxIdentifierDoesNotFallback(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = s.GetOrCreateSource("mbox", "imported@example.com")
	if err != nil {
		t.Fatalf("create mbox source: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Test both sync and sync-full commands.
	for _, tc := range []struct {
		name string
		runE func(*cobra.Command, []string) error
	}{
		{"sync", syncIncrementalCmd.RunE},
		{"sync-full", syncFullCmd.RunE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testCmd := &cobra.Command{
				Use:  tc.name + " [email]",
				Args: cobra.MaximumNArgs(1),
				RunE: tc.runE,
			}

			root := newTestRootCmd()
			root.AddCommand(testCmd)
			root.SetArgs([]string{
				tc.name, "imported@example.com",
			})

			err := root.Execute()
			if err == nil {
				t.Fatal("expected error for non-syncable source")
			}

			errMsg := err.Error()

			if !strings.Contains(errMsg, "cannot be synced") {
				t.Errorf(
					"expected unsupported-source error; got: %s",
					errMsg,
				)
			}
		})
	}
}

// TestSyncFullCmd_OAuthSkipDoesNotBlockIMAP verifies that in a
// mixed Gmail+IMAP setup without OAuth configured, sync-full skips
// the Gmail source and still syncs the IMAP source.
func TestSyncFullCmd_OAuthSkipDoesNotBlockIMAP(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	if err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	_, err = s.GetOrCreateSource("imap", "i@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	defer func() {
		cfg = savedCfg
		logger = savedLogger
	}()

	// No OAuth configured — ClientSecrets is empty.
	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "sync-full [email]",
		Args: cobra.MaximumNArgs(1),
		RunE: syncFullCmd.RunE,
	}

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"sync-full"})

	// Capture stdout to check skip messages.
	getOutput := captureStdout(t)
	execErr := root.Execute()
	output := getOutput()

	// IMAP source should be attempted (and fail due to missing
	// config), but the command should NOT abort entirely because
	// of the Gmail OAuth failure.
	if execErr == nil {
		t.Fatal("expected error (IMAP has no config)")
	}

	// Gmail should be skipped, not cause an abort.
	if !strings.Contains(output, "Skipping g@example.com") {
		t.Errorf(
			"Gmail source should be skipped; output:\n%s",
			output,
		)
	}

	// IMAP source should have been attempted.
	if !strings.Contains(output, "i@example.com") {
		t.Errorf(
			"IMAP source should be attempted; output:\n%s",
			output,
		)
	}
}

// TestSyncCmd_BrokenOAuthDoesNotBlockIMAP verifies that a malformed
// client_secrets file does not prevent IMAP sources from syncing in
// the no-args discovery path. The OAuth error should be reported
// after IMAP work completes.
func TestSyncCmd_BrokenOAuthDoesNotBlockIMAP(t *testing.T) {
	for _, tc := range []struct {
		name string
		runE func(*cobra.Command, []string) error
	}{
		{"sync", syncIncrementalCmd.RunE},
		{"sync-full", syncFullCmd.RunE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dbPath := tmpDir + "/msgvault.db"

			s, err := store.Open(dbPath)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if err := s.InitSchema(); err != nil {
				t.Fatalf("init schema: %v", err)
			}

			gmailSrc, err := s.GetOrCreateSource(
				"gmail", "g@example.com",
			)
			if err != nil {
				t.Fatalf("create gmail source: %v", err)
			}
			// Give Gmail source a cursor so it passes
			// the sync command's discovery checks.
			if err := s.UpdateSourceSyncCursor(gmailSrc.ID, "1"); err != nil {
				t.Fatalf("set cursor: %v", err)
			}

			_, err = s.GetOrCreateSource(
				"imap", "i@example.com",
			)
			if err != nil {
				t.Fatalf("create imap source: %v", err)
			}
			_ = s.Close()

			// Write a malformed client_secret.json.
			secretsPath := filepath.Join(
				tmpDir, "client_secret.json",
			)
			err = os.WriteFile(
				secretsPath, []byte("not json"), 0600,
			)
			if err != nil {
				t.Fatalf("write secrets: %v", err)
			}

			savedCfg := cfg
			savedLogger := logger
			defer func() {
				cfg = savedCfg
				logger = savedLogger
			}()

			cfg = &config.Config{
				HomeDir: tmpDir,
				Data:    config.DataConfig{DataDir: tmpDir},
				OAuth: config.OAuthConfig{
					ClientSecrets: secretsPath,
				},
			}
			logger = slog.New(
				slog.NewTextHandler(os.Stderr, nil),
			)

			testCmd := &cobra.Command{
				Use:  tc.name + " [email]",
				Args: cobra.MaximumNArgs(1),
				RunE: tc.runE,
			}

			root := newTestRootCmd()
			root.AddCommand(testCmd)
			root.SetArgs([]string{tc.name})

			getOutput := captureStdout(t)
			execErr := root.Execute()
			output := getOutput()

			if execErr == nil {
				t.Fatal("expected error")
			}

			errMsg := execErr.Error()

			// IMAP source should be attempted (appears in
			// output), not blocked by the OAuth failure.
			if !strings.Contains(output, "i@example.com") {
				t.Errorf(
					"IMAP source should be attempted; "+
						"output:\n%s",
					output,
				)
			}

			// The OAuth error should be surfaced, not
			// masked as "no accounts are ready to sync".
			if strings.Contains(errMsg, "no accounts are ready") {
				t.Errorf(
					"should surface OAuth error, not "+
						"generic message; got: %s",
					errMsg,
				)
			}

			// The actual OAuth parse error must appear in
			// the returned error, not just a count.
			if !strings.Contains(errMsg, "parse client secrets") {
				t.Errorf(
					"returned error should contain OAuth "+
						"parse error; got: %s",
					errMsg,
				)
			}
		})
	}
}

// TestSyncFullCmd_MalformedDateRejectsBeforeSync verifies that a
// malformed --after flag is rejected before any source is synced,
// even in a mixed Gmail+IMAP setup where Gmail would otherwise
// succeed first.
func TestSyncFullCmd_MalformedDateRejectsBeforeSync(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	// Create both Gmail and IMAP sources. The Gmail source is
	// made fully syncable (OAuth config + token) so that without
	// the early validation it would be selected and synced before
	// the IMAP source rejects the malformed date.
	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	if err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	_, err = s.GetOrCreateSource("imap", "i@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	_ = s.Close()

	// Write OAuth client secrets and a fake token so the Gmail
	// source passes discovery checks (HasAnyConfig + HasToken).
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	if err := os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600); err != nil {
		t.Fatalf("write client secrets: %v", err)
	}
	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("create tokens dir: %v", err)
	}
	fakeToken := `{"access_token":"fake","token_type":"Bearer"}`
	if err := os.WriteFile(filepath.Join(tokensDir, "g@example.com.json"), []byte(fakeToken), 0600); err != nil {
		t.Fatalf("write fake token: %v", err)
	}

	savedCfg := cfg
	savedLogger := logger
	savedAfter := syncAfter
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		syncAfter = savedAfter
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	syncAfter = "not-a-date"

	testCmd := &cobra.Command{
		Use:  "sync-full [email]",
		Args: cobra.MaximumNArgs(1),
		RunE: syncFullCmd.RunE,
	}

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"sync-full"})

	getOutput := captureStdout(t)
	err = root.Execute()
	output := getOutput()

	if err == nil {
		t.Fatal("expected error for malformed date")
	}
	if !strings.Contains(err.Error(), "--after") {
		t.Errorf("error should mention --after; got: %s", err.Error())
	}
	// No source should have been attempted — the date error
	// must fire before source discovery, not after Gmail syncs.
	if strings.Contains(output, "Starting full sync") {
		t.Error("no sync should start when date flag is invalid")
	}
}

// TestSyncFullCmd_MalformedIMAPDateFlagErrors verifies that malformed
// --after/--before flags produce a clear error for IMAP sources
// instead of silently syncing the entire mailbox.
func TestSyncFullCmd_MalformedIMAPDateFlagErrors(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	src, err := s.GetOrCreateSource("imap", "i@example.com")
	if err != nil {
		t.Fatalf("create imap source: %v", err)
	}
	// Store a minimal IMAP config so buildAPIClient reaches
	// the date-parsing code instead of failing on missing config.
	if err := s.UpdateSourceSyncConfig(src.ID, `{"host":"localhost","port":993,"username":"i@example.com","tls":true}`); err != nil {
		t.Fatalf("set sync config: %v", err)
	}
	_ = s.Close()

	savedCfg := cfg
	savedLogger := logger
	savedAfter := syncAfter
	savedBefore := syncBefore
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		syncAfter = savedAfter
		syncBefore = savedBefore
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	for _, tc := range []struct {
		name   string
		after  string
		before string
		errStr string
	}{
		{"bad after", "not-a-date", "", "--after"},
		{"bad before", "", "2024/01/01", "--before"},
		{"bad both", "Jan 1", "tomorrow", "--after"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			syncAfter = tc.after
			syncBefore = tc.before

			testCmd := &cobra.Command{
				Use:  "sync-full [email]",
				Args: cobra.MaximumNArgs(1),
				RunE: syncFullCmd.RunE,
			}

			root := newTestRootCmd()
			root.AddCommand(testCmd)
			root.SetArgs([]string{
				"sync-full", "i@example.com",
			})

			err := root.Execute()
			if err == nil {
				t.Fatal("expected error for malformed date")
			}
			if !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf(
					"error should mention %q; got: %s",
					tc.errStr, err.Error(),
				)
			}
			if !strings.Contains(err.Error(), "YYYY-MM-DD") {
				t.Errorf(
					"error should mention expected format; got: %s",
					err.Error(),
				)
			}
		})
	}
}

// TestSyncCmd_GmailOnlyBrokenOAuthSurfacesError verifies that when
// only Gmail sources exist and OAuth is broken, the actual error is
// returned, not "no accounts are ready to sync".
func TestSyncCmd_GmailOnlyBrokenOAuthSurfacesError(t *testing.T) {
	for _, tc := range []struct {
		name string
		runE func(*cobra.Command, []string) error
	}{
		{"sync", syncIncrementalCmd.RunE},
		{"sync-full", syncFullCmd.RunE},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			dbPath := tmpDir + "/msgvault.db"

			s, err := store.Open(dbPath)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if err := s.InitSchema(); err != nil {
				t.Fatalf("init schema: %v", err)
			}

			gmailSrc, err := s.GetOrCreateSource(
				"gmail", "g@example.com",
			)
			if err != nil {
				t.Fatalf("create source: %v", err)
			}
			if err := s.UpdateSourceSyncCursor(gmailSrc.ID, "1"); err != nil {
				t.Fatalf("set cursor: %v", err)
			}
			_ = s.Close()

			secretsPath := filepath.Join(
				tmpDir, "client_secret.json",
			)
			err = os.WriteFile(
				secretsPath, []byte("not json"), 0600,
			)
			if err != nil {
				t.Fatalf("write secrets: %v", err)
			}

			savedCfg := cfg
			savedLogger := logger
			defer func() {
				cfg = savedCfg
				logger = savedLogger
			}()

			cfg = &config.Config{
				HomeDir: tmpDir,
				Data:    config.DataConfig{DataDir: tmpDir},
				OAuth: config.OAuthConfig{
					ClientSecrets: secretsPath,
				},
			}
			logger = slog.New(
				slog.NewTextHandler(os.Stderr, nil),
			)

			testCmd := &cobra.Command{
				Use:  tc.name + " [email]",
				Args: cobra.MaximumNArgs(1),
				RunE: tc.runE,
			}

			root := newTestRootCmd()
			root.AddCommand(testCmd)
			root.SetArgs([]string{tc.name})

			err = root.Execute()
			if err == nil {
				t.Fatal("expected error")
			}

			errMsg := err.Error()

			// Should surface the real OAuth parse error.
			if !strings.Contains(errMsg, "client secrets") {
				t.Errorf(
					"expected OAuth parse error; got: %s",
					errMsg,
				)
			}
		})
	}
}
