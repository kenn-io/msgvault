package daemonclient

import (
	"net/http"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
)

// Remote mode must keep the daemon's is_from_me flag on message details.
// Dropping it would make every message read through a daemon look incoming.
func TestRemoteMessageDetailKeepsIsFromMe(t *testing.T) {
	for _, fromMe := range []bool{true, false} {
		t.Run(map[bool]string{true: "sent", false: "received"}[fromMe], func(t *testing.T) {
			assert := Assert.New(t)
			require := Require.New(t)
			client := newGeneratedClientAdapterStore(t, func(w http.ResponseWriter, r *http.Request) {
				assert.Equal("/api/v1/messages/42", r.URL.Path)
				writeJSONResponse(t, w, map[string]any{"id": 42, "source_id": 9, "conversation_id": 7, "source_message_id": "synthetic", "subject": "Synthetic", "from": "sender@example.test", "to": []string{}, "sent_at": "2026-10-06T12:00:00Z", "snippet": "", "labels": []string{}, "body": "", "attachments": []any{}, "is_from_me": fromMe})
			})
			stored, err := client.GetMessageContext(t.Context(), 42)
			require.NoError(err)
			require.NotNil(stored)
			assert.Equal(fromMe, stored.IsFromMe, "store adapter")
			detail, err := NewEngineAdapter(client).GetMessage(t.Context(), 42)
			require.NoError(err)
			require.NotNil(detail)
			assert.Equal(fromMe, detail.IsFromMe, "engine adapter")
		})
	}
}
