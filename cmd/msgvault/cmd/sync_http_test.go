package cmd

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	extOAuth2 "golang.org/x/oauth2"
)

func TestSyncUsesConfiguredRemoteHTTPAndPreservesOutput(t *testing.T) {
	assert := assert.New(t)

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/cli/sync", r.URL.Path, "path")
		assert.Equal("alice@example.com", r.URL.Query().Get("email"), "email query")
		requests.Add(1)

		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"type":"stdout","data":"Starting incremental sync for alice@example.com\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"stderr","data":"sync warning\n"}` + "\n"))
		_, _ = w.Write([]byte(`{"type":"complete"}` + "\n"))
	}))
	t.Cleanup(server.Close)

	configureRemoteSyncTest(t, server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: syncIncrementalCmd.Use, Args: syncIncrementalCmd.Args, RunE: syncIncrementalCmd.RunE}
	cmd.SetArgs([]string{"alice@example.com"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()
	require.NoError(t, err, "sync command")
	assert.Equal(int32(1), requests.Load(), "HTTP requests")
	assert.Equal("Starting incremental sync for alice@example.com\n", stdout.String())
	assert.Equal("sync warning\n", stderr.String())
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

	configureRemoteSyncTest(t, server.URL)
	resetSyncFullFlagsForTest(t)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := &cobra.Command{Use: syncFullCmd.Use, Args: syncFullCmd.Args, RunE: syncFullCmd.RunE}
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

func configureRemoteSyncTest(t *testing.T, remoteURL string) {
	t.Helper()

	dataDir := t.TempDir()
	withStoreResolverConfig(t, &config.Config{
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
		ManagerFor: func(appName string) (preflightReauthManager, error) {
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
			manager:       &mockReauthorizer{hasTokenVal: false},
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
			err := preflightReauth(context.Background(), tt.config(managers), "")
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

	require.NoError(t, preflightReauth(context.Background(), c, "bob@example.com"))
	assert.Equal(t, 0, alice.authorizeCount, "alice must not be re-authorized")
	assert.Equal(t, 1, bob.authorizeCount, "bob must be re-authorized")
}

// TestPreflightReauth_IMAPRequestSkips verifies a request for a non-Gmail
// account (absent from the Gmail account list) triggers no reauth.
func TestPreflightReauth_IMAPRequestSkips(t *testing.T) {
	gmailMgr := newExpiredPreflightManager()
	managers := map[string]*mockReauthorizer{"": gmailMgr}
	c := basePreflight(managers, preflightAccount{Email: "alice@example.com"})

	require.NoError(t, preflightReauth(context.Background(), c, "imap-user@example.com"))
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

	require.NoError(t, preflightReauth(context.Background(), c, "Work Gmail"))
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

	require.NoError(t, preflightReauth(context.Background(), c, "Nonexistent"))
	assert.Equal(t, 0, mgr.authorizeCount, "no reauth when nothing matches")
}

func resetSyncFullFlagsForTest(t *testing.T) {
	t.Helper()

	oldQuery := syncQuery
	oldNoResume := syncNoResume
	oldBefore := syncBefore
	oldAfter := syncAfter
	oldLimit := syncLimit
	syncQuery = ""
	syncNoResume = false
	syncBefore = ""
	syncAfter = ""
	syncLimit = 0
	t.Cleanup(func() {
		syncQuery = oldQuery
		syncNoResume = oldNoResume
		syncBefore = oldBefore
		syncAfter = oldAfter
		syncLimit = oldLimit
	})
}
