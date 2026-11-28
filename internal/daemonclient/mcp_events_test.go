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
	assertions := assert.New(t)
	requirements := require.New(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertions.Equal("/api/v1/mcp-events/list", r.URL.Path)
		_, _ = w.Write([]byte(`{"enabled":true,"events":[]}`))
	}))
	defer server.Close()
	c, err := New(Config{URL: server.URL, AllowInsecure: true})
	requirements.NoError(err)
	defer func() { requirements.NoError(c.Close()) }()
	got, err := c.MCPEventsList(t.Context())
	requirements.NoError(err)
	assertions.True(got.Enabled)
}

func TestMCPEventsPreservesUnavailableReasons(t *testing.T) {
	for _, tc := range []struct {
		reason       string
		status, code int
	}{{"disabled", 503, -32010}, {"head_unavailable", 503, -32010}, {"store_unavailable", 503, -32010}, {"invalid_scope", 400, -32602}, {"subscriptions", http.StatusTooManyRequests, mcpevents.ResourceExhausted}, {"subscription", http.StatusNotFound, mcpevents.NotFound}, {"credential_unavailable", http.StatusServiceUnavailable, mcpevents.Unavailable}, {"challenge_failed", http.StatusBadGateway, -32027}, {"private_callback_detail", http.StatusBadGateway, -32027}, {"unknown_private_detail", http.StatusBadRequest, mcpevents.InvalidParams}} {
		reason := tc.reason
		t.Run(reason, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"code":%d,"reason":%q}`, tc.code, reason)
			}))
			defer server.Close()
			c, err := New(Config{URL: server.URL, AllowInsecure: true})
			requirements.NoError(err)
			defer func() { requirements.NoError(c.Close()) }()
			_, err = c.MCPEventsSubscribe(t.Context(), mcpevents.MCPEventsSubscribeRequest{})
			var eventErr *mcpevents.Error
			requirements.ErrorAs(err, &eventErr)
			assertions.Equal(tc.code, eventErr.Code)
			want := reason
			if reason == "private_callback_detail" {
				want = "challenge_failed"
			}
			if reason == "unknown_private_detail" {
				want = "request_rejected"
			}
			assertions.Equal(want, eventErr.Reason)
		})
	}
}
