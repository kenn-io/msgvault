package beeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Beeper's archive endpoint is an external contract: reserved characters in
// room IDs must never change which resource receives the whole-chat mutation.
func FuzzArchiveChatPath(f *testing.F) {
	f.Add("!room:example.test", true)
	f.Add("!room/with?query#fragment%:example.test", false)
	f.Fuzz(func(t *testing.T, chatID string, archived bool) {
		if chatID == "" || len(chatID) > 512 || !utf8.ValidString(chatID) {
			t.Skip()
		}
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/v1/chats/"+chatID+"/archive", r.URL.Path)
			assert.Empty(t, r.URL.RawQuery)
			var body map[string]bool
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, map[string]bool{"archived": archived}, body)
			w.WriteHeader(http.StatusNoContent)
		}))
		t.Cleanup(server.Close)
		client := NewClient(server.URL, func(context.Context) (string, error) { return "fixture", nil }, 1000)
		require.NoError(t, client.ArchiveChat(t.Context(), chatID, archived))
		assert.Equal(t, 1, calls)
	})
}
