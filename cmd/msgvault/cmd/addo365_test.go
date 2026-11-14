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
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
)

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
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`))}, nil
	})
	for _, graph := range []bool{false, true} {
		for _, tc := range []struct{ email, name, refresh string }{{"a@example.com", "", "refresh-a"}, {"b@example.com", "orgb", "refresh-b"}} {
			src := &store.Source{Identifier: tc.email, OAuthApp: sql.NullString{String: tc.name, Valid: tc.name != ""}}
			var mgr interface {
				TokenPath(email string) string
				TokenSource(ctx context.Context, email string) (func(context.Context) (string, error), error)
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
	cfg.Microsoft.Apps["orgb"] = config.MicrosoftApp{ClientID: "client-c"}
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
			if !graph {
				s, cleanup, err := openWritableStoreAndInitForIngestInvocation(ctx, state)
				require.NoError(t, err)
				other, err := s.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.com@other.example:993")
				require.NoError(t, err)
				require.NoError(t, s.UpdateSourceSyncConfig(ctx, other.ID, `{"host":"other.example","username":"user@example.com","auth_method":"xoauth2"}`))
				cleanup()
			}
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
				if !graph {
					other, err := s.GetSourceByIdentifier(ctx, "imaps://user@example.com@other.example:993")
					require.NoError(t, err)
					assert.Contains(t, other.SyncConfig.String, "other.example")
				}
				cleanup()
			}
		})
	}
}

func TestAddO365FailedRegistrationPreservesOldPair(t *testing.T) {
	oldGraph, oldApp, oldIdentity := o365Graph, o365OAuthApp, noDefaultIdentityAddO365
	t.Cleanup(func() { o365Graph, o365OAuthApp, noDefaultIdentityAddO365 = oldGraph, oldApp, oldIdentity })
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Microsoft.ClientID = "old-client"
	cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"new": {ClientID: "new-client"}}
	state := testInvocationWithConfig(cfg)
	ctx := withInvocation(t.Context(), state)
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(ctx, state)
	require.NoError(t, err)
	identifier := "imaps://user@example.com@outlook.office365.com:993"
	src, err := s.GetOrCreateSource(sourceTypeIMAP, identifier)
	require.NoError(t, err)
	oldConfig := `{"host":"outlook.office365.com","username":"user@example.com","auth_method":"xoauth2"}`
	require.NoError(t, s.UpdateSourceSyncConfig(ctx, src.ID, oldConfig))
	_, err = s.DB().ExecContext(ctx, `CREATE TRIGGER fail_binding BEFORE UPDATE OF oauth_app ON sources BEGIN SELECT RAISE(ABORT, 'binding failed'); END`)
	require.NoError(t, err)
	cleanup()
	_, dir, err := microsoftMailApp(cfg, "new")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	newPath := microsoft.NewManager("", "", "", dir, nil).TokenPath("user@example.com")
	require.NoError(t, os.WriteFile(newPath, []byte(`{"access_token":"new","scopes":["https://outlook.office.com/IMAP.AccessAsUser.All"]}`), 0o600))
	cmd := newAddO365LocalCmd()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("oauth-preflighted", "true"))
	require.NoError(t, cmd.Flags().Set("oauth-app", "new"))
	require.ErrorContains(t, runAddO365Local(cmd, []string{"user@example.com"}), "binding failed")
	s, cleanup, err = openWritableStoreAndInitForIngestInvocation(ctx, state)
	require.NoError(t, err)
	defer cleanup()
	saved, err := s.GetSourceByID(src.ID)
	require.NoError(t, err)
	assert.Equal(t, identifier, saved.Identifier)
	assert.Empty(t, sourceOAuthApp(saved))
	assert.JSONEq(t, oldConfig, saved.SyncConfig.String)
}

func TestAddO365FailedNewGraphBindingLeavesNoSource(t *testing.T) {
	oldGraph, oldApp := o365Graph, o365OAuthApp
	t.Cleanup(func() { o365Graph, o365OAuthApp = oldGraph, oldApp })
	cfg := lifecycleTestConfig(t.TempDir())
	cfg.Microsoft.ClientID = "default"
	cfg.Microsoft.Apps = map[string]config.MicrosoftApp{"new": {ClientID: "new-client"}}
	state := testInvocationWithConfig(cfg)
	ctx := withInvocation(t.Context(), state)
	s, cleanup, err := openWritableStoreAndInitForIngestInvocation(ctx, state)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(ctx, `CREATE TRIGGER fail_binding BEFORE UPDATE OF oauth_app ON sources BEGIN SELECT RAISE(ABORT, 'binding failed'); END`)
	require.NoError(t, err)
	cleanup()
	cmd := newAddO365LocalCmd()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("oauth-preflighted", "true"))
	require.NoError(t, cmd.Flags().Set("oauth-app", "new"))
	require.NoError(t, cmd.Flags().Set("graph", "true"))
	require.ErrorContains(t, runAddO365Local(cmd, []string{"user@example.com"}), "binding failed")
	s, cleanup, err = openWritableStoreAndInitForIngestInvocation(ctx, state)
	require.NoError(t, err)
	defer cleanup()
	_, err = s.GetSourceByIdentifier(ctx, "user@example.com")
	require.ErrorIs(t, err, store.ErrSourceNotFound)
}
