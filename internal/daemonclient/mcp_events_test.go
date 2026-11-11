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

func TestMCPEventsKeepsSafeInvalidScopeReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":-32602,"reason":"invalid_scope"}`))
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, AllowInsecure: true})
	require.NoError(t, err)
	defer c.Close()
	_, err = c.MCPEventsSubscribe(t.Context(), mcpevents.MCPEventsSubscribeRequest{})
	var eventErr *mcpevents.Error
	require.ErrorAs(t, err, &eventErr)
	assert.Equal(t, "invalid_scope", eventErr.Reason)
}

func TestMCPEventsPreservesUnavailableReasons(t *testing.T) {
	for _, reason := range []string{"disabled", "key_unavailable", "head_unavailable", "store_unavailable"} {
		t.Run(reason, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = fmt.Fprintf(w, `{"code":-32010,"reason":%q}`, reason)
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
