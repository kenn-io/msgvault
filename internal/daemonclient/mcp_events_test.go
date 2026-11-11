package daemonclient

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mcpevents"
)

func TestMCPEventsListReadsDaemonEnabledState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/mcp-events/list", r.URL.Path)
		_, _ = w.Write([]byte(`{"enabled":true,"events":[]}`))
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	defer c.Close()
	got, err := c.MCPEventsList(t.Context())
	require.NoError(t, err)
	assert.True(t, got.Enabled)
}

func TestMCPEventsPreservesUnavailableReasons(t *testing.T) {
	for _, tc := range []struct {
		reason       string
		status, code int
	}{{"disabled", 503, -32010}, {"key_unavailable", 503, -32010}, {"head_unavailable", 503, -32010}, {"store_unavailable", 503, -32010}, {"invalid_scope", 400, -32602}} {
		reason := tc.reason
		t.Run(reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"code":%d,"reason":%q}`, tc.code, reason)
			}))
			defer server.Close()
			c, err := New(Config{URL: server.URL, AllowInsecure: true})
			require.NoError(t, err)
			defer c.Close()
			_, err = c.MCPEventsSubscribe(t.Context(), mcpevents.MCPEventsSubscribeRequest{})
			var eventErr *mcpevents.Error
			require.ErrorAs(t, err, &eventErr)
			assert.Equal(t, reason, eventErr.Reason)
		})
	}
}
