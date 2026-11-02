package daemonclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetMessageWithBodyRejectsProviderIDCollision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/cli/message", r.URL.Path, "path")
		assert.Equal("42", r.URL.Query().Get("id"), "requested local ID")
		writeJSONResponse(t, w, map[string]any{
			"id":                99,
			"source_message_id": "42",
			"subject":           "Unrelated provider-ID collision",
			"body_text":         "unrelated message body",
		})
	}))
	defer srv.Close()

	message, err := NewEngineAdapter(newTestStore(srv, "")).GetMessageWithBody(t.Context(), 42)
	require.NoError(err)
	assert.Nil(message, "a provider-ID collision must not return another local message")
}
