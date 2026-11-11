package mcp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventsNameGuardRejectsBeforeHandler(t *testing.T) {
	for _, tc := range []struct {
		name, header, body string
		status             int
		called             bool
	}{
		{"missing", "", `{"jsonrpc":"2.0","id":1,"method":"events/subscribe","params":{"name":"msgvault.messages.changed"}}`, 400, false},
		{"mismatch", "other", `{"jsonrpc":"2.0","id":1,"method":"events/subscribe","params":{"name":"msgvault.messages.changed"}}`, 400, false},
		{"duplicate", "msgvault.messages.changed", `{"jsonrpc":"2.0","id":1,"method":"events/subscribe","params":{"name":"msgvault.messages.changed","name":"other"}}`, 400, false},
		{"matching", "msgvault.messages.changed", `{"jsonrpc":"2.0","id":1,"method":"events/subscribe","params":{"name":"msgvault.messages.changed"}}`, 204, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := eventsNameHeaderHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", strings.NewReader(tc.body))
			r.Header.Set("Mcp-Protocol-Version", modernProtocolVersion)
			r.Header.Set("Mcp-Method", "events/subscribe")
			r.Header.Set("Mcp-Name", tc.header)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			assert.Equal(t, tc.status, w.Code)
			assert.Equal(t, tc.called, called)
			if !tc.called {
				assert.Contains(t, w.Body.String(), `-32020`)
			}
		})
	}
}
