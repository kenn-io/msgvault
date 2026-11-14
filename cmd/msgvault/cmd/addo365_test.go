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
	imapclient "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
)

func TestIsMicrosoftIMAPSource(t *testing.T) {
	xoauth2Config := func(username string) string {
		cfg := &imapclient.Config{
			Host:       "outlook.office365.com",
			Port:       993,
			TLS:        true,
			Username:   username,
			AuthMethod: imapclient.AuthXOAuth2,
		}
		j, _ := cfg.ToJSON()
		return j
	}
	passwordConfig := func(username string) string {
		cfg := &imapclient.Config{
			Host:     "imap.example.com",
			Port:     993,
			TLS:      true,
			Username: username,
		}
		j, _ := cfg.ToJSON()
		return j
	}

	tests := []struct {
		name  string
		src   *store.Source
		email string
		want  bool
	}{
		{
			name: "microsoft xoauth2 matching username",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: xoauth2Config("user@company.com")},
			},
			email: "user@company.com",
			want:  true,
		},
		{
			name: "microsoft xoauth2 username case-insensitive",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: xoauth2Config("User@Company.com")},
			},
			email: "user@company.com",
			want:  true,
		},
		{
			name: "non-microsoft password-auth source same display name",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: passwordConfig("user@company.com")},
			},
			email: "user@company.com",
			want:  false,
		},
		{
			name: "xoauth2 but different username",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: xoauth2Config("other@company.com")},
			},
			email: "user@company.com",
			want:  false,
		},
		{
			name: "no sync_config",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: false},
			},
			email: "user@company.com",
			want:  false,
		},
		{
			name: "invalid sync_config json",
			src: &store.Source{
				SourceType: "imap",
				SyncConfig: sql.NullString{Valid: true, String: "not-json"},
			},
			email: "user@company.com",
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isMicrosoftIMAPSource(tt.src, tt.email)
			assert.Equal(t, tt.want, got, "isMicrosoftIMAPSource()")
		})
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
	oldDir, err := microsoftTokensDir(cfg, "orgb")
	require.NoError(t, err)
	cfg.Microsoft.Apps["orgb"] = config.MicrosoftApp{ClientID: "client-c"}
	newDir, err := microsoftTokensDir(cfg, "orgb")
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
				dir, err := microsoftTokensDir(cfg, tc.want)
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
