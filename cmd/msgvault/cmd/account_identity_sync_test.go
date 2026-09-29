package cmd

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
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
		require.NoError(cmd.ExecuteContext(ctx))
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		src, err := findGmailSource(st, "user@example.com")
		require.NoError(err)
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
		}
	}
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
		require.NoError(cmd.ExecuteContext(ctx))
		st, err := store.Open(cfg.DatabaseDSN())
		require.NoError(err)
		t.Cleanup(func() { _ = st.Close() })
		sources, err := st.ListSources(sourceTypeIMAP)
		require.NoError(err)
		require.Len(sources, 1)
		src := sources[0]
		// Removing the last identity on an account that did not opt out must
		// still allow the next scheduled sync to restore it.
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
		if optOut {
			assert.Empty(ids, "scheduled sync must preserve the opt-out")
		} else {
			require.Len(ids, 1)
			assert.Equal(testutil.IMAPTestUsername, ids[0].Address)
		}
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
				require.NoError(cmd.ExecuteContext(ctx))
				st, err := store.Open(cfg.DatabaseDSN())
				require.NoError(err)
				t.Cleanup(func() { _ = st.Close() })
				sources, err := st.ListSources("")
				require.NoError(err)
				require.Len(sources, 1)
				if tc.name == "graph" {
					// Identity setup precedes token loading. With no Graph token,
					// the real scheduled path stops before making network requests.
					err := runScheduledMSMailSync(ctx, sources[0], st, invocationFromContext(ctx))
					require.ErrorContains(err, "no valid token")
				} else {
					confirmDefaultIdentity(io.Discard, st, sources[0].ID, email, email, "account-identifier", testDiscardLogger())
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
