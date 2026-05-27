package cmd

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
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
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	// Insert IMAP *before* Gmail so that an ambiguous single-row
	// lookup (the old GetSourceByIdentifier bug) would return the
	// IMAP row, not the Gmail one. This ensures the test only
	// passes when the resolved Gmail source is actually used.
	_, err = s.GetOrCreateSource("imap", "shared@example.com")
	require.NoError(err, "create imap source")

	gmailSrc, err := s.GetOrCreateSource(
		"gmail", "shared@example.com",
	)
	require.NoError(err, "create gmail source")
	// Set a history cursor so runIncrementalSync proceeds past
	// the "no history ID" guard and into getTokenSourceWithReauth.
	require.NoError(s.UpdateSourceSyncCursor(gmailSrc.ID, "99999"), "set sync cursor")
	_ = s.Close()

	// Write a minimal client_secret.json so the OAuth manager
	// can be created without error.
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	), "write client secrets")

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

	require.Error(execErr, "expected error (no credentials/token)")

	errMsg := execErr.Error()

	// Should NOT hit the legacy Gmail-only fallback, which sets
	// source to nil and produces "no source found".
	assert.NotContains(output, "no source found", "should not hit legacy Gmail-only fallback path")

	// Both sources should be resolved and attempted, producing
	// 2 failures (IMAP: missing config, Gmail: missing token).
	assert.Contains(errMsg, "2 account(s) failed", "expected both sources resolved")

	// The Gmail error should come from inside runIncrementalSync
	// (reaching getTokenSourceWithReauth), not from OAuth manager
	// creation. "add-account" appears only in the token-missing
	// error produced by getTokenSourceWithReauth.
	assert.Contains(output, "add-account",
		"Gmail error should originate from runIncrementalSync; output:\n%s", output)
}

// TestSyncCmd_SingleSourceNoAmbiguity verifies that a single
// source for an identifier works without the legacy fallback.
func TestSyncCmd_SingleSourceNoAmbiguity(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("imap", "solo@example.com")
	require.NoError(err, "create imap source")
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
	require.Error(err, "expected error (no IMAP config)")

	errMsg := err.Error()

	// Exactly 1 source should fail (IMAP with missing config).
	assert.Contains(errMsg, "1 account(s) failed", "expected 1 failed account")

	// Should NOT hit legacy fallback (source exists in DB).
	assert.NotContains(errMsg, "no source found", "should not hit legacy fallback path")
}

// TestSyncCmd_MboxIdentifierDoesNotFallback verifies that an
// identifier that exists only as a non-syncable source type (mbox)
// returns a clear error instead of falling back to the legacy
// Gmail path.
func TestSyncCmd_MboxIdentifierDoesNotFallback(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	requirepkg.NoError(t, err, "open store")
	requirepkg.NoError(t, s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("mbox", "imported@example.com")
	requirepkg.NoError(t, err, "create mbox source")
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
			requirepkg.Error(t, err, "expected error for non-syncable source")
			assertpkg.ErrorContains(t, err, "cannot be synced", "expected unsupported-source error")
		})
	}
}

// TestSyncFullCmd_OAuthSkipDoesNotBlockIMAP verifies that in a
// mixed Gmail+IMAP setup without OAuth configured, sync-full skips
// the Gmail source and still syncs the IMAP source.
func TestSyncFullCmd_OAuthSkipDoesNotBlockIMAP(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	require.NoError(err, "create gmail source")
	_, err = s.GetOrCreateSource("imap", "i@example.com")
	require.NoError(err, "create imap source")
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
	require.Error(execErr, "expected error (IMAP has no config)")

	// Gmail should be skipped, not cause an abort.
	assert.Contains(output, "Skipping g@example.com",
		"Gmail source should be skipped; output:\n%s", output)

	// IMAP source should have been attempted.
	assert.Contains(output, "i@example.com",
		"IMAP source should be attempted; output:\n%s", output)
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
			require := requirepkg.New(t)
			assert := assertpkg.New(t)
			tmpDir := t.TempDir()
			dbPath := tmpDir + "/msgvault.db"

			s, err := store.Open(dbPath)
			require.NoError(err, "open store")
			require.NoError(s.InitSchema(), "init schema")

			gmailSrc, err := s.GetOrCreateSource(
				"gmail", "g@example.com",
			)
			require.NoError(err, "create gmail source")
			// Give Gmail source a cursor so it passes
			// the sync command's discovery checks.
			require.NoError(s.UpdateSourceSyncCursor(gmailSrc.ID, "1"), "set cursor")

			_, err = s.GetOrCreateSource(
				"imap", "i@example.com",
			)
			require.NoError(err, "create imap source")
			_ = s.Close()

			// Write a malformed client_secret.json.
			secretsPath := filepath.Join(
				tmpDir, "client_secret.json",
			)
			require.NoError(os.WriteFile(
				secretsPath, []byte("not json"), 0600,
			), "write secrets")

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

			require.Error(execErr, "expected error")

			errMsg := execErr.Error()

			// IMAP source should be attempted (appears in
			// output), not blocked by the OAuth failure.
			assert.Contains(output, "i@example.com",
				"IMAP source should be attempted; output:\n%s", output)

			// The OAuth error should be surfaced, not
			// masked as "no accounts are ready to sync".
			assert.NotContains(errMsg, "no accounts are ready",
				"should surface OAuth error, not generic message")

			// The actual OAuth parse error must appear in
			// the returned error, not just a count.
			assert.Contains(errMsg, "parse client secrets",
				"returned error should contain OAuth parse error")
		})
	}
}

// TestSyncFullCmd_MalformedDateRejectsBeforeSync verifies that a
// malformed --after flag is rejected before any source is synced,
// even in a mixed Gmail+IMAP setup where Gmail would otherwise
// succeed first.
func TestSyncFullCmd_MalformedDateRejectsBeforeSync(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	// Create both Gmail and IMAP sources. The Gmail source is
	// made fully syncable (OAuth config + token) so that without
	// the early validation it would be selected and synced before
	// the IMAP source rejects the malformed date.
	_, err = s.GetOrCreateSource("gmail", "g@example.com")
	require.NoError(err, "create gmail source")
	_, err = s.GetOrCreateSource("imap", "i@example.com")
	require.NoError(err, "create imap source")
	_ = s.Close()

	// Write OAuth client secrets and a fake token so the Gmail
	// source passes discovery checks (HasAnyConfig + HasToken).
	secretsPath := filepath.Join(tmpDir, "client_secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write client secrets")
	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "create tokens dir")
	fakeToken := `{"access_token":"fake","token_type":"Bearer"}`
	require.NoError(os.WriteFile(filepath.Join(tokensDir, "g@example.com.json"), []byte(fakeToken), 0600), "write fake token")

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

	require.Error(err, "expected error for malformed date")
	require.ErrorContains(err, "--after")
	// No source should have been attempted — the date error
	// must fire before source discovery, not after Gmail syncs.
	assert.NotContains(output, "Starting full sync", "no sync should start when date flag is invalid")
}

// TestSyncFullCmd_MalformedIMAPDateFlagErrors verifies that malformed
// --after/--before flags produce a clear error for IMAP sources
// instead of silently syncing the entire mailbox.
func TestSyncFullCmd_MalformedIMAPDateFlagErrors(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := tmpDir + "/msgvault.db"

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")

	src, err := s.GetOrCreateSource("imap", "i@example.com")
	require.NoError(err, "create imap source")
	// Store a minimal IMAP config so buildAPIClient reaches
	// the date-parsing code instead of failing on missing config.
	require.NoError(s.UpdateSourceSyncConfig(src.ID, `{"host":"localhost","port":993,"username":"i@example.com","tls":true}`), "set sync config")
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
			requirepkg.Error(t, err, "expected error for malformed date")
			requirepkg.ErrorContains(t, err, tc.errStr, "error should mention %q", tc.errStr)
			assertpkg.ErrorContains(t, err, "YYYY-MM-DD", "error should mention expected format")
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
			require := requirepkg.New(t)
			tmpDir := t.TempDir()
			dbPath := tmpDir + "/msgvault.db"

			s, err := store.Open(dbPath)
			require.NoError(err, "open store")
			require.NoError(s.InitSchema(), "init schema")

			gmailSrc, err := s.GetOrCreateSource(
				"gmail", "g@example.com",
			)
			require.NoError(err, "create source")
			require.NoError(s.UpdateSourceSyncCursor(gmailSrc.ID, "1"), "set cursor")
			_ = s.Close()

			secretsPath := filepath.Join(
				tmpDir, "client_secret.json",
			)
			require.NoError(os.WriteFile(
				secretsPath, []byte("not json"), 0600,
			), "write secrets")

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
			require.Error(err, "expected error")

			errMsg := err.Error()

			// Should surface the real OAuth parse error.
			assertpkg.Contains(t, errMsg, "client secrets", "expected OAuth parse error")
		})
	}
}
