package line

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/lineapi"
)

const testChannel = "2000000001"
const testUser = "U00000000000000000000000000000002"
const testGroup = "C00000000000000000000000000000001"

func TestRetryableEventDoesNotHideJoinedArchiveFailures(t *testing.T) {
	checks := assert.New(t)
	provider := retryEvent(errors.New("synthetic provider failure"))
	archive := errors.New("synthetic archive failure")
	checks.True(retryableEvent(provider))
	checks.True(retryableEvent(fmt.Errorf("wrapped: %w", provider)))
	checks.False(retryableEvent(errors.Join(provider, archive)))
	checks.False(retryableEvent(fmt.Errorf("wrapped: %w", errors.Join(provider, archive))))
}

func TestGroupSummaryPermanentErrorPreservesArchivedMessage(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			path := "/v2/bot/group/" + testGroup + "/summary"
			permanent := status == http.StatusForbidden || status == http.StatusNotFound
			count := 1
			if permanent {
				count = 3
			}
			for i := range count {
				f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: status})
				f.enqueue(t, f.message(strconv.Itoa(i+1), "archived text", f.now.UnixMilli()+int64(i)), time.Time{})
			}
			f.run(t)
			for i := range count {
				checks.Equal("archived text", f.archived(t, strconv.Itoa(i+1)).body)
			}
			var state string
			must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
			want := "retry"
			if permanent {
				want = "processed"
				checks.Equal(1, f.api.Count(http.MethodGet, path))
				inbox, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
				must.NoError(err)
				checks.Zero(inbox.Pending)
			}
			checks.Equal(want, state)
		})
	}
}

type inboxFixture struct {
	st          *store.Store
	source      *store.Source
	api         *lineapi.Server
	client      *Client
	processor   *Processor
	now         time.Time
	attachments string
}

func newInboxFixture(t *testing.T) *inboxFixture {
	t.Helper()
	fake := lineapi.New()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	f := &inboxFixture{st: testutil.NewTestStore(t), api: fake, now: time.Now().UTC().Truncate(time.Second), attachments: t.TempDir()}
	f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
	var err error
	f.source, err = Register(t.Context(), f.st, f.client, testChannel)
	require.NoError(t, err)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	return f
}
func (f *inboxFixture) enqueue(t *testing.T, e Event, received time.Time) {
	t.Helper()
	raw := []byte(e.Raw)
	if len(raw) == 0 {
		var err error
		raw, err = json.Marshal(e)
		require.NoError(t, err)
	}
	clean, err := SanitizeEvent(raw)
	require.NoError(t, err)
	target := ""
	if e.Message != nil {
		target = e.Message.ID
	}
	if e.Unsend != nil {
		target = e.Unsend.MessageID
	}
	if received.IsZero() {
		received = f.now
	}
	require.NoError(t, f.st.EnqueueLineEventsContext(t.Context(), f.source.ID, f.source.Identifier, []store.LineWebhookEvent{{WebhookEventID: e.WebhookEventID, EventType: e.Type, TargetMessageID: target, EventTimestamp: e.Timestamp, RawJSON: string(clean), ReceivedAt: received}}))
}
func (f *inboxFixture) message(id, text string, stamp int64) Event {
	return Event{Type: "message", Timestamp: stamp, WebhookEventID: "event-" + id, Source: Source{Type: "group", GroupID: testGroup, UserID: testUser}, Message: &Message{Type: "text", ID: id, Text: text}}
}
func (f *inboxFixture) run(t *testing.T) {
	t.Helper()
	require.NoError(t, f.processor.Process(t.Context()))
}

type archivedMessageState struct {
	id      int64
	body    string
	edited  bool
	deleted bool
	sent    time.Time
}

func (f *inboxFixture) archived(t *testing.T, key string) archivedMessageState {
	t.Helper()
	var id int64
	var edited bool
	var deleted, sent sql.NullTime
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT id,is_edited,deleted_from_source_at,sent_at FROM messages WHERE source_id=? AND source_message_id=?`), f.source.ID, key).Scan(&id, &edited, &deleted, &sent))
	var body sql.NullString
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
	return archivedMessageState{id: id, body: body.String, edited: edited, deleted: deleted.Valid, sent: sent.Time}
}

func TestProcessorMessageTypes(t *testing.T) {
	for _, item := range []struct {
		name string
		m    Message
		body string
	}{{"text", Message{Type: "text", Text: "  synthetic\ntext  "}, "  synthetic\ntext  "}, {"sticker", Message{Type: "sticker", Text: "hello", Keywords: []string{"wave"}}, "[Sticker]\nhello\nwave"}, {"location", Message{Type: "location", Title: "Example place", Address: "Example address", Latitude: 1.25, Longitude: -2.5}, "Example place\nExample address\n1.25,-2.5"}} {
		t.Run(item.name, func(t *testing.T) {
			checks := assert.New(t)

			f := newInboxFixture(t)
			e := f.message("1", "", f.now.UnixMilli())
			item.m.ID = "1"
			e.Message = &item.m
			f.enqueue(t, e, time.Time{})
			f.run(t)
			archived := f.archived(t, "1")
			body, edited, deleted, sent := archived.body, archived.edited, archived.deleted, archived.sent
			checks.Equal(item.body, body)
			checks.False(edited)
			checks.False(deleted)
			checks.Equal(e.Timestamp, sent.UnixMilli())
			var mine bool
			require.NoError(t, f.st.DB().QueryRowContext(t.Context(), `SELECT is_from_me FROM messages`).Scan(&mine))
			checks.False(mine)
		})
	}
}

func TestProcessorQuoteResolvesWhenParentArrives(t *testing.T) {
	f := newInboxFixture(t)
	parent := f.message("1", "parent", f.now.UnixMilli())
	child := f.message("2", "child", f.now.UnixMilli()+1)
	require.NoError(t, json.Unmarshal([]byte(fmt.Sprintf(`{"type":"message","timestamp":%d,"webhookEventId":"event-2","source":{"type":"group","groupId":"C00000000000000000000000000000001","userId":"U00000000000000000000000000000002"},"message":{"id":"2","type":"text","text":"child","quotedMessageId":"1"}}`, child.Timestamp)), &child))
	for _, e := range []Event{child, parent} {
		f.enqueue(t, e, time.Time{})
		f.run(t)
	}
	parentID := f.archived(t, "1").id
	childID := f.archived(t, "2").id
	var reply sql.NullInt64
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), childID).Scan(&reply))
	assert.Equal(t, parentID, reply.Int64)
}

func TestProcessorInterruptedPassSettlesCompletedConversations(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	groupY := "C00000000000000000000000000000002"
	userY := "U00000000000000000000000000000003"
	ctx, cancel := context.WithCancel(jobctx.WithProgress(t.Context()))
	defer cancel()
	var interrupted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/group/"+groupY+"/member/") && !interrupted.Swap(true) {
			cancel()
			<-r.Context().Done()
			return
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))

	parent := f.message("repair-parent", "parent", f.now.UnixMilli())
	child := f.message("repair-child", "child", f.now.UnixMilli()+1)
	must.NoError(json.Unmarshal([]byte(fmt.Sprintf(`{"type":"message","timestamp":%d,"webhookEventId":"event-repair-child","source":{"type":"group","groupId":"C00000000000000000000000000000001","userId":"U00000000000000000000000000000002"},"message":{"id":"repair-child","type":"text","text":"child","quotedMessageId":"repair-parent"}}`, child.Timestamp)), &child))
	f.enqueue(t, parent, time.Time{})
	f.enqueue(t, child, time.Time{})
	f.enqueue(t, Event{
		Type: "memberJoined", Timestamp: f.now.UnixMilli() + 2, WebhookEventID: "cancel-group-y",
		Source: Source{Type: "group", GroupID: groupY},
		Joined: &Members{Members: []Source{{Type: "user", UserID: userY}}},
	}, time.Time{})

	must.ErrorIs(f.processor.Process(ctx), context.Canceled)
	checks.True(jobctx.HasProgress(ctx), "committed text must trigger cache refresh even when enrichment is canceled")
	checks.Equal("child", f.archived(t, "repair-child").body)
	status, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending, "an interrupted pass settles and finishes the events it completed")

	var parentID, childID, conversationID int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT id,conversation_id FROM messages WHERE source_id=? AND source_message_id=?`), f.source.ID, "repair-parent").Scan(&parentID, &conversationID))
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), f.source.ID, "repair-child").Scan(&childID))
	var reply sql.NullInt64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), childID).Scan(&reply))
	checks.True(reply.Valid)
	checks.Equal(parentID, reply.Int64)
	var messageCount int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), conversationID).Scan(&messageCount))
	checks.Equal(int64(2), messageCount)

	f.now = f.now.Add(time.Minute)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	status, err = f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
}

func TestProcessorRejectsSourceBoundToAnotherChannel(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	f.enqueue(t, f.message("wrong-channel", "synthetic", f.now.UnixMilli()), time.Time{})
	p := NewProcessor(f.st, f.client, f.source.ID, "2000000002", f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	must.Error(p.Process(t.Context()))
	status, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending)
	count, err := f.st.CountMessagesForSource(f.source.ID)
	must.NoError(err)
	checks.Zero(count)
}

func TestProcessorEditMarkerFailureRollsBackBody(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	base := f.message("1", "original", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)
	if f.st.IsPostgreSQL() {
		_, err := f.st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_line_edit_marker() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic edit marker failure'; END $$`)
		must.NoError(err)
		_, err = f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_line_edit_marker BEFORE UPDATE ON messages FOR EACH ROW
 WHEN (NEW.is_edited AND NOT OLD.is_edited) EXECUTE FUNCTION fail_line_edit_marker()`)
		must.NoError(err)
	} else {
		_, err := f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER fail_line_edit_marker BEFORE UPDATE ON messages
 WHEN NEW.is_edited AND NOT OLD.is_edited BEGIN SELECT RAISE(ABORT,'synthetic edit marker failure'); END`)
		must.NoError(err)
	}
	edit := base
	edit.Type = "messageEdited"
	edit.WebhookEventID = "edit-marker-failure"
	edit.Timestamp++
	edit.Message = &Message{ID: "1", Type: "text", Text: "edited"}
	f.enqueue(t, edit, time.Time{})
	must.ErrorContains(f.processor.Process(t.Context()), "synthetic edit marker failure")
	archived := f.archived(t, "1")
	checks.Equal("original", archived.body)
	checks.False(archived.edited)
}

func TestProcessorLatestEditWins(t *testing.T) {
	checks := assert.New(t)
	f := newInboxFixture(t)
	latestCounts := func() (int64, int64) {
		t.Helper()
		var added, updated sql.NullInt64
		require.NoError(t, f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_added,messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&added, &updated))
		return added.Int64, updated.Int64
	}
	base := f.message("1", "original", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)
	added, updated := latestCounts()
	checks.Equal(int64(1), added)
	checks.Zero(updated)
	for _, update := range []struct {
		id, text string
		offset   int64
	}{{"edit-new", "latest", 30}, {"edit-old", "older", 10}, {"edit-middle", "middle", 20}} {
		e := base
		e.Type = "messageEdited"
		e.WebhookEventID = update.id
		e.Timestamp += update.offset
		e.Message = &Message{ID: "1", Type: "text", Text: update.text}
		f.enqueue(t, e, time.Time{})
		f.run(t)
		added, updated = latestCounts()
		checks.Zero(added)
		if update.id == "edit-new" {
			checks.Equal(int64(1), updated)
		} else {
			checks.Zero(updated)
		}
		checks.Equal("latest", f.archived(t, "1").body, "delivery %s must preserve the latest edit before original replay", update.id)
	}
	replay := base
	replay.WebhookEventID = "delayed-original"
	f.enqueue(t, replay, time.Time{})
	f.run(t)
	added, updated = latestCounts()
	checks.Zero(added, "replaying an existing message is not a new addition")
	checks.Zero(updated, "unchanged enrichment must preserve the cache update watermark")
	archived := f.archived(t, "1")
	body, edited, sent := archived.body, archived.edited, archived.sent
	checks.Equal("latest", body)
	checks.True(edited)
	checks.Equal(base.Timestamp, sent.UnixMilli())
	for _, order := range [][]string{{"z", "a"}, {"a", "z"}} {
		t.Run(strings.Join(order, ""), func(t *testing.T) {
			f := newInboxFixture(t)
			base := f.message("1", "original", f.now.UnixMilli())
			f.enqueue(t, base, time.Time{})
			f.run(t)
			for _, key := range order {
				e := base
				e.Type = "messageEdited"
				e.Timestamp++
				e.WebhookEventID = "edit-" + key
				e.Message = &Message{ID: "1", Type: "text", Text: key}
				f.enqueue(t, e, time.Time{})
				f.run(t)
			}
			archived := f.archived(t, "1")
			checks := assert.New(t)
			checks.Equal("z", archived.body)
			checks.True(archived.edited)
		})
	}
}

func TestProcessorInterruptedEditPassCommitsBeforeFinalizationFailure(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	base := f.message("1", "original", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)

	edit := base
	edit.Type = "messageEdited"
	edit.WebhookEventID = "edit-1"
	edit.Timestamp++
	edit.Message = &Message{ID: "1", Type: "text", Text: "edited"}
	f.enqueue(t, edit, time.Time{})
	if f.st.IsPostgreSQL() {
		_, err := f.st.DB().ExecContext(t.Context(), `CREATE FUNCTION fail_line_finalize() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic finalization failure'; END $$`)
		must.NoError(err)
		_, err = f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER line_finalize_fails BEFORE UPDATE OF message_count ON conversations FOR EACH ROW EXECUTE FUNCTION fail_line_finalize()`)
		must.NoError(err)
	} else {
		_, err := f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER line_finalize_fails BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT,'synthetic finalization failure'); END`)
		must.NoError(err)
	}
	must.Error(f.processor.Process(t.Context()))
	checks.Equal("edited", f.archived(t, "1").body, "the edit commits before finalization fails")
	status, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending)
	if f.st.IsPostgreSQL() {
		_, err = f.st.DB().ExecContext(t.Context(), `DROP TRIGGER line_finalize_fails ON conversations`)
	} else {
		_, err = f.st.DB().ExecContext(t.Context(), `DROP TRIGGER line_finalize_fails`)
	}
	must.NoError(err)
	if f.st.IsPostgreSQL() {
		_, err = f.st.DB().ExecContext(t.Context(), `CREATE FUNCTION reject_line_message_rewrite() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'completed edit must not rewrite messages'; END $$`)
		must.NoError(err)
		_, err = f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_line_message_rewrite BEFORE UPDATE ON messages FOR EACH ROW EXECUTE FUNCTION reject_line_message_rewrite()`)
		must.NoError(err)
	} else {
		_, err = f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_line_message_rewrite BEFORE UPDATE ON messages BEGIN SELECT RAISE(ABORT,'completed edit must not rewrite messages'); END`)
		must.NoError(err)
	}
	f.run(t)
	status, err = f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
	checks.Equal("edited", f.archived(t, "1").body)
	checks.True(f.archived(t, "1").edited)
}

func TestProcessorUnsendUpdatesSyncCounters(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	path := "/v2/bot/group/" + testGroup + "/member/" + testUser
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusServiceUnavailable})
	f.enqueue(t, f.message("1", "original", f.now.UnixMilli()), time.Time{})
	f.run(t)
	f.enqueue(t, Event{Type: "unsend", WebhookEventID: "unsend-1", Timestamp: f.now.UnixMilli() + 1, Source: Source{Type: "group", GroupID: testGroup}, Unsend: &Unsend{MessageID: "1"}}, time.Time{})
	ctx := jobctx.WithProgress(t.Context())
	must.NoError(f.processor.Process(ctx))
	checks.True(f.archived(t, "1").deleted)
	checks.True(jobctx.HasProgress(ctx), "committed unsends must schedule a cache refresh")
	var added, updated int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_added,messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&added, &updated))
	checks.Zero(added)
	checks.Equal(int64(1), updated, "unsends must invalidate already exported messages")
	f.now = f.now.Add(time.Minute)
	f.run(t)
	checks.True(f.archived(t, "1").deleted)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_added,messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&added, &updated))
	checks.Zero(added)
	checks.Zero(updated, "reconciling an unchanged unsend must preserve the cache update watermark")
}

func TestProcessorUnsendBeforeMessage(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	stamp := f.now.UnixMilli()
	base := f.message("1", "retained unsent text", stamp)
	unsend := Event{Type: "unsend", WebhookEventID: "unsend", Timestamp: stamp + 2, Source: base.Source, Unsend: &Unsend{MessageID: "1"}}
	f.enqueue(t, unsend, f.now.Add(-25*time.Hour))
	f.run(t)
	status, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	checks.Zero(status.Failed, "an expired unsend awaiting its original remains retained evidence")
	f.enqueue(t, base, time.Time{})
	f.run(t)
	archived := f.archived(t, "1")
	body, deleted := archived.body, archived.deleted
	checks.Equal("retained unsent text", body)
	checks.True(deleted)
	status, err = f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	require.NoError(t, err)
	checks.Zero(status.Failed)
	base.WebhookEventID = "late-original"
	f.enqueue(t, base, time.Time{})
	f.run(t)
	deleted = f.archived(t, "1").deleted
	checks.True(deleted)
}

func TestProcessorLateOriginalReconcilesExpiredRetryInSamePass(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	base := f.message("1", "original", f.now.UnixMilli())
	edit := base
	edit.Type = "messageEdited"
	edit.Timestamp++
	edit.WebhookEventID = "orphan-edit"
	edit.Message = &Message{ID: "1", Type: "text", Text: "edited before original"}
	f.enqueue(t, edit, f.now.Add(-time.Hour))
	f.run(t)

	due, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now.Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, due, 1)
	checks.Equal("retry", due[0].State)

	f.now = f.now.Add(25 * time.Hour)
	f.enqueue(t, base, f.now)
	f.run(t)

	archived := f.archived(t, "1")
	checks.Equal("edited before original", archived.body)
	checks.True(archived.edited)
	var state string
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT state FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, edit.WebhookEventID).Scan(&state))
	checks.Equal("processed", state)
}

func TestProcessorEditPreservesOriginalRaw(t *testing.T) {
	for _, nativeMetadata := range []bool{false, true} {
		t.Run(fmt.Sprintf("native metadata=%t", nativeMetadata), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			message := f.message("original-raw", "original text", f.now.UnixMilli())
			if nativeMetadata {
				must.NoError(json.Unmarshal([]byte(fmt.Sprintf(`{"type":"message","timestamp":%d,"webhookEventId":"event-1","source":{"type":"group","groupId":"C00000000000000000000000000000001","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"original","quoteToken":"synthetic-quote-token","emojis":[{"index":0,"length":1,"productId":"synthetic-product","emojiId":"synthetic-emoji"}],"mention":{"mentionees":[{"type":"user","userId":"U00000000000000000000000000000002"}]}}}`, message.Timestamp)), &message))
			}
			f.enqueue(t, message, time.Time{})
			f.run(t)
			id := f.archived(t, message.Message.ID).id
			original, err := f.st.GetMessageRawContext(t.Context(), id)
			must.NoError(err)
			must.Contains(string(original), `"text":"`+message.Message.Text+`"`)

			edit := f.message(message.Message.ID, "edited text", message.Timestamp+1)
			edit.Type = "messageEdited"
			edit.WebhookEventID = "edit-original-raw"
			f.enqueue(t, edit, time.Time{})
			f.run(t)
			checks.Equal("edited text", f.archived(t, message.Message.ID).body)
			raw, err := f.st.GetMessageRawContext(t.Context(), id)
			must.NoError(err)
			checks.Equal(original, raw)
			if nativeMetadata {
				archived, err := f.st.LineMessageContext(t.Context(), f.source.ID, message.Message.ID)
				must.NoError(err)
				var metadata map[string]json.RawMessage
				must.NoError(json.Unmarshal([]byte(archived.Metadata.String), &metadata))
				checks.NotContains(metadata, "emojis")
				checks.NotContains(metadata, "mention")
				checks.JSONEq(`"synthetic-quote-token"`, string(metadata["quoteToken"]))
			}
		})
	}
}

func TestProcessorEditPreservesOriginalSenderDisplayNameAfterRename(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	profile, err := json.Marshal(Profile{UserID: testUser, DisplayName: "Original Name"})
	must.NoError(err)
	must.NoError(f.st.CacheLineProfileContext(t.Context(), f.source.ID, testUser, string(profile), f.now))
	base := f.message("1", "original searchable text", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)

	f.now = f.now.Add(25 * time.Hour)
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: http.StatusOK, Body: `{"userId":"` + testUser + `","displayName":"Renamed Sender"}`})
	f.enqueue(t, f.message("2", "after profile refresh", f.now.UnixMilli()), time.Time{})
	f.run(t)

	edit := base
	edit.Type = "messageEdited"
	edit.Timestamp++
	edit.WebhookEventID = "edit-search"
	edit.Message = &Message{ID: "1", Type: "text", Text: "updated searchable text"}
	f.enqueue(t, edit, time.Time{})
	f.run(t)

	archived, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessageBySourceID(t.Context(), "1", []int64{f.source.ID})
	must.NoError(err)
	must.Len(archived.From, 1)
	checks.Equal("Original Name", archived.From[0].Name)
}

func TestProcessorIdlePassSkipsSyncRunAndScopesTouchedStats(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	ctx := t.Context()
	otherConversationID, err := f.st.EnsureConversationWithTypeContext(ctx, f.source.ID, "group:C00000000000000000000000000000009", "group_chat", "")
	must.NoError(err)
	for index := range 99 {
		_, err := f.st.PersistMessageContext(ctx, &store.MessagePersistData{Message: &store.Message{SourceID: f.source.ID, SourceMessageID: fmt.Sprintf("other-%d", index), MessageType: "line", ConversationID: otherConversationID}})
		must.NoError(err)
	}
	must.NoError(f.st.RecomputeConversationStatsForConversationContext(ctx, otherConversationID))
	if f.st.IsPostgreSQL() {
		_, err = f.st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE FUNCTION reject_untouched_line_stats() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF OLD.id = %d THEN RAISE EXCEPTION 'recomputed untouched conversation'; END IF; RETURN NEW; END; $$`, otherConversationID))
		must.NoError(err)
		_, err = f.st.DB().ExecContext(t.Context(), `CREATE TRIGGER untouched_line_stats BEFORE UPDATE OF message_count ON conversations FOR EACH ROW EXECUTE FUNCTION reject_untouched_line_stats()`)
	} else {
		_, err = f.st.DB().ExecContext(t.Context(), fmt.Sprintf(`CREATE TRIGGER untouched_line_stats BEFORE UPDATE OF message_count ON conversations WHEN OLD.id=%d BEGIN SELECT RAISE(ABORT,'recomputed untouched conversation'); END`, otherConversationID))
	}
	must.NoError(err)
	var runsBefore, runsAfter int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsBefore))
	f.run(t)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsAfter))
	checks.Equal(runsBefore, runsAfter)
	var otherCount int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), otherConversationID).Scan(&otherCount))
	checks.Equal(99, otherCount)

	f.enqueue(t, f.message("1", "first due message", f.now.UnixMilli()), time.Time{})
	f.run(t)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsAfter))
	checks.Equal(runsBefore+1, runsAfter)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT message_count FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, "group:"+testGroup).Scan(&otherCount))
	checks.Equal(1, otherCount)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), otherConversationID).Scan(&otherCount))
	checks.Equal(99, otherCount)
}

func TestProcessorRefreshesRenamedGroupTitle(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	title := func() string {
		t.Helper()
		var value string
		must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT title FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, "group:"+testGroup).Scan(&value))
		return value
	}
	f.enqueue(t, f.message("1", "before rename", f.now.UnixMilli()), time.Time{})
	f.run(t)
	checks.Equal("Example Group", title())

	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/summary", lineapi.Response{Status: 200, Body: `{"groupId":"` + testGroup + `","groupName":"Renamed Group"}`})
	replay := f.message("1", "before rename", f.now.UnixMilli())
	replay.WebhookEventID = "title-refresh"
	f.enqueue(t, replay, time.Time{})
	f.run(t)
	checks.Equal("Renamed Group", title())
	var updated int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
	checks.Equal(int64(1), updated, "a title change affects exported conversations")
}

func TestProcessorProfileFailureKeepsText(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/group/" + testGroup + "/member/" + testUser
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: 503, Body: `{"message":"synthetic failure"}`}, lineapi.Response{Status: 503})
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/summary", lineapi.Response{Status: 503}, lineapi.Response{Status: 503})
	f.enqueue(t, f.message("1", "text survives enrichment failure", f.now.UnixMilli()), time.Time{})
	f.run(t)
	body := f.archived(t, "1").body
	checks.Equal("text survives enrichment failure", body)
	status, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending)
	f.now = f.now.Add(time.Minute)
	f.run(t)
	var updated int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
	checks.Zero(updated, "an unchanged retry must preserve the cache update watermark")
	f.now = f.now.Add(time.Hour)
	f.run(t)
	status, err = f.st.LineInboxStatusContext(t.Context(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
	checks.Equal(int64(1), updated)
	archived, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessageBySourceID(t.Context(), "1", []int64{f.source.ID})
	must.NoError(err)
	must.Len(archived.From, 1)
	checks.Equal("Example User", archived.From[0].Name)
	count, err := f.st.CountMessagesForSource(f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), count)
}

func TestProcessorGCPurgesRetryPayloads(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("archive GC is SQLite-only")
	}
	ctx := t.Context()
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 503})
	message := f.message("gc-retry", "text to purge", f.now.UnixMilli())
	f.enqueue(t, message, time.Time{})
	f.run(t)
	checks.Equal("text to purge", f.archived(t, message.Message.ID).body)
	var state string
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id=?`, message.WebhookEventID).Scan(&state))
	must.Equal("retry", state)
	var retryAt time.Time
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT next_attempt_at FROM line_webhook_events WHERE webhook_event_id=?`, message.WebhookEventID).Scan(&retryAt))

	edit := f.message(message.Message.ID, "edited text to purge", message.Timestamp+1)
	edit.Type = "messageEdited"
	edit.WebhookEventID = "gc-edit"
	f.enqueue(t, edit, time.Time{})
	f.enqueue(t, Event{Type: "unsend", WebhookEventID: "gc-unsend", Timestamp: message.Timestamp + 2, Source: message.Source, Unsend: &Unsend{MessageID: message.Message.ID}}, time.Time{})
	f.run(t)
	must.True(f.archived(t, message.Message.ID).deleted)
	plan, err := f.st.PlanGCContext(ctx)
	must.NoError(err)
	deleted, err := f.st.ExecuteGCContext(ctx, plan)
	must.NoError(err)
	must.Equal(int64(1), deleted)

	f.now = retryAt
	f.run(t)
	_, err = f.st.LineMessageContext(ctx, f.source.ID, message.Message.ID)
	must.ErrorIs(err, sql.ErrNoRows)
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id=?`, message.WebhookEventID).Scan(&state))
	checks.Equal("ignored", state)
	// Scrubbed edits and unsends must remain harmless when reconciled.
	must.NoError(f.processor.reconcileMutations(ctx, message.Message.ID))

	f.enqueue(t, message, time.Time{})
	f.run(t)
	_, err = f.st.LineMessageContext(ctx, f.source.ID, message.Message.ID)
	must.ErrorIs(err, sql.ErrNoRows)
	var rows int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM line_webhook_events WHERE webhook_event_id=?`, message.WebhookEventID).Scan(&rows))
	checks.Equal(1, rows)
}

func TestProcessorProfileNotFoundCompletesText(t *testing.T) {
	for _, source := range []struct {
		name   string
		source Source
		path   string
	}{
		{"group", Source{Type: "group", GroupID: testGroup, UserID: testUser}, "/v2/bot/group/" + testGroup + "/member/" + testUser},
	} {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cached=%t", source.name, cached), func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				f := newInboxFixture(t)
				cachedAt := f.now.Add(-25 * time.Hour)
				cachedRaw := `{"userId":"` + testUser + `","displayName":"Cached Name"}`
				wantName := ""
				if cached {
					must.NoError(f.st.CacheLineProfileContext(t.Context(), f.source.ID, testUser, cachedRaw, cachedAt))
					wantName = "Cached Name"
				}
				f.api.Enqueue(http.MethodGet, source.path, lineapi.Response{Status: http.StatusNotFound})
				message := f.message("1", "text survives a missing profile", f.now.UnixMilli())
				message.Source = source.source
				f.enqueue(t, message, time.Time{})
				f.run(t)

				archived := f.archived(t, "1")
				checks.Equal("text survives a missing profile", archived.body)
				var state string
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT state FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, message.WebhookEventID).Scan(&state))
				checks.Equal("processed", state)
				var name sql.NullString
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`), archived.id).Scan(&name))
				checks.Equal(wantName, name.String)
				raw, at, err := f.st.LineProfileContext(t.Context(), f.source.ID, testUser)
				if cached {
					must.NoError(err)
					checks.Equal(cachedRaw, raw)
					checks.True(cachedAt.Equal(at), "a missing profile must leave the cache timestamp unchanged")
				} else {
					must.ErrorIs(err, sql.ErrNoRows)
				}

				message.Message.ID = "2"
				message.WebhookEventID = "event-2"
				f.enqueue(t, message, time.Time{})
				f.run(t)
				checks.Equal(2, f.api.Count(http.MethodGet, source.path), "the next event fetches a fresh profile")
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT display_name FROM message_recipients WHERE message_id=? AND recipient_type='from'`), f.archived(t, "2").id).Scan(&name))
				checks.Equal("Example User", name.String)
			})
		}
	}
}

func TestProcessorFriendshipEventsAreIgnored(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	for _, kind := range []string{"follow", "unfollow"} {
		f.enqueue(t, Event{Type: kind, WebhookEventID: kind, Timestamp: f.now.UnixMilli(), Source: Source{Type: "user", UserID: testUser}}, time.Time{})
	}
	f.run(t)
	var conversations, participants, ignored int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversations`).Scan(&conversations))
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_participants`).Scan(&participants))
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM line_webhook_events WHERE state='ignored'`).Scan(&ignored))
	checks.Zero(conversations)
	checks.Zero(participants)
	checks.Equal(2, ignored)
}

func TestProcessorCanceledMembershipPreservesSyncProgress(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	ctx, cancel := context.WithCancel(jobctx.WithProgress(t.Context()))
	defer cancel()
	profilePath := "/v2/bot/group/" + testGroup + "/member/" + testUser
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == profilePath {
			cancel()
			<-r.Context().Done()
			return
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
	processor := NewProcessor(f.st, client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.enqueue(t, Event{Type: "memberJoined", WebhookEventID: "join-canceled", Timestamp: f.now.UnixMilli(), Source: Source{Type: "group", GroupID: testGroup}, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}, {Type: "user", UserID: "U00000000000000000000000000000003"}}}}, time.Time{})
	must.ErrorIs(processor.Process(ctx), context.Canceled)
	var members int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_participants WHERE joined_at IS NOT NULL AND left_at IS NULL`).Scan(&members))
	checks.Equal(2, members, "membership commits before profile enrichment")
	checks.True(jobctx.HasProgress(ctx), "committed memberships must schedule a cache refresh despite cancellation")
	var updated int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
	checks.Equal(int64(2), updated, "each committed membership must invalidate cached participants")
}

func TestProcessorMembershipEventsProjectTransitions(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/group/" + testGroup + "/member/" + testUser
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: 503}, lineapi.Response{Status: 503})
	stamp := f.now.UnixMilli()
	source := Source{Type: "group", GroupID: testGroup}
	for _, e := range []Event{{Type: "memberJoined", WebhookEventID: "joined", Timestamp: stamp, Source: source, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}}}}, {Type: "memberLeft", WebhookEventID: "left", Timestamp: stamp + 20, Source: source, Left: &Members{Members: []Source{{Type: "user", UserID: testUser}}}}} {
		f.enqueue(t, e, time.Time{})
		f.run(t)
		var updated int64
		must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
		checks.Equal(int64(1), updated, "a membership transition must invalidate cached participants")
		if e.Type == "memberJoined" {
			f.now = f.now.Add(time.Minute)
			f.run(t)
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
			checks.Zero(updated, "an unchanged membership retry must preserve the cache update watermark")
		}
	}
	var joined, left sql.NullTime
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT joined_at,left_at FROM conversation_participants`).Scan(&joined, &left))
	must.True(joined.Valid)
	must.True(left.Valid)
	checks.Equal(stamp, joined.Time.UnixMilli())
	checks.Equal(stamp+20, left.Time.UnixMilli())
}

func TestProcessorMembershipClocksSurviveParticipantMerge(t *testing.T) {
	for _, chat := range []struct {
		name, conversationKey, identifierType, identifier string
		source                                            Source
		messageOnly                                       bool
	}{
		{"explicit_group", "group:" + testGroup, "line_user_id", UserIdentifier(testChannel, "U00000000000000000000000000000009"), Source{Type: "group", GroupID: testGroup}, false},
		{"message_only_group", "group:" + testGroup, "line_user_id", UserIdentifier(testChannel, testUser), Source{Type: "group", GroupID: testGroup, UserID: testUser}, true},
		{"anonymous_group", "group:" + testGroup, "line_anonymous", "channel:2000000001:group:" + testGroup, Source{Type: "group", GroupID: testGroup}, true},
	} {
		for _, direction := range []string{"departed_into_active", "active_into_departed"} {
			t.Run(chat.name+"/"+direction, func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				f := newInboxFixture(t)
				ctx := t.Context()
				stamp := f.now.UnixMilli()
				departedUser := testUser
				if chat.messageOnly {
					departedUser = "U00000000000000000000000000000009"
				}
				membershipSource := chat.source
				membershipSource.UserID = ""
				membership := func(kind, eventID, userID string, timestamp int64) {
					t.Helper()
					members := &Members{Members: []Source{{Type: "user", UserID: userID}}}
					event := Event{Type: kind, WebhookEventID: eventID, Timestamp: timestamp, Source: membershipSource}
					if kind == "memberJoined" {
						event.Joined = members
					} else {
						event.Left = members
					}
					f.enqueue(t, event, time.Time{})
					f.run(t)
				}
				if chat.messageOnly {
					message := f.message("message-only", "message proves this sender is present", stamp)
					message.Source = chat.source
					f.enqueue(t, message, time.Time{})
					f.run(t)
					membership("memberLeft", "other-left", departedUser, stamp+20)
				} else {
					membership("memberJoined", "a-joined", departedUser, stamp)
					membership("memberLeft", "a-left", departedUser, stamp+10)
					membership("memberJoined", "b-joined", "U00000000000000000000000000000009", stamp+20)
					// A's later leave clock is newer than B's join, but does not make B inactive.
					membership("memberLeft", "a-left-after-b-joined", departedUser, stamp+30)
				}
				active, err := f.st.EnsureParticipantByIdentifierContext(ctx, chat.identifierType, chat.identifier, "")
				must.NoError(err)
				departed, err := f.st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", UserIdentifier(testChannel, departedUser), "")
				must.NoError(err)
				absorbed, survivor := departed, active
				if direction == "active_into_departed" {
					absorbed, survivor = active, departed
				}
				must.NoError(f.st.MergeParticipantsContext(ctx, absorbed, survivor))
				var conversationID int64
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, chat.conversationKey).Scan(&conversationID))
				leftAt := func() sql.NullTime {
					t.Helper()
					var left sql.NullTime
					must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conversationID, survivor).Scan(&left))
					return left
				}
				checkMergedMembership := func() {
					t.Helper()
					checks.False(leftAt().Valid, "one active linked LINE identity keeps the merged participant active")
					var routes int
					must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), conversationID).Scan(&routes))
					checks.Equal(1, routes, "merged LINE identities project to one conversation membership")
				}
				checkMergedMembership()
				if !chat.messageOnly {
					// Replayed old events from A must not overwrite B's active membership.
					membership("memberJoined", "a-delayed-old-join", departedUser, stamp-1)
					checkMergedMembership()
					// A's newer join is still followed by A's leave; B remains independently active.
					membership("memberJoined", "a-delayed-join-before-leave", departedUser, stamp+25)
					checkMergedMembership()
					return
				}
				f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
				membership("memberLeft", "other-left-again", departedUser, stamp+30)
				checks.False(leftAt().Valid, "message presence survives later events and processor restart")
				if chat.source.UserID == "" {
					return // Anonymous chat placeholders have no identifiable provider leave event.
				}
				membership("memberLeft", "message-sender-left", testUser, stamp+40)
				left := leftAt()
				must.True(left.Valid)
				checks.Equal(stamp+40, left.Time.UnixMilli(), "a later leave of the message sender makes every identity departed")
				late := f.message("delayed-message", "older than the sender's leave", stamp+5)
				late.Source = chat.source
				f.enqueue(t, late, time.Time{})
				f.run(t)
				checks.True(leftAt().Valid, "out-of-order message presence cannot override a later leave")
			})
		}
	}
}

func TestProcessorExistingChatRecordsOwner(t *testing.T) {
	for _, source := range []Source{
		{Type: "user", UserID: testUser},
		{Type: "group", GroupID: testGroup, UserID: testUser},
	} {
		t.Run(source.Type, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			message := f.message("existing-chat", "received before any join event", f.now.UnixMilli())
			message.Source = source
			f.enqueue(t, message, time.Time{})
			f.run(t)
			candidates, err := f.st.LoadActivityCandidatesByIDContext(t.Context(), []int64{f.archived(t, message.Message.ID).id})
			must.NoError(err)
			must.Len(candidates, 1)
			bot, err := f.st.LineBotUserIDContext(t.Context(), f.source.ID)
			must.NoError(err)
			var owners []string
			for _, counterpart := range candidates[0].Counterparts {
				if counterpart.IsOwner {
					owners = append(owners, counterpart.OwnerAddress)
				}
			}
			checks.Equal([]string{UserIdentifier(testChannel, bot)}, owners)
			if source.Type == "user" {
				messageID := f.archived(t, message.Message.ID).id
				var senderID int64
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), messageID).Scan(&senderID))
				checks.Positive(senderID)
				var kind string
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT c.conversation_type FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE m.id=?`), messageID).Scan(&kind))
				checks.Equal("direct_chat", kind)
				var clocks int
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM line_memberships WHERE source_id=?`), f.source.ID).Scan(&clocks))
				checks.Zero(clocks, "direct chats do not infer group membership")
			}
		})
	}
}

func TestProcessorMessageMembershipWriteFailureKeepsEventPending(t *testing.T) {
	for _, tc := range []struct {
		name, trigger, messageID, text, failure string
	}{
		{"archive write", `CREATE TRIGGER line_write_fails BEFORE INSERT ON messages WHEN NEW.message_type='line' BEGIN SELECT RAISE(FAIL,'synthetic archive write failure'); END`, "1", "text must survive a storage failure", "synthetic archive write failure"},
		{"membership write", `CREATE TRIGGER line_write_fails BEFORE INSERT ON line_memberships BEGIN SELECT RAISE(ABORT,'synthetic membership failure'); END`, "membership-write", "text survives a failed membership write", "synthetic membership failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			if f.st.IsPostgreSQL() {
				t.Skip("SQLite triggers inject storage failures; ordinary processor contracts run on both backends")
			}
			_, err := f.st.DB().ExecContext(t.Context(), tc.trigger)
			must.NoError(err)
			message := f.message(tc.messageID, tc.text, f.now.UnixMilli())
			f.enqueue(t, message, f.now.Add(-25*time.Hour))
			must.ErrorContains(f.processor.Process(t.Context()), tc.failure)
			var messages, members int
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=?`), f.source.ID).Scan(&messages))
			must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_participants`).Scan(&members))
			checks.Zero(messages, "message and inferred clock commit together")
			checks.Zero(members, "a failed clock write cannot expose a clockless active membership")
			var raw string
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT raw_json FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, message.WebhookEventID).Scan(&raw))
			checks.Contains(raw, message.Message.Text, "the durable inbox retains text when message persistence rolls back")
			var state string
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT state FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, message.WebhookEventID).Scan(&state))
			checks.Equal("pending", state, "archive write failures never become expiring provider retries")
			checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser))
			_, err = f.st.DB().ExecContext(t.Context(), `DROP TRIGGER line_write_fails`)
			must.NoError(err)
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT next_attempt_at FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, message.WebhookEventID).Scan(&f.now))
			f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
			f.run(t)
			checks.Equal(message.Message.Text, f.archived(t, message.Message.ID).body)
			var clocks int
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT COUNT(*) FROM line_memberships WHERE source_id=?`), f.source.ID).Scan(&clocks))
			checks.Equal(1, clocks, "recovery persists text and its per-identity membership evidence together")
		})
	}
}

func TestProcessorMemberProfileFailureDoesNotStarveOtherMembers(t *testing.T) {
	t.Run("unavailable profile preserves transitions", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		f := newInboxFixture(t)
		path := "/v2/bot/group/" + testGroup + "/member/" + testUser
		f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusServiceUnavailable})
		e := Event{Type: "memberJoined", WebhookEventID: "join-many", Timestamp: f.now.UnixMilli(), Source: Source{Type: "group", GroupID: testGroup}, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}, {Type: "user", UserID: "U00000000000000000000000000000003"}}}}
		f.enqueue(t, e, time.Time{})
		f.run(t)
		var count int
		must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM conversation_participants WHERE joined_at IS NOT NULL AND left_at IS NULL`).Scan(&count))
		checks.Equal(2, count)
		checks.Equal(1, f.api.Count(http.MethodGet, path))
		checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/U00000000000000000000000000000003"))
		var state string
		must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id='join-many'`).Scan(&state))
		checks.Equal("retry", state)
		f.now = f.now.Add(25 * time.Hour)
		f.run(t)
		must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id='join-many'`).Scan(&state))
		checks.Equal("failed", state)
		checks.Equal(1, f.api.Count(http.MethodGet, path), "expired events must stop fetching profiles")
	})
	t.Run("forbidden profile allows other members", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		f := newInboxFixture(t)
		other := "U00000000000000000000000000000003"
		firstPath := "/v2/bot/group/" + testGroup + "/member/" + testUser
		otherPath := "/v2/bot/group/" + testGroup + "/member/" + other
		f.api.Enqueue(http.MethodGet, firstPath, lineapi.Response{Status: http.StatusForbidden})
		f.enqueue(t, Event{Type: "memberJoined", WebhookEventID: "members-joined", Timestamp: f.now.UnixMilli(), Source: Source{Type: "group", GroupID: testGroup}, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}, {Type: "user", UserID: other}}}}, time.Time{})
		f.run(t)
		var name sql.NullString
		must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers i WHERE i.participant_id=p.id AND i.identifier_type='line_user_id' AND i.identifier_value=?)`), UserIdentifier(testChannel, other)).Scan(&name))
		checks.Equal("Example User", name.String)
		checks.Equal(1, f.api.Count(http.MethodGet, otherPath))
		var state string
		must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id='members-joined'`).Scan(&state))
		checks.Equal("retry", state)
	})
	t.Run("missing profile allows other members", func(t *testing.T) {
		must := require.New(t)
		checks := assert.New(t)
		f := newInboxFixture(t)
		other := "U00000000000000000000000000000003"
		firstPath := "/v2/bot/group/" + testGroup + "/member/" + testUser
		otherPath := "/v2/bot/group/" + testGroup + "/member/" + other
		f.api.Enqueue(http.MethodGet, firstPath, lineapi.Response{Status: http.StatusNotFound})
		f.enqueue(t, Event{Type: "memberJoined", WebhookEventID: "members-joined", Timestamp: f.now.UnixMilli(), Source: Source{Type: "group", GroupID: testGroup}, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}, {Type: "user", UserID: other}}}}, time.Time{})
		f.run(t)
		status, err := f.st.LineInboxStatusContext(t.Context(), f.source.ID)
		must.NoError(err)
		checks.Zero(status.Pending)
		checks.Zero(status.Failed)
		var name sql.NullString
		must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT p.display_name FROM participants p WHERE EXISTS(SELECT 1 FROM participant_identifiers i WHERE i.participant_id=p.id AND i.identifier_type='line_user_id' AND i.identifier_value=?)`), UserIdentifier(testChannel, other)).Scan(&name))
		checks.Equal("Example User", name.String)
		checks.Equal(1, f.api.Count(http.MethodGet, otherPath))
	})
}

func TestProcessorProfileCacheSurvivesRestartAndExpires(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/group/" + testGroup + "/member/" + testUser
	f.enqueue(t, f.message("1", "first", f.now.UnixMilli()), time.Time{})
	f.run(t)
	checks.Equal(1, f.api.Count(http.MethodGet, path))
	f.now = f.now.Add(23 * time.Hour)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.enqueue(t, f.message("2", "cached", f.now.UnixMilli()), time.Time{})
	f.run(t)
	checks.Equal(1, f.api.Count(http.MethodGet, path))
	f.now = f.now.Add(2 * time.Hour)
	f.enqueue(t, f.message("3", "refresh", f.now.UnixMilli()), time.Time{})
	f.run(t)
	checks.Equal(2, f.api.Count(http.MethodGet, path))
	raw, _, err := f.st.LineProfileContext(t.Context(), f.source.ID, testUser)
	require.NoError(t, err)
	checks.JSONEq(`{"userId":"`+testUser+`","displayName":"Example User"}`, raw)
}

func TestProcessorAnonymousIdentityIsChannelScoped(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	otherClient := NewClient("2000000002", "synthetic-secret", WithBaseURLs(f.client.apiURL, f.client.dataURL))
	f.api.Enqueue(http.MethodGet, "/v2/bot/info", lineapi.Response{Status: http.StatusOK, Body: `{"userId":"U00000000000000000000000000000004","basicId":"@other","displayName":"Other Bot"}`})
	other, err := Register(t.Context(), f.st, otherClient, "2000000002")
	must.NoError(err)
	e := f.message("1", "anonymous", f.now.UnixMilli())
	e.Source.UserID = ""
	f.enqueue(t, e, time.Time{})
	f.run(t)
	raw, err := json.Marshal(e)
	must.NoError(err)
	must.NoError(f.st.EnqueueLineEventsContext(t.Context(), other.ID, other.Identifier, []store.LineWebhookEvent{{WebhookEventID: e.WebhookEventID, EventType: e.Type, TargetMessageID: e.Message.ID, EventTimestamp: e.Timestamp, RawJSON: string(raw), ReceivedAt: f.now}}))
	must.NoError(NewProcessor(f.st, otherClient, other.ID, "2000000002", f.attachments, WithProcessorClock(func() time.Time { return f.now })).Process(t.Context()))
	var a, b int64
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT sender_id FROM messages WHERE source_id=?`), f.source.ID).Scan(&a))
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT sender_id FROM messages WHERE source_id=?`), other.ID).Scan(&b))
	checks.NotEqual(a, b)
	var classified int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='line_anonymous' AND service_id IS NOT NULL`).Scan(&classified))
	checks.Zero(classified)
}

func TestProcessorDirectIdentityGapRetainsEvidence(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	e := f.message("1", "synthetic unidentifiable direct message", f.now.UnixMilli())
	e.Source = Source{Type: "user"}
	f.enqueue(t, e, time.Time{})
	f.run(t)
	count, err := f.st.CountMessagesForSource(f.source.ID)
	must.NoError(err)
	checks.Zero(count)
	var state, raw string
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state,raw_json FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state, &raw))
	checks.Equal("failed", state)
	checks.Contains(raw, "synthetic unidentifiable")
}

func TestProcessorUnknownEventPayloadShapeIsIgnored(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	before := len(f.api.Requests())
	raw := `{"type":"futureEvent","timestamp":1,"webhookEventId":"future-shape","source":23,"message":"opaque future payload"}`
	must.NoError(f.st.EnqueueLineEventsContext(t.Context(), f.source.ID, f.source.Identifier, []store.LineWebhookEvent{{WebhookEventID: "future-shape", EventType: "futureEvent", EventTimestamp: 1, RawJSON: raw, ReceivedAt: f.now}}))
	f.run(t)
	var state string
	must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id='future-shape'`).Scan(&state))
	checks.Equal("ignored", state)
	checks.Len(f.api.Requests(), before)
}

func TestProcessorUnknownMessagePayloadRetainsEvidence(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	raw := `{"type":"message","timestamp":1,"webhookEventId":"future-message","source":{"type":"group","groupId":"` + testGroup + `","userId":"` + testUser + `"},"message":{"id":"future-1","type":"future","text":{"new":"shape"},"fileSize":[1,2]}}`
	must.NoError(f.st.EnqueueLineEventsContext(t.Context(), f.source.ID, f.source.Identifier, []store.LineWebhookEvent{{WebhookEventID: "future-message", EventType: "message", TargetMessageID: "future-1", EventTimestamp: 1, RawJSON: raw, ReceivedAt: f.now}}))
	f.run(t)
	body := f.archived(t, "future-1").body
	checks.Equal("[LINE future]", body)
	metadata, err := f.st.LineMessageContext(t.Context(), f.source.ID, "future-1")
	must.NoError(err)
	var native map[string]json.RawMessage
	must.NoError(json.Unmarshal([]byte(metadata.Metadata.String), &native))
	checks.JSONEq(`{"new":"shape"}`, string(native["text"]))
}

func TestProcessorProfileRefreshUpdatesObservedPresentation(t *testing.T) {
	for _, curated := range []bool{false, true} {
		t.Run(strconv.FormatBool(curated), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)

			f := newInboxFixture(t)
			f.enqueue(t, f.message("1", "initial", f.now.UnixMilli()), time.Time{})
			f.run(t)
			if !curated {
				archived, err := query.NewEngine(f.st.DB(), f.st.IsPostgreSQL()).GetMessageBySourceID(t.Context(), "1", []int64{f.source.ID})
				must.NoError(err)
				must.Len(archived.From, 1)
				checks.Equal("Example User", archived.From[0].Name)
				count, err := f.st.CountMessagesForSource(f.source.ID)
				must.NoError(err)
				checks.Equal(int64(1), count)
			}
			var pid int64
			must.NoError(f.st.DB().QueryRowContext(t.Context(), `SELECT sender_id FROM messages WHERE source_message_id='1'`).Scan(&pid))
			if curated {
				_, err := f.st.DB().ExecContext(t.Context(), f.st.Rebind(`UPDATE participants SET display_name='Chosen label' WHERE id=?`), pid)
				must.NoError(err)
			}
			f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 200, Body: `{"userId":"` + testUser + `","displayName":"Updated user"}`})
			f.now = f.now.Add(25 * time.Hour)
			f.enqueue(t, f.message("2", "refreshed", f.now.UnixMilli()), time.Time{})
			f.run(t)
			var name string
			must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
			want := "Updated user"
			if curated {
				want = "Chosen label"
			}
			checks.Equal(want, name)
			if !curated {
				must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind(`SELECT mr.display_name FROM message_recipients mr JOIN messages m ON m.id=mr.message_id WHERE m.source_id=? AND m.source_message_id=? AND mr.recipient_type='from'`), f.source.ID, "2").Scan(&name))
				checks.Equal("Updated user", name)
			}
		})
	}
}

func TestProcessorCombinedRetryAfterUsesLatestDeadline(t *testing.T) {
	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 429, Header: http.Header{"Retry-After": []string{"3600"}}})
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/summary", lineapi.Response{Status: 429, Header: http.Header{"Retry-After": []string{"30"}}})
	f.enqueue(t, f.message("1", "rate limited enrichments", f.now.UnixMilli()), time.Time{})
	f.run(t)
	due, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now.Add(time.Minute), 10)
	require.NoError(t, err)
	assert.Empty(t, due)
}

func TestProcessorExpiredRetryDoesNotCallProviderAfterRetryHorizon(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/group/" + testGroup + "/member/" + testUser
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: 429, Header: http.Header{"Retry-After": []string{"172800"}}})
	f.enqueue(t, f.message("1", "already archived", f.now.UnixMilli()), time.Time{})
	f.run(t)
	checks.Equal(1, f.api.Count(http.MethodGet, path))
	f.now = f.now.Add(24*time.Hour + time.Second)
	f.run(t)
	checks.Equal(1, f.api.Count(http.MethodGet, path))
	var state string
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), `SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("failed", state)
}

func TestProcessorDrainLeavesNewReceiptsAndFutureRetries(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	f.enqueue(t, f.fileEvent(), time.Time{})
	for i := range 501 {
		f.enqueue(t, f.message(fmt.Sprintf("batch-%03d", i), "synthetic", f.now.UnixMilli()), time.Time{})
	}
	rows, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 503)
	must.NoError(err)
	must.Len(rows, 502)
	f.api.Enqueue(http.MethodGet, "/v2/bot/message/1/content", lineapi.Response{Status: 202})
	later := f.message("later", "next drain", f.now.UnixMilli())
	raw, err := json.Marshal(later)
	must.NoError(err)
	clean, err := SanitizeEvent(raw)
	must.NoError(err)
	enqueued := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/bot/message/1/content" {
			enqueued <- f.st.EnqueueLineEventsContext(r.Context(), f.source.ID, f.source.Identifier, []store.LineWebhookEvent{{WebhookEventID: later.WebhookEventID, EventType: later.Type, TargetMessageID: later.Message.ID, EventTimestamp: later.Timestamp, RawJSON: string(clean), ReceivedAt: f.now}})
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client := NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL))
	processor := NewProcessor(f.st, client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	must.NoError(processor.Drain(t.Context()))
	select {
	case err := <-enqueued:
		must.NoError(err)
	case <-time.After(10 * time.Second):
		must.FailNow("drain did not enqueue the later receipt")
	}
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	var archived int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id LIKE 'batch-%'"), f.source.ID).Scan(&archived))
	checks.Equal(501, archived)
	var laterMessages int
	must.NoError(f.st.DB().QueryRowContext(t.Context(), f.st.Rebind("SELECT COUNT(*) FROM messages WHERE source_id=? AND source_message_id='later'"), f.source.ID).Scan(&laterMessages))
	checks.Zero(laterMessages)
	due, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
	must.NoError(err)
	must.Len(due, 1)
	checks.Equal("event-later", due[0].WebhookEventID)
	must.NoError(processor.Drain(t.Context()))
	checks.Equal("next drain", f.archived(t, "later").body)
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	must.ErrorIs(processor.Drain(ctx), context.Canceled)
}

func TestProcessorArchivesLaterTextBeforeEarlierMediaCompletes(t *testing.T) {
	f := newInboxFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/bot/message/1/content" {
			close(started)
			<-release
		}
		f.api.ServeHTTP(w, r)
	}))
	defer server.Close()
	f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments)
	f.enqueue(t, f.fileEvent(), time.Time{})
	f.enqueue(t, f.message("2", "later text", f.now.UnixMilli()+1), time.Time{})
	done := make(chan error, 1)
	go func() { done <- f.processor.Process(t.Context()) }()
	defer func() {
		close(release)
		require.NoError(t, <-done)
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "processor did not reach media fetch")
	}
	var count int
	require.NoError(t, f.st.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM messages WHERE source_message_id='2'").Scan(&count))
	assert.Equal(t, 1, count, "the text in an acknowledged batch must be available while earlier optional media is downloading")
}

func TestProcessorCooperativeStopAfterEnrichment(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	first := f.fileEvent()
	second := f.fileEvent()
	second.WebhookEventID = "event-2"
	second.Message.ID = "2"
	second.Timestamp++
	f.enqueue(t, first, time.Time{})
	f.enqueue(t, second, time.Time{})
	before, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
	must.NoError(err)
	must.Len(before, 2)
	ctx, request := jobctx.WithPreemption(t.Context())
	request()
	must.ErrorIs(f.processor.Process(ctx), jobctx.ErrYieldedToWaiter)
	checks.Equal(1, f.api.Count(http.MethodGet, "/v2/bot/message/1/content"))
	checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/message/2/content"))
	after, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
	must.NoError(err)
	must.Len(after, 1)
	checks.Equal(before[1].ID, after[0].ID)
	checks.Equal(before[1].State, after[0].State)
	checks.Equal(before[1].Attempts, after[0].Attempts)
	checks.Equal(before[1].NextAttemptAt, after[0].NextAttemptAt)
	run, err := f.st.GetLatestSync(f.source.ID)
	must.NoError(err)
	checks.Equal(store.SyncStatusCancelled, run.Status)
	checks.Equal(int64(1), run.MessagesProcessed)
	checks.Equal(int64(2), run.MessagesAdded)
	checks.Zero(run.ErrorsCount)
}

func TestProcessorHandoffStatus(t *testing.T) {
	for _, tc := range []struct {
		state     string
		itemError bool
	}{
		{"pending", false},
		{"retry", true},
	} {
		t.Run(tc.state+"/itemError="+strconv.FormatBool(tc.itemError), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			f.enqueue(t, f.message("text", "committed before download", f.now.UnixMilli()-1), time.Time{})
			f.enqueue(t, f.fileEvent(), time.Time{})
			rows, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
			must.NoError(err)
			must.Len(rows, 2)
			if tc.state == "retry" {
				must.NoError(f.st.FinishLineEventContext(t.Context(), rows[1].ID, "retry", "provider failure", f.now.Add(-time.Minute)))
			}
			before, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now, 10)
			must.NoError(err)
			must.Len(before, 2)
			if tc.itemError {
				f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/summary", lineapi.Response{Status: http.StatusInternalServerError})
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v2/bot/message/1/content" {
					cancel(jobctx.ErrRunBudgetExceeded)
					<-r.Context().Done()
					return
				}
				f.api.ServeHTTP(w, r)
			}))
			defer server.Close()
			client := NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL))
			processor := NewProcessor(f.st, client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
			must.ErrorIs(processor.Process(ctx), context.Canceled)
			checks.Equal("committed before download", f.archived(t, "text").body)
			after, err := f.st.DueLineEventsContext(t.Context(), f.source.ID, f.now.Add(time.Hour), 10)
			must.NoError(err)
			var mediaRows []store.LineWebhookEvent
			for _, row := range after {
				if row.TargetMessageID == "1" {
					mediaRows = append(mediaRows, row)
				}
			}
			must.Len(mediaRows, 1)
			checks.Equal(before[1].State, mediaRows[0].State)
			checks.Equal(before[1].Attempts, mediaRows[0].Attempts)
			checks.Equal(before[1].NextAttemptAt, mediaRows[0].NextAttemptAt)
			run, err := f.st.GetLatestSync(f.source.ID)
			must.NoError(err)
			checks.Equal(int64(2), run.MessagesAdded)
			if tc.itemError {
				checks.Equal(int64(1), run.ErrorsCount)
				checks.Zero(run.MessagesProcessed)
			} else {
				checks.Zero(run.ErrorsCount)
				checks.Equal(int64(1), run.MessagesProcessed)
				checks.Empty(run.ErrorMessage.String)
			}
			f.run(t)
			checks.Equal(attachmentpolicy.StateStored, f.media(t).State)
		})
	}
}
