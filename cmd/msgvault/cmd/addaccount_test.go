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
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/store"
)

func TestFindGmailSource(t *testing.T) {
	tmpDir := t.TempDir()
	s, err := store.Open(tmpDir + "/msgvault.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}

	const email = "user@company.com"

	// No sources at all — should suggest add-account.
	src, err := findGmailSource(s, email)
	if err != nil {
		t.Fatalf("findGmailSource error: %v", err)
	}
	if src != nil {
		t.Error("expected nil with no sources")
	}

	// Non-Gmail source exists — should still suggest add-account.
	if _, err := s.GetOrCreateSource("mbox", email); err != nil {
		t.Fatalf("create mbox source: %v", err)
	}
	src, err = findGmailSource(s, email)
	if err != nil {
		t.Fatalf("findGmailSource error: %v", err)
	}
	if src != nil {
		t.Error("expected nil with only mbox source")
	}

	// Gmail source exists — should suppress the hint.
	if _, err := s.GetOrCreateSource("gmail", email); err != nil {
		t.Fatalf("create gmail source: %v", err)
	}
	src, err = findGmailSource(s, email)
	if err != nil {
		t.Fatalf("findGmailSource error: %v", err)
	}
	if src == nil {
		t.Fatal("expected non-nil with gmail source")
	}
	if src.SourceType != "gmail" {
		t.Errorf("source type = %q, want gmail", src.SourceType)
	}
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
			tmpDir := t.TempDir()
			dbPath := filepath.Join(tmpDir, "msgvault.db")

			s, err := store.Open(dbPath)
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			if err := s.InitSchema(); err != nil {
				t.Fatalf("init schema: %v", err)
			}
			source, err := s.GetOrCreateSource("gmail", "user@acme.com")
			if err != nil {
				t.Fatalf("create source: %v", err)
			}
			err = s.UpdateSourceOAuthApp(source.ID, sql.NullString{String: "acme", Valid: true})
			if err != nil {
				t.Fatalf("set oauth_app: %v", err)
			}
			_ = s.Close()

			tokensDir := filepath.Join(tmpDir, "tokens")
			if err := os.MkdirAll(tokensDir, 0700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			tokenData, _ := json.Marshal(map[string]string{
				"access_token":  "fake",
				"refresh_token": "fake",
				"token_type":    "Bearer",
				"client_id":     tc.clientID,
			})
			if err := os.WriteFile(filepath.Join(tokensDir, "user@acme.com.json"), tokenData, 0600); err != nil {
				t.Fatalf("write token: %v", err)
			}

			secretsPath := filepath.Join(tmpDir, "secret.json")
			if err := os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600); err != nil {
				t.Fatalf("write secrets: %v", err)
			}

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
			if tc.wantError && err == nil {
				t.Fatal("expected error for mismatched token")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestAddAccount_RebindWithExistingToken verifies that switching
// OAuth app binding with an existing token updates the binding
// without re-authorizing (headless rebind scenario).
func TestAddAccount_RebindWithExistingToken(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	// Set up DB with a source bound to "old-app"
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	source, err := s.GetOrCreateSource("gmail", "user@acme.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	err = s.UpdateSourceOAuthApp(source.ID, sql.NullString{
		String: "old-app", Valid: true,
	})
	if err != nil {
		t.Fatalf("set oauth_app: %v", err)
	}
	_ = s.Close()

	// Write a fake token file
	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	// client_id must match the fake client secrets so
	// TokenMatchesClient returns true (headless rebind scenario).
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	tokenPath := filepath.Join(tokensDir, "user@acme.com.json")
	if err := os.WriteFile(tokenPath, tokenData, 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	// Write fake client secrets
	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Token file should still exist
	if _, statErr := os.Stat(tokenPath); os.IsNotExist(statErr) {
		t.Error("token file was deleted during rebind")
	}

	// Binding should be updated to "new-app"
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s2.Close() }()

	src, err := findGmailSource(s2, "user@acme.com")
	if err != nil {
		t.Fatalf("find source: %v", err)
	}
	if src == nil {
		t.Fatal("source not found after rebind")
	}
	if !src.OAuthApp.Valid || src.OAuthApp.String != "new-app" {
		t.Errorf("oauth_app = %v, want new-app", src.OAuthApp)
	}
}

// TestAddAccount_ForceRebindPreservesBindingOnFailure verifies that
// --force --oauth-app with a cancelled auth does not update the binding.
// TestAddAccount_NewRegistrationRejectsMismatchedToken verifies that
// add-account --oauth-app with no existing source row rejects a token
// minted by a different OAuth client (forces re-auth, not silent accept).
func TestAddAccount_NewRegistrationRejectsMismatchedToken(t *testing.T) {
	tmpDir := t.TempDir()

	// Write a token with a DIFFERENT client_id than the fake secrets
	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "wrong-client.apps.googleusercontent.com",
	})
	if err := os.WriteFile(
		filepath.Join(tokensDir, "new@acme.com.json"),
		tokenData, 0600,
	); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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
	err := root.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected error: mismatched token should not be silently accepted")
	}
}

// TestAddAccount_ExplicitDefaultRejectsMismatchedToken verifies that
// --oauth-app "" rejects a token minted by a different client.
func TestAddAccount_ExplicitDefaultRejectsMismatchedToken(t *testing.T) {
	tmpDir := t.TempDir()

	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	// Token with a client_id that does NOT match the default secrets
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "wrong-client.apps.googleusercontent.com",
	})
	if err := os.WriteFile(
		filepath.Join(tokensDir, "user@example.com.json"),
		tokenData, 0600,
	); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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

	err := root.ExecuteContext(ctx)
	if err == nil {
		t.Fatal("expected error: mismatched token should be rejected with explicit --oauth-app \"\"")
	}
}

// TestAddAccount_ExplicitDefaultAcceptsMatchingToken verifies that
// --oauth-app "" accepts a token minted by the default client.
func TestAddAccount_ExplicitDefaultAcceptsMatchingToken(t *testing.T) {
	tmpDir := t.TempDir()

	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	// Token with client_id matching the fake secrets
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	if err := os.WriteFile(
		filepath.Join(tokensDir, "user@example.com.json"),
		tokenData, 0600,
	); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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
	err := root.ExecuteContext(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAddAccount_ForceRebindPreservesBindingOnFailure(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	source, err := s.GetOrCreateSource("gmail", "user@acme.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	err = s.UpdateSourceOAuthApp(source.ID, sql.NullString{
		String: "old-app", Valid: true,
	})
	if err != nil {
		t.Fatalf("set oauth_app: %v", err)
	}
	_ = s.Close()

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(
		secretsPath, []byte(fakeClientSecrets), 0600,
	); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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
	if err == nil {
		t.Fatal("expected error from cancelled auth")
	}

	// Binding should still be "old-app"
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s2.Close() }()

	src, err := findGmailSource(s2, "user@acme.com")
	if err != nil {
		t.Fatalf("find source: %v", err)
	}
	if src == nil {
		t.Fatal("source not found")
	}
	if !src.OAuthApp.Valid || src.OAuthApp.String != "old-app" {
		t.Errorf(
			"oauth_app = %v, want old-app "+
				"(binding should not change on auth failure)",
			src.OAuthApp,
		)
	}
}

// TestAddAccount_HeadlessExplicitEmptyOAuthApp verifies that
// --headless --oauth-app "" does not re-inherit the stored binding.
func TestAddAccount_HeadlessExplicitEmptyOAuthApp(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	// Set up DB with a source bound to "acme"
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := s.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	source, err := s.GetOrCreateSource("gmail", "user@acme.com")
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	err = s.UpdateSourceOAuthApp(source.ID, sql.NullString{
		String: "acme", Valid: true,
	})
	if err != nil {
		t.Fatalf("set oauth_app: %v", err)
	}
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

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The output should NOT contain --oauth-app acme since we
	// explicitly passed an empty --oauth-app to clear to default.
	if strings.Contains(output, "--oauth-app") {
		t.Errorf(
			"explicit empty --oauth-app should not inherit stored "+
				"binding; output contains --oauth-app:\n%s",
			output,
		)
	}
}

// TestAddAccount_AutoDefaultIdentityFires verifies that running add-account
// with a reusable token writes an account-identifier identity row.
func TestAddAccount_AutoDefaultIdentityFires(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	if err := os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	if err != nil {
		t.Fatalf("find source: %v", err)
	}
	if src == nil {
		t.Fatal("source not found")
	}

	ids, err := s.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	if len(ids) != 1 {
		t.Fatalf("expected 1 identity row, got %d", len(ids))
	}
	if ids[0].Address != "user@example.com" {
		t.Errorf("address = %q, want user@example.com", ids[0].Address)
	}
	if ids[0].SourceSignal != "account-identifier" {
		t.Errorf("source_signal = %q, want account-identifier", ids[0].SourceSignal)
	}
}

// TestAddAccount_NoDefaultIdentitySuppresses verifies that --no-default-identity
// prevents the auto-identity write.
func TestAddAccount_NoDefaultIdentitySuppresses(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	if err := os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	if err != nil {
		t.Fatalf("find source: %v", err)
	}
	if src == nil {
		t.Fatal("source not found")
	}

	ids, err := s.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("expected 0 identity rows with --no-default-identity, got %d", len(ids))
	}
}

// TestAddAccount_DeferredLegacyIdentityMigrationFires verifies that legacy
// [identity] addresses configured before any source exists are migrated
// onto the first source created in the same add-account invocation.
// Regression test for iter10: previously, runStartupMigrations ran before
// GetOrCreateSource, so the deferred migration parked at startup and only
// applied on the *next* command — leaving the new source without its
// configured identities until then.
func TestAddAccount_DeferredLegacyIdentityMigrationFires(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	if err := os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The user-facing notice must only describe the applied path.
	// Emitting the "deferred — will run on the next command" notice
	// inside an invocation that DID apply the migration is misleading
	// and a regression of the iter10 polish fix.
	logs := logBuf.String()
	if strings.Contains(logs, "migration deferred until a source exists") {
		t.Errorf("deferred notice fired in same invocation that applied the migration; logs:\n%s", logs)
	}
	if !strings.Contains(logs, "legacy identity migrated") {
		t.Errorf("expected applied notice in logs; got:\n%s", logs)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	if err != nil {
		t.Fatalf("find source: %v", err)
	}
	if src == nil {
		t.Fatal("source not found")
	}

	ids, err := s.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("expected 2 legacy-migrated identity rows on first invocation, got %d: %+v", len(ids), ids)
	}
	got := map[string]string{ids[0].Address: ids[0].SourceSignal, ids[1].Address: ids[1].SourceSignal}
	for _, addr := range []string{"alias@example.com", "alt@work.com"} {
		signal, ok := got[addr]
		if !ok {
			t.Errorf("missing identity row for %q (have %+v)", addr, got)
			continue
		}
		if signal != "config_migration" {
			t.Errorf("address %q: source_signal = %q, want config_migration", addr, signal)
		}
	}

	applied, err := s.IsMigrationApplied("legacy_identity_to_per_account")
	if err != nil {
		t.Fatalf("IsMigrationApplied: %v", err)
	}
	if !applied {
		t.Error("migration sentinel should be set after first successful add-account")
	}
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
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "msgvault.db")

	tokensDir := filepath.Join(tmpDir, "tokens")
	if err := os.MkdirAll(tokensDir, 0700); err != nil {
		t.Fatalf("mkdir tokens: %v", err)
	}
	tokenData, _ := json.Marshal(map[string]string{
		"access_token":  "fake-access",
		"refresh_token": "fake-refresh",
		"token_type":    "Bearer",
		"client_id":     "test.apps.googleusercontent.com",
	})
	if err := os.WriteFile(filepath.Join(tokensDir, "user@example.com.json"), tokenData, 0600); err != nil {
		t.Fatalf("write token: %v", err)
	}

	secretsPath := filepath.Join(tmpDir, "secret.json")
	if err := os.WriteFile(secretsPath, []byte(fakeClientSecrets), 0600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

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

	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()

	src, err := findGmailSource(s, "user@example.com")
	if err != nil {
		t.Fatalf("find source: %v", err)
	}
	if src == nil {
		t.Fatal("source not found")
	}

	ids, err := s.ListAccountIdentities(src.ID)
	if err != nil {
		t.Fatalf("ListAccountIdentities: %v", err)
	}
	// Want 3 rows: 2 legacy-migrated + 1 account-identifier.
	if len(ids) != 3 {
		t.Fatalf("expected 3 identity rows (2 legacy + 1 account-identifier), got %d: %+v", len(ids), ids)
	}
	got := make(map[string]bool, len(ids))
	for _, ai := range ids {
		got[ai.Address] = true
	}
	for _, want := range []string{"alias@example.com", "alt@work.com", "user@example.com"} {
		if !got[want] {
			t.Errorf("missing identity row for %q (have %v)", want, got)
		}
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

	if err == nil {
		t.Fatal("expected --headless service account error")
	}
	if !strings.Contains(err.Error(), "service accounts do not use --headless") {
		t.Fatalf("error = %v, want service accounts do not use --headless", err)
	}
	if strings.Contains(output, "Headless Server Setup") {
		t.Fatalf("service account path should not print browser OAuth headless instructions:\n%s", output)
	}
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
	if err == nil {
		t.Fatal("expected --force service account error")
	}
	if !strings.Contains(err.Error(), "service accounts do not use --force") {
		t.Fatalf("error = %v, want service accounts do not use --force", err)
	}
}
