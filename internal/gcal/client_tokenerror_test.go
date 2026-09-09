package gcal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// unauthorizedTokenSource mimics a service account whose domain-wide
// delegation does not cover the requested scope: Google answers the token
// exchange with 401 unauthorized_client before any API request is made.
type unauthorizedTokenSource struct{ calls int }

func (u *unauthorizedTokenSource) Token() (*oauth2.Token, error) {
	u.calls++
	return nil, &oauth2.RetrieveError{
		Response: &http.Response{StatusCode: http.StatusUnauthorized},
		Body:     []byte(`{"error":"unauthorized_client"}`),
	}
}

// A credential failure must surface immediately with Google's error text —
// not be retried as a network error for ten minutes of backoff.
func TestRequest_TokenSourceErrorFailsFast(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("API must not be reached when the token exchange fails")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ts := &unauthorizedTokenSource{}
	c := NewClient(ts, WithBaseURL(srv.URL))

	start := time.Now()
	_, err := c.ListCalendars(context.Background(), "")
	require.Error(t, err)

	var rerr *oauth2.RetrieveError
	assert.True(t, errors.As(err, &rerr), "the oauth2 RetrieveError must be preserved in the chain")
	assert.Contains(t, err.Error(), "unauthorized_client")
	assert.Equal(t, 1, ts.calls, "no retry on a credential error")
	assert.Less(t, time.Since(start), 2*time.Second, "must not back off")
}

// "API not enabled in this project" arrives as a 403 in domain usageLimits —
// the same domain as real quota errors — but must not be retried.
func TestRequest_AccessNotConfiguredFailsFast(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"Google Calendar API has not been used in project 123 before or it is disabled.","errors":[{"message":"...","domain":"usageLimits","reason":"accessNotConfigured"}],"status":"PERMISSION_DENIED"}}`))
	}))
	defer srv.Close()

	c := NewClient(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t"}))
	c.baseURL = srv.URL
	start := time.Now()
	_, err := c.ListCalendars(context.Background(), "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not been used in project")
	assert.Equal(t, 1, calls, "no retry on accessNotConfigured")
	assert.Less(t, time.Since(start), 2*time.Second)
}
