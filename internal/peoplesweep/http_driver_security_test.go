package peoplesweep

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHTTPDriverRejectsProtectedAndUnsupportedFixedHeadersBeforeNetwork(t *testing.T) {
	tests := []struct {
		name   string
		header string
	}{
		{name: "authorization", header: "Authorization"},
		{name: "authorization case variant", header: "aUtHoRiZaTiOn"},
		{name: "Anthropic credential", header: "x-api-key"},
		{name: "Anthropic credential case variant", header: "X-API-Key"},
		{name: "Google credential", header: "x-goog-api-key"},
		{name: "content type", header: "Content-Type"},
		{name: "host", header: "Host"},
		{name: "unsupported custom header", header: "X-Custom-Header"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			var calls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			defer server.Close()

			_, err := newHTTPDriver(server.Client()).postWithHeaders(
				t.Context(), server.URL, ProviderProfile{Auth: AuthXAPIKey},
				NewCredential(AuthXAPIKey, "credential-secret-canary"),
				[]byte(`{"ok":true}`), map[string]string{test.header: "header-secret-canary"},
			)
			require.ErrorContains(err, "header")
			assert.NotContains(err.Error(), "header-secret-canary")
			assert.NotContains(err.Error(), "credential-secret-canary")
			assert.Zero(calls.Load())
		})
	}
}

func TestHTTPDriverRejectsCaseFoldedDuplicateFixedHeadersBeforeNetwork(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()

	_, err := newHTTPDriver(server.Client()).postWithHeaders(
		t.Context(), server.URL, ProviderProfile{Auth: AuthXAPIKey},
		NewCredential(AuthXAPIKey, "credential-secret-canary"),
		[]byte(`{"ok":true}`), map[string]string{
			"anthropic-version": "2023-06-01",
			"Anthropic-Version": "2023-06-01",
		},
	)
	require.EqualError(t, err, "inference provider request header is duplicated")
	assert.NotContains(t, err.Error(), "credential-secret-canary")
	assert.Zero(t, calls.Load())
}

func TestHTTPDriverRejectsInvalidFixedHeadersBeforeNetwork(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
	}{
		{name: "invalid name", headers: map[string]string{"anthropic version": "2023-06-01"}},
		{name: "invalid value", headers: map[string]string{"anthropic-version": "2023-06-01\r\nsecret-canary"}},
		{name: "empty value", headers: map[string]string{"anthropic-version": ""}},
		{name: "unsupported version", headers: map[string]string{"anthropic-version": "2024-secret-canary"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				calls.Add(1)
			}))
			defer server.Close()

			_, err := newHTTPDriver(server.Client()).postWithHeaders(
				t.Context(), server.URL, ProviderProfile{Auth: AuthXAPIKey},
				NewCredential(AuthXAPIKey, "credential-secret-canary"),
				[]byte(`{"ok":true}`), test.headers,
			)
			require.ErrorContains(t, err, "header")
			assert.NotContains(t, err.Error(), "secret-canary")
			assert.Zero(t, calls.Load())
		})
	}
}

func TestHTTPDriverAllowsOnlyCanonicalAnthropicVersionFixedHeader(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.Equal(t, "2023-06-01", r.Header.Get("Anthropic-Version"))
		assert.Equal(t, "credential-key", r.Header.Get("X-Api-Key"))
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		assert.Equal(t, []byte(`{"ok":true}`), body)
		_, err = io.WriteString(w, `{}`)
		assert.NoError(t, err)
	}))
	defer server.Close()

	_, err := newHTTPDriver(server.Client()).postWithHeaders(
		t.Context(), server.URL, ProviderProfile{Auth: AuthXAPIKey},
		NewCredential(AuthXAPIKey, "credential-key"),
		[]byte(`{"ok":true}`), map[string]string{"Anthropic-Version": "2023-06-01"},
	)
	require.NoError(t, err)
	assert.Equal(t, int64(1), calls.Load())
}
