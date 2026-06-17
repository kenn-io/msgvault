package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestFindGmailSource(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	s, err := store.Open(tmpDir + "/msgvault.db")
	require.NoError(err, "open store")
	defer func() { _ = s.Close() }()
	require.NoError(s.InitSchema(), "init schema")

	const email = "user@company.com"

	// No sources at all — should suggest add-account.
	src, err := findGmailSource(s, email)
	require.ErrorIs(err, errGmailSourceNotFound, "findGmailSource")
	assert.Nil(src, "expected nil with no sources")

	// Non-Gmail source exists — should still suggest add-account.
	_, err = s.GetOrCreateSource("mbox", email)
	require.NoError(err, "create mbox source")
	src, err = findGmailSource(s, email)
	require.ErrorIs(err, errGmailSourceNotFound, "findGmailSource")
	assert.Nil(src, "expected nil with only mbox source")

	// Gmail source exists — should suppress the hint.
	_, err = s.GetOrCreateSource("gmail", email)
	require.NoError(err, "create gmail source")
	src, err = findGmailSource(s, email)
	require.NoError(err, "findGmailSource")
	require.NotNil(src, "expected non-nil with gmail source")
	assert.Equal("gmail", src.SourceType, "source type")
}

// TestAddAccount_InheritedBindingValidatesToken verifies that re-running
// add-account without --oauth-app on a named-app account validates the
// token's client_id against the inherited binding.
func TestAddAccount_InheritedBindingValidatesToken(t *testing.T) {
	for _, tc := range []struct {
		name      string
		clientID  string
		wantError bool
	}{
		{"matching token reused", "test.apps.googleusercontent.com", false},
		{"mismatched token rejected", "wrong.apps.googleusercontent.com", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require := requirepkg.New(t)
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "msgvault.db")

			s, err := store.Open(dbPath)
			require.NoError(err, "open store")
			require.NoError(s.InitSchema(), "init schema")
			source, err := s.GetOrCreateSource("gmail", "user@acme.com")
			require.NoError(err, "create source")
			require.NoError(s.UpdateSourceOAuthApp(source.ID, sql.NullString{String: "acme", Valid: true}), "set oauth_app")
			_ = s.Close()

			tokensDir := filepath.Join(tmpDir, "tokens")
			require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir")
			tokenData, _ := json.Marshal(map[string]string{
				"access_token":  "fake",
				"refresh_token": "fake",
				"token_type":    "Bearer",
				"client_id":     tc.clientID,
			})
			require.NoError(os.WriteFile(filepath.Join(tokensDir, "user@acme.com.json"), tokenData, 0600), "write token")

			secretsPath := filepath.Join(tmpDir, "secret.json")
			require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write secrets")

			savedCfg, savedLogger, savedOAuthApp := cfg, logger, oauthAppName
			defer func() { cfg, logger, oauthAppName = savedCfg, savedLogger, savedOAuthApp }()

			cfg = &config.Config{
				HomeDir: tmpDir,
				Data:    config.DataConfig{DataDir: tmpDir},
				OAuth: config.OAuthConfig{
					Apps: map[string]config.OAuthApp{
						"acme": {ClientSecrets: secretsPath},
					},
				},
			}
			logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			testCmd := &cobra.Command{
				Use: "add-account <email>", Args: cobra.ExactArgs(1),
				RunE: addAccountCmd.RunE,
			}
			testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
			testCmd.Flags().BoolVar(&headless, "headless", false, "")
			testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
			testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
			testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

			root := newTestRootCmd()
			root.AddCommand(testCmd)
			// No --oauth-app flag: binding inherited from DB
			root.SetArgs([]string{"add-account", "user@acme.com"})

			err = root.ExecuteContext(ctx)
			if tc.wantError {
				require.Error(err, "expected error for mismatched token")
			} else {
				require.NoError(err)
			}
		})
	}
}

// TestAddAccount_RebindWithExistingToken verifies that switching
// OAuth app binding with an existing token updates the binding
// without re-authorizing (headless rebind scenario).
func TestAddAccount_RebindWithExistingToken(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	// Set up DB with a source bound to "old-app"
	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	source, err := s.GetOrCreateSource("gmail", "user@acme.com")
	require.NoError(err, "create source")
	require.NoError(s.UpdateSourceOAuthApp(source.ID, sql.NullString{
		String: "old-app", Valid: true,
	}), "set oauth_app")
	_ = s.Close()

	// Write a fake token file
	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	// client_id must match the fake client secrets so
	// TokenMatchesClient returns true (headless rebind scenario).
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	tokenPath := filepath.Join(tokensDir, "user@acme.com.json")
	require.NoError(os.WriteFile(tokenPath, tokenData, 0600), "write token")

	// Write fake client secrets
	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"old-app": {ClientSecrets: secretsPath},
				"new-app": {ClientSecrets: secretsPath},
			},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@acme.com", "--oauth-app", "new-app",
	})

	// Should succeed without opening a browser — token exists
	err = root.Execute()
	require.NoError(err)

	// Token file should still exist
	_, statErr := os.Stat(tokenPath)
	assert.False(os.IsNotExist(statErr), "token file was deleted during rebind")

	// Binding should be updated to "new-app"
	s2, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s2.Close() }()

	src, err := findGmailSource(s2, "user@acme.com")
	require.NoError(err, "find source")
	require.NotNil(src, "source not found after rebind")
	assert.True(src.OAuthApp.Valid && src.OAuthApp.String == "new-app",
		"oauth_app = %v, want new-app", src.OAuthApp)
}

// TestAddAccount_ForceRebindPreservesBindingOnFailure verifies that
// --force --oauth-app with a cancelled auth does not update the binding.
// TestAddAccount_NewRegistrationRejectsMismatchedToken verifies that
// add-account --oauth-app with no existing source row rejects a token
// minted by a different OAuth client (forces re-auth, not silent accept).
func TestAddAccount_NewRegistrationRejectsMismatchedToken(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()

	// Write a token with a DIFFERENT client_id than the fake secrets
	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "wrong-client.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(
		filepath.Join(tokensDir, "new@acme.com.json"),
		tokenData, 0600,
	), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"acme": {ClientSecrets: secretsPath},
			},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Pre-cancel so if it falls through to Authorize, it fails fast
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "new@acme.com", "--oauth-app", "acme",
	})

	// Should fail: token exists but from wrong client, auth cancelled
	err = root.ExecuteContext(ctx)
	require.Error(err, "mismatched token should not be silently accepted")
}

// TestAddAccount_ExplicitDefaultRejectsMismatchedToken verifies that
// --oauth-app "" rejects a token minted by a different client.
func TestAddAccount_ExplicitDefaultRejectsMismatchedToken(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	// Token with a client_id that does NOT match the default secrets
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "wrong-client.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(
		filepath.Join(tokensDir, "user@example.com.json"),
		tokenData, 0600,
	), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@example.com", "--oauth-app", "",
	})

	err = root.ExecuteContext(ctx)
	require.Error(err, "mismatched token should be rejected with explicit --oauth-app \"\"")
}

// TestAddAccount_ExplicitDefaultAcceptsMatchingToken verifies that
// --oauth-app "" accepts a token minted by the default client.
func TestAddAccount_ExplicitDefaultAcceptsMatchingToken(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	// Token with client_id matching the fake secrets
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(
		filepath.Join(tokensDir, "user@example.com.json"),
		tokenData, 0600,
	), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	// Pre-cancel so if regression causes auth attempt, it fails fast
	// instead of opening a browser.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@example.com", "--oauth-app", "",
	})

	// Should succeed: token's client_id matches, no auth needed
	err = root.ExecuteContext(ctx)
	require.NoError(err)
}

func TestAddAccount_ForceRebindPreservesBindingOnFailure(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	source, err := s.GetOrCreateSource("gmail", "user@acme.com")
	require.NoError(err, "create source")
	require.NoError(s.UpdateSourceOAuthApp(source.ID, sql.NullString{
		String: "old-app", Valid: true,
	}), "set oauth_app")
	_ = s.Close()

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	savedForce := forceReauth
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
		forceReauth = savedForce
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"old-app": {ClientSecrets: secretsPath},
				"new-app": {ClientSecrets: secretsPath},
			},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Pre-cancel context so Authorize fails immediately
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@acme.com",
		"--force", "--oauth-app", "new-app",
	})

	err = root.ExecuteContext(ctx)
	require.Error(err, "expected error from cancelled auth")

	// Binding should still be "old-app"
	s2, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s2.Close() }()

	src, err := findGmailSource(s2, "user@acme.com")
	require.NoError(err, "find source")
	require.NotNil(src, "source not found")
	assertpkg.True(t, src.OAuthApp.Valid && src.OAuthApp.String == "old-app",
		"oauth_app = %v, want old-app (binding should not change on auth failure)", src.OAuthApp)
}

// TestAddAccount_HeadlessExplicitEmptyOAuthApp verifies that
// --headless --oauth-app "" does not re-inherit the stored binding.
func TestAddAccount_HeadlessExplicitEmptyOAuthApp(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	// Set up DB with a source bound to "acme"
	s, err := store.Open(dbPath)
	require.NoError(err, "open store")
	require.NoError(s.InitSchema(), "init schema")
	source, err := s.GetOrCreateSource("gmail", "user@acme.com")
	require.NoError(err, "create source")
	require.NoError(s.UpdateSourceOAuthApp(source.ID, sql.NullString{
		String: "acme", Valid: true,
	}), "set oauth_app")
	_ = s.Close()

	// Save/restore globals
	savedCfg := cfg
	savedLogger := logger
	savedHeadless := headless
	savedOAuthApp := oauthAppName
	savedForce := forceReauth
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		headless = savedHeadless
		oauthAppName = savedOAuthApp
		forceReauth = savedForce
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	// The RunE reads package-level flag vars, but uses
	// cmd.Flags().Changed() to detect explicit --oauth-app.
	// We need to register the flags on the test command so
	// Changed() works, and bind them to the package-level vars.
	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@acme.com",
		"--headless", "--oauth-app", "",
	})

	getOutput := captureStdout(t)
	err = root.Execute()
	output := getOutput()

	require.NoError(err)

	// The output should NOT contain --oauth-app acme since we
	// explicitly passed an empty --oauth-app to clear to default.
	assertpkg.NotContains(t, output, "--oauth-app",
		"explicit empty --oauth-app should not inherit stored binding; output:\n%s", output)
}

// TestAddAccount_AutoDefaultIdentityFires verifies that running add-account
// with a reusable token writes an account-identifier identity row.
func TestAddAccount_AutoDefaultIdentityFires(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	savedNoDefault := noDefaultIdentityAddAccount
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
		noDefaultIdentityAddAccount = savedNoDefault
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"add-account", "user@example.com"})

	require.NoError(root.Execute())

	s, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	require.NoError(err, "find source")
	require.NotNil(src, "source not found")

	ids, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 1, "expected 1 identity row")
	assert.Equal("user@example.com", ids[0].Address, "address")
	assert.Equal("account-identifier", ids[0].SourceSignal, "source_signal")
}

// TestAddAccount_NoDefaultIdentitySuppresses verifies that --no-default-identity
// prevents the auto-identity write.
func TestAddAccount_NoDefaultIdentitySuppresses(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	savedNoDefault := noDefaultIdentityAddAccount
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
		noDefaultIdentityAddAccount = savedNoDefault
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{"add-account", "user@example.com", "--no-default-identity"})

	require.NoError(root.Execute())

	s, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	require.NoError(err, "find source")
	require.NotNil(src, "source not found")

	ids, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	assertpkg.Empty(t, ids, "expected 0 identity rows with --no-default-identity")
}

// TestAddAccount_DeferredLegacyIdentityMigrationFires verifies that legacy
// [identity] addresses configured before any source exists are migrated
// onto the first source created in the same add-account invocation.
// Regression test for iter10: previously, runStartupMigrations ran before
// GetOrCreateSource, so the deferred migration parked at startup and only
// applied on the *next* command — leaving the new source without its
// configured identities until then.
func TestAddAccount_DeferredLegacyIdentityMigrationFires(t *testing.T) {
	require := requirepkg.New(t)
	assert := assertpkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	savedNoDefault := noDefaultIdentityAddAccount
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
		noDefaultIdentityAddAccount = savedNoDefault
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
		Identity: config.IdentityConfig{
			Addresses: []string{"alias@example.com", "alt@work.com"},
		},
	}
	var logBuf strings.Builder
	logger = slog.New(slog.NewTextHandler(&logBuf, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	// --no-default-identity isolates the test to the legacy migration path:
	// the auto-default would otherwise add a third identity row.
	root.SetArgs([]string{"add-account", "user@example.com", "--no-default-identity"})

	require.NoError(root.Execute())

	// The user-facing notice must only describe the applied path.
	// Emitting the "deferred — will run on the next command" notice
	// inside an invocation that DID apply the migration is misleading
	// and a regression of the iter10 polish fix.
	logs := logBuf.String()
	assert.NotContains(logs, "migration deferred until a source exists",
		"deferred notice fired in same invocation that applied the migration; logs:\n%s", logs)
	assert.Contains(logs, "legacy identity migrated", "expected applied notice in logs")

	s, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	require.NoError(err, "find source")
	require.NotNil(src, "source not found")

	ids, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	require.Len(ids, 2, "expected 2 legacy-migrated identity rows on first invocation, got %+v", ids)
	got := map[string]string{ids[0].Address: ids[0].SourceSignal, ids[1].Address: ids[1].SourceSignal}
	for _, addr := range []string{"alias@example.com", "alt@work.com"} {
		signal, ok := got[addr]
		if !assert.True(ok, "missing identity row for %q (have %+v)", addr, got) {
			continue
		}
		assert.Equal("config_migration", signal, "address %q signal", addr)
	}

	applied, err := s.IsMigrationApplied("legacy_identity_to_per_account")
	require.NoError(err, "IsMigrationApplied")
	assert.True(applied, "migration sentinel should be set after first successful add-account")
}

// TestAddAccount_LegacyMigrationDoesNotSuppressDefaultIdentity verifies
// that the legacy [identity] migration writing rows DOES NOT suppress
// the auto-default-identity write for the source's own account
// identifier. Regression test for iter15 codex Medium: previously,
// runPostSourceCreateMigrations ran BEFORE confirmDefaultIdentity, so
// the legacy migration populated account_identities first, then
// confirmDefaultIdentity's `len(existing) > 0` guard skipped the
// account-identifier write entirely — leaving the source without its
// own identifier and breaking dedup sent-copy detection.
func TestAddAccount_LegacyMigrationDoesNotSuppressDefaultIdentity(t *testing.T) {
	require := requirepkg.New(t)
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	require.NoError(os.MkdirAll(tokensDir, 0700), "mkdir tokens")
	tokenData, err := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	require.NoError(err, "marshal token")
	require.NoError(os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600), "write token")

	secretsPath := filepath.Join(tmpDir, "secret.json")
	require.NoError(os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600), "write secrets")

	savedCfg := cfg
	savedLogger := logger
	savedOAuthApp := oauthAppName
	savedNoDefault := noDefaultIdentityAddAccount
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		oauthAppName = savedOAuthApp
		noDefaultIdentityAddAccount = savedNoDefault
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth:   config.OAuthConfig{ClientSecrets: secretsPath},
		// Legacy [identity] block with two addresses, neither of which
		// is the account being added. The migration must fire BUT also
		// the auto-default identity must be written for user@example.com.
		Identity: config.IdentityConfig{
			Addresses: []string{"alias@example.com", "alt@work.com"},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")
	testCmd.Flags().BoolVar(&noDefaultIdentityAddAccount, "no-default-identity", false, "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	// Note: NOT passing --no-default-identity. The bug only manifests
	// when the auto-default write is supposed to fire.
	root.SetArgs([]string{"add-account", "user@example.com"})

	require.NoError(root.Execute())

	s, err := store.Open(dbPath)
	require.NoError(err, "reopen store")
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	require.NoError(err, "find source")
	require.NotNil(src, "source not found")

	ids, err := s.ListAccountIdentities(src.ID)
	require.NoError(err, "ListAccountIdentities")
	// Want 3 rows: 2 legacy-migrated + 1 account-identifier.
	require.Len(ids, 3, "expected 3 identity rows (2 legacy + 1 account-identifier), got %+v", ids)
	got := make(map[string]bool, len(ids))
	for _, ai := range ids {
		got[ai.Address] = true
	}
	for _, want := range []string{"alias@example.com", "alt@work.com", "user@example.com"} {
		assertpkg.True(t, got[want], "missing identity row for %q (have %v)", want, got)
	}
}

func TestAddAccount_HeadlessServiceAccountReturnsActionableError(t *testing.T) {
	tmpDir := t.TempDir()

	savedCfg := cfg
	savedLogger := logger
	savedHeadless := headless
	savedOAuthApp := oauthAppName
	savedForce := forceReauth
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		headless = savedHeadless
		oauthAppName = savedOAuthApp
		forceReauth = savedForce
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"workspace": {ServiceAccountKey: filepath.Join(tmpDir, "service-account.json")},
			},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@company.com",
		"--headless", "--oauth-app", "workspace",
	})

	getOutput := captureStdout(t)
	err := root.Execute()
	output := getOutput()

	requirepkg.Error(t, err, "expected --headless service account error")
	requirepkg.ErrorContains(t, err, "service accounts do not use --headless")
	assertpkg.NotContains(t, output, "Headless Server Setup",
		"service account path should not print browser OAuth headless instructions:\n%s", output)
}

func TestAddAccount_ForceServiceAccountReturnsActionableError(t *testing.T) {
	tmpDir := t.TempDir()

	savedCfg := cfg
	savedLogger := logger
	savedHeadless := headless
	savedOAuthApp := oauthAppName
	savedForce := forceReauth
	defer func() {
		cfg = savedCfg
		logger = savedLogger
		headless = savedHeadless
		oauthAppName = savedOAuthApp
		forceReauth = savedForce
	}()

	cfg = &config.Config{
		HomeDir: tmpDir,
		Data:    config.DataConfig{DataDir: tmpDir},
		OAuth: config.OAuthConfig{
			Apps: map[string]config.OAuthApp{
				"workspace": {ServiceAccountKey: filepath.Join(tmpDir, "service-account.json")},
			},
		},
	}
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))

	testCmd := &cobra.Command{
		Use:  "add-account <email>",
		Args: cobra.ExactArgs(1),
		RunE: addAccountCmd.RunE,
	}
	testCmd.Flags().StringVar(&oauthAppName, "oauth-app", "", "")
	testCmd.Flags().BoolVar(&headless, "headless", false, "")
	testCmd.Flags().BoolVar(&forceReauth, "force", false, "")
	testCmd.Flags().StringVar(&accountDisplayName, "display-name", "", "")

	root := newTestRootCmd()
	root.AddCommand(testCmd)
	root.SetArgs([]string{
		"add-account", "user@company.com",
		"--force", "--oauth-app", "workspace",
	})

	err := root.Execute()
	requirepkg.Error(t, err, "expected --force service account error")
	requirepkg.ErrorContains(t, err, "service accounts do not use --force")
}
