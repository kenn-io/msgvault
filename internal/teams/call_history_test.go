package teams

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestArchivedTeamsCallCanBeImported(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	raw := `{"id":"event","chatId":"chat","messageType":"systemEventMessage","createdDateTime":"2026-10-01T10:00:00Z","lastModifiedDateTime":"2026-10-01T10:00:00Z","from":null,"body":{"contentType":"html","content":"<systemEventMessage/>"},"eventDetail":{"@odata.type":"#microsoft.graph.callEndedEventMessageDetail","callId":"call","callDuration":"PT45S","callEventType":"call","callParticipants":[{"participant":{"user":{"id":"peer"}}}]},"future":"preserved"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if selfChatAbsent(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/me/chats":
			_, _ = w.Write([]byte(`{"value":[{"id":"chat","chatType":"oneOnOne"}]}`))
		case strings.HasSuffix(r.URL.Path, "/members"):
			_, _ = w.Write([]byte(`{"value":[{"id":"member","userId":"peer","email":"peer@example.com","displayName":"Example Peer"}]}`))
		case r.URL.Path == "/me/chats/chat/messages":
			_, _ = w.Write([]byte(`{"value":[` + raw + `]}`))
		default:
			assertions.Fail("unexpected Graph request", r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	defer srv.Close()
	archive := testutil.NewTestStore(t)
	client := NewClient(srv.URL, func(context.Context) (string, error) { return "synthetic", nil }, 50)
	_, err := NewImporter(archive, client).Import(t.Context(), ImportOptions{Email: "owner@example.com"})
	requirements.NoError(err)
	var messageID, peer int64
	requirements.NoError(archive.DB().QueryRow(`SELECT id FROM messages WHERE message_type = 'teams'`).Scan(&messageID))
	requirements.NoError(archive.DB().QueryRow(`SELECT id FROM participants WHERE email_address = 'peer@example.com'`).Scan(&peer))
	stored, err := archive.GetMessageRaw(messageID)
	requirements.NoError(err)
	assertions.Equal(raw, string(stored))
	// This is the raw payload the existing export-eml/raw endpoint serves.
	request := callhistory.ImportRequest{Format: "teams", Account: "owner@example.com", Record: stored, Bindings: map[string]int64{"peer": peer}}
	envelope, err := json.Marshal(request)
	requirements.NoError(err)
	o, err := callhistory.DecodeImport(strings.NewReader(string(envelope)))
	requirements.NoError(err)
	assertions.Equal([]callhistory.Participant{{ID: peer, Participation: "joined"}}, o.Participants)
	assertions.Equal("unknown", o.Outcome)
	source, err := archive.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	requirements.NoError(err)
	for range 2 {
		_, err = archive.PutCallObservationContext(t.Context(), source.ID, o)
		requirements.NoError(err)
	}
}
