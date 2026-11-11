package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMeetingDecisionOwnerHTTP(t *testing.T) {
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
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger(), OperationGate: NewSerialOperationGate()})
	t.Cleanup(srv.agentGrants.Close)
	body := fmt.Sprintf(`{"left":"msg:%d","right":"msg:%d","decision":"link"}`, ids[0], ids[1])
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Key", "owner")
	w := httptest.NewRecorder()
	srv.Router().ServeHTTP(w, req)
	requirements.Equal(http.StatusOK, w.Code, w.Body.String())
	assertions.JSONEq(fmt.Sprintf(`{"left":"msg:%d","right":"msg:%d","state":"link"}`, ids[0], ids[1]), w.Body.String())
	ref := func(id int64) store.MeetingLinkRef {
		return store.MeetingLinkRef{Kind: "msg", MessageID: id}
	}
	_, err = st.DecideMeetingLinkContext(t.Context(), ref(ids[1]), ref(ids[2]), "link")
	requirements.NoError(err)
	calendar, err := st.GetOrCreateSource("gcal", "owner@example.com")
	requirements.NoError(err)
	persist := func(key, raw string) int64 {
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: calendar.ID, SourceMessageID: key, MessageType: "calendar_event"}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "calendar"}, RawMIME: []byte(raw), RawFormat: "gcal_json"})
		requirements.NoError(err)
		return id
	}
	master := persist("master", `{"id":"series","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"recurrence":["RRULE:FREQ=WEEKLY;COUNT=2"]}`)
	edit := persist("edit", `{"id":"edit","recurringEventId":"series","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"status":"cancelled"}`)
	for _, tc := range []struct {
		left, right, decision string
		status                int
		contains              string
	}{
		{fmt.Sprintf("msg:%d", ids[0]), fmt.Sprintf("msg:%d", ids[1]), "invalid", http.StatusBadRequest, ""},
		{fmt.Sprintf("msg:%d", ids[0]), fmt.Sprintf("msg:%d", ids[0]), "link", http.StatusBadRequest, ""},
		{fmt.Sprintf("msg:%d", ids[0]), fmt.Sprintf("msg:%d", ids[2]), "unlink", http.StatusConflict, ""},
		{fmt.Sprintf("evt:%d@20270314T100000Z", master), fmt.Sprintf("evt:%d", edit), "link", http.StatusBadRequest, store.ErrMeetingLinkSameCopy.Error()},
	} {
		body := fmt.Sprintf(`{"left":%q,"right":%q,"decision":%q}`, tc.left, tc.right, tc.decision)
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Api-Key", "owner")
		w := httptest.NewRecorder()
		srv.Router().ServeHTTP(w, req)
		requirements.Equal(tc.status, w.Code, w.Body.String())
		if tc.contains != "" {
			assertions.Contains(w.Body.String(), tc.contains)
		}
	}
}

func TestMeetingDecisionConcurrentOwnerHTTP(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL transaction contract")
	}
	source, err := st.GetOrCreateSource("meeting_import", "owner@example.com")
	requirements.NoError(err)
	var refs []store.MeetingLinkRef
	for i := range 4 {
		key := fmt.Sprintf("concurrent-%d", i)
		id, err := st.PersistMessage(&store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: key, MessageType: "meeting_transcript"}, Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting"}, RawMIME: []byte(`{}`), RawFormat: "meeting_json"})
		requirements.NoError(err)
		refs = append(refs, store.MeetingLinkRef{Kind: "msg", MessageID: id})
	}
	cfg := &config.Config{HomeDir: t.TempDir(), Data: config.DataConfig{DataDir: t.TempDir()}, Server: config.ServerConfig{APIKey: "owner", AgentAccess: true}}
	srv := NewServerWithOptions(ServerOptions{Config: cfg, Store: st, Logger: testLogger()})
	t.Cleanup(srv.agentGrants.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	run := func(blocked int, pairs [][2]int, decisions []string) []int {
		blocker, err := st.DB().BeginTx(ctx, nil)
		requirements.NoError(err)
		defer func() { _ = blocker.Rollback() }()
		var pid int
		requirements.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
		var id int64
		requirements.NoError(blocker.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=$1 FOR UPDATE`, refs[blocked].MessageID).Scan(&id))
		done := make(chan int, 2)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for i, pair := range pairs {
			workers.Go(func() {
				body := fmt.Sprintf(`{"left":"msg:%d","right":"msg:%d","decision":%q}`, refs[pair[0]].MessageID, refs[pair[1]].MessageID, decisions[i])
				req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/v1/meetings/links/decisions", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Api-Key", "owner")
				w := httptest.NewRecorder()
				srv.Router().ServeHTTP(w, req)
				done <- w.Code
			})
			for {
				var count int
				requirements.NoError(st.DB().QueryRowContext(ctx, `WITH RECURSIVE waiting(pid) AS (SELECT pid FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) UNION SELECT a.pid FROM pg_stat_activity a JOIN waiting w ON w.pid=ANY(pg_blocking_pids(a.pid))) SELECT count(*) FROM waiting`, pid).Scan(&count))
				if count >= i+1 {
					break
				}
				select {
				case <-ticker.C:
				case <-ctx.Done():
					requirements.FailNow("requests did not overlap")
				}
			}
		}
		requirements.NoError(blocker.Commit())
		return []int{<-done, <-done}
	}
	statuses := run(1, [][2]int{{0, 1}, {2, 3}}, []string{"link", "link"})
	requirements.Equal([]int{http.StatusOK, http.StatusOK}, statuses)
}
