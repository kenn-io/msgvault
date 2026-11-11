package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/testutil"
	extOAuth2 "golang.org/x/oauth2"
)

func TestSyncUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/sync", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("email"), "email query")
		assert.ElementsMatch([]string{"INBOX", "Archive"}, r.URL.Query()["folder"], "folder query")
		assert.Equal([]string{"Trash"}, r.URL.Query()["skip-folder"], "skip-folder query")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Starting incremental sync for alice@example.com\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"sync warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	testCtx := configureRemoteSyncTest(t, server.URL)
	resetSyncFullFlagsForTest(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: syncIncrementalCmd.Use, Args: syncIncrementalCmd.Args, RunE: syncIncrementalCmd.RunE}
	cmd.SetContext(testCtx)
	cmd.Flags().StringArrayVar(&syncFolders, "folder", []string{}, "IMAP folders to include")
	cmd.Flags().StringArrayVar(&syncSkipFolders, "skip-folder", []string{}, "IMAP folders to exclude")
	cmd.SetArgs([]string{
		"alice@example.com",
		"--folder", "INBOX",
		"--folder", "Archive",
		"--skip-folder", "Trash",
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "sync command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Starting incremental sync for alice@example.com\n", stdout.String())
	assert.Equal("sync warning\n", stderr.String())
}

func TestSyncFullInterruptionHint(t *testing.T) {
	for _, mode := range []string{"client", "child", "child-extra", "error"} {
		t.Run(mode, func(t *testing.T) {
			var cancel context.CancelFunc
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "client" {
					cancel()
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/x-ndjson")
				if mode == "child" || mode == "child-extra" {
					waitErr := helperProcessCommand(r.Context(), "interrupted").Run()
					err := classifyDaemonCLIWaitErr(waitErr, []string{"sync-full"}, func(stream, data string) error {
						return json.NewEncoder(w).Encode(map[string]string{"type": stream, "data": data})
					})
					assert.EqualError(t, err, cliSubprocessExitSentinel)
					message := err.Error()
					if mode == "child-extra" {
						message += "\ncache refresh failed"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"type": "error", "error": message})
				} else {
					_, _ = w.Write([]byte("{\"type\":\"error\",\"error\":\"provider failed\"}\n"))
				}
			}))
			t.Cleanup(server.Close)
			ctx := configureRemoteSyncTest(t, server.URL)
			ctx, cancel = context.WithCancel(ctx)
			defer cancel()
			resetSyncFullFlagsForTest(t)
			var stderr bytes.Buffer
			command := &cobra.Command{Use: syncFullCmd.Use, Args: syncFullCmd.Args, RunE: syncFullCmd.RunE}
			command.SetContext(ctx)
			command.SetErr(&stderr)
			command.SetArgs([]string{"reader@example.test"})
			err := command.Execute()
			switch mode {
			case "client":
				require.ErrorIs(t, err, context.Canceled)
			case "child", "child-extra":
				require.ErrorIs(t, err, ErrSyncInterrupted)
			default:
				require.Error(t, err)
			}
			if mode == "error" {
				assert.NotContains(t, stderr.String(), "To run it again")
			} else {
				assert.Equal(t, 1, strings.Count(stderr.String(), "Sync interrupted."))
				assert.Contains(t, stderr.String(), "reader@example.test")
				assert.NotContains(t, stderr.String(), cliSubprocessExitSentinel)
				if mode == "child-extra" {
					assert.Equal(t, 1, strings.Count(stderr.String(), "cache refresh failed"))
				}
			}
		})
	}
}

func TestSyncResumeCommandPreservesExecutableWhitespace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	fixture := testutil.SecretCommand(t, "argv")[0]
	data, err := os.ReadFile(fixture)
	require.NoError(t, err)
	executable := filepath.Join(t.TempDir(), "sync executable ")
	require.NoError(t, os.WriteFile(executable, data, 0o700))
	line, err := formatCommand(executable, []string{"argv", "--query=subject:'a b' $HOME;echo"}, "linux")
	require.NoError(t, err)
	output, err := exec.CommandContext(t.Context(), "sh", "-c", line).CombinedOutput()
	require.NoError(t, err, "%s", output)
	var got []string
	require.NoError(t, json.Unmarshal(output, &got))
	assert.Equal(t, []string{"--query=subject:'a b' $HOME;echo"}, got)
}

func TestSyncResumeCommandNativeArguments(t *testing.T) {
	resetSyncFullFlagsForTest(t)
	oldResolver := daemonCLIExecutableResolver
	daemonCLIExecutableResolver = func() (string, error) { return "msgvault", nil }
	t.Cleanup(func() { daemonCLIExecutableResolver = oldResolver })
	root := &cobra.Command{Use: "msgvault"}
	root.PersistentFlags().String("config", "", "")
	root.PersistentFlags().String("home", "", "")
	root.PersistentFlags().Bool("local", true, "")
	root.PersistentFlags().String("log-file", "", "")
	root.PersistentFlags().Bool("verbose", true, "")
	command := &cobra.Command{Use: "sync-full"}
	root.AddCommand(command)
	command.Flags().String("query", "", "")
	command.Flags().StringArray("folder", nil, "")
	command.Flags().Bool("noresume", false, "")
	command.Flags().Bool("build-cache", true, "")
	command.Flags().String("sync-operation-id", "", "")
	query := `from:"a b" $HOME;echo`
	require.NoError(t, command.ParseFlags([]string{"--config", "config file", "--home", "home dir", "--local=false", "--log-file=", "--verbose=false", "--query", query, "--folder", "A B", "--folder", "C", "--noresume", "--build-cache=false", "--sync-operation-id", "internal", "--", "reader@example.test"}))
	line, err := syncResumeCommand(command, command.Flags().Args())
	require.NoError(t, err)
	shell := "sh"
	if runtime.GOOS == "windows" {
		shell = "pwsh"
		line = strings.TrimPrefix(line, "& ")
	}
	got := headlessNativeArgs(t, shell, testutil.SecretCommand(t, "argv")[0], line)
	assert.Equal(t, []string{"--config=config file", "--home=home dir", "--local=false", "--log-file=", "--verbose=false", "sync-full", "--build-cache=false", "--folder=A B", "--folder=C", "--noresume", "--query=" + query, "--", "reader@example.test"}, got)
}

func TestSyncFullHintCancellationPhases(t *testing.T) {
	for _, phase := range []string{"connection", "preflight", "completed"} {
		t.Run(phase, func(t *testing.T) {
			var cancel context.CancelFunc
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { cancel() }))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithCancel(configureRemoteSyncTest(t, server.URL))
			defer cancel()
			var stderr bytes.Buffer
			command := &cobra.Command{}
			command.SetContext(ctx)
			command.SetErr(&stderr)
			err := runSyncFullWithHint(command, "msgvault sync-full", func() (bool, error) {
				if phase == "preflight" {
					p := preflightConfig{Local: true, Interactive: true, OAuthConfigured: true, ListGmailAccounts: func(ctx context.Context) ([]preflightAccount, error) {
						cancel()
						return nil, ctx.Err()
					}}
					return false, preflightReauth(ctx, p, "", 0)
				}
				if phase == "connection" {
					return runSyncHTTPWithOutcome(command, daemonclient.CLISyncRequest{Full: true})
				}
				cancel()
				return false, nil
			})
			if phase == "completed" {
				require.NoError(t, err)
				assert.Empty(t, stderr.String())
			} else {
				require.ErrorIs(t, err, context.Canceled)
				assert.Equal(t, 1, strings.Count(stderr.String(), "Sync interrupted."))
			}
		})
	}
}

func TestSyncResumeCommandPreservesControlCharacters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture")
	}
	fixture := testutil.SecretCommand(t, "argv")[0]
	oldResolver := daemonCLIExecutableResolver
	daemonCLIExecutableResolver = func() (string, error) { return fixture, nil }
	t.Cleanup(func() { daemonCLIExecutableResolver = oldResolver })
	root := &cobra.Command{Use: "root"}
	mode := &cobra.Command{Use: "argv"}
	command := &cobra.Command{Use: "sync-full"}
	root.AddCommand(mode)
	mode.AddCommand(command)
	command.Flags().String("query", "", "")
	query := "subject:one\ntwo\x1b[31m\tend\n"
	require.NoError(t, command.ParseFlags([]string{"--query", query}))
	line, err := syncResumeCommand(command, nil)
	require.NoError(t, err)
	var out bytes.Buffer
	require.NoError(t, printSyncResumeCommand(&out, line, "linux"))
	assert.NotContains(t, out.String(), "\x1b")
	assert.NotContains(t, out.String(), "\t")
	assert.Equal(t, 2, strings.Count(out.String(), "\n"))
	line = strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[1])
	data, err := exec.CommandContext(t.Context(), "sh", "-c", line).CombinedOutput()
	require.NoError(t, err, "%s", data)
	var got []string
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, []string{"sync-full", "--query=" + query}, got)
}

func TestSyncFullHintWriteFailurePreservesInterruption(t *testing.T) {
	want := errors.New("write failed")
	command := &cobra.Command{}
	command.SetErr(evalRerankFailingWriter{err: want})
	err := runSyncFullWithHint(command, "msgvault sync-full", func() (bool, error) { return true, nil })
	assert.ErrorIs(t, err, ErrSyncInterrupted)
	assert.ErrorIs(t, err, want)
}

func TestSyncResumeCommandPowerShell(t *testing.T) {
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("PowerShell unavailable")
	}
	fixture := testutil.SecretCommand(t, "argv")[0]
	if runtime.GOOS != "windows" {
		data, err := os.ReadFile(fixture)
		require.NoError(t, err)
		fixture = filepath.Join(t.TempDir(), "sync executable ")
		require.NoError(t, os.WriteFile(fixture, data, 0o700))
	}
	for _, query := range []string{`--query=from:'a b' $HOME;echo`, "--query=😀\n\x1b[31m\tend\n"} {
		want := []string{"argv", "sync-full", query, "--folder=A B", "--folder=C", "--", "reader@example.test"}
		line, err := formatCommand(fixture, want, "windows")
		require.NoError(t, err)
		var out bytes.Buffer
		require.NoError(t, printSyncResumeCommand(&out, line, "windows"))
		assert.Contains(t, out.String(), "In PowerShell 7.3+, run:")
		assert.NotContains(t, out.String(), "\x1b")
		assert.NotContains(t, out.String(), "\t")
		assert.Equal(t, 2, strings.Count(out.String(), "\n"))
		line = strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[1])
		data, err := exec.CommandContext(t.Context(), pwsh, "-NoProfile", "-NonInteractive", "-Command", line).CombinedOutput()
		require.NoError(t, err, "%s", data)
		var got []string
		require.NoError(t, json.Unmarshal(data, &got))
		assert.Equal(t, want[1:], got)
	}
}

func TestSyncSourceIDUsesConfiguredRemoteHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"2.4.0"}`))
			return
		}
		assert.Equal(t, "42", r.URL.Query().Get("source_id"))
		assert.Empty(t, r.URL.Query().Get("email"))
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)
	testCtx := configureRemoteSyncTest(t, server.URL)
	resetSyncFullFlagsForTest(t)

	cmd := &cobra.Command{Use: syncIncrementalCmd.Use, Args: syncIncrementalCmd.Args, RunE: syncIncrementalCmd.RunE}
	cmd.SetContext(testCtx)
	cmd.Flags().Int64("source-id", 0, "Exact source ID")
	cmd.SetArgs([]string{"--source-id", "42"})
	require.NoError(t, cmd.Execute())
}

func TestSyncSourceIDSelectorValidation(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "explicit zero", args: []string{"--source-id", "0"}, want: "positive"},
		{name: "token plus ID", args: []string{"alice@example.test", "--source-id", "42"}, want: "mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: syncIncrementalCmd.Use, Args: syncIncrementalCmd.Args, RunE: syncIncrementalCmd.RunE}
			cmd.Flags().Int64("source-id", 0, "Exact source ID")
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
		})
	}
}

func TestSyncFullUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/sync-full", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("email"), "email query")
		assert.Equal("from:bob@example.com", r.URL.Query().Get("query"), "query flag")
		assert.Equal("2024-01-01", r.URL.Query().Get("after"), "after flag")
		assert.Equal("2024-12-31", r.URL.Query().Get("before"), "before flag")
		assert.Equal("25", r.URL.Query().Get("limit"), "limit flag")
		assert.Equal("true", r.URL.Query().Get("noresume"), "noresume flag")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Starting full sync for alice@example.com\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"sync-full warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	testCtx := configureRemoteSyncTest(t, server.URL)
	resetSyncFullFlagsForTest(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: syncFullCmd.Use, Args: syncFullCmd.Args, RunE: syncFullCmd.RunE}
	cmd.SetContext(testCtx)
	cmd.Flags().StringVar(&syncQuery, "query", "", "Gmail search query")
	cmd.Flags().BoolVar(&syncNoResume, "noresume", false, "Force fresh sync")
	cmd.Flags().StringVar(&syncBefore, "before", "", "Only messages before this date")
	cmd.Flags().StringVar(&syncAfter, "after", "", "Only messages after this date")
	cmd.Flags().IntVar(&syncLimit, "limit", 0, "Limit number of messages")
	cmd.SetArgs([]string{
		"alice@example.com",
		"--query", "from:bob@example.com",
		"--after", "2024-01-01",
		"--before", "2024-12-31",
		"--limit", "25",
		"--noresume",
	})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "sync-full command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Starting full sync for alice@example.com\n", stdout.String())
	assert.Equal("sync-full warning\n", stderr.String())
}

func configureRemoteSyncTest(t *testing.T, remoteURL string) context.Context {
	t.Helper()
	logger := testLoggerValue()

	dataDir := t.TempDir()
	testCtx := withStoreResolverConfig(t, &config.Config{
		HomeDir: dataDir,
		Data:    config.DataConfig{DataDir: dataDir},
		Remote: config.RemoteConfig{
			URL:           remoteURL,
			AllowInsecure: true,
		},
	})
	oldLogger := logger
	logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	t.Cleanup(func() { logger = oldLogger })
	return testCtx
}

// newValidPreflightManager returns a mock whose token source always succeeds.
func newValidPreflightManager() *mockReauthorizer {
	return &mockReauthorizer{
		hasTokenVal: true,
		tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
			return fakeTokenSource{}, nil
		},
	}
}

// newExpiredPreflightManager returns a mock whose token source reports
// invalid_grant, the auth-invalid signal that warrants reauth.
func newExpiredPreflightManager() *mockReauthorizer {
	return &mockReauthorizer{
		hasTokenVal: true,
		tokenSourceFn: func(_ context.Context, _ string) (extOAuth2.TokenSource, error) {
			return nil, &extOAuth2.RetrieveError{ErrorCode: "invalid_grant"}
		},
	}
}

// basePreflight builds a local+interactive+configured preflight over the given
// Gmail accounts, wiring ManagerFor to the supplied per-app mock map.
func basePreflight(managers map[string]*mockReauthorizer, accounts ...preflightAccount) preflightConfig {
	return preflightConfig{
		Local:             true,
		Interactive:       true,
		OAuthConfigured:   true,
		Out:               &bytes.Buffer{},
		ServiceAccountKey: func(string) string { return "" },
		ListGmailAccounts: func(context.Context) ([]preflightAccount, error) {
			return accounts, nil
		},
		ManagerFor: func(ctx context.Context, appName string) (preflightReauthManager, error) {
			return managers[appName], nil
		},
	}
}

func TestPreflightReauth(t *testing.T) {
	acct := preflightAccount{Email: "alice@example.com"}

	tests := []struct {
		name          string
		manager       *mockReauthorizer
		config        func(map[string]*mockReauthorizer) preflightConfig
		wantErr       bool
		wantAuthorize int
	}{
		{
			name:          "valid token → no reauth",
			manager:       newValidPreflightManager(),
			config:        func(m map[string]*mockReauthorizer) preflightConfig { return basePreflight(m, acct) },
			wantAuthorize: 0,
		},
		{
			name:          "expired token, interactive+local → authorize",
			manager:       newExpiredPreflightManager(),
			config:        func(m map[string]*mockReauthorizer) preflightConfig { return basePreflight(m, acct) },
			wantAuthorize: 1,
		},
		{
			name:          "no token → skip (no enroll)",
			manager:       &mockReauthorizer{tokenSourceFn: func(context.Context, string) (extOAuth2.TokenSource, error) { return nil, os.ErrNotExist }},
			config:        func(m map[string]*mockReauthorizer) preflightConfig { return basePreflight(m, acct) },
			wantAuthorize: 0,
		},
		{
			name:    "non-interactive → skip",
			manager: newExpiredPreflightManager(),
			config: func(m map[string]*mockReauthorizer) preflightConfig {
				c := basePreflight(m, acct)
				c.Interactive = false
				return c
			},
			wantAuthorize: 0,
		},
		{
			name:    "remote daemon → skip",
			manager: newExpiredPreflightManager(),
			config: func(m map[string]*mockReauthorizer) preflightConfig {
				c := basePreflight(m, acct)
				c.Local = false
				return c
			},
			wantAuthorize: 0,
		},
		{
			name:    "oauth not configured → skip",
			manager: newExpiredPreflightManager(),
			config: func(m map[string]*mockReauthorizer) preflightConfig {
				c := basePreflight(m, acct)
				c.OAuthConfigured = false
				return c
			},
			wantAuthorize: 0,
		},
		{
			name:    "service account → skip",
			manager: newExpiredPreflightManager(),
			config: func(m map[string]*mockReauthorizer) preflightConfig {
				c := basePreflight(m, acct)
				c.ServiceAccountKey = func(string) string { return "/keys/sa.json" }
				return c
			},
			wantAuthorize: 0,
		},
		{
			name: "authorize failure → error",
			manager: func() *mockReauthorizer {
				m := newExpiredPreflightManager()
				m.authorizeFn = func(_ context.Context, _ string) error {
					return errors.New("browser flow failed")
				}
				return m
			}(),
			config:        func(m map[string]*mockReauthorizer) preflightConfig { return basePreflight(m, acct) },
			wantErr:       true,
			wantAuthorize: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			managers := map[string]*mockReauthorizer{"": tt.manager}
			err := preflightReauth(t.Context(), tt.config(managers), "", 0)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantAuthorize, tt.manager.authorizeCount, "authorize count")
		})
	}
}

// TestPreflightReauth_SpecificEmail verifies only the requested Gmail account
// is probed, leaving other accounts untouched.
func TestPreflightReauth_SpecificEmail(t *testing.T) {
	alice := newExpiredPreflightManager()
	bob := newExpiredPreflightManager()
	managers := map[string]*mockReauthorizer{"alice-app": alice, "bob-app": bob}
	c := basePreflight(managers,
		preflightAccount{Email: "alice@example.com", OAuthApp: "alice-app"},
		preflightAccount{Email: "bob@example.com", OAuthApp: "bob-app"},
	)

	require.NoError(t, preflightReauth(t.Context(), c, "bob@example.com", 0))
	assert.Equal(t, 0, alice.authorizeCount, "alice must not be re-authorized")
	assert.Equal(t, 1, bob.authorizeCount, "bob must be re-authorized")
}

func TestPreflightReauth_SourceIDTargetsOneDuplicateAccount(t *testing.T) {
	first := newExpiredPreflightManager()
	second := newExpiredPreflightManager()
	managers := map[string]*mockReauthorizer{"first-app": first, "second-app": second}
	c := basePreflight(managers,
		preflightAccount{ID: 41, Email: "shared@example.com", OAuthApp: "first-app"},
		preflightAccount{ID: 42, Email: "shared@example.com", OAuthApp: "second-app"},
	)

	require.NoError(t, preflightReauth(t.Context(), c, "", 42))
	assert.Equal(t, 0, first.authorizeCount)
	assert.Equal(t, 1, second.authorizeCount)
}

// TestPreflightReauth_IMAPRequestSkips verifies a request for a non-Gmail
// account (absent from the Gmail account list) triggers no reauth.
func TestPreflightReauth_IMAPRequestSkips(t *testing.T) {
	gmailMgr := newExpiredPreflightManager()
	managers := map[string]*mockReauthorizer{"": gmailMgr}
	c := basePreflight(managers, preflightAccount{Email: "alice@example.com"})

	require.NoError(t, preflightReauth(t.Context(), c, "imap-user@example.com", 0))
	assert.Equal(t, 0, gmailMgr.authorizeCount, "no reauth for non-Gmail request")
}

// TestPreflightReauth_DisplayName verifies a request that matches a Gmail
// account's display name (not its email) still re-authorizes it. sync/sync-full
// accept a display name via GetSourcesByIdentifierOrDisplayName.
func TestPreflightReauth_DisplayName(t *testing.T) {
	work := newExpiredPreflightManager()
	personal := newExpiredPreflightManager()
	managers := map[string]*mockReauthorizer{"work-app": work, "personal-app": personal}
	c := basePreflight(managers,
		preflightAccount{Email: "alice@example.com", DisplayName: "Work Gmail", OAuthApp: "work-app"},
		preflightAccount{Email: "bob@example.com", DisplayName: "Personal", OAuthApp: "personal-app"},
	)

	require.NoError(t, preflightReauth(t.Context(), c, "Work Gmail", 0))
	assert.Equal(t, 1, work.authorizeCount, "display-name match must be re-authorized")
	assert.Equal(t, 0, personal.authorizeCount, "non-matching account untouched")
}

// TestPreflightReauth_NoMatch verifies a request that matches neither an email
// nor a display name re-authorizes nothing.
func TestPreflightReauth_NoMatch(t *testing.T) {
	mgr := newExpiredPreflightManager()
	managers := map[string]*mockReauthorizer{"": mgr}
	c := basePreflight(managers,
		preflightAccount{Email: "alice@example.com", DisplayName: "Work Gmail"},
	)

	require.NoError(t, preflightReauth(t.Context(), c, "Nonexistent", 0))
	assert.Equal(t, 0, mgr.authorizeCount, "no reauth when nothing matches")
}

func resetSyncFullFlagsForTest(t *testing.T) {
	t.Helper()

	oldQuery := syncQuery
	oldNoResume := syncNoResume
	oldBefore := syncBefore
	oldAfter := syncAfter
	oldLimit := syncLimit
	oldFolders := syncFolders
	oldSkipFolders := syncSkipFolders
	syncQuery = ""
	syncNoResume = false
	syncBefore = ""
	syncAfter = ""
	syncLimit = 0
	syncFolders = []string{}
	syncSkipFolders = []string{}
	t.Cleanup(func() {
		syncQuery = oldQuery
		syncNoResume = oldNoResume
		syncBefore = oldBefore
		syncAfter = oldAfter
		syncLimit = oldLimit
		syncFolders = oldFolders
		syncSkipFolders = oldSkipFolders
	})
}
