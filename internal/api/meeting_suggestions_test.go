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
	}{
		{"both", sources, fmt.Sprintf("msg:%d", note), 200, 1},
		{"event denied", sources[:1], fmt.Sprintf("evt:%d", event), 404, 0},
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
		})
	}
	t.Run("limit", func(t *testing.T) {
		for _, limit := range []string{"0", "-1", "101", "bad"} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/suggestions?ref=msg:%d&limit=%s", note, limit), nil)
			req.Header.Set("Authorization", "Bearer owner")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		}
	})
	t.Run("title mask", func(t *testing.T) {
		r := require.New(t)
		title := "482913"
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: notes.ID, SourceMessageID: title, MessageType: "meeting_transcript", Subject: sql.NullString{String: title, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: title, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		r.NoError(err)
		_, limited, _, err := srv.agentGrants.Issue("limited", []agentgrant.Permission{agentgrant.PermissionMessageRead}, sources, time.Time{})
		r.NoError(err)
		for _, tc := range []struct {
			name, token string
			reveal      bool
		}{{"limited", limited, false}, {"owner", "", true}} {
			t.Run(tc.name, func(t *testing.T) {
				ref := fmt.Sprintf("msg:%d", id)
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/suggestions?ref="+ref+"&limit=1", nil)
				if tc.token == "" {
					req.Header.Set("Authorization", "Bearer owner")
				} else {
					req.Header.Set(apiprotocol.AgentTokenHeader, tc.token)
				}
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())
				var page store.MeetingSuggestionPage
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
				require.Len(t, page.Suggestions, 1)
				item := page.Suggestions[0]
				if tc.reveal {
					require.Equal(t, "Your verification code is 482913", item.Title)
				} else {
					require.Equal(t, "[verification content masked]", item.Title)
				}
			})
		}
	})
}
