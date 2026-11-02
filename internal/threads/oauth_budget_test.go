package threads

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsOAuthPreservesWholeRunBudgetPause(t *testing.T) {
	fixture := newOAuthFixture(t)
	clock := newTestClock()
	budget := clock.budget(1)
	opts := fixture.options()
	opts.HTTPClient = NewBudgetedHTTPClient(opts.HTTPClient, budget)
	_, err := ExchangeCredential(t.Context(), opts, testOAuthRequest())
	var pause *PauseError
	require.ErrorAs(t, err, &pause)
	assert.Equal(t, "attempt_limit", pause.Reason)
	assert.Equal(t, 1, budget.Attempts())
}

func TestThreadsOAuthUsageStopsNextAttempt(t *testing.T) {
	for _, path := range []string{"/oauth/access_token", "/debug_token"} {
		t.Run(path, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			fixture := newOAuthFixture(t)
			handler := fixture.server.Config.Handler
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == path {
					w.Header().Set("X-App-Usage", `{"call_count":80}`)
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			clock := newTestClock()
			budget := clock.budget(100)
			opts := fixture.options()
			opts.OAuthBaseURL = server.URL
			opts.HTTPClient = NewBudgetedHTTPClient(server.Client(), budget)
			record, err := ExchangeCredential(t.Context(), opts, testOAuthRequest())
			if path == "/oauth/access_token" {
				var pause *PauseError
				require.ErrorAs(err, &pause)
				assert.Equal("high_usage", pause.Reason)
				assert.Equal(int32(1), fixture.calls.Load())
			} else {
				require.NoError(err)
				assert.NotEmpty(record.AccessToken())
				client, err := NewClient(ClientOptions{HTTPClient: server.Client(), BaseURL: server.URL, AccessToken: record.AccessToken(), Budget: budget})
				require.NoError(err)
				_, err = client.Me(t.Context())
				var pause *PauseError
				require.ErrorAs(err, &pause)
				assert.Equal("high_usage", pause.Reason)
				assert.Equal(int32(4), fixture.calls.Load())
			}
		})
	}
}
func TestThreadsOAuthBindsSourceBeforeLongExchange(t *testing.T) {
	fixture := newOAuthFixture(t)
	opts := fixture.options()
	called := false
	opts.WithOwnerLock = func(ctx context.Context, source string, exchange func() (TokenRecord, error)) (TokenRecord, error) {
		called = true
		assert.Equal(t, "threads:123:"+oauthTestOwner, source)
		assert.Equal(t, int32(1), fixture.calls.Load())
		return TokenRecord{}, errors.New("binding barrier")
	}
	_, err := ExchangeCredential(t.Context(), opts, testOAuthRequest())
	require.ErrorContains(t, err, "binding barrier")
	assert.True(t, called)
	assert.Equal(t, int32(1), fixture.calls.Load())
}
