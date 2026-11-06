package msgraph

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailPollRetryFloorIsUncappedAndDeferred(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(status)
			}))
			defer server.Close()
			client := NewClient(server.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
			client.DeferProviderRetries()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var profile struct{ Mail string }
			start := time.Now()
			err := client.GetJSON(ctx, "/me", &profile)
			require.Error(t, err)
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			assert.False(t, client.RetryNotBefore().Before(start.Add(time.Hour)))
			if status == http.StatusTooManyRequests {
				var quota *ThrottledError
				require.ErrorAs(t, err, &quota)
				assert.Equal(t, time.Hour, quota.RetryAfter)
				assert.Equal(t, client.RetryNotBefore(), quota.RetryNotBefore())
			}
			err = client.GetJSON(ctx, "/me", &profile)
			require.Error(t, err)
			assert.EqualValues(t, 1, requests.Load())
		})
	}
}

func TestEmailPollMIMEFloorIsSharedWithOriginalClient(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := NewClient(server.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
	client.DeferProviderRetries()
	start := time.Now()
	_, err := client.GetRawWithTimeout(t.Context(), "/mime", time.Minute)
	require.Error(t, err)
	assert.False(t, client.RetryNotBefore().Before(start.Add(time.Hour)), "per-request timeout clones share floor evidence with the acquisition client")
	_, err = client.GetRawWithTimeout(t.Context(), "/mime", time.Minute)
	require.Error(t, err)
	assert.EqualValues(t, 1, requests.Load(), "a new MIME timeout clone must respect the existing shared floor")
}

type emailPollQueuedContextKey struct{}

func TestEmailPollQueuedRequestRechecksProviderFloorBeforeSend(t *testing.T) {
	firstStarted, releaseFirst := make(chan struct{}), make(chan struct{})
	queuedAdmitted, releaseQueued := make(chan struct{}), make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/first" {
			close(firstStarted)
			select {
			case <-releaseFirst:
			case <-r.Context().Done():
				return
			}
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("unexpected queued MIME"))
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	client := NewClient(server.URL, func(ctx context.Context) (string, error) {
		if ctx.Value(emailPollQueuedContextKey{}) == true {
			// Token acquisition is the real public blocking seam AFTER local
			// rate admission. It holds this unsent request deterministically;
			// there is no dependency on how fast the HTTP runner responds.
			close(queuedAdmitted)
			select {
			case <-releaseQueued:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return "fixture-token", nil
	}, 1000)
	client.DeferProviderRetries()
	await := func(signal <-chan struct{}) {
		t.Helper()
		select {
		case <-signal:
		case <-ctx.Done():
			require.NoError(t, ctx.Err(), "expected request transition did not occur")
		}
	}
	firstResult, queuedResult := make(chan error, 1), make(chan error, 1)
	go func() { _, err := client.GetRawWithTimeout(ctx, "/first", time.Minute); firstResult <- err }()
	await(firstStarted)
	go func() {
		_, err := client.GetRawWithTimeout(context.WithValue(ctx, emailPollQueuedContextKey{}, true), "/queued", time.Minute)
		queuedResult <- err
	}()
	await(queuedAdmitted)
	close(releaseFirst)
	firstErr := <-firstResult
	var firstFloor *ThrottledError
	require.ErrorAs(t, firstErr, &firstFloor)
	assert.NotErrorIs(t, firstErr, ErrNotSent, "the initial 429 request reached the real HTTP server")
	close(releaseQueued)
	err := <-queuedResult
	var queuedFloor *ThrottledError
	require.ErrorAs(t, err, &queuedFloor)
	assert.ErrorIs(t, err, ErrNotSent, "the queued request is deferred before its first send")
	assert.Equal(t, firstFloor.RetryNotBefore(), queuedFloor.RetryNotBefore())
	assert.Equal(t, firstFloor.RetryNotBefore(), client.RetryNotBefore())
	assert.EqualValues(t, 1, requests.Load(), "a floor learned during local admission blocks every still-unsent MIME worker")
}

func TestEmailPollOrdinaryGraphRetriesKeepExistingBounds(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path == "/throttle" {
					w.Header().Set("Retry-After", "3600")
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`{"mail":"sample@example.test"}`))
			}))
			defer server.Close()
			client := NewClient(server.URL, func(context.Context) (string, error) { return "fixture-token", nil }, 1000)
			// The deadline rejects an impossible retry wait immediately; this
			// regression does not actually wait through the ordinary 15m cap.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var profile struct {
				Mail string `json:"mail"`
			}
			err := client.GetJSON(ctx, "/throttle", &profile)
			require.Error(t, err)
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			if status == http.StatusTooManyRequests {
				var quota *ThrottledError
				require.ErrorAs(t, err, &quota)
				assert.Equal(t, 15*time.Minute, quota.RetryAfter, "ordinary retries retain the existing cap")
			}
			assert.True(t, client.RetryNotBefore().IsZero(), "ordinary clients do not retain the rapid cross-request floor")
			require.NoError(t, client.GetJSON(ctx, "/next", &profile), "a later ordinary first attempt retains its existing admission behavior")
			assert.Equal(t, "sample@example.test", profile.Mail)
			assert.EqualValues(t, 2, requests.Load())
		})
	}
}
