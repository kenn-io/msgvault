package msmail

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/msgraph"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"golang.org/x/oauth2"
)

// An idle sync must apply a narrowed refresh grant without relying on a MIME
// download. Only provider HTTP is synthetic; OAuth refresh, Graph requests,
// importer control flow, and persisted Events authority run unchanged.
func TestNativeMicrosoftIdleSyncRechecksRefreshedGrant(t *testing.T) {
	for _, stage := range []string{"profile", "profile failure", "folders", "empty folders", "folder continuation", "folder lookup", "delta", "delta continuation"} {
		t.Run(stage, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			enableNativeMailEvents(t, st)
			graph := newFakeGraph(t)
			initial := NewClient(graph.srv.URL, func(context.Context) (string, error) { return "synthetic-initial", nil }, 1000,
				WithGrantedScopesReader(func() ([]string, error) { return []string{"Mail.Read"}, nil }))
			_, err := Import(t.Context(), st, initial, Options{Email: "me@example.com"}, slog.Default())
			require.NoError(err)
			source, err := st.GetSourceByTypeAndIdentifier(SourceType, "me@example.com")
			require.NoError(err)
			before, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			allowed, err := st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			require.True(allowed)

			var refresh, refreshed atomic.Bool
			refresh.Store(stage == "profile" || stage == "profile failure")
			var refreshes atomic.Int32
			tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/token", r.URL.Path)
				assert.NoError(r.ParseForm())
				assert.Equal("refresh_token", r.Form.Get("grant_type"))
				refreshes.Add(1)
				w.Header().Set("Content-Type", "application/json")
				assert.NoError(json.NewEncoder(w).Encode(map[string]any{
					"access_token": "synthetic-narrowed", "token_type": "Bearer", "expires_in": 3600,
					"scope": "User.Read Mail.ReadBasic",
				}))
			}))
			t.Cleanup(tokens.Close)
			config := &oauth2.Config{ClientID: "synthetic-client", Endpoint: oauth2.Endpoint{TokenURL: tokens.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}}
			tokenSource := config.TokenSource(t.Context(), &oauth2.Token{AccessToken: "synthetic-expired", RefreshToken: "synthetic-refresh", Expiry: time.Unix(1, 0)})
			var grants atomic.Value
			grants.Store([]string{"Mail.Read"})
			client := NewClient(graph.srv.URL, func(context.Context) (string, error) {
				if !refresh.Load() {
					return "synthetic-initial", nil
				}
				token, err := tokenSource.Token()
				if err != nil {
					return "", fmt.Errorf("refresh Microsoft token: %w", err)
				}
				scopeValue := token.Extra("scope")
				scope, ok := scopeValue.(string)
				if !ok {
					return "", fmt.Errorf("refreshed token scope has type %T, want string", scopeValue)
				}
				grants.Store(strings.Fields(scope))
				refreshed.Store(true)
				return token.AccessToken, nil
			}, 1000, WithGrantedScopesReader(func() ([]string, error) {
				value := grants.Load()
				scopes, ok := value.([]string)
				if !ok {
					return nil, fmt.Errorf("refreshed scope state has type %T, want []string", value)
				}
				return scopes, nil
			}))
			graph.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path := r.URL.Path
				if stage == "profile failure" && path == "/me" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if path == "/me/mailFolders" {
					switch stage {
					case "empty folders":
						graph.writeJSON(w, map[string]any{"value": []any{}})
						return
					case "folder continuation":
						if r.URL.Query().Get("continuation") == "" {
							graph.writeJSON(w, map[string]any{"value": []any{}, "@odata.nextLink": graph.srv.URL + "/me/mailFolders?continuation=1"})
							refresh.Store(true)
							return
						}
					}
				}
				if stage == "delta continuation" && path == "/me/mailFolders/inbox/messages/delta" && r.URL.Query().Get("continuation") == "" {
					graph.writeJSON(w, map[string]any{"value": []any{}, "@odata.nextLink": graph.srv.URL + path + "?token=0&continuation=1"})
					refresh.Store(true)
					return
				}
				graph.serve(w, r)
				if path == "/me" && (stage == "folders" || stage == "empty folders") ||
					path == "/me/mailFolders" && stage == "folder lookup" ||
					path == "/me/mailFolders/recoverableitemsdeletions" && stage == "delta" {
					refresh.Store(true)
				}
			})

			_, err = Import(t.Context(), st, client, Options{Email: "me@example.com"}, slog.Default())
			if stage == "profile failure" {
				require.Error(err, "the synthetic transient profile failure must remain visible")
			} else {
				require.NoError(err, "ordinary idle archive sync remains usable")
			}
			require.True(refreshed.Load(), "the selected provider request must cross an actual OAuth refresh")
			assert.Equal(int32(1), refreshes.Load())
			assert.Zero(graph.mimeCalls.Load(), "the sync must not depend on MIME capture to reduce authority")
			allowed, err = st.NativeSourceMessagePermission(t.Context(), source.ID)
			require.NoError(err)
			assert.False(allowed, "a narrowed current native grant must disable Events authority")
			after, err := st.GetMCPResource(t.Context(), "source", source.ID)
			require.NoError(err)
			assert.NotEqual(before.Reference, after.Reference, "known permission loss must retire retained Events authority")
		})
	}
}

func TestNativeMicrosoftProfileDenialRetiresUnknownGrant(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "me@example.com")
	require.NoError(err)
	before, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	require.NoError(st.ObserveNativeSourcePermission(t.Context(), store.NativeSourcePermission{SourceID: source.ID, SourceReference: before.Reference, SourceType: SourceType, NativeIdentity: source.Identifier, Known: true, MessageRead: true}))
	var changed atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/me", r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(provider.Close)
	client := NewClient(provider.URL, func(context.Context) (string, error) {
		changed.Store(true)
		return "synthetic-token", nil
	}, 1000, WithGrantedScopesReader(func() ([]string, error) {
		if changed.Load() {
			return nil, nil
		}
		return []string{"Mail.Read"}, nil
	}))
	_, err = observeNativeMailPermission(t.Context(), st, source, client)
	require.ErrorIs(err, msgraph.ErrForbidden)
	after, err := st.GetMCPResource(t.Context(), "source", source.ID)
	require.NoError(err)
	assert.NotEqual(before.Reference, after.Reference, "explicit provider denial must retire authority even when the current grant is unknown")
}
