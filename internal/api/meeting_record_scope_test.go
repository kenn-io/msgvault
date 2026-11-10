package api

import (
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
	"go.kenn.io/msgvault/internal/calrecur"
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
	master := persist(calendar.ID, "master", "calendar_event", "calendar", "Your verification code is 482913", "gcal_json", `{"id":"series","iCalUID":"uid@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	cancelled := persist(calendar.ID, "cancelled", "calendar_event", "calendar", "", "gcal_json", `{"id":"cancelled","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	note := persist(notes.ID, "note", "meeting_transcript", "meeting", "", "twenty_json", `{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"uid@example.org","startsAt":"2027-03-14T10:00:00Z"}}`)
	other, err := st.GetOrCreateSource("meeting_import", "other@example.com")
	require.NoError(err)
	outside := persist(other.ID, "outside", "meeting_transcript", "meeting", "", "meeting_json", `{}`)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Engine: query.NewEngine(st.DB(), st.IsPostgreSQL()), Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	sources := []agentgrant.SourceRef{{ID: calendar.ID, Type: calendar.SourceType, Identifier: calendar.Identifier}, {ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}}
	for _, tc := range []struct {
		name        string
		permissions []agentgrant.Permission
		want        string
	}{
		{"restricted", []agentgrant.Permission{agentgrant.PermissionMessageRead}, "[verification content masked]"},
		{"verification", []agentgrant.Permission{agentgrant.PermissionMessageRead, agentgrant.PermissionVerificationRead}, "Your verification code is 482913"},
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
	_, restricted, _, err := srv.agentGrants.Issue("notes-only", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: notes.ID, Type: notes.SourceType, Identifier: notes.Identifier}}, time.Time{})
	require.NoError(err)
	for _, tc := range []struct {
		ref    string
		status int
	}{{fmt.Sprintf("msg:%d", note), http.StatusOK}, {fmt.Sprintf("evt:%d", cancelled), http.StatusNotFound}, {fmt.Sprintf("evt:%d", master), http.StatusNotFound}, {fmt.Sprintf("msg:%d", outside), http.StatusNotFound}} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/record?ref="+tc.ref, nil)
		req.Header.Set(apiprotocol.AgentTokenHeader, restricted)
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		require.Equal(tc.status, w.Code, w.Body.String())
		if tc.status == http.StatusOK {
			var record store.MeetingRecord
			require.NoError(json.Unmarshal(w.Body.Bytes(), &record))
			assert.Empty(record.Links)
		}
	}
	explicit := persist(calendar.ID, "explicit", "calendar_event", "calendar", "Ordinary exception", "gcal_json", `{"id":"explicit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-21T10:00:00Z"},"status":"cancelled","summary":"Ordinary exception"}`)
	persist(calendar.ID, "ordinary-master", "calendar_event", "calendar", "Ordinary meeting", "gcal_json", `{"id":"ordinary","iCalUID":"ordinary@example.org","start":{"dateTime":"2027-04-01T10:00:00Z"},"end":{"dateTime":"2027-04-01T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	ordinary := persist(calendar.ID, "ordinary-cancel", "calendar_event", "calendar", "", "gcal_json", `{"id":"ordinary-cancel","recurringEventId":"ordinary","originalStartTime":{"dateTime":"2027-04-01T10:00:00Z"},"status":"cancelled"}`)
	body := persist(calendar.ID, "body-cancel", "calendar_event", "calendar", "", "gcal_json", `{"id":"body-cancel","recurringEventId":"ordinary","originalStartTime":{"dateTime":"2027-04-08T10:00:00Z"},"status":"cancelled"}`)
	require.NoError(st.UpsertMessageBody(body, sql.NullString{String: "Your verification code is 482913", Valid: true}, sql.NullString{}))
	_, token, _, err := srv.agentGrants.Issue("ordinary-reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, sources, time.Time{})
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
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.test")
	require.NoError(err)
	id, err := st.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "series", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "series", ConversationType: "calendar"},
		RawMIME:      []byte(`{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`), RawFormat: "gcal_json",
	})
	require.NoError(err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger()})
	for _, tc := range []struct {
		ref    string
		status int
	}{
		{fmt.Sprintf("evt:%d@20270314T100000Z", id), http.StatusOK},
		{"evt:999999", http.StatusNotFound},
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/record?ref="+tc.ref, nil)
		response := httptest.NewRecorder()
		srv.Router().ServeHTTP(response, request)
		require.Equal(tc.status, response.Code, response.Body.String())
		if tc.status == http.StatusOK {
			assert.JSONEq(fmt.Sprintf(`{"links":[],"message_ids":[],"unexpandable":[{"message_id":%d,"reason":"unsupported recurrence"}]}`, id), response.Body.String())
		}
	}
}

func TestParseMeetingLinkRef(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	for _, tc := range []struct {
		input string
		want  store.MeetingLinkRef
	}{
		{"msg:5", store.MeetingLinkRef{Kind: "msg", MessageID: 5}},
		{"evt:9@20270314", store.MeetingLinkRef{Kind: "evt", MessageID: 9, Identity: calrecur.Identity{Date: "2027-03-14", AllDay: true}}},
		{"evt:10@20270314T100000Z", store.MeetingLinkRef{Kind: "evt", MessageID: 10, Identity: calrecur.Identity{Instant: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC)}}},
	} {
		ref, err := parseMeetingLinkRef(tc.input)
		require.NoError(err)
		assert.Equal(tc.want, ref)
	}
	for _, input := range []string{"msg:5@20270314", "evt:9@", "evt:9@20270230", "evt:0", "other:5"} {
		_, err := parseMeetingLinkRef(input)
		require.Error(err, input)
	}
}

func TestMeetingRecordRequiresSeriesOccurrence(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.test")
	require.NoError(t, err)
	notes, err := st.GetOrCreateSource("meeting_import", "notes@example.test")
	require.NoError(t, err)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger()})
	for _, tc := range []struct {
		name, raw    string
		keys, starts []string
		unsupported  bool
	}{
		{"timed", `{"id":"timed","iCalUID":"timed@example.test","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, []string{"20270314T100000Z", "20270321T100000Z"}, []string{"2027-03-14T10:00:00Z", "2027-03-21T10:00:00Z"}, false},
		{"all-day", `{"id":"all-day","iCalUID":"all-day@example.test","start":{"date":"2027-03-14"},"end":{"date":"2027-03-15"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`, []string{"20270314", "20270321"}, []string{"2027-03-14T00:00:00Z", "2027-03-21T00:00:00Z"}, false},
		{"unsupported", `{"id":"unsupported","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=YEARLY;BYWEEKNO=20"]}`, []string{"20270314T100000Z"}, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: tc.name, MessageType: "calendar_event"}, Conversation: &store.ConversationPersistData{SourceConversationID: tc.name, ConversationType: "calendar"}, RawMIME: []byte(tc.raw), RawFormat: "gcal_json"})
			require.NoError(t, err)
			var noteIDs []int64
			for i, start := range tc.starts {
				key := fmt.Sprintf("%s-%d", tc.name, i)
				raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"%s@example.test","startsAt":"%s"}}`, tc.name, start)
				note, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: notes.ID, SourceMessageID: key, MessageType: "meeting_transcript"}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(raw), RawFormat: "twenty_json"})
				require.NoError(t, err)
				noteIDs = append(noteIDs, note)
			}
			request := func(ref string) *httptest.ResponseRecorder {
				req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/meetings/record?ref="+ref, nil)
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				return w
			}
			bare := request(fmt.Sprintf("evt:%d", id))
			require.Equal(t, http.StatusBadRequest, bare.Code, bare.Body.String())
			assert.Contains(t, bare.Body.String(), "invalid_argument")
			assert.Contains(t, bare.Body.String(), "calendar events")
			for i, key := range tc.keys {
				explicit := request(fmt.Sprintf("evt:%d@%s", id, key))
				require.Equal(t, http.StatusOK, explicit.Code, explicit.Body.String())
				var record store.MeetingRecord
				require.NoError(t, json.Unmarshal(explicit.Body.Bytes(), &record))
				if tc.unsupported {
					assert.Len(t, record.Unexpandable, 1)
					continue
				}
				require.Len(t, record.Links, 1)
				assert.Equal(t, fmt.Sprintf("evt:%d@%s", id, key), record.Links[0].Ref)
				assert.Equal(t, []int64{noteIDs[i]}, record.MessageIDs)
			}
		})
	}
}
