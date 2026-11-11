package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMeetingDecisionOwnerAndDelegatedHTTP(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	var ids []int64
	for _, key := range []string{"first", "second", "third"} {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript"}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		requirements.NoError(err)
		ids = append(ids, id)
	}
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("reader", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}, time.Time{})
	requirements.NoError(err)
	body := fmt.Sprintf(`{"left":"msg:%d","right":"msg:%d","decision":"link"}`, ids[0], ids[1])
	for _, tc := range []struct {
		name  string
		owner bool
		want  int
	}{{"delegated", false, http.StatusUnauthorized}, {"owner", true, http.StatusOK}} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if tc.owner {
			req.Header.Set("X-Api-Key", "owner")
		} else {
			req.Header.Set(apiprotocol.AgentTokenHeader, token)
		}
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(tc.want, w.Code, tc.name+": "+w.Body.String())
		if tc.owner {
			assertions.JSONEq(fmt.Sprintf(`{"left":"msg:%d","right":"msg:%d","state":"link"}`, ids[0], ids[1]), w.Body.String())
		}
	}
	for _, tc := range []struct {
		left, right int64
		decision    string
		status      int
		state       string
	}{
		{ids[0], ids[1], "invalid", http.StatusBadRequest, ""},
		{ids[0], ids[0], "link", http.StatusBadRequest, ""},
		{ids[1], ids[2], "link", http.StatusOK, "link"},
		{ids[0], ids[2], "unlink", http.StatusConflict, ""},
	} {
		body := fmt.Sprintf(`{"left":"msg:%d","right":"msg:%d","decision":%q}`, tc.left, tc.right, tc.decision)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "owner")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(tc.status, w.Code, w.Body.String())
		if tc.state != "" {
			var result store.MeetingLinkDecisionResult
			requirements.NoError(json.Unmarshal(w.Body.Bytes(), &result))
			assertions.Equal(tc.state, result.State)
		}
	}
}

func TestMeetingDecisionScopedReadHidesOtherSource(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	visible, err := st.GetOrCreateSource("meeting_import", "visible@example.com")
	requirements.NoError(err)
	hidden, err := st.GetOrCreateSource("meeting_import", "hidden@example.com")
	requirements.NoError(err)
	ids := make([]int64, 0, 2)
	for i, source := range []int64{visible.ID, hidden.ID} {
		key := fmt.Sprintf("note-%d", i)
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source, SourceMessageID: key, MessageType: "meeting_transcript"}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		requirements.NoError(err)
		ids = append(ids, id)
	}
	refs := []store.MeetingLinkRef{{Kind: "msg", OccurrenceRef: store.OccurrenceRef{MessageID: ids[0]}}, {Kind: "msg", OccurrenceRef: store.OccurrenceRef{MessageID: ids[1]}}}
	_, err = st.DecideMeetingLinkContext(t.Context(), refs[0], refs[1], "link")
	requirements.NoError(err)
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	_, token, _, err := srv.agentGrants.Issue("restricted", []agentgrant.Permission{agentgrant.PermissionMessageRead}, []agentgrant.SourceRef{{ID: visible.ID, Type: visible.SourceType, Identifier: visible.Identifier}}, time.Time{})
	requirements.NoError(err)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("/api/v1/meetings/record?ref=msg:%d", ids[0]), nil)
	req.Header.Set(apiprotocol.AgentTokenHeader, token)
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	var record store.MeetingRecord
	requirements.NoError(json.Unmarshal(w.Body.Bytes(), &record))
	assertions.Equal([]int64{ids[0]}, record.MessageIDs)
}

func TestMeetingDecisionRejectsTwoReferencesToSameOccurrence(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gcal", "owner@example.com")
	requirements.NoError(err)
	persist := func(key, raw string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "calendar_event"}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "calendar"}, RawMIME: []byte(raw), RawFormat: "gcal_json"})
		requirements.NoError(err)
		return id
	}
	master := persist("master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	edit := persist("edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	srv := NewServerWithOptions(ServerOptions{Config: &config.Config{}, Store: st, Logger: testLogger()})
	body := fmt.Sprintf(`{"left":"evt:%d@20270314T100000Z","right":"evt:%d","decision":"link"}`, master, edit)
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	requirements.Equal(http.StatusBadRequest, w.Code, w.Body.String())
}
