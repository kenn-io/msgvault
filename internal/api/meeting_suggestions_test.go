package api

import (
	"context"
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
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type suggestionClassificationStore struct {
	*store.Store
	counts map[int64]int
}

func (s *suggestionClassificationStore) PopulateMessageBodiesContext(ctx context.Context, messages []store.APIMessage) error {
	for _, message := range messages {
		s.counts[message.ID]++
	}
	return s.Store.PopulateMessageBodiesContext(ctx, messages)
}

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
	t.Run("embedded source scope", func(t *testing.T) {
		for _, tc := range []struct {
			sources []int64
			count   int
		}{{[]int64{notes.ID, calendar.ID}, 1}, {[]int64{notes.ID}, 0}} {
			ctx := sourcescope.WithSourceIDs(t.Context(), tc.sources)
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("/api/v1/meetings/suggestions?ref=msg:%d", note), nil)
			w := httptest.NewRecorder()
			srv.Handler(nil).ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			var page store.MeetingSuggestionPage
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &page))
			require.Len(t, page.Suggestions, tc.count)
		}
	})
	t.Run("limit", func(t *testing.T) {
		for _, limit := range []string{"0", "-1", "101", "bad"} {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/suggestions?ref=msg:%d&limit=%s", note, limit), nil)
			req.Header.Set("Authorization", "Bearer owner")
			w := httptest.NewRecorder()
			srv.Router().ServeHTTP(w, req)
			require.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		}
	})
}

func TestGrantedMeetingSuggestionsTitleWords(t *testing.T) {
	r := require.New(t)
	st := testutil.NewTestStore(t)
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	r.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	r.NoError(err)
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	ids := make([]int64, 0, 2)
	for _, title := range []string{"standup", "482913"} {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: notes.ID, SourceMessageID: title, MessageType: "meeting_transcript", Subject: sql.NullString{String: title, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: title, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		r.NoError(err)
		ids = append(ids, id)
	}
	event, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: calendar.ID, SourceMessageID: "event", MessageType: "calendar_event", Subject: sql.NullString{String: "Your verification code is 482913", Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"}, RawMIME: []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json"})
	r.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	spy := &suggestionClassificationStore{Store: st, counts: map[int64]int{}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: spy, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	sources := []agentgrant.SourceRef{{ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}, {ID: calendar.ID, Type: calendar.SourceType, Identifier: calendar.Identifier}}
	_, limited, _, err := srv.agentGrants.Issue("limited", []agentgrant.Permission{agentgrant.PermissionMessageRead}, sources, time.Time{})
	r.NoError(err)
	_, reveal, _, err := srv.agentGrants.Issue("reveal", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionVerificationRead}, sources, time.Time{})
	r.NoError(err)
	for _, tc := range []struct {
		name, token string
		reveal      bool
	}{{"limited", limited, false}, {"verification", reveal, true}, {"owner", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, ref := range []string{fmt.Sprintf("msg:%d", ids[1]), fmt.Sprintf("evt:%d", event)} {
				t.Run(ref, func(t *testing.T) {
					clear(spy.counts)
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
					if !tc.reveal {
						require.NotEmpty(t, spy.counts)
					}
					for _, count := range spy.counts {
						require.Equal(t, 1, count)
					}
					if tc.reveal {
						require.Equal(t, 1, item.TitleTokens)
						if ref[0:3] == "evt" {
							require.Equal(t, ids[1], item.MemberMessageID)
						} else {
							require.Equal(t, "Your verification code is 482913", item.Title)
						}
					} else {
						require.Zero(t, item.TitleTokens)
						if ref[0:3] == "evt" {
							require.Equal(t, ids[0], item.MemberMessageID)
						} else {
							require.Equal(t, "[verification content masked]", item.Title)
						}
					}
				})
			}
		})
	}
}

func TestGrantedMeetingSuggestionsReadableTitleRanking(t *testing.T) {
	r := require.New(t)
	st := testutil.NewTestStore(t)
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	r.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	r.NoError(err)
	at := time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)
	var planning int64
	for _, title := range []string{"Budget", "Planning"} {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: notes.ID, SourceMessageID: title, MessageType: "meeting_transcript", Subject: sql.NullString{String: title, Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: title, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		r.NoError(err)
		if title == "Planning" {
			planning = id
		}
	}
	event, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: calendar.ID, SourceMessageID: "event", MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "event", ConversationType: "calendar"}, RawMIME: []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), RawFormat: "gcal_json"})
	r.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	sources := []agentgrant.SourceRef{{ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}, {ID: calendar.ID, Type: calendar.SourceType, Identifier: calendar.Identifier}}
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, sources, time.Time{})
	r.NoError(err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/suggestions?ref=evt:%d&limit=1", event), nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	r.Equal(http.StatusOK, w.Code, w.Body.String())
	var page store.MeetingSuggestionPage
	r.NoError(json.Unmarshal(w.Body.Bytes(), &page))
	r.Len(page.Suggestions, 1)
	r.Equal(planning, page.Suggestions[0].MemberMessageID)
	r.Equal(1, page.Suggestions[0].TitleTokens)
}
