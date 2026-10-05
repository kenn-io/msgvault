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
