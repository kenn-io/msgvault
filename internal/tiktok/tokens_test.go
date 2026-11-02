package tiktok

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAmbiguousRefreshRequiresReconnect(t *testing.T) {
	initial, err := json.Marshal(TokenBundle{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", ExpiresAt: time.Now().Add(-time.Hour), RefreshExpiresAt: time.Now().Add(time.Hour)})
	require.NoError(t, err)
	calls := 0
	fixture := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "fixture server failure", http.StatusServiceUnavailable)
	})
	c, err := NewManagedClient(t.TempDir(), "app-example", "account-example", string(initial), "fixture-secret")
	require.NoError(t, err)
	c.http, c.baseURL = fixture.http, fixture.baseURL
	c.wait = func(context.Context, time.Duration) error { return nil }
	_, err = c.Account(t.Context(), "account-example")
	require.Error(t, err)
	_, err = c.Account(t.Context(), "account-example")
	require.ErrorContains(t, err, "reconnect")
	assert.Equal(t, 1, calls)
}

func TestManagedTokenRefreshSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	initial, err := json.Marshal(TokenBundle{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", ExpiresAt: time.Now().Add(-time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)})
	require.NoError(t, err)
	refreshes := 0
	fixture := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tt_user/oauth2/refresh_token/" {
			refreshes++
			var payload map[string]string
			if !assert.NoError(t, json.UnmarshalRead(r.Body, &payload)) {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			assert.Equal(t, "fixture-old-refresh", payload["refresh_token"])
			assert.Equal(t, "fixture-app-secret", payload["client_secret"])
			assert.Empty(t, r.Header.Get("Access-Token"))
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh","expires_in":86400,"refresh_token_expires_in":31536000}}`)
			return
		}
		assert.Equal(t, "fixture-new-access", r.Header.Get("Access-Token"))
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
	})
	for range 2 {
		c, err := NewManagedClient(root, "app-example", "account-example", string(initial), "fixture-app-secret")
		require.NoError(t, err)
		c.http, c.baseURL = fixture.http, fixture.baseURL
		_, err = c.Account(t.Context(), "account-example")
		require.NoError(t, err)
	}
	assert.Equal(t, 1, refreshes)
	files, err := filepath.Glob(filepath.Join(root, "token-*.json"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	info, err := os.Stat(files[0])
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm())
	data, err := os.ReadFile(files[0])
	require.NoError(t, err)
	assert.Contains(t, string(data), "fixture-new-refresh")
	assert.NotContains(t, string(data), "fixture-app-secret")
}
