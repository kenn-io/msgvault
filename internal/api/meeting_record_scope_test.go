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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMeetingRecordInheritedVerificationSubject(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	calendar, err := st.GetOrCreateSource("gcal", "owner@example.com")
	require.NoError(err)
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	require.NoError(err)
	persist := func(source int64, key, kind, conversationType, subject, format, raw string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source, SourceMessageID: key, MessageType: kind, Subject: sql.NullString{String: subject, Valid: subject != ""}}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: conversationType}, RawMIME: []byte(raw), RawFormat: format})
		require.NoError(err)
		return id
	}
	persist(calendar.ID, "master", "calendar_event", "calendar", "Your verification code is 482913", "gcal_json", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	persist(calendar.ID, "cancelled", "calendar_event", "calendar", "", "gcal_json", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	note := persist(notes.ID, "note", "meeting_transcript", "meeting", "", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	sources := []agentgrant.SourceRef{{ID: calendar.ID, Type: calendar.SourceType, Identifier: calendar.Identifier}, {ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}}
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		want        string
	}{
		{"restricted", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionCalendarEventRead}, "[verification content masked]"},
		{"verification", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionCalendarEventRead, agentgrant.PermissionVerificationRead}, "Your verification code is 482913"},
	} {
		_, token, _, err := srv.agentGrants.Issue(tc.name, tc.permissions, sources, time.Time{})
		require.NoError(err)
		ref := fmt.Sprintf("msg:%d", note)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/record?ref="+ref, nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		var record store.MeetingRecord
		require.NoError(json.Unmarshal(w.Body.Bytes(), &record))
		require.Len(record.Links, 1)
		assert.Equal(tc.want, record.Links[0].Subject, "%s %s", tc.name, ref)
	}
	explicit := persist(calendar.ID, "explicit", "calendar_event", "calendar", "Ordinary exception", "gcal_json", `{"id":"explicit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled","summary":"Ordinary exception"}`)
	persist(calendar.ID, "ordinary-master", "calendar_event", "calendar", "Ordinary meeting", "gcal_json", `{"id":"ordinary","iCalUID":"ordinary@example.org","start":{"dateTime":"2027-04-01T10:00:00Z"},"end":{"dateTime":"2027-04-01T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	ordinary := persist(calendar.ID, "ordinary-cancel", "calendar_event", "calendar", "", "gcal_json", `{"id":"ordinary-cancel","recurringEventId":"ordinary","originalStartTime":{"dateTime":"2027-04-01T10:00:00Z"},"status":"cancelled"}`)
	body := persist(calendar.ID, "body-cancel", "calendar_event", "calendar", "", "gcal_json", `{"id":"body-cancel","recurringEventId":"ordinary","originalStartTime":{"dateTime":"2027-04-08T10:00:00Z"},"status":"cancelled"}`)
	require.NoError(st.UpsertMessageBody(body, sql.NullString{String: "Your verification code is 482913", Valid: true}, sql.NullString{}))
	_, token, _, err := srv.agentGrants.Issue("ordinary-reader", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionCalendarEventRead}, sources, time.Time{})
	require.NoError(err)
	for _, tc := range []struct {
		id   int64
		want string
	}{{explicit, "Ordinary exception"}, {ordinary, "Ordinary meeting"}, {body, "[verification content masked]"}} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/record?ref=evt:%d", tc.id), nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, token)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(http.StatusOK, w.Code, w.Body.String())
		var record store.MeetingRecord
		require.NoError(json.Unmarshal(w.Body.Bytes(), &record))
		require.Len(record.Links, 1)
		assert.Equal(tc.want, record.Links[0].Subject)
	}
}

func TestMeetingRecordPartialHTTP(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: &partialMeetingRecordStore{mockStore: &mockStore{}}, Logger: testLogger()})
	for _, tc := range []struct {
		ref    string
		status int
	}{
		{"evt:42", http.StatusBadRequest},
		{"evt:999999", http.StatusNotFound},
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/record?ref="+tc.ref, nil)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		require.Equal(tc.status, response.Code, response.Body.String())
		if tc.status == http.StatusBadRequest {
			assert.Contains(response.Body.String(), "invalid_argument")
			assert.Contains(response.Body.String(), "calendar events")
		}
		if tc.status == http.StatusNotFound {
			assert.Contains(response.Body.String(), "use msg:<id> for notes or copy REF from calendar events")
		}
	}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/record?ref=msg:9007199254740992", nil)
	response := httptest.NewRecorder()
	srv.Router().ServeHTTP(response, request)
	assert.Equal(http.StatusBadRequest, response.Code, response.Body.String())
	assert.Contains(response.Body.String(), "invalid meeting reference")
}

func TestMeetingRecordSeriesRequiresOccurrenceHTTP(t *testing.T) {
	r := require.New(t)
	st := testutil.NewTestStore(t)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	r.NoError(err)
	r.NoError(st.UpdateSourceSyncConfig(t.Context(), calendar.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	series, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: calendar.ID, SourceMessageID: "series", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "series", ConversationType: "calendar"},
		RawMIME:      []byte(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`), RawFormat: "gcal_json",
	})
	r.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/record?ref=evt:%d", series), nil)
	req.Header.Set("Authorization", "Bearer owner")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	r.Equal(http.StatusBadRequest, w.Code, w.Body.String())
	r.Contains(w.Body.String(), "copy the full occurrence reference from calendar events")
}

type partialMeetingRecordStore struct {
	*mockStore
}

func (s *partialMeetingRecordStore) MeetingRecordContext(_ context.Context, ref store.MeetingLinkRef) (store.MeetingRecord, error) {
	if ref.MessageID == 999999 {
		return store.MeetingRecord{}, sql.ErrNoRows
	}
	if ref.MessageID == 9007199254740992 {
		return store.MeetingRecord{}, nil
	}
	return store.MeetingRecord{}, store.ErrMeetingOccurrenceRequired
}

func TestMeetingRecordMessageReadSourceScope(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	calendar, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceSyncConfig(t.Context(), calendar.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	persist := func(source int64, key, kind, conversationType, format, raw string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source, SourceMessageID: key, MessageType: kind}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: conversationType}, RawMIME: []byte(raw), RawFormat: format})
		requirements.NoError(err)
		return id
	}
	event := persist(calendar.ID, "event", "calendar_event", "calendar", "gcal_json", `{"id":"event","iCalUID":"meeting@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	notesScope := agentgrant.SourceRef{ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}
	_, token, _, err := srv.agentGrants.Issue("event outside scope", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionCalendarEventRead}, []agentgrant.SourceRef{notesScope}, time.Time{})
	requirements.NoError(err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/record?ref=evt:%d", event), nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	requirements.Equal(http.StatusNotFound, w.Code, w.Body.String())
}

func TestMeetingRecordMessageOnlyAgentSeesOwnerChoices(t *testing.T) {
	r := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	r.NoError(err)
	add := func(key string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript"},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"},
			RawMIME:      []byte(`{}`), RawFormat: "meeting_json",
		})
		r.NoError(err)
		return id
	}
	first, second := add("first"), add("second")
	_, err = st.DecideMeetingLinkContext(t.Context(), store.MeetingLinkRef{Kind: "msg", MessageID: first}, store.MeetingLinkRef{Kind: "msg", MessageID: second}, "link")
	r.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("notes-reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	r.NoError(err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/record?ref=msg:%d", first), nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	r.Equal(http.StatusOK, w.Code, w.Body.String())
	var record store.MeetingRecord
	r.NoError(json.Unmarshal(w.Body.Bytes(), &record))
	assert.Equal(t, []int64{first, second}, record.MessageIDs)
	assert.Empty(t, record.Links)
}
