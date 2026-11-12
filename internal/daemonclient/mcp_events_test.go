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
		reason, want string
		code, status int
	}{
		{"disabled", "disabled", mcpevents.Unavailable, http.StatusServiceUnavailable},
		{"credential_unavailable", "credential_unavailable", mcpevents.Unavailable, http.StatusServiceUnavailable},
		{"head_unavailable", "head_unavailable", mcpevents.Unavailable, http.StatusServiceUnavailable},
		{"store_unavailable", "store_unavailable", mcpevents.Unavailable, http.StatusServiceUnavailable},
		{"verification_changed", "verification_changed", mcpevents.Unavailable, http.StatusServiceUnavailable},
		{"source_scope_unavailable", "source_scope_unavailable", mcpevents.Unavailable, http.StatusServiceUnavailable},
		{"invalid_scope", "invalid_scope", mcpevents.InvalidParams, http.StatusBadRequest},
		{"subscriptions", "subscriptions", mcpevents.ResourceExhausted, http.StatusTooManyRequests},
		{"permission_denied", "permission_denied", mcpevents.Forbidden, http.StatusForbidden},
		{"person_scope_exceeds_grant", "person_scope_exceeds_grant", mcpevents.Forbidden, http.StatusForbidden},
		{"private_daemon_detail", "request_rejected", mcpevents.Forbidden, http.StatusForbidden},
		{"dns_error", "dns_error", mcpevents.CallbackEndpoint, http.StatusBadGateway},
		{"subscription", "subscription", mcpevents.NotFound, http.StatusNotFound},
		{"challenge_failed", "challenge_failed", mcpevents.CallbackEndpoint, http.StatusBadGateway},
		{"unknown_private_detail", "request_rejected", mcpevents.InvalidParams, http.StatusBadRequest},
		{"private_callback_detail", "challenge_failed", mcpevents.CallbackEndpoint, http.StatusBadGateway},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"code":%d,"reason":%q}`, tc.code, tc.reason)
			}))
			defer server.Close()
			client, err := New(Config{URL: server.URL, AllowInsecure: true})
			require.NoError(err)
			defer func() { require.NoError(client.Close()) }()
			_, err = client.MCPEventsSubscribe(t.Context(), mcpevents.MCPEventsSubscribeRequest{})
			var eventErr *mcpevents.Error
			require.ErrorAs(err, &eventErr)
			assert.Equal(tc.code, eventErr.Code)
			assert.Equal(tc.want, eventErr.Reason)
		})
	}
}
