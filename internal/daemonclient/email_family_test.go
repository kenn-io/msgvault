package daemonclient_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
)

func TestEmailFamilyClientRejectsOlderDaemon(t *testing.T) {
	familyRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/health" {
			_, _ = w.Write([]byte(`{"status":"ok","api_schema_version":"3.25.0"}`))
			return
		}
		familyRequests++
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "fixture-key", AllowInsecure: true})
	require.NoError(t, err)
	_, err = client.EmailFamilyContext(t.Context(), 1)
	require.ErrorContains(t, err, "require daemon API schema 3.26.0 or newer")
	assert.Zero(t, familyRequests)
}
