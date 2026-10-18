package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func meetingRecordAPINote(t *testing.T, st *store.Store, source int64, key string) int64 {
	t.Helper()
	requirements := require.New(t)
	conv, err := st.EnsureConversation(source, key, "Synthetic meeting")
	requirements.NoError(err)
	note, err := st.UpsertMessage(&store.Message{SourceID: source, ConversationID: conv, SourceMessageID: key, MessageType: "meeting_transcript", Subject: sql.NullString{String: "Synthetic meeting", Valid: true}, SentAt: sql.NullTime{Time: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(note, []byte(`{"summary_text":"Meeting evidence"}`), "meeting_json"))
	return note
}

func TestLinkUnlinkOwnerAccessMatrix(t *testing.T) {
	for _, mode := range []AuthMode{AuthModeAPIKey, AuthModeDelegated, AuthModeRemoteClient, AuthModeSession, AuthModeLoopback, AuthModeCaller} {
		t.Run(string(mode), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
			requirements.NoError(err)
			conv, err := st.EnsureConversation(source.ID, "event", "Planning")
			requirements.NoError(err)
			event, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "event", MessageType: "calendar_event"})
			requirements.NoError(err)
			requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(`{"id":"event","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), "gcal_json"))
			note := meetingRecordAPINote(t, st, source.ID, "note")
			srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
			t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
			allowed := mode == AuthModeAPIKey || mode == AuthModeCaller
			for _, method := range []string{http.MethodPost, http.MethodDelete} {
				body := bytes.NewBufferString(fmt.Sprintf(`{"message_id":%d}`, note))
				req := httptest.NewRequest(method, "/api/v1/meetings/ref/links", body)
				req.Header.Set("Content-Type", "application/json")
				req.SetPathValue("ref", fmt.Sprintf("evt:%d", event))
				req.SetPathValue("message_id", strconv.FormatInt(note, 10))
				req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{auth: requestAuthentication{Mode: mode}}))
				w := httptest.NewRecorder()
				if method == http.MethodPost {
					srv.handleLinkMeeting(w, req)
				} else {
					srv.handleUnlinkMeeting(w, req)
				}
				if allowed {
					requirements.Equal(http.StatusOK, w.Code, w.Body.String())
					var record store.MeetingRecord
					requirements.NoError(json.Unmarshal(w.Body.Bytes(), &record))
					if method == http.MethodPost {
						assertions.Len(record.Items, 1)
					} else {
						assertions.Empty(record.Items)
					}
				} else {
					assertions.Equal(http.StatusForbidden, w.Code, w.Body.String())
					var count int
					requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM meeting_link_decisions`).Scan(&count))
					assertions.Zero(count)
				}
			}
		})
	}
}

func TestMeetingRecordWritesValidateBeforeMutation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	note := meetingRecordAPINote(t, st, source.ID, "note")
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	for _, tc := range []struct{ ref, body string }{
		{fmt.Sprintf("msg:%d", note), fmt.Sprintf(`{"message_id":%d}`, note)},
		{"evt:0", fmt.Sprintf(`{"message_id":%d}`, note)},
		{"evt:1", `{"message_id":0}`}, {"evt:1", `{"message_id":1.5}`}, {"evt:1", `{"message_id":null}`},
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/meetings/ref/links", bytes.NewBufferString(tc.body))
		req.SetPathValue("ref", tc.ref)
		req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{auth: requestAuthentication{Mode: AuthModeAPIKey}}))
		w := httptest.NewRecorder()
		srv.handleLinkMeeting(w, req)
		assertions.Equal(http.StatusBadRequest, w.Code, w.Body.String())
	}
	var count int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM meeting_link_decisions`).Scan(&count))
	assertions.Zero(count)
}

func TestMeetingRecordGeneratedClientAcceptsSparseResponses(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(source.ID, "ordinary", "Ordinary event")
	requirements.NoError(err)
	event, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "ordinary", MessageType: "calendar_event"})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(`{"id":"ordinary","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`), "gcal_json"))
	note := meetingRecordAPINote(t, st, source.ID, "standalone")
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	for _, ref := range []string{fmt.Sprintf("msg:%d", note), fmt.Sprintf("evt:%d", event)} {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/meetings/"+ref, nil)
		req.Header.Set("X-Api-Key", "owner")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(http.StatusOK, w.Code, w.Body.String())
		var record generated.MeetingRecord
		requirements.NoError(json.Unmarshal(w.Body.Bytes(), &record))
		requirements.NoError(record.Validate())
		if strings.HasPrefix(ref, "msg:") {
			requirements.Len(record.Items, 1)
			assertions.Empty(record.Items[0].Basis)
			assertions.Empty(record.Items[0].Origin)
			continue
		}
		assertions.Empty(record.OwnerRsvp)
		requirements.NotNil(record.EventDetail)
		assertions.Empty(record.EventDetail.Location)
		assertions.Empty(record.EventDetail.Description)
	}
}

func TestMeetingWritesAndSuggestionsRequireQualifiedSeriesRef(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	conv, err := st.EnsureConversation(src.ID, "weekly", "Weekly planning")
	requirements.NoError(err)
	event, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conv, SourceMessageID: "weekly", MessageType: "calendar_event"})
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(event, []byte(`{"id":"weekly","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=3"]}`), "gcal_json"))
	note := meetingRecordAPINote(t, st, src.ID, "note")
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{Server: config.ServerConfig{APIKey: "owner"}}, Store: st, Logger: testLogger()})
	t.Cleanup(func() { require.NoError(t, srv.Shutdown(context.Background())) })
	ref := fmt.Sprintf("evt:%d", event)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/api/v1/meetings/ref", bytes.NewBufferString(fmt.Sprintf(`{"message_id":%d}`, note)))
		req.SetPathValue("ref", ref)
		req.SetPathValue("message_id", strconv.FormatInt(note, 10))
		req = req.WithContext(context.WithValue(req.Context(), requestSecurityContextKey{}, requestSecurity{auth: requestAuthentication{Mode: AuthModeAPIKey}}))
		w := httptest.NewRecorder()
		switch method {
		case http.MethodGet:
			srv.handleSuggestMeetingLinks(w, req)
		case http.MethodPost:
			srv.handleLinkMeeting(w, req)
		case http.MethodDelete:
			srv.handleUnlinkMeeting(w, req)
		}
		assertions.Equal(http.StatusBadRequest, w.Code, w.Body.String())
	}
}
