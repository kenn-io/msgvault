package googlereviews

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func TestGoogleExactEndpoints(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.FromRegex(`[a-zA-Z0-9_-]{1,256}`, true))
		account := hegel.Draw(ht, hegel.FromRegex(`[a-zA-Z0-9_-]{1,256}`, true))
		location := hegel.Draw(ht, hegel.FromRegex(`[a-zA-Z0-9_-]{1,256}`, true))
		parent := "accounts/" + account + "/locations/" + location
		g := NewGoogle(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			assert.Equal(ht, "https", r.URL.Scheme)
			assert.Equal(ht, "mybusiness.googleapis.com", r.URL.Host)
			assert.Equal(ht, "/v4/"+parent+"/reviews/"+id, r.URL.Path)
			return response(200, `{"reviewId":"`+id+`","reviewer":{"isAnonymous":true},"starRating":"FUTURE","reviewReply":{"comment":"Reply","reviewReplyState":"FUTURE","policyViolation":"FUTURE"}}`), nil
		})})
		v, err := g.Get(t.Context(), parent+"/reviews/"+id)
		require.NoError(ht, err)
		assert.Equal(ht, id, v.ReviewID)
		assert.True(ht, v.Reviewer.IsAnonymous)
		assert.Equal(ht, "FUTURE", v.ReviewReply.ReviewReplyState)
	})
}
func TestGoogleRejectsInvalidBeforeHTTP(t *testing.T) {
	require := require.New(t)
	calls := 0
	g := NewGoogle(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(200, `{}`), nil })})
	for _, name := range []string{"accounts/a/locations/l/reviews/../r", "https://evil.example/r", "accounts/a/locations/l/reviews/%2f"} {
		_, err := g.Get(t.Context(), name)
		require.Error(err)
	}
	_, err := g.UpdateReply(t.Context(), "accounts/a/locations/l/reviews/r", strings.Repeat("é", 2049))
	require.ErrorIs(err, ErrInvalid)
	_, err = g.List(t.Context(), "accounts/a/locations/l", "", 51)
	require.ErrorIs(err, ErrInvalid)
	assert.Zero(t, calls)
}
func TestGoogleReadRetryAndWriteOnce(t *testing.T) {
	for _, tc := range []struct {
		status, readCalls int
		readErr, writeErr error
	}{
		{401, 1, ErrCredentials, ErrCredentials}, {403, 1, ErrPermission, ErrPermission},
		{404, 1, ErrNotFound, ErrNotFound}, {429, 3, ErrQuota, ErrQuota},
		{500, 3, ErrTemporary, ErrUnknown}, {502, 3, ErrTemporary, ErrUnknown},
		{503, 3, ErrTemporary, ErrUnknown}, {504, 3, ErrTemporary, ErrUnknown},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			calls := 0
			g := NewGoogle(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return response(tc.status, "PRIVATE PROVIDER BODY"), nil
			})})
			g.sleep = func(context.Context, time.Duration) error { return nil }
			_, err := g.Get(t.Context(), "accounts/a/locations/l/reviews/r")
			require.ErrorIs(err, tc.readErr)
			assert.Equal(tc.readCalls, calls)
			assert.NotContains(err.Error(), "PRIVATE")
			calls = 0
			_, err = g.UpdateReply(t.Context(), "accounts/a/locations/l/reviews/r", "Thanks")
			require.ErrorIs(err, tc.writeErr)
			assert.Equal(1, calls)
		})
	}
}
func TestReadBackoffBoundsAndCancellation(t *testing.T) {
	assert := assert.New(t)
	now := time.Now()
	for _, header := range []string{"9223372036854775807", "-1", "bad", "999999", "3", now.Add(time.Hour).UTC().Format(http.TimeFormat)} {
		delay := retryDelay(header, 0, now)
		assert.GreaterOrEqual(delay, time.Duration(0))
		assert.LessOrEqual(delay, 5*time.Second)
		if header == "9223372036854775807" {
			assert.Equal(5*time.Second, delay)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, waitRetry(ctx, time.Hour), context.Canceled)
}
func TestGoogleWriteAmbiguity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	for _, body := range []string{`{`, `null`, `{}`, `{"comment":"wrong"}`, strings.Repeat("x", 2<<20+1)} {
		calls := 0
		g := NewGoogle(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { calls++; return response(200, body), nil })})
		_, err := g.UpdateReply(t.Context(), "accounts/a/locations/l/reviews/r", "Thanks")
		require.ErrorIs(err, ErrUnknown)
		assert.Equal(1, calls)
	}
	g := NewGoogle(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("PRIVATE URL") })})
	_, err := g.UpdateReply(t.Context(), "accounts/a/locations/l/reviews/r", "Thanks")
	require.ErrorIs(err, ErrUnknown)
	assert.NotContains(err.Error(), "PRIVATE")
}
func TestGoogleListAndReplyWire(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	g := NewGoogle(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet {
			assert.Equal("20", r.URL.Query().Get("pageSize"))
			assert.Equal("updateTime desc", r.URL.Query().Get("orderBy"))
			assert.Equal("next+/=", r.URL.Query().Get("pageToken"))
			return response(200, `{"reviews":[{"reviewId":"r","reviewer":{"isAnonymous":true},"starRating":"FIVE"}],"nextPageToken":"next","totalReviewCount":2}`), nil
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(err)
		assert.JSONEq(`{"comment":"Thank you é"}`, string(body))
		assert.Equal("/v4/accounts/a/locations/l/reviews/r/reply", r.URL.Path)
		return response(200, `{"comment":"Thank you é","updateTime":"now"}`), nil
	})})
	page, err := g.List(t.Context(), "accounts/a/locations/l", "next+/=", 0)
	require.NoError(err)
	require.Len(page.Reviews, 1)
	assert.Empty(page.Reviews[0].Comment)
	assert.Equal("next", page.NextPageToken)
	assert.Equal(int64(2), page.TotalReviewCount)
	reply, err := g.UpdateReply(t.Context(), "accounts/a/locations/l/reviews/r", "Thank you é")
	require.NoError(err)
	assert.Equal("now", reply.UpdateTime)
}

func TestGoogleNestedModerationWire(t *testing.T) {
	for _, state := range []string{"REJECTED", "FUTURE_STATE"} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			g := NewGoogle(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return response(200, `{"reviews":[{"reviewId":"r","reviewer":{"isAnonymous":true},"reviewReply":{"comment":"Pending reply","reviewReplyState":"`+state+`","policyViolation":"FUTURE_POLICY"}}]}`), nil
			})})
			page, err := g.List(t.Context(), "accounts/a/locations/l", "", 20)
			require.NoError(err)
			require.Len(page.Reviews, 1)
			require.NotNil(page.Reviews[0].ReviewReply)
			assert.Equal(state, page.Reviews[0].ReviewReply.ReviewReplyState)
			assert.Equal("FUTURE_POLICY", page.Reviews[0].ReviewReply.PolicyViolation)
		})
	}
}
