package gmail

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type emailPollTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (t emailPollTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.URL.Scheme, clone.URL.Host = t.target.Scheme, t.target.Host
	return t.base.RoundTrip(clone)
}

func TestEmailPollQuotaDeferralRetainsProviderFloor(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "3600")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"errors":[{"reason":"rateLimitExceeded"}]}}`))
			}))
			defer server.Close()
			target, err := url.Parse(server.URL)
			require.NoError(err)
			client := NewClient(nil)
			client.httpClient = &http.Client{Transport: emailPollTransport{target, http.DefaultTransport}}
			client.DeferProviderRetries()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			start := time.Now()
			_, err = client.GetProfile(ctx)
			var quota *ThrottledError
			require.ErrorAs(err, &quota)
			assert.NotErrorIs(err, context.DeadlineExceeded, "a quota outcome releases the work gate instead of waiting")
			assert.False(quota.RetryNotBefore().Before(start.Add(time.Hour)))
			assert.Equal(quota.RetryNotBefore(), client.RetryNotBefore(), "partial-summary callers retain the same floor")
			_, err = client.GetProfile(ctx)
			require.ErrorAs(err, &quota)
			assert.EqualValues(1, requests.Load(), "a known floor prevents another request on this client")
		})
	}
}
