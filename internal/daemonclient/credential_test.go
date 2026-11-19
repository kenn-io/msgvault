package daemonclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCredentialBearerReachesOnlyTheConfiguredServer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	t.Cleanup(other.Close)
	var authorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization.Store(r.Header.Get("Authorization"))
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, AllowInsecure: true, Credential: func(context.Context) (string, error) {
		return "synthetic-bearer", nil
	}})
	require.NoError(err)

	_, err = client.Health(t.Context())
	require.ErrorContains(err, "does not follow redirects")
	assert.Equal("Bearer synthetic-bearer", authorization.Load())
	assert.Zero(elsewhere.Load(), "a redirect never carries the bearer to another server")

	refused := errors.New("synthetic refusal")
	client, err = New(Config{URL: other.URL, AllowInsecure: true, Credential: func(context.Context) (string, error) {
		return "", refused
	}})
	require.NoError(err)
	_, err = client.Health(t.Context())
	require.ErrorIs(err, refused)
	assert.Zero(elsewhere.Load(), "a failed credential sends no request")

	_, err = New(Config{URL: other.URL, AllowInsecure: true, APIKey: "synthetic-key", Credential: func(context.Context) (string, error) { return "", nil }})
	require.EqualError(err, "a credential excludes APIKey and AgentToken")
}
