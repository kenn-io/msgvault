package cmd

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
)

func TestDaemonGmailLimiterSurvivesCancellation(t *testing.T) {
	cfg := testGmailServiceAccountConfig(t)
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var profiles atomic.Int32
			var throttleNext atomic.Bool
			old := http.DefaultTransport
			http.DefaultTransport = testTransport(func(r *http.Request) (*http.Response, error) {
				body := `{"access_token":"token","token_type":"Bearer","expires_in":3600}`
				code := http.StatusOK
				header := make(http.Header)
				if r.URL.Host == "gmail.googleapis.com" {
					if profiles.Add(1) == 1 || throttleNext.Swap(false) {
						code = status
						header.Set("Retry-After", "600")
						body = `{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`
					} else {
						body = `{"emailAddress":"user@example.test","historyId":"100"}`
					}
				}
				return &http.Response{StatusCode: code, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			synctest.Test(t, func(t *testing.T) {
				ctx := testInvocationContext(t.Context(), cfg, invocationOptions{})
				state := invocationFromContext(ctx)
				client, _, err := newDaemonGmailClient(ctx, "user.name+tag@gmail.com", nil, nil, state, oauth.Scopes)
				require.NoError(t, err)
				runCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				go func() { _, err := client.GetProfile(runCtx); done <- err }()
				synctest.Wait()
				cancel()
				require.Error(t, <-done)
				client, _, err = newDaemonGmailClient(ctx, "username@googlemail.com", nil, nil, state, oauth.Scopes)
				require.NoError(t, err)
				waitCtx, stop := context.WithTimeout(ctx, time.Second)
				defer stop()
				_, err = client.GetProfile(waitCtx)
				require.Error(t, err)
				assert.EqualValues(t, 1, profiles.Load())
				poll, _, err := newDaemonGmailClient(ctx, "username@gmail.com", nil, nil, state, oauth.Scopes, gmail.WithDeferredQuota())
				require.NoError(t, err)
				_, err = poll.GetProfile(ctx)
				require.ErrorIs(t, err, gmail.ErrQuotaDeferred)
				assert.EqualValues(t, 1, profiles.Load())
				concurrent := make(chan error, 8)
				for range 8 {
					go func() {
						c, _, err := newDaemonGmailClient(ctx, "username@gmail.com", nil, nil, state, oauth.Scopes, gmail.WithDeferredQuota())
						if err == nil {
							_, err = c.GetProfile(ctx)
						}
						concurrent <- err
					}()
				}
				for range 8 {
					require.ErrorIs(t, <-concurrent, gmail.ErrQuotaDeferred)
				}
				assert.EqualValues(t, 1, profiles.Load())
				other, _, err := newDaemonGmailClient(ctx, "other@example.test", nil, nil, state, oauth.Scopes)
				require.NoError(t, err)
				_, err = other.GetProfile(ctx)
				require.NoError(t, err)
				assert.EqualValues(t, 2, profiles.Load())
				throttleNext.Store(true)
				fresh, _, err := newDaemonGmailClient(ctx, "fresh@example.test", nil, nil, state, oauth.Scopes, gmail.WithDeferredQuota())
				require.NoError(t, err)
				_, err = fresh.GetProfile(ctx)
				require.ErrorIs(t, err, gmail.ErrQuotaDeferred)
				_, err = other.GetProfile(ctx)
				require.NoError(t, err)
				assert.EqualValues(t, 4, profiles.Load())
				time.Sleep(598 * time.Second)
				beforeDeadline, stopBefore := context.WithTimeout(ctx, 500*time.Millisecond)
				_, err = client.GetProfile(beforeDeadline)
				stopBefore()
				require.Error(t, err)
				assert.EqualValues(t, 4, profiles.Load())
				time.Sleep(500 * time.Millisecond)
				_, err = client.GetProfile(ctx)
				require.NoError(t, err)
				assert.EqualValues(t, 5, profiles.Load())
			})
		})
	}
}
