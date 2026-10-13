package cmd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/microsoft"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestNativeMicrosoftFactoryPreservesCurrentGrant(t *testing.T) {
	for _, mode := range []string{"known", "new", "legacy", "missing"} {
		t.Run(mode, func(t *testing.T) {
			assert := Assert.New(t)

			require := Require.New(t)

			cfg := testConfigValue()
			cfg.HomeDir = t.TempDir()
			cfg.Data.DataDir = cfg.HomeDir
			cfg.Microsoft.ClientID = "synthetic-client"
			inv := testInvocationWithConfig(cfg)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("msmail", "owner@example.net")
			require.NoError(err)
			resource, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: resource.Reference, SourceType: "msmail", NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
			if mode == "new" {
				require.NoError(st.RemoveSource(source.ID))
			}
			manager := newGraphMailManager(inv)
			path := manager.TokenPath(source.Identifier)
			if mode != "missing" {
				token := map[string]any{"access_token": "synthetic-native-access", "token_type": "Bearer", "expiry": "2099-01-01T00:00:00Z"}
				if mode == "known" || mode == "new" {
					token["scopes"] = microsoft.GraphMailScopes()
				}
				raw, err := json.Marshal(token)
				require.NoError(err)
				require.NoError(os.MkdirAll(cfg.TokensDir(), 0700))
				require.NoError(os.WriteFile(path, raw, 0600))
			}
			var profiles atomic.Int32
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				Assert.Equal(t, "Bearer synthetic-native-access", r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				switch strings.TrimPrefix(r.URL.Path, "/v1.0") {
				case "/me":
					profiles.Add(1)
					Assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"id": "synthetic-account", "mail": source.Identifier, "userPrincipalName": source.Identifier}))
				case "/me/mailFolders":
					Assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{"value": []any{}}))
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(provider.Close)
			destination, err := url.Parse(provider.URL)
			require.NoError(err)
			original := http.DefaultTransport
			http.DefaultTransport = testTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "graph.microsoft.com" {
					return nil, errors.New("unexpected native synthetic destination")
				}
				copied := request.Clone(request.Context())
				u := *request.URL
				u.Scheme = destination.Scheme
				u.Host = destination.Host
				copied.URL = &u
				return original.RoundTrip(copied)
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			_, err = runMSMailSync(t.Context(), st, source.Identifier, nil, inv)
			if mode == "missing" {
				require.Error(err)
			} else {
				require.NoError(err)
			}
			source, err = st.GetSourceByTypeAndIdentifier("msmail", "owner@example.net")
			require.NoError(err)
			require.NotNil(source)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.Equal(mode == "known" || mode == "new", allowed)
			if mode == "known" || mode == "new" {
				assert.Positive(profiles.Load())
			} else {
				assert.Zero(profiles.Load())
			}
			current, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			if mode != "new" {
				assert.Equal(resource.Reference, current.Reference, "unknown credential state preserves source lifetime")
			}
		})
	}
}
