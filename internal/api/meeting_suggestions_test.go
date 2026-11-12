package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestGrantedMeetingSuggestions(t *testing.T) {
	r := require.New(t)
	st := testutil.NewTestStore(t)
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	r.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	r.NoError(err)
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	note, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: notes.ID, SourceMessageID: "note", MessageType: "meeting_transcript", SentAt: sql.NullTime{Time: at, Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "note", ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
	r.NoError(err)
	event, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: calendar.ID, SourceMessageID: "event", MessageType: "calendar_event", Subject: sql.NullString{String: "Your verification code is 482913", Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"}, RawMIME: []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json"})
	r.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	sources := []agentgrant.SourceRef{{ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}, {ID: calendar.ID, Type: calendar.SourceType, Identifier: calendar.Identifier}}
	for _, tc := range []struct {
		name          string
		sources       []agentgrant.SourceRef
		ref           string
		status, count int
		title         string
	}{
		{"both", sources, fmt.Sprintf("msg:%d", note), 200, 1, "[verification content masked]"},
		{"notes only", sources[:1], fmt.Sprintf("msg:%d", note), 200, 0, ""},
		{"event denied", sources[:1], fmt.Sprintf("evt:%d", event), 404, 0, ""},
		{"calendar only", sources[1:], fmt.Sprintf("evt:%d", event), 200, 0, ""},
		{"empty scope", []agentgrant.SourceRef{{ID: 999999, Type: "meeting_import", Identifier: "missing@example.com"}}, fmt.Sprintf("msg:%d", note), 404, 0, ""},
		{"reverse", sources, fmt.Sprintf("evt:%d", event), 200, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := require.New(t)
			_, token, _, err := srv.agentGrants.Issue(tc.name, []agentgrant.Permission{agentgrant.PermissionMessageRead}, tc.sources, time.Time{})
			r.NoError(err)
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/suggestions?ref="+tc.ref, nil)
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			r.Equal(tc.status, w.Code, w.Body.String())
			if tc.status != 200 {
				return
			}
			var page store.MeetingSuggestionPage
			r.NoError(json.Unmarshal(w.Body.Bytes(), &page))
			r.Len(page.Suggestions, tc.count)
			if tc.count > 0 {
				r.Equal(tc.title, page.Suggestions[0].Title)
			}
		})
	}
}
