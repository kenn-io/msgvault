package api

import (
	"encoding/json/v2"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorRecoveryHint(t *testing.T) {
	for _, hint := range []string{"", "run msgvault search"} {
		response := httptest.NewRecorder()
		writeErrorHint(response, 404, "message_not_found", "missing", hint)
		var got map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &got))
		assert.Equal(t, "message_not_found", got["error"])
		if hint == "" {
			assert.NotContains(t, got, "hint")
		} else {
			assert.Equal(t, hint, got["hint"])
		}
	}
}

func TestCLICompleteEventOmitsErrorMetadata(t *testing.T) {
	data, err := json.Marshal(CLIRunEvent{Type: "complete"})
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	assert.Equal(t, map[string]any{"type": "complete"}, got)
}
