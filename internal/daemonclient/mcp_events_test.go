package daemonclient

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
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
