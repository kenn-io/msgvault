package youtube

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

// Stable provider identities never depend on a display name or local profile.
func TestSourceIdentityProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		id := hegel.Draw(ht, hegel.Text())
		got := SourceIdentifier(id)
		valid, err := regexp.MatchString(`^[A-Za-z0-9_-]{1,256}$`, id)
		require.NoError(ht, err)
		if valid {
			assert.Equal(ht, "https://www.youtube.com/channel/"+id, got)
		} else {
			assert.Empty(ht, got)
		}
	}, hegel.WithTestCases(2000))
}

func fixtureClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c, err := NewClient("fixture-client", "fixture-secret", "fixture-refresh")
	require.NoError(t, err)
	c.baseURL = srv.URL
	c.refreshEndpoint = srv.URL + "/token"
	return c
}

func TestClientRefreshAndSafeErrors(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	tokens := 0
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			tokens++
			assertions.NoError(r.ParseForm())
			assertions.Equal("fixture-refresh", r.Form.Get("refresh_token"))
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		assertions.Equal("Bearer fixture-access", r.Header.Get("Authorization"))
		if r.URL.Path == "/error" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, `{"error":{"message":"fixture-secret","errors":[{"reason":"quotaExceeded"}]}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"items":[]}`)
	})
	var out struct {
		Items []Comment `json:"items"`
	}
	requirements.NoError(c.get(t.Context(), "/comments", url.Values{"parentId": {"p"}}, &out))
	err := c.get(t.Context(), "/error", nil, &out)
	requirements.ErrorIs(err, ErrQuota)
	assertions.NotContains(err.Error(), "fixture-secret")
	assertions.Equal(1, tokens)
}

func TestClientRejectsMissingCredentialsAndMalformedJSON(t *testing.T) {
	_, err := NewClient("", "x", "y")
	require.Error(t, err)
	c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
			return
		}
		_, _ = fmt.Fprint(w, "fixture-secret")
	})
	var out any
	err = c.get(t.Context(), "/comments", nil, &out)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "fixture-secret")
}

type clientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f clientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type failedResponseBody struct{}

func (failedResponseBody) Read([]byte) (int, error) { return 0, errors.New("fixture read failure") }
func (failedResponseBody) Close() error             { return nil }

func TestClientPreserves4xxWhenErrorBodyCannotBeRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c, err := NewClient("fixture-client", "fixture-secret", "fixture-refresh")
	require.NoError(err)
	c.baseURL = "https://api.example.test"
	c.accessToken = "fixture-access"
	c.expires = time.Now().Add(time.Hour)
	c.api = &http.Client{Transport: clientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		assert.Equal(http.MethodPost, req.Method)
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       failedResponseBody{},
			Request:    req,
		}, nil
	})}

	resp, err := c.request(t.Context(), http.MethodPost, "/comments", nil, nil)
	if resp != nil {
		require.NoError(resp.Body.Close())
	}

	var apiErr *APIError
	require.ErrorAs(err, &apiErr)
	assert.Equal(http.StatusBadRequest, apiErr.StatusCode)
	assert.Equal("unknown", apiErr.Reason)
	assert.NotErrorIs(err, ErrQuota)
}

func TestListingRejectsInvalidSuccessfulEnvelopes(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `{"error":{"code":500}}`, `{"items":null}`} {
		t.Run(body, func(t *testing.T) {
			c := fixtureClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = fmt.Fprint(w, `{"access_token":"fixture-access","expires_in":3600}`)
					return
				}
				_, _ = fmt.Fprint(w, body)
			})
			var out page[Comment]
			require.Error(t, c.get(t.Context(), "/comments", nil, &out))
		})
	}
}

func TestQuotaReasonCannotBeDowngradedByResourceReason(t *testing.T) {
	for _, body := range []string{
		`{"error":{"errors":[{"reason":"commentsDisabled"},{"reason":"quotaExceeded"}]}}`,
		`{"error":{"errors":[{"reason":"quotaExceeded"},{"reason":"commentsDisabled"}]}}`,
	} {
		assert.Equal(t, "quotaExceeded", safeReason([]byte(body)))
	}
}
