package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/config"
	"github.com/wesm/msgvault/internal/logging"
	"github.com/wesm/msgvault/internal/oauth"
	"github.com/wesm/msgvault/internal/store"
	"golang.org/x/oauth2"
)

var (
	cfgFile    string
	homeDir    string
	verbose    bool
	useLocal   bool // Force local database even when remote is configured
	logFile    string
	logLevel   string
	noLogFile  bool
	logSQL     bool
	logSQLSlow int64
	cfg        *config.Config
	// logger is always non-nil so code paths outside the normal
	// PersistentPreRunE flow (tests, library embeds) don't have
	// to nil-check before calling logger.Info. PersistentPreRunE
	// replaces this with a properly configured multi-handler at
	// CLI startup.
	logger    = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logResult *logging.Result // non-nil after PersistentPreRunE runs
)

var rootCmd = &cobra.Command{
	Use:   "msgvault",
	Short: "Offline email archive tool",
	Long: `msgvault is an offline email archive tool that exports and stores
email data locally with full-text search capabilities.

This is the Go implementation providing sync, search, and TUI functionality
in a single binary.`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		// Skip config loading (and therefore logging setup) for
		// commands that must run without touching disk or config.
		if cmd.Name() == "version" || cmd.Name() == "update" ||
			cmd.Name() == "quickstart" || cmd.Name() == "completion" ||
			cmd.Name() == cobra.ShellCompRequestCmd ||
			cmd.Name() == cobra.ShellCompNoDescRequestCmd {
			return nil
		}

		// Load config first; logging options live under [log].
		var err error
		cfg, err = config.Load(cfgFile, homeDir)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		if err := cfg.EnsureHomeDir(); err != nil {
			return fmt.Errorf(
				"create data directory %s: %w",
				cfg.HomeDir, err,
			)
		}

		// Resolve logging options. CLI flags override config;
		// --verbose forces debug level regardless of other
		// settings.
		var levelOverride *slog.Level
		if verbose {
			lv := slog.LevelDebug
			levelOverride = &lv
		}
		levelString := logLevel
		if levelString == "" {
			levelString = cfg.Log.Level
		}
		logsDir := cfg.LogsDir()
		// File logging is opt-in: requires [log].enabled,
		// [log].dir, or --log-file. --no-log-file overrides.
		fileDisabled := noLogFile || (logFile == "" && !cfg.Log.Enabled && cfg.Log.Dir == "")

		// Close a previous log handler if tests re-enter
		// PersistentPreRunE without going through ExecuteContext.
		if logResult != nil {
			logResult.Close()
			logResult = nil
		}

		logResult, err = logging.BuildHandler(logging.Options{
			LogsDir:       logsDir,
			FilePath:      logFile,
			FileDisabled:  fileDisabled,
			LevelOverride: levelOverride,
			LevelString:   levelString,
		})
		if err != nil {
			return fmt.Errorf("build logger: %w", err)
		}
		logger = slog.New(logResult.Handler)
		// logResult.RunID is available for any command that needs it.
		slog.SetDefault(logger)

		// Configure the store's SQL logging adapter now that
		// slog.Default is set. Flag overrides config; a zero
		// SlowMs falls back to the built-in default (100 ms).
		sqlTrace := logSQL || cfg.Log.SQLTrace
		slowMs := logSQLSlow
		if slowMs == 0 {
			slowMs = cfg.Log.SQLSlowMs
		}
		store.ConfigureSQLLogging(store.SQLLogOptions{
			SlowMs:    slowMs,
			FullTrace: sqlTrace,
		})

		// Startup header: one structured line per run that
		// captures everything you'd want to correlate later.
		// Positional args may contain email addresses, search
		// queries, or other PII — log only the count at info
		// level and the full (sanitized) values at debug.
		logger.Info("msgvault startup",
			"command", cmd.CommandPath(),
			"argc", len(args),
			"version", Version,
			"go_version", runtime.Version(),
			"os", runtime.GOOS,
			"arch", runtime.GOARCH,
			"config_path", cfg.ConfigFilePath(),
			"data_dir", cfg.Data.DataDir,
			"log_file", logResult.FilePath,
			"level", logResult.Level.String(),
		)
		logger.Debug("msgvault startup args",
			"args", sanitizeArgs(args),
		)
		return nil
	},
	// Note: log file closing is handled by ExecuteContext's deferred
	// shutdown, which runs after the exit record is written. Do not
	// close logResult in PersistentPostRunE — doing so drops the
	// "msgvault exit" log line on successful runs.
}

// sanitizeArgs removes anything that might carry a secret before
// the argv hits the log file. Values for flags known to contain
// credentials (--password, --token, --client-secret, ...) are
// replaced with "<redacted>". Unknown flags pass through so the
// log still captures the user's intent.
func sanitizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	redactNext := false
	sensitive := map[string]bool{
		"--password":       true,
		"--token":          true,
		"--client-secret":  true,
		"--access-token":   true,
		"--refresh-token":  true,
		"--client-secrets": true,
	}
	for _, a := range args {
		if redactNext {
			out = append(out, "<redacted>")
			redactNext = false
			continue
		}
		if eq := strings.IndexByte(a, '='); eq != -1 {
			key := a[:eq]
			if sensitive[key] {
				out = append(out, key+"=<redacted>")
				continue
			}
		}
		if sensitive[a] {
			out = append(out, a)
			redactNext = true
			continue
		}
		out = append(out, a)
	}
	return out
}

// recoverAndLogPanic catches a panic and records it as a single
// structured log line with a stack trace before re-raising the
// process exit. Called in a deferred statement at the top of
// Execute/ExecuteContext so crashes always leave a trail on disk.
func recoverAndLogPanic() {
	r := recover()
	if r == nil {
		return
	}
	if logger != nil {
		logger.Error("msgvault panic",
			"panic", fmt.Sprint(r),
			"stack", string(debug.Stack()),
		)
	} else {
		fmt.Fprintf(os.Stderr,
			"msgvault panic: %v\n%s\n", r, debug.Stack(),
		)
	}
	if logResult != nil {
		logResult.Close()
	}
	os.Exit(2)
}

// Execute runs the root command with a background context.
// Prefer ExecuteContext for signal-aware execution.
func Execute() error {
	return ExecuteContext(context.Background())
}

// ExecuteContext runs the root command with the given context,
// enabling graceful shutdown when the context is cancelled.
// Installs a panic recovery and closes the log file handler on
// return so every run ends cleanly in the log.
func ExecuteContext(ctx context.Context) error {
	// Defer ordering is load-bearing. LIFO means recoverAndLogPanic
	// runs before the log-file close. Because recoverAndLogPanic calls
	// os.Exit (which skips remaining defers), it closes logResult
	// itself before exiting. Do not reorder these defers.
	defer func() {
		if logResult != nil {
			logResult.Close()
		}
	}()
	defer recoverAndLogPanic()

	err := rootCmd.ExecuteContext(ctx)

	// Record the exit outcome so users can see the per-run
	// result in the log without parsing error messages.
	if logger != nil {
		if err != nil {
			logger.Info("msgvault exit",
				"outcome", "error", "error", err.Error(),
			)
		} else {
			logger.Info("msgvault exit", "outcome", "ok")
		}
	}
	return err
}

// oauthSetupHint returns help text for OAuth configuration issues,
// using the actual config file path so it's clear on all platforms.
func oauthSetupHint() string {
	configPath := "<config file>"
	if cfg != nil {
		configPath = cfg.ConfigFilePath()
	}
	hint := fmt.Sprintf(`
To use msgvault, you need a Google Cloud OAuth credential:
  1. Follow the setup guide: https://msgvault.io/guides/oauth-setup/
  2. Download the client_secret.json file
  3. Create or edit %s:
       [oauth]
       client_secrets = "/path/to/client_secret.json"`, configPath)
	if cfg != nil && len(cfg.OAuth.Apps) > 0 {
		hint += "\n\nNamed OAuth apps are configured. " +
			"Use 'add-account <email> --oauth-app <name>' to bind an account."
	}
	return hint
}

// errOAuthNotConfigured returns a helpful error when OAuth client secrets are missing.
// It also searches for client_secret*.json files in common locations.
func errOAuthNotConfigured() error {
	// Check common locations for client_secret*.json
	hint := tryFindClientSecrets()
	if hint != "" {
		return fmt.Errorf("OAuth client secrets not configured.%s", hint)
	}
	return fmt.Errorf("OAuth client secrets not configured.%s", oauthSetupHint())
}

// tryFindClientSecrets looks for client_secret*.json in common locations
// and returns a hint if found.
func tryFindClientSecrets() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(home, "Downloads", "client_secret*.json"),
		"client_secret*.json",
	}
	if cfg != nil {
		candidates = append(candidates, filepath.Join(cfg.HomeDir, "client_secret*.json"))
	}

	for _, pattern := range candidates {
		matches, _ := filepath.Glob(pattern)
		if len(matches) > 0 {
			configPath := "<config file>"
			if cfg != nil {
				configPath = cfg.ConfigFilePath()
			}
			return fmt.Sprintf(`

Found OAuth credentials at: %s

To use this file, add to %s:
  [oauth]
  client_secrets = %q

Or copy the file to your msgvault home directory:
  cp %q ~/.msgvault/client_secret.json`, matches[0], configPath, matches[0], matches[0])
		}
	}
	return ""
}

// wrapOAuthError wraps an oauth/client-secrets error with setup instructions
// if the root cause is a missing or unreadable secrets file.
func wrapOAuthError(err error) error {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("OAuth client secrets file not accessible.%s", oauthSetupHint())
	}
	return err
}

// isAuthInvalidError returns true if the error indicates the OAuth token is
// permanently invalid (expired or revoked), as opposed to a transient failure
// like a network error or context cancellation.
func isAuthInvalidError(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	if errors.As(err, &retrieveErr) {
		// Google returns "invalid_grant" when refresh tokens are expired or revoked
		return retrieveErr.ErrorCode == "invalid_grant"
	}
	return false
}

// tokenReauthorizer abstracts the oauth.Manager methods used by
// getTokenSourceWithReauth, making the function testable without real OAuth.
type tokenReauthorizer interface {
	TokenSource(ctx context.Context, email string) (oauth2.TokenSource, error)
	HasToken(email string) bool
	Authorize(ctx context.Context, email string) error
	AuthorizeManual(ctx context.Context, email string) error
}

// getTokenSourceWithReauth tries to get a token source for the given email.
// If the token exists but is expired/revoked (invalid_grant), it automatically
// deletes the old token and re-initiates the OAuth browser flow.
// Transient errors (network, context cancellation) are returned as-is without
// deleting the token.
// The interactive parameter controls whether the function can open a browser
// for re-authorization. Callers should pass the result of an isatty check.
func getTokenSourceWithReauth(
	ctx context.Context,
	mgr tokenReauthorizer,
	email string,
	interactive bool,
) (oauth2.TokenSource, error) {
	tokenSource, err := mgr.TokenSource(ctx, email)
	if err == nil {
		return tokenSource, nil
	}

	// No token at all — user needs to run add-account
	if !mgr.HasToken(email) {
		return nil, fmt.Errorf("get token source: %w (run 'add-account %s' first)", err, email)
	}

	// Token exists but failed — only auto-reauth for auth-invalid errors
	if !isAuthInvalidError(err) {
		return nil, fmt.Errorf("get token source for %s: %w", email, err)
	}

	// Non-interactive session cannot open a browser for reauth
	if !interactive {
		return nil, fmt.Errorf(
			"token for %s is expired or revoked, but cannot re-authorize "+
				"in a non-interactive session (run this command from an "+
				"interactive terminal to re-authorize automatically)",
			email,
		)
	}

	fmt.Printf("Token for %s is expired or revoked. Re-authorizing...\n", email)

	// Use manual flow (no browser auto-launch) so the user sees which
	// account needs authorization and can select the correct one.
	// AuthorizeManual validates the token and atomically saves it,
	// so the old token is only overwritten after validation succeeds.
	if authErr := mgr.AuthorizeManual(ctx, email); authErr != nil {
		var mismatch *oauth.TokenMismatchError
		if errors.As(authErr, &mismatch) {
			return nil, fmt.Errorf(
				"re-authorize %s: %w\n"+
					"If this account uses an alias, remove "+
					"and re-add with the primary address:\n"+
					"  msgvault remove-account %s --type gmail\n"+
					"  msgvault add-account %s",
				email, authErr,
				mismatch.Expected, mismatch.Actual,
			)
		}
		return nil, fmt.Errorf("re-authorize %s: %w", email, authErr)
	}

	// Retry with the new token
	tokenSource, err = mgr.TokenSource(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("get token source after re-authorization: %w", err)
	}

	return tokenSource, nil
}

// oauthManagerCache returns a resolver function that lazily creates and
// caches oauth.Manager instances keyed by app name. The cache is safe
// for concurrent use (serve runs scheduled syncs in goroutines).
func oauthManagerCache() func(appName string) (*oauth.Manager, error) {
	var mu sync.Mutex
	managers := map[string]*oauth.Manager{}
	return func(appName string) (*oauth.Manager, error) {
		mu.Lock()
		defer mu.Unlock()
		if mgr, ok := managers[appName]; ok {
			return mgr, nil
		}
		secretsPath, err := cfg.OAuth.ClientSecretsFor(appName)
		if err != nil {
			return nil, err
		}
		mgr, err := oauth.NewManager(secretsPath, cfg.TokensDir(), logger)
		if err != nil {
			return nil, wrapOAuthError(fmt.Errorf("create oauth manager: %w", err))
		}
		managers[appName] = mgr
		return mgr, nil
	}
}

// sourceOAuthApp extracts the oauth app name from a Source, returning ""
// for the default app.
func sourceOAuthApp(src *store.Source) string {
	if src != nil && src.OAuthApp.Valid {
		return src.OAuthApp.String
	}
	return ""
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ~/.msgvault/config.toml)")
	rootCmd.PersistentFlags().StringVar(&homeDir, "home", "", "home directory (overrides MSGVAULT_HOME)")
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "verbose output (implies --log-level=debug)")
	rootCmd.PersistentFlags().BoolVar(&useLocal, "local", false, "force local database (override remote config)")
	rootCmd.PersistentFlags().StringVar(&logFile, "log-file", "",
		"override log file path (default: <data dir>/logs/msgvault-YYYY-MM-DD.log)")
	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "",
		"log level: debug, info, warn, error (default: info)")
	rootCmd.PersistentFlags().BoolVar(&noLogFile, "no-log-file", false,
		"disable the log file for this run (stderr output stays on)")
	rootCmd.PersistentFlags().BoolVar(&logSQL, "log-sql", false,
		"log every SQL query at info level (verbose; for debugging)")
	rootCmd.PersistentFlags().Int64Var(&logSQLSlow, "log-sql-slow-ms", 0,
		"threshold in ms above which a SQL query is logged as slow "+
			"(default 100; 0 uses the default)")
}
