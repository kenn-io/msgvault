package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
)

func TestSelectO365AccountRejectsAmbiguousMailboxes(t *testing.T) {
	accounts := []daemonclient.CLIAccount{
		{Type: sourceTypeIMAP, Identifier: "imaps://user@example.com@outlook.office.com:993", AuthMethod: "xoauth2", OAuthApp: "a"},
		{Type: sourceTypeIMAP, Identifier: "imaps://user@example.com@outlook.office365.com:993", AuthMethod: "xoauth2", OAuthApp: "b"},
	}
	for i := 0; i < 2; i++ {
		_, _, err := selectO365Account(accounts, "USER@example.com", false)
		require.ErrorContains(t, err, accounts[0].Identifier)
		require.ErrorContains(t, err, accounts[1].Identifier)
		accounts[0], accounts[1] = accounts[1], accounts[0]
	}
}

func TestMicrosoftMailAppsRefreshOwnTokens(t *testing.T) {
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Microsoft.ClientID = "client-a"
	cfg.Microsoft.TenantID = "tenant-a"
	cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"orgb": {ClientID: "client-b", TenantID: "tenant-b"}}
	state := testInvocationWithConfig(cfg)
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	requests := map[string]int{}
	http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
		require.NoError(t, req.ParseForm())
		client := req.Form.Get("client_id")
		if client == "" {
			client, _, _ = req.BasicAuth()
		}
		switch req.URL.Path {
		case "/tenant-a/oauth2/v2.0/token":
			assert.Equal(t, "client-a", client)
			assert.Equal(t, "refresh-a", req.Form.Get("refresh_token"))
		case "/tenant-b/oauth2/v2.0/token":
			assert.Equal(t, "client-b", client)
			assert.Equal(t, "refresh-b", req.Form.Get("refresh_token"))
		default:
			assert.Fail(t, "unexpected token endpoint", req.URL.Path)
		}
		requests[client]++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`))}, nil
	})
	for _, graph := range []bool{false, true} {
		for _, tc := range []struct{ email, name, refresh string }{{"a@example.com", "", "refresh-a"}, {"b@example.com", "orgb", "refresh-b"}} {
			src := &store.Source{Identifier: tc.email, OAuthApp: sql.NullString{String: tc.name, Valid: tc.name != ""}}
			var mgr interface {
				TokenPath(string) string
				TokenSource(context.Context, string) (func(context.Context) (string, error), error)
			}
			var err error
			if graph {
				mgr, err = newGraphMailManager(state, src)
			} else {
				mgr, err = newMicrosoftIMAPManager(cfg, testDiscardLogger(), src)
			}
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(filepath.Dir(mgr.TokenPath(tc.email)), 0o700))
			token := fmt.Sprintf(`{"access_token":"expired","refresh_token":%q,"expiry":"2000-01-01T00:00:00Z"}`, tc.refresh)
			require.NoError(t, os.WriteFile(mgr.TokenPath(tc.email), []byte(token), 0o600))
			fn, err := mgr.TokenSource(t.Context(), tc.email)
			require.NoError(t, err)
			access, err := fn(t.Context())
			require.NoError(t, err)
			assert.Equal(t, "fresh", access)
		}
	}
	assert.Equal(t, 2, requests["client-a"])
	assert.Equal(t, 2, requests["client-b"])
	_, oldDir, err := microsoftMailApp(cfg, "orgb")
	require.NoError(t, err)
	cfg.Microsoft.Apps["orgb"] = config.MicrosoftApp{ClientID: "client-c"}
	_, newDir, err := microsoftMailApp(cfg, "orgb")
	require.NoError(t, err)
	assert.NotEqual(t, oldDir, newDir)
	changed, err := newGraphMailManager(state, &store.Source{OAuthApp: sql.NullString{String: "orgb", Valid: true}})
	require.NoError(t, err)
	assert.False(t, changed.HasToken("b@example.com"))
	cfg.Microsoft.Apps["orgb"] = config.MicrosoftApp{ClientID: "client-b", TenantID: "tenant-b"}
	restored, err := newGraphMailManager(state, &store.Source{OAuthApp: sql.NullString{String: "orgb", Valid: true}})
	require.NoError(t, err)
	assert.True(t, restored.HasToken("b@example.com"))
}

func TestAddO365StoresAndChangesAppBinding(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(fmt.Sprintf("graph=%t", graph), func(t *testing.T) {
			oldGraph, oldApp, oldIdentity := o365Graph, o365OAuthApp, noDefaultIdentityAddO365
			t.Cleanup(func() { o365Graph, o365OAuthApp, noDefaultIdentityAddO365 = oldGraph, oldApp, oldIdentity })
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Microsoft.ClientID = "default"
			cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"orgb": {ClientID: "b"}, "other": {ClientID: "c"}}
			state := testInvocationWithConfig(cfg)
			ctx := withInvocation(t.Context(), state)
			email := "user@example.com"
			for _, tc := range []struct {
				flag *string
				want string
			}{{new("orgb"), "orgb"}, {nil, "orgb"}, {new("other"), "other"}, {new(""), ""}} {
				cmd := newAddO365LocalCmd()
				cmd.SetContext(ctx)
				require.NoError(t, cmd.Flags().Set("oauth-preflighted", "true"))
				require.NoError(t, cmd.Flags().Set("no-default-identity", "true"))
				if graph {
					require.NoError(t, cmd.Flags().Set("graph", "true"))
				}
				if tc.flag != nil {
					require.NoError(t, cmd.Flags().Set("oauth-app", *tc.flag))
				}
				_, dir, err := microsoftMailApp(cfg, tc.want)
				require.NoError(t, err)
				require.NoError(t, os.MkdirAll(dir, 0o700))
				var path string
				if graph {
					path = microsoft.NewGraphMailManager("", "", "", dir, nil).TokenPath(email)
				} else {
					path = microsoft.NewManager("", "", "", dir, nil).TokenPath(email)
				}
				require.NoError(t, os.WriteFile(path, []byte(`{"access_token":"token"}`), 0o600))
				require.NoError(t, runAddO365Local(cmd, []string{email}))
				s, cleanup, err := openWritableStoreAndInitForIngestInvocation(ctx, state)
				require.NoError(t, err)
				sources, err := s.GetSourcesByDisplayName(email)
				require.NoError(t, err)
				require.Len(t, sources, 1)
				assert.Equal(t, tc.want, sourceOAuthApp(sources[0]))
				cleanup()
			}
		})
	}
}

func TestAddO365FailedRegistrationPreservesOldPair(t *testing.T) {
	for _, graph := range []bool{false, true} {
		t.Run(fmt.Sprintf("graph=%t", graph), func(t *testing.T) {
			oldGraph, oldApp, oldIdentity := o365Graph, o365OAuthApp, noDefaultIdentityAddO365
			t.Cleanup(func() { o365Graph, o365OAuthApp, noDefaultIdentityAddO365 = oldGraph, oldApp, oldIdentity })
			cfg := lifecycleTestConfig(t.TempDir())
			cfg.Microsoft.ClientID = "old-client"
			cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"new": {ClientID: "new-client"}}
			state := testInvocationWithConfig(cfg)
			ctx := withInvocation(t.Context(), state)
			s, cleanup, err := openWritableStoreAndInitForIngestInvocation(ctx, state)
			require.NoError(t, err)
			typ, identifier := sourceTypeIMAP, "imaps://user@example.com@outlook.office365.com:993"
			if graph {
				typ, identifier = sourceTypeMSMail, "user@example.com"
			}
			src, err := s.GetOrCreateSource(typ, identifier)
			require.NoError(t, err)
			oldConfig := `{"host":"outlook.office365.com","username":"user@example.com","auth_method":"xoauth2"}`
			if !graph {
				require.NoError(t, s.UpdateSourceSyncConfig(ctx, src.ID, oldConfig))
			}
			_, err = s.DB().ExecContext(ctx, `CREATE TRIGGER fail_binding BEFORE UPDATE OF oauth_app ON sources BEGIN SELECT RAISE(ABORT, 'binding failed'); END`)
			require.NoError(t, err)
			cleanup()
			_, dir, err := microsoftMailApp(cfg, "new")
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			require.NoError(t, os.MkdirAll(cfg.TokensDir(), 0o700))
			var oldPath, newPath string
			if graph {
				oldPath = microsoft.NewGraphMailManager("", "", "", cfg.TokensDir(), nil).TokenPath("user@example.com")
				newPath = microsoft.NewGraphMailManager("", "", "", dir, nil).TokenPath("user@example.com")
			} else {
				oldPath = microsoft.NewManager("", "", "", cfg.TokensDir(), nil).TokenPath("user@example.com")
				newPath = microsoft.NewManager("", "", "", dir, nil).TokenPath("user@example.com")
			}
			oldToken := `{"access_token":"old","refresh_token":"old-refresh","expiry":"2000-01-01T00:00:00Z"}`
			require.NoError(t, os.WriteFile(oldPath, []byte(oldToken), 0o600))
			require.NoError(t, os.WriteFile(newPath, []byte(`{"access_token":"new"}`), 0o600))
			cmd := newAddO365LocalCmd()
			cmd.SetContext(ctx)
			require.NoError(t, cmd.Flags().Set("oauth-preflighted", "true"))
			require.NoError(t, cmd.Flags().Set("oauth-app", "new"))
			if graph {
				require.NoError(t, cmd.Flags().Set("graph", "true"))
			}
			require.ErrorContains(t, runAddO365Local(cmd, []string{"user@example.com"}), "binding failed")
			s, cleanup, err = openWritableStoreAndInitForIngestInvocation(ctx, state)
			require.NoError(t, err)
			defer cleanup()
			saved, err := s.GetSourceByID(src.ID)
			require.NoError(t, err)
			assert.Equal(t, identifier, saved.Identifier)
			assert.Empty(t, sourceOAuthApp(saved))
			if !graph {
				assert.JSONEq(t, oldConfig, saved.SyncConfig.String)
			}
			contents, err := os.ReadFile(oldPath)
			require.NoError(t, err)
			assert.JSONEq(t, oldToken, string(contents))
			previous := http.DefaultTransport
			http.DefaultTransport = testTransport(func(req *http.Request) (*http.Response, error) {
				require.NoError(t, req.ParseForm())
				client := req.Form.Get("client_id")
				if client == "" {
					client, _, _ = req.BasicAuth()
				}
				assert.Equal(t, "old-client", client)
				assert.Equal(t, "old-refresh", req.Form.Get("refresh_token"))
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = previous })
			var tokenFn func(context.Context) (string, error)
			if graph {
				mgr, err := newGraphMailManager(state, saved)
				require.NoError(t, err)
				tokenFn, err = mgr.TokenSource(ctx, "user@example.com")
				require.NoError(t, err)
			} else {
				mgr, err := newMicrosoftIMAPManager(cfg, testDiscardLogger(), saved)
				require.NoError(t, err)
				tokenFn, err = mgr.TokenSource(ctx, "user@example.com")
				require.NoError(t, err)
			}
			access, err := tokenFn(ctx)
			require.NoError(t, err)
			assert.Equal(t, "fresh", access)
		})
	}
}
