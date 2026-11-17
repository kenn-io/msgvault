package beeper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Beeper's archive endpoint is an external contract: reserved characters in
// room IDs must never change which resource receives the whole-chat mutation.
func TestArchiveChatPath(t *testing.T) {
	for _, tc := range []struct {
		id, path string
		archived bool
	}{
		{"!room:example.test", "/v1/chats/%21room:example.test/archive", true},
		{"!room/with?query#fragment%:example.test", "/v1/chats/%21room%2Fwith%3Fquery%23fragment%25:example.test/archive", false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				assert.Equal(t, http.MethodPost, r.Method)
				assert.Equal(t, tc.path, r.URL.EscapedPath())
				assert.Empty(t, r.URL.RawQuery)
				var body map[string]bool
				if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&body)) {
					http.Error(w, "invalid fixture request", http.StatusBadRequest)
					return
				}
				assert.Equal(t, map[string]bool{"archived": tc.archived}, body)
				w.WriteHeader(http.StatusNoContent)
			}))
			t.Cleanup(server.Close)
			client := NewClient(server.URL, func(context.Context) (string, error) { return "fixture", nil }, 1000)
			require.NoError(t, client.ArchiveChat(t.Context(), tc.id, tc.archived))
			assert.Equal(t, 1, calls)
		})
	}
}
