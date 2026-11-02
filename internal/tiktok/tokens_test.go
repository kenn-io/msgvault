package tiktok

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAmbiguousRefreshRequiresReconnect(t *testing.T) {
	requirements := require.New(t)
	initial, err := json.Marshal(TokenBundle{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", ExpiresAt: time.Now().Add(-time.Hour), RefreshExpiresAt: time.Now().Add(time.Hour)})
	requirements.NoError(err)
	calls := 0
	fixture := fixtureClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "fixture server failure", http.StatusServiceUnavailable)
	})
	c, err := NewManagedClient(t.TempDir(), "app-example", "account-example", string(initial), "fixture-secret")
	requirements.NoError(err)
	c.http, c.baseURL = fixture.http, fixture.baseURL
	c.wait = func(context.Context, time.Duration) error { return nil }
	_, err = c.Account(t.Context(), "account-example")
	requirements.Error(err)
	_, err = c.Account(t.Context(), "account-example")
	requirements.ErrorContains(err, "reconnect")
	assert.Equal(t, 1, calls)
}

func TestManagedTokenRefreshSurvivesRestart(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root := t.TempDir()
	initial, err := json.Marshal(TokenBundle{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", ExpiresAt: time.Now().Add(-time.Hour), RefreshExpiresAt: time.Now().Add(24 * time.Hour)})
	requirements.NoError(err)
	refreshes := 0
	fixture := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tt_user/oauth2/refresh_token/" {
			refreshes++
			var payload map[string]string
			if !assertions.NoError(json.UnmarshalRead(r.Body, &payload)) {
				http.Error(w, "invalid fixture request", http.StatusBadRequest)
				return
			}
			assertions.Equal("fixture-old-refresh", payload["refresh_token"])
			assertions.Equal("fixture-app-secret", payload["client_secret"])
			assertions.Empty(r.Header.Get("Access-Token"))
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh","expires_in":86400,"refresh_token_expires_in":31536000}}`)
			return
		}
		assertions.Equal("fixture-new-access", r.Header.Get("Access-Token"))
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
	})
	for range 2 {
		c, err := NewManagedClient(root, "app-example", "account-example", string(initial), "fixture-app-secret")
		requirements.NoError(err)
		c.http, c.baseURL = fixture.http, fixture.baseURL
		_, err = c.Account(t.Context(), "account-example")
		requirements.NoError(err)
	}
	assertions.Equal(1, refreshes)
	files, err := filepath.Glob(filepath.Join(root, "token-*.json"))
	requirements.NoError(err)
	requirements.Len(files, 1)
	info, err := os.Stat(files[0])
	requirements.NoError(err)
	if runtime.GOOS != "windows" {
		assertions.Equal(os.FileMode(0600), info.Mode().Perm())
	}
	data, err := os.ReadFile(files[0])
	requirements.NoError(err)
	assertions.Contains(string(data), "fixture-new-refresh")
	assertions.NotContains(string(data), "fixture-app-secret")
}

func TestManagedRefreshCancellationBeforeDispatchCanRetry(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	root := t.TempDir()
	initial, err := json.Marshal(TokenBundle{AccessToken: "fixture-old-access", RefreshToken: "fixture-old-refresh", ExpiresAt: time.Now().Add(-time.Hour), RefreshExpiresAt: time.Now().Add(time.Hour)})
	requirements.NoError(err)
	refreshes := 0
	fixture := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tt_user/oauth2/refresh_token/" {
			refreshes++
			_, _ = fmt.Fprint(w, `{"code":0,"data":{"access_token":"fixture-new-access","refresh_token":"fixture-new-refresh","expires_in":86400,"refresh_token_expires_in":31536000}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"code":0,"data":{"username":"example"}}`)
	})
	c, err := NewManagedClient(root, "app-example", "account-example", string(initial), "fixture-secret")
	requirements.NoError(err)
	c.http, c.baseURL = fixture.http, fixture.baseURL
	ctx, cancel := context.WithCancel(t.Context())
	c.limiter = NewLimiter("")
	c.limiter.state.Next = time.Now().Add(time.Hour)
	c.limiter.wait = func(context.Context, time.Duration) error { cancel(); return ctx.Err() }
	_, err = c.Account(ctx, "account-example")
	requirements.ErrorIs(err, context.Canceled)
	assertions.Zero(refreshes)
	// Restart with the same bootstrap. Cancellation never sent the rotating
	// request, so the existing refresh token must remain usable.
	restarted, err := NewManagedClient(root, "app-example", "account-example", string(initial), "fixture-secret")
	requirements.NoError(err)
	restarted.http, restarted.baseURL, restarted.limiter = fixture.http, fixture.baseURL, fixture.limiter
	_, err = restarted.Account(t.Context(), "account-example")
	requirements.NoError(err)
	assertions.Equal(1, refreshes)
}
