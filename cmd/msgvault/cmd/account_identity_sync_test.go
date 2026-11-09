package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	oauth2 "golang.org/x/oauth2"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAddServiceAccountDefaultIdentityScheduledSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	saveAddAccountFlags(t)
	// Only Google's token and profile responses are simulated. Registration,
	// service-account token creation, scheduled sync, and database writes are real.
	// Scheduled Gmail sync constructs its HTTP client with context.Background(),
	// so oauth2.HTTPClient in the invocation context cannot intercept its requests.
	// Keep this test nonparallel and restore the transport after it runs.
	savedTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = savedTransport })
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.Method + " " + req.URL.String() {
		case "POST https://token.example.com/oauth2":
			body = `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`
		case "GET https://gmail.googleapis.com/gmail/v1/users/me/profile":
			body = `{"emailAddress":"user@example.com","historyId":"100"}`
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	home := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(err)
	keyJSON, err := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "service@example.com",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		"token_uri":    "https://token.example.com/oauth2",
	})
	require.NoError(err)
	keyPath := filepath.Join(home, "service-account.json")
	require.NoError(os.WriteFile(keyPath, keyJSON, 0600))
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
		OAuth: config.OAuthConfig{ServiceAccountKey: keyPath}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	for _, flag := range []string{"--no-default-identity", "", "--no-default-identity=false"} {
		optOut := flag != "--no-default-identity=false"
		cmd := &cobra.Command{Use: addAccountUse, RunE: runAddAccountLocal}
		registerAddAccountFlags(cmd)
		args := []string{"user@example.com"}
		if flag != "" {
			args = append(args, flag)
		}
		cmd.SetArgs(args)
		capture := captureStdout(t)
		err := cmd.ExecuteContext(ctx)
		output := capture()
		require.NoError(err)
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		src, err := findGmailSource(st, "user@example.com")
		require.NoError(err)
		if flag == "--no-default-identity" {
			assertRegistrationSyncCommand(t, output, src.ID, "sync-full")
		}
		// Seed the cursor of an already-synced mailbox so this scheduled run
		// completes with no new messages.
		require.NoError(st.UpdateSourceSyncCursor(src.ID, "100"))
		summary, err := runScheduledGmailSync(ctx, "user@example.com", src, st, nil, invocationFromContext(ctx))
		require.NoError(err)
		assert.Zero(summary.Errors)
		ids, err := st.ListAccountIdentities(src.ID)
		require.NoError(err)
		if optOut {
			assert.Empty(ids, "scheduled sync must preserve the service-account opt-out")
		} else {
			require.Len(ids, 1)
			assert.Equal("user@example.com", ids[0].Address)
			if flag == "--no-default-identity=false" {
				removed, err := st.RemoveAccountIdentity(src.ID, "user@example.com")
				require.NoError(err)
				require.EqualValues(1, removed)
				summary, err = runScheduledGmailSync(ctx, "user@example.com", src, st, nil, invocationFromContext(ctx))
				require.NoError(err)
				assert.Zero(summary.Errors)
				ids, err = st.ListAccountIdentities(src.ID)
				require.NoError(err)
				assert.Empty(ids, "scheduled Gmail sync must not restore an explicitly removed last identity")
			}
		}
	}
}

func TestScheduledSyncResolvesGmailAliasToCanonicalIdentifier(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	savedTransport := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = savedTransport })
	profileEmail := "reader@example.test"
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		var body string
		switch req.Method + " " + req.URL.String() {
		case "POST https://token.example.com/oauth2":
			body = `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":3600}`
		case "GET https://gmail.googleapis.com/gmail/v1/users/me/profile":
			body = fmt.Sprintf(`{"emailAddress":%q,"historyId":"100"}`, profileEmail)
		default:
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
			Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	home := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(err)
	keyJSON, err := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "service@example.test",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		"token_uri":   "https://token.example.com/oauth2",
	})
	require.NoError(err)
	keyPath := filepath.Join(home, "service-account.json")
	require.NoError(os.WriteFile(keyPath, keyJSON, 0600))
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
		OAuth: config.OAuthConfig{ServiceAccountKey: keyPath}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	st := testutil.NewSQLiteTestStoreAt(t, cfg.DatabaseDSN())

	canonicalEmail := "reader@example.test"
	source, err := st.GetOrCreateSource(sourceTypeGmail, canonicalEmail)
	require.NoError(err)
	selector := "primary-mailbox"
	_, err = st.UpdateSourceSettingsContext(ctx, source.ID, store.SourceSettingsUpdate{Alias: &selector})
	require.NoError(err)
	require.NoError(st.UpdateSourceSyncCursor(source.ID, "100"))

	originalRebuild := rebuildCacheAfterScheduledSourceRun
	var rebuiltSelectors []string
	rebuildCacheAfterScheduledSourceRun = func(_ context.Context, identifier string) error {
		rebuiltSelectors = append(rebuiltSelectors, identifier)
		return nil
	}
	t.Cleanup(func() { rebuildCacheAfterScheduledSourceRun = originalRebuild })

	assert.Equal(canonicalEmail, scheduledGmailAccountIdentifier(selector, source))
	assert.Equal(selector, scheduledGmailAccountIdentifier(selector, nil),
		"missing sources retain the token-first selector")
	require.NoError(runScheduledSync(ctx, selector, st, nil, invocationFromContext(ctx)))
	assert.Equal([]string{selector}, rebuiltSelectors, "cache rebuild logging keeps the supplied selector")
	resolved, err := st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeGmail, canonicalEmail)
	require.NoError(err)
	assert.Equal(source.ID, resolved.ID)
	_, err = st.GetSourceByTypeAndIdentifier(t.Context(), sourceTypeGmail, selector)
	assert.ErrorIs(err, store.ErrSourceNotFound, "scheduled sync must not create a source under the alias")
}

func TestAddIMAPDefaultIdentityScheduledSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	savedHost, savedPort, savedUsername := imapHost, imapPort, imapUsername
	savedNoTLS, savedSTARTTLS, savedNoDefault := imapNoTLS, imapSTARTTLS, noDefaultIdentityAddImap
	t.Cleanup(func() {
		imapHost, imapPort, imapUsername = savedHost, savedPort, savedUsername
		imapNoTLS, imapSTARTTLS, noDefaultIdentityAddImap = savedNoTLS, savedSTARTTLS, savedNoDefault
	})
	t.Setenv(daemonCLISubprocessEnv, strconv.Itoa(os.Getppid()))
	t.Setenv("MSGVAULT_IMAP_PASSWORD", testutil.IMAPTestPassword)
	addr, _ := testutil.StartIMAPMemServerWithSpecialUse(t, map[string]int{"INBOX": 1}, nil)
	host, port, err := net.SplitHostPort(addr)
	require.NoError(err)
	home := t.TempDir()
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}}
	ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})

	// Re-registering preserves the choice unless the flag is explicit.
	for _, flag := range []string{"--no-default-identity", "", "--no-default-identity=false"} {
		optOut := flag != "--no-default-identity=false"
		cmd := newAddIMAPCmd()
		args := []string{"--host", host, "--port", port, "--username", testutil.IMAPTestUsername, "--no-tls"}
		if flag != "" {
			args = append(args, flag)
		}
		cmd.SetArgs(args)
		capture := captureStdout(t)
		err := cmd.ExecuteContext(ctx)
		output := capture()
		require.NoError(err)
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		sources, err := st.ListSources(sourceTypeIMAP)
		require.NoError(err)
		require.Len(sources, 1)
		src := sources[0]
		if flag == "--no-default-identity" {
			assertRegistrationSyncCommand(t, output, src.ID, "sync-full")
		}
		// Re-enabling defaults creates the identity before this explicit removal.
		// The next scheduled sync must preserve the removal.
		if !optOut {
			removed, err := st.RemoveAccountIdentity(src.ID, testutil.IMAPTestUsername)
			require.NoError(err)
			require.EqualValues(1, removed)
		}
		summary, err := runScheduledIMAPSync(ctx, src, st, invocationFromContext(ctx))
		require.NoError(err)
		assert.Zero(summary.Errors)
		ids, err := st.ListAccountIdentities(src.ID)
		require.NoError(err)
		assert.Empty(ids, "scheduled sync must preserve the saved opt-out or last-identity removal")
	}
}

func TestAddMicrosoftDefaultIdentityOptOut(t *testing.T) {
	savedGraph := o365Graph
	savedO365, savedTeams := noDefaultIdentityAddO365, noDefaultIdentityAddTeams
	savedO365Headless, savedTeamsHeadless := o365Headless, teamsHeadless
	savedO365Tenant, savedTeamsTenant := o365TenantID, teamsTenantID
	t.Cleanup(func() {
		o365Graph = savedGraph
		noDefaultIdentityAddO365, noDefaultIdentityAddTeams = savedO365, savedTeams
		o365Headless, teamsHeadless = savedO365Headless, savedTeamsHeadless
		o365TenantID, teamsTenantID = savedO365Tenant, savedTeamsTenant
	})
	for _, tc := range []struct {
		name       string
		newCommand func() *cobra.Command
		args       []string
	}{
		{"o365", newAddO365LocalCmd, nil},
		{"graph", newAddO365LocalCmd, []string{"--graph"}},
		{"teams", newAddTeamsLocalCmd, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			home := t.TempDir()
			cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home},
				Microsoft: config.MicrosoftConfig{ClientID: "synthetic-client"}}
			ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
			const email = "user@example.com"
			mgr := microsoft.NewManager(cfg.Microsoft.ClientID, "common", cfg.Microsoft.EffectiveRedirectURI(), cfg.TokensDir(), testDiscardLogger())
			require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
			require.NoError(os.WriteFile(mgr.TokenPath(email), []byte(`{"access_token":"synthetic-token"}`), 0600))
			for _, flag := range []string{"--no-default-identity", "", "--no-default-identity=false"} {
				cmd := tc.newCommand()
				args := append([]string{email, "--" + oauthPreflightedFlag}, tc.args...)
				if flag != "" {
					args = append(args, flag)
				}
				cmd.SetArgs(args)
				capture := captureStdout(t)
				err := cmd.ExecuteContext(ctx)
				output := capture()
				require.NoError(err)
				st, err := store.Open(cfg.DatabaseDSN())
				require.NoError(err)
				t.Cleanup(func() { _ = st.Close() })
				sources, err := st.ListSources("")
				require.NoError(err)
				require.Len(sources, 1)
				if flag == "--no-default-identity" && tc.name != "teams" {
					verb := "sync-full"
					if tc.name == "graph" {
						verb = "sync"
					}
					assertRegistrationSyncCommand(t, output, sources[0].ID, verb)
				}
				if tc.name == "graph" {
					// Identity setup precedes token loading. With no Graph token,
					// the real scheduled path stops before making network requests.
					err := runScheduledMSMailSync(ctx, sources[0], st, invocationFromContext(ctx))
					require.ErrorContains(err, "no valid token")
				} else {
					confirmDefaultIdentity(t.Context(), io.Discard, st, sources[0].ID, email, email, "account-identifier", testDiscardLogger())
				}
				ids, err := st.ListAccountIdentities(sources[0].ID)
				require.NoError(err)
				if flag == "--no-default-identity=false" {
					require.Len(ids, 1, "explicit false restores the default")
					assert.Equal(email, ids[0].Address)
				} else {
					assert.Empty(ids, "default identity confirmation must honor the saved opt-out (flag %q)", flag)
				}
			}
		})
	}
}

func assertRegistrationSyncCommand(t *testing.T, output string, sourceID int64, verb string) {
	t.Helper()
	start := strings.Index(output, "msgvault "+verb+" ")
	require.NotEqual(t, -1, start, "registration must print its first-sync command")
	line, _, _ := strings.Cut(output[start:], "\n")
	assert.Equal(t, fmt.Sprintf("msgvault %s --source-id %d", verb, sourceID), line)
}

func testAddAccountNewAuthorizationPrintsExactSyncCommand(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses the Linux browser opener boundary")
	}
	require := require.New(t)
	saveAddAccountFlags(t)
	curl, err := exec.LookPath("curl")
	require.NoError(err, "curl is required for the browser opener boundary")
	home := t.TempDir()
	// The browser is an external boundary. Fetch its authorization URL against
	// the local provider so the real callback and token exchange complete.
	require.NoError(os.WriteFile(filepath.Join(home, "xdg-open"), []byte("#!/bin/sh\nexec "+curl+" -fsSL \"$1\"\n"), 0700))
	t.Setenv("PATH", home+string(os.PathListSeparator)+os.Getenv("PATH"))
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/authorize" {
			query := r.URL.Query()
			http.Redirect(w, r, query.Get("redirect_uri")+"?code=synthetic&state="+url.QueryEscape(query.Get("state")), http.StatusFound) //nolint:gosec // The local test provider follows the callback URL generated by the test client.
			return
		}
		assert.Equal(t, "/token", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.MarshalWrite(w, map[string]any{"access_token": "synthetic-token", "token_type": "Bearer", "expires_in": 3600, "scope": strings.Join(oauth.Scopes, " ")})
	}))
	defer provider.Close()
	secrets, err := json.Marshal(map[string]any{"installed": map[string]any{"client_id": "synthetic-client", "client_secret": "synthetic-secret", "auth_uri": provider.URL + "/authorize", "token_uri": provider.URL + "/token", "redirect_uris": []string{"http://localhost"}}})
	require.NoError(err)
	secretPath := filepath.Join(home, "client.json")
	require.NoError(os.WriteFile(secretPath, secrets, 0600))
	cfg := &config.Config{HomeDir: home, Data: config.DataConfig{DataDir: home}, OAuth: config.OAuthConfig{ClientSecrets: secretPath}}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "gmail.googleapis.com" {
			return (gmailProfileTransport{email: "reader@example.org", statusCode: http.StatusOK}).RoundTrip(r)
		}
		return http.DefaultTransport.RoundTrip(r)
	})})
	ctx = testInvocationContext(ctx, cfg, invocationOptions{})
	cmd := &cobra.Command{Use: addAccountUse, Args: validateAddAccountArgs, RunE: runAddAccountLocal}
	registerAddAccountFlags(cmd)
	cmd.SetArgs([]string{"--no-default-identity", "reader@example.org"})
	capture := captureStdout(t)
	err = cmd.ExecuteContext(ctx)
	output := capture()
	require.NoError(err)
	st, err := store.Open(cfg.DatabaseDSN())
	require.NoError(err)
	defer func() { _ = st.Close() }()
	source, err := findGmailSource(st, "reader@example.org")
	require.NoError(err)
	assertRegistrationSyncCommand(t, output, source.ID, "sync-full")
}

func TestRegistrationPrintsExactSyncCommand(t *testing.T) {
	t.Run("new authorization", testAddAccountNewAuthorizationPrintsExactSyncCommand)
	t.Run("existing token", func(t *testing.T) {
		saveAddAccountFlags(t)
		_, restore := seedTokenEnv(t, legacyTokenJSON)
		defer restore()
		output, err := runAddAccountForTestContext(gmailProfileContext(t, scopeEscalationAccount), t, scopeEscalationAccount, "--no-default-identity")
		require.NoError(t, err)
		st, err := store.Open(filepath.Join(os.Getenv("MSGVAULT_HOME"), "msgvault.db"))
		require.NoError(t, err)
		defer func() { _ = st.Close() }()
		source, err := findGmailSource(st, scopeEscalationAccount)
		require.NoError(t, err)
		assertRegistrationSyncCommand(t, output, source.ID, "sync-full")
	})
}
