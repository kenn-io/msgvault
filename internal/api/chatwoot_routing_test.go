package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChatwootCommandsRouteThroughDaemon(t *testing.T) {
	for _, name := range []string{"add-chatwoot", "sync-chatwoot"} {
		t.Run(name, func(t *testing.T) {
			// The runner is an external process boundary. This test exercises HTTP admission
			// and streaming; importer behavior is exercised by CLI tests with a real Store.
			invoked := false
			srv := newCLIHandlerTestServer(&mockStore{runFunc: func(_ context.Context, req CLIRunRequest, emit func(CLIRunEvent) error) error {
				invoked = true
				assert.Equal(t, []string{name, "support"}, req.Args)
				return emit(CLIRunEvent{Type: "stdout", Data: "Chatwoot inboxes\n"})
			}})
			req := httptest.NewRequest(http.MethodPost, "/api/v1/cli/run", strings.NewReader(`{"args":["`+name+`","support"]}`))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			srv.Router().ServeHTTP(resp, req)
			requireNDJSONResponse(t, resp)
			assert.True(t, invoked)
			assert.Contains(t, resp.Body.String(), "Chatwoot inboxes")
		})
	}
}

func TestChatwootSchedulerSourcesShareAccountJob(t *testing.T) {
	expected := "chatwoot:https://chatwoot.example.com/support/accounts/9"
	for _, id := range []string{"https://chatwoot.example.com/support/accounts/9/inboxes/7", "https://chatwoot.example.com/support/accounts/9/inboxes/8"} {
		got, ok := SchedulerJobNameForSource("chatwoot", id)
		assert.True(t, ok)
		assert.Equal(t, expected, got)
	}
	for _, invalid := range []string{"support", "https://chatwoot.example.com/accounts/0/inboxes/7", "https://chatwoot.example.com/accounts/9/inboxes/0", "https://secret@chatwoot.example.com/accounts/9/inboxes/7"} {
		_, ok := SchedulerJobNameForSource("chatwoot", invalid)
		assert.False(t, ok)
	}
}
