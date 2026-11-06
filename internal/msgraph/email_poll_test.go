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
