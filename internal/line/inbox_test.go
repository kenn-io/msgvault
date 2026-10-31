package line

import (
	"context"
	"database/sql"
	"encoding/json"
	"encoding/json/jsontext"
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
	"go.kenn.io/msgvault/internal/jobctx"
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
	f.source, err = Register(context.Background(), f.st, f.client, testChannel)
	require.NoError(t, err)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	return f
}
func (f *inboxFixture) enqueue(t *testing.T, e Event, received time.Time) {
	t.Helper()
	raw, err := json.Marshal(e)
	require.NoError(t, err)
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
	botID, err := f.st.LineBotUserIDContext(context.Background(), f.source.ID)
	require.NoError(t, err)
	require.NoError(t, f.st.EnqueueLineEventsContext(context.Background(), f.source.ID, f.source.Identifier, botID, []store.LineWebhookEvent{{WebhookEventID: e.WebhookEventID, EventType: e.Type, TargetMessageID: target, EventTimestamp: e.Timestamp, RawJSON: string(clean), ReceivedAt: received}}))
}
func (f *inboxFixture) message(id, text string, stamp int64) Event {
	return Event{Type: "message", Timestamp: stamp, WebhookEventID: "event-" + id, Source: Source{Type: "group", GroupID: testGroup, UserID: testUser}, Message: &Message{Type: "text", ID: id, Text: text}}
}
func (f *inboxFixture) run(t *testing.T) {
	t.Helper()
	require.NoError(t, f.processor.Process(context.Background()))
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
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT id,is_edited,deleted_from_source_at,sent_at FROM messages WHERE source_id=? AND source_message_id=?`), f.source.ID, key).Scan(&id, &edited, &deleted, &sent))
	var body sql.NullString
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id=?`), id).Scan(&body))
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
			require.NoError(t, f.st.DB().QueryRow(`SELECT is_from_me FROM messages`).Scan(&mine))
			checks.False(mine)
		})
	}
}

func TestProcessorQuoteResolvesWhenParentArrives(t *testing.T) {
	f := newInboxFixture(t)
	parent := f.message("1", "parent", f.now.UnixMilli())
	child := f.message("2", "child", f.now.UnixMilli()+1)
	child.Message.QuotedMessageID = "1"
	for _, e := range []Event{child, parent} {
		f.enqueue(t, e, time.Time{})
		f.run(t)
	}
	parentID := f.archived(t, "1").id
	childID := f.archived(t, "2").id
	var reply sql.NullInt64
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), childID).Scan(&reply))
	assert.Equal(t, parentID, reply.Int64)
}

func TestProcessorInterruptedPassSettlesCompletedConversations(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	groupY := "C00000000000000000000000000000002"
	userY := "U00000000000000000000000000000003"
	ctx, cancel := context.WithCancel(jobctx.WithProgress(context.Background()))
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
	child.Message.QuotedMessageID = "repair-parent"
	f.enqueue(t, parent, time.Time{})
	f.enqueue(t, child, time.Time{})
	f.enqueue(t, Event{
		Type: "memberJoined", Timestamp: f.now.UnixMilli() + 2, WebhookEventID: "cancel-group-y",
		Source: Source{Type: "group", GroupID: groupY},
		Joined: &Members{Members: []Source{{Type: "user", UserID: userY}}},
	}, time.Time{})

	must.ErrorIs(f.processor.Process(ctx), context.Canceled)
	status, err := f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending, "an interrupted pass settles and finishes the events it completed")

	f.run(t)
	status, err = f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
	var parentID, childID, conversationID int64
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT id,conversation_id FROM messages WHERE source_id=? AND source_message_id=?`), f.source.ID, "repair-parent").Scan(&parentID, &conversationID))
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT id FROM messages WHERE source_id=? AND source_message_id=?`), f.source.ID, "repair-child").Scan(&childID))
	var reply sql.NullInt64
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), childID).Scan(&reply))
	checks.True(reply.Valid)
	checks.Equal(parentID, reply.Int64)
	var messageCount int64
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), conversationID).Scan(&messageCount))
	checks.Equal(int64(2), messageCount)
}

func TestProcessorRejectsSourceBoundToAnotherChannel(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	f.enqueue(t, f.message("wrong-channel", "synthetic", f.now.UnixMilli()), time.Time{})
	p := NewProcessor(f.st, f.client, f.source.ID, "2000000002", f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	must.Error(p.Process(context.Background()))
	status, err := f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending)
	count, err := f.st.CountMessagesForSource(f.source.ID)
	must.NoError(err)
	checks.Zero(count)
}

func TestProcessorLatestEditWins(t *testing.T) {
	checks := assert.New(t)
	f := newInboxFixture(t)
	base := f.message("1", "original", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)
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
		checks.Equal("latest", f.archived(t, "1").body, "delivery %s must preserve the latest edit before original replay", update.id)
	}
	replay := base
	replay.WebhookEventID = "delayed-original"
	f.enqueue(t, replay, time.Time{})
	f.run(t)
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

func TestProcessorEditCountsUpdatedMessage(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	latestUpdated := func() int64 {
		t.Helper()
		var updated sql.NullInt64
		must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT messages_updated FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&updated))
		return updated.Int64
	}
	base := f.message("1", "original", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)
	checks.Zero(latestUpdated())

	edit := base
	edit.Type = "messageEdited"
	edit.WebhookEventID = "edit-1"
	edit.Timestamp++
	edit.Message = &Message{ID: "1", Type: "text", Text: "edited"}
	f.enqueue(t, edit, time.Time{})
	f.run(t)
	checks.Equal(int64(1), latestUpdated())

	replay := base
	replay.WebhookEventID = "delayed-original"
	f.enqueue(t, replay, time.Time{})
	f.run(t)
	checks.Zero(latestUpdated(), "replaying an original reapplies its edit without changing content")
}

func TestProcessorCountsAddedMessages(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	latestAdded := func() int64 {
		t.Helper()
		var added int64
		must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT messages_added FROM sync_runs WHERE source_id=? ORDER BY id DESC LIMIT 1`), f.source.ID).Scan(&added))
		return added
	}

	base := f.message("1", "new searchable message", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)
	checks.Equal(int64(1), latestAdded())

	replay := base
	replay.WebhookEventID = "delayed-original"
	f.enqueue(t, replay, time.Time{})
	f.run(t)
	checks.Zero(latestAdded(), "replaying an existing message is not a new addition")
}

func TestProcessorInterruptedEditPassLeavesCacheRebuildSignal(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("SQLite trigger injects a real failed finalization; normal processor contracts run on both backends")
	}
	base := f.message("1", "original", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)

	edit := base
	edit.Type = "messageEdited"
	edit.WebhookEventID = "edit-1"
	edit.Timestamp++
	edit.Message = &Message{ID: "1", Type: "text", Text: "edited"}
	f.enqueue(t, edit, time.Time{})
	_, err := f.st.DB().Exec(`CREATE TRIGGER line_finalize_fails BEFORE UPDATE OF message_count ON conversations BEGIN SELECT RAISE(ABORT,'synthetic finalization failure'); END`)
	must.NoError(err)
	must.Error(f.processor.Process(context.Background()))
	checks.Equal("edited", f.archived(t, "1").body, "the edit commits before finalization fails")
	status, err := f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending)
	// The analytics cache rebuilds fully after any failed run with a completion time.
	var failedRuns int
	must.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM sync_runs WHERE source_id=? AND status='failed' AND completed_at IS NOT NULL`, f.source.ID).Scan(&failedRuns))
	checks.Equal(1, failedRuns)

	_, err = f.st.DB().Exec(`DROP TRIGGER line_finalize_fails`)
	must.NoError(err)
	f.run(t)
	status, err = f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
}

func TestProcessorUnsendBeforeMessage(t *testing.T) {
	checks := assert.New(t)

	f := newInboxFixture(t)
	stamp := f.now.UnixMilli()
	base := f.message("1", "retained unsent text", stamp)
	wrong := base
	wrong.Type = "messageEdited"
	wrong.Timestamp++
	wrong.WebhookEventID = "wrong-chat"
	wrong.Source.GroupID = "C00000000000000000000000000000002"
	wrong.Message = &Message{ID: "1", Type: "text", Text: "wrong"}
	unsend := Event{Type: "unsend", WebhookEventID: "unsend", Timestamp: stamp + 2, Source: base.Source, Unsend: &Unsend{MessageID: "1"}}
	for _, event := range []Event{wrong, unsend} {
		f.enqueue(t, event, f.now.Add(-25*time.Hour))
		f.run(t)
	}
	status, err := f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	require.NoError(t, err)
	checks.Equal(int64(2), status.Failed)
	f.enqueue(t, base, time.Time{})
	f.run(t)
	archived := f.archived(t, "1")
	body, deleted := archived.body, archived.deleted
	checks.Equal("retained unsent text", body)
	checks.True(deleted)
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

	due, err := f.st.DueLineEventsContext(context.Background(), f.source.ID, f.now.Add(time.Hour), 10)
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
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT state FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, edit.WebhookEventID).Scan(&state))
	checks.Equal("processed", state)
}

func TestProcessorEditPreservesCachedSenderSearch(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	profile, err := json.Marshal(Profile{UserID: testUser, DisplayName: "Cached Sender"})
	must.NoError(err)
	must.NoError(f.st.CacheLineProfileContext(context.Background(), f.source.ID, testUser, string(profile), f.now))
	base := f.message("1", "original searchable text", f.now.UnixMilli())
	f.enqueue(t, base, time.Time{})
	f.run(t)

	_, total, err := f.st.SearchMessagesContext(context.Background(), "Cached Sender", 0, 10)
	must.NoError(err)
	checks.Equal(int64(1), total)

	edit := base
	edit.Type = "messageEdited"
	edit.Timestamp++
	edit.WebhookEventID = "edit-search"
	edit.Message = &Message{ID: "1", Type: "text", Text: "updated searchable text"}
	f.enqueue(t, edit, time.Time{})
	f.run(t)

	_, total, err = f.st.SearchMessagesContext(context.Background(), "Cached Sender", 0, 10)
	must.NoError(err)
	checks.Equal(int64(1), total)
}

func TestProcessorFirstMessageSearchableAfterProfileResolution(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(strconv.FormatBool(retry), func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)
			f := newInboxFixture(t)
			path := "/v2/bot/group/" + testGroup + "/member/" + testUser
			if retry {
				f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: http.StatusServiceUnavailable})
			}
			f.enqueue(t, f.message("1", "first message text", f.now.UnixMilli()), time.Time{})
			f.run(t)
			if retry {
				f.now = f.now.Add(time.Minute)
				f.run(t)
			}
			_, total, err := f.st.SearchMessagesContext(context.Background(), "Example User", 0, 10)
			must.NoError(err)
			checks.Equal(int64(1), total)
			count, err := f.st.CountMessagesForSource(f.source.ID)
			must.NoError(err)
			checks.Equal(int64(1), count)
		})
	}
}

func TestProcessorEditDropsRemovedNativeTextMetadata(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	base := f.message("1", "original", f.now.UnixMilli())
	base.Message.QuoteToken = "synthetic-quote-token"
	base.Message.Emojis = jsontext.Value(`[{"index":0,"length":1,"productId":"synthetic-product","emojiId":"synthetic-emoji"}]`)
	base.Message.Mention = jsontext.Value(`{"mentionees":[{"type":"user","userId":"U00000000000000000000000000000002"}]}`)
	f.enqueue(t, base, time.Time{})
	f.run(t)

	edit := base
	edit.Type = "messageEdited"
	edit.WebhookEventID = "edit-without-native-text-fields"
	edit.Timestamp++
	edit.Message = &Message{ID: "1", Type: "text", Text: "edited without emoji or mention"}
	f.enqueue(t, edit, time.Time{})
	f.run(t)

	archived, err := f.st.LineMessageContext(context.Background(), f.source.ID, "1")
	must.NoError(err)
	var metadata map[string]json.RawMessage
	must.NoError(json.Unmarshal([]byte(archived.Metadata.String), &metadata))
	checks.NotContains(metadata, "emojis")
	checks.NotContains(metadata, "mention")
	checks.JSONEq(`"synthetic-quote-token"`, string(metadata["quoteToken"]))
}

func TestProcessorIdlePassSkipsSyncRunAndScopesTouchedStats(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	ctx := context.Background()
	otherConversationID, err := f.st.EnsureConversationWithTypeContext(ctx, f.source.ID, "group:C00000000000000000000000000000009", "group_chat", "")
	must.NoError(err)
	_, err = f.st.DB().Exec(f.st.Rebind(`UPDATE conversations SET message_count=99 WHERE id=?`), otherConversationID)
	must.NoError(err)
	var runsBefore, runsAfter int
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsBefore))
	f.run(t)
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsAfter))
	checks.Equal(runsBefore, runsAfter)
	var otherCount int
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), otherConversationID).Scan(&otherCount))
	checks.Equal(99, otherCount)

	f.enqueue(t, f.message("1", "first due message", f.now.UnixMilli()), time.Time{})
	f.run(t)
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsAfter))
	checks.Equal(runsBefore+1, runsAfter)
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT message_count FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, "group:"+testGroup).Scan(&otherCount))
	checks.Equal(1, otherCount)
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), otherConversationID).Scan(&otherCount))
	checks.Equal(99, otherCount)
}

func TestProcessorIdlePassPrunesExpiredPayloads(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	f.enqueue(t, f.message("1", "synthetic", f.now.UnixMilli()), time.Time{})
	f.run(t)
	var runsBefore, runsAfter int
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsBefore))

	f.now = f.now.Add(31 * 24 * time.Hour)
	f.run(t)
	var raw string
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT raw_json FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, "event-1").Scan(&raw))
	checks.Equal("{}", raw)
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM sync_runs WHERE source_id=?`), f.source.ID).Scan(&runsAfter))
	checks.Equal(runsBefore, runsAfter)
}

func TestProcessorRefreshesRenamedGroupTitle(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	title := func() string {
		t.Helper()
		var value string
		must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT title FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, "group:"+testGroup).Scan(&value))
		return value
	}
	f.enqueue(t, f.message("1", "before rename", f.now.UnixMilli()), time.Time{})
	f.run(t)
	checks.Equal("Example Group", title())

	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/summary", lineapi.Response{Status: 200, Body: `{"groupId":"` + testGroup + `","groupName":"Renamed Group"}`})
	f.enqueue(t, f.message("2", "after rename", f.now.UnixMilli()+1), time.Time{})
	f.run(t)
	checks.Equal("Renamed Group", title())
}

func TestProcessorProfileFailureKeepsText(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	path := "/v2/bot/group/" + testGroup + "/member/" + testUser
	f.api.Enqueue(http.MethodGet, path, lineapi.Response{Status: 503, Body: `{"message":"synthetic failure"}`})
	f.enqueue(t, f.message("1", "text survives enrichment failure", f.now.UnixMilli()), time.Time{})
	f.run(t)
	body := f.archived(t, "1").body
	checks.Equal("text survives enrichment failure", body)
	status, err := f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), status.Pending)
	f.now = f.now.Add(time.Minute)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	status, err = f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
	count, err := f.st.CountMessagesForSource(f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), count)
}

func TestProcessorMembershipEventsProjectTransitions(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	stamp := f.now.UnixMilli()
	source := Source{Type: "group", GroupID: testGroup}
	for _, e := range []Event{{Type: "memberJoined", WebhookEventID: "joined", Timestamp: stamp, Source: source, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}}}}, {Type: "memberLeft", WebhookEventID: "left", Timestamp: stamp + 20, Source: source, Left: &Members{Members: []Source{{Type: "user", UserID: testUser}}}}} {
		f.enqueue(t, e, time.Time{})
		f.run(t)
	}
	var joined, left sql.NullTime
	must.NoError(f.st.DB().QueryRow(`SELECT joined_at,left_at FROM conversation_participants`).Scan(&joined, &left))
	must.True(joined.Valid)
	must.True(left.Valid)
	checks.Equal(stamp, joined.Time.UnixMilli())
	checks.Equal(stamp+20, left.Time.UnixMilli())
}

func TestProcessorMembershipClocksSurviveParticipantMerge(t *testing.T) {
	for _, direction := range []string{"departed_into_active", "active_into_departed"} {
		t.Run(direction, func(t *testing.T) {
			must := require.New(t)
			checks := assert.New(t)

			f := newInboxFixture(t)
			ctx := context.Background()
			stamp := f.now.UnixMilli()
			source := Source{Type: "group", GroupID: testGroup}
			userA := testUser
			userB := "U00000000000000000000000000000009"
			membership := func(kind, eventID, userID string, timestamp int64) {
				t.Helper()
				members := &Members{Members: []Source{{Type: "user", UserID: userID}}}
				event := Event{Type: kind, WebhookEventID: eventID, Timestamp: timestamp, Source: source}
				if kind == "memberJoined" {
					event.Joined = members
				} else {
					event.Left = members
				}
				f.enqueue(t, event, time.Time{})
				f.run(t)
			}
			membership("memberJoined", "a-joined", userA, stamp)
			membership("memberLeft", "a-left", userA, stamp+10)
			membership("memberJoined", "b-joined", userB, stamp+20)
			// A's later leave clock is newer than B's join, but does not make B inactive.
			membership("memberLeft", "a-left-after-b-joined", userA, stamp+30)

			departed, err := f.st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", UserIdentifier(testChannel, userA), "")
			must.NoError(err)
			active, err := f.st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", UserIdentifier(testChannel, userB), "")
			must.NoError(err)
			absorbed, survivor := departed, active
			if direction == "active_into_departed" {
				absorbed, survivor = active, departed
			}
			must.NoError(f.st.MergeParticipantsContext(ctx, absorbed, survivor))

			var conversationID int64
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, "group:"+testGroup).Scan(&conversationID))
			checkMergedMembership := func() {
				t.Helper()
				var left sql.NullTime
				must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conversationID, survivor).Scan(&left))
				checks.False(left.Valid, "one active linked LINE identity keeps the merged participant active")
				var routes int
				must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM conversation_participants WHERE conversation_id=?`), conversationID).Scan(&routes))
				checks.Equal(1, routes, "merged LINE identities project to one conversation membership")
			}
			checkMergedMembership()

			// Replayed old events from A must not overwrite B's active membership.
			membership("memberJoined", "a-delayed-old-join", userA, stamp-1)
			checkMergedMembership()
			// A's newer join is still followed by A's leave; B remains independently active.
			membership("memberJoined", "a-delayed-join-before-leave", userA, stamp+25)
			checkMergedMembership()
		})
	}
}

func TestProcessorMessageOnlyMembershipSurvivesParticipantMerge(t *testing.T) {
	for _, chat := range []struct {
		name, conversationKey, identifierType, identifier string
		source                                            Source
	}{
		{"group", "group:" + testGroup, "line_user_id", UserIdentifier(testChannel, testUser), Source{Type: "group", GroupID: testGroup, UserID: testUser}},
		{"room", "room:R00000000000000000000000000000001", "line_user_id", UserIdentifier(testChannel, testUser), Source{Type: "room", RoomID: "R00000000000000000000000000000001", UserID: testUser}},
		{"anonymous_group", "group:" + testGroup, "line_anonymous", "channel:2000000001:group:" + testGroup, Source{Type: "group", GroupID: testGroup}},
	} {
		for _, direction := range []string{"message_only_into_departed", "departed_into_message_only"} {
			t.Run(chat.name+"/"+direction, func(t *testing.T) {
				must := require.New(t)
				checks := assert.New(t)
				f := newInboxFixture(t)
				ctx := context.Background()
				stamp := f.now.UnixMilli()
				departedUser := "U00000000000000000000000000000009"
				membershipSource := chat.source
				membershipSource.UserID = ""
				message := f.message("message-only", "message proves this sender is present", stamp)
				message.Source = chat.source
				f.enqueue(t, message, time.Time{})
				f.run(t)
				membership := func(user, eventID string, at int64) {
					t.Helper()
					f.enqueue(t, Event{Type: "memberLeft", Timestamp: at, WebhookEventID: eventID, Source: membershipSource, Left: &Members{Members: []Source{{Type: "user", UserID: user}}}}, time.Time{})
					f.run(t)
				}
				membership(departedUser, "other-left", stamp+20)
				present, err := f.st.EnsureParticipantByIdentifierContext(ctx, chat.identifierType, chat.identifier, "")
				must.NoError(err)
				departed, err := f.st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", UserIdentifier(testChannel, departedUser), "")
				must.NoError(err)
				absorbed, survivor := present, departed
				if direction == "departed_into_message_only" {
					absorbed, survivor = departed, present
				}
				must.NoError(f.st.MergeParticipantsContext(ctx, absorbed, survivor))
				var conversationID int64
				must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?`), f.source.ID, chat.conversationKey).Scan(&conversationID))
				leftAt := func() sql.NullTime {
					t.Helper()
					var left sql.NullTime
					must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT left_at FROM conversation_participants WHERE conversation_id=? AND participant_id=?`), conversationID, survivor).Scan(&left))
					return left
				}
				checks.False(leftAt().Valid, "a message-only active identity survives merging with a departed identity")
				f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
				membership(departedUser, "other-left-again", stamp+30)
				checks.False(leftAt().Valid, "message presence survives later events and processor restart")
				if chat.source.UserID == "" {
					return // Anonymous chat placeholders have no identifiable provider leave event.
				}
				membership(testUser, "message-sender-left", stamp+40)
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

func TestProcessorDirectMessageDoesNotRecordGroupMembershipClock(t *testing.T) {
	f := newInboxFixture(t)
	message := f.message("direct", "direct chat text", f.now.UnixMilli())
	message.Source = Source{Type: "user", UserID: testUser}
	f.enqueue(t, message, time.Time{})
	f.run(t)
	var clocks int
	require.NoError(t, f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM line_webhook_events WHERE source_id=? AND event_type='_membership'`), f.source.ID).Scan(&clocks))
	assert.Zero(t, clocks, "direct chats do not infer group membership")
}

func TestProcessorFirstGroupMessageWritesClockBeforeConversation(t *testing.T) {
	f := newInboxFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("SQLite trigger pins statement order; the PG contract checks actual table locks")
	}
	_, err := f.st.DB().Exec(`CREATE TRIGGER line_conversation_requires_clock BEFORE INSERT ON conversations
 WHEN NEW.conversation_type='group_chat' AND NOT EXISTS (
 SELECT 1 FROM line_webhook_events WHERE source_id=NEW.source_id AND event_type='_membership')
 BEGIN SELECT RAISE(ABORT,'LINE clock must precede first conversation'); END`)
	require.NoError(t, err)
	message := f.message("first-group-message", "clock before conversation", f.now.UnixMilli())
	f.enqueue(t, message, time.Time{})
	require.NoError(t, f.processor.Process(context.Background()))
	assert.Equal(t, message.Message.Text, f.archived(t, message.Message.ID).body)
}

func TestProcessorMessageMembershipWriteFailureKeepsEventPending(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	f := newInboxFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("SQLite trigger injects membership failure; ordinary membership contracts run on both backends")
	}
	_, err := f.st.DB().Exec(`CREATE TRIGGER line_membership_write_fails BEFORE INSERT ON line_webhook_events
 WHEN NEW.event_type='_membership' BEGIN SELECT RAISE(ABORT,'synthetic membership failure'); END`)
	must.NoError(err)
	message := f.message("membership-write", "text survives a failed membership write", f.now.UnixMilli())
	f.enqueue(t, message, time.Time{})
	must.ErrorContains(f.processor.Process(context.Background()), "synthetic membership failure")
	var messages, members int
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM messages WHERE source_id=?`), f.source.ID).Scan(&messages))
	must.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants`).Scan(&members))
	checks.Zero(messages, "message and inferred clock commit together")
	checks.Zero(members, "a failed clock write cannot expose a clockless active membership")
	var raw string
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT raw_json FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, message.WebhookEventID).Scan(&raw))
	checks.Contains(raw, message.Message.Text, "the durable inbox retains text when message persistence rolls back")
	var state string
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT state FROM line_webhook_events WHERE source_id=? AND webhook_event_id=?`), f.source.ID, message.WebhookEventID).Scan(&state))
	checks.Equal("pending", state, "archive write failures never become expiring provider retries")
	checks.Zero(f.api.Count(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser))
	_, err = f.st.DB().Exec(`DROP TRIGGER line_membership_write_fails`)
	must.NoError(err)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	checks.Equal(message.Message.Text, f.archived(t, message.Message.ID).body)
	var clocks int
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT COUNT(*) FROM line_webhook_events WHERE source_id=? AND event_type='_membership'`), f.source.ID).Scan(&clocks))
	checks.Equal(1, clocks, "recovery persists text and its per-identity membership evidence together")
}

func TestProcessorRestartAfterPersistence(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	ctx, cancel := context.WithCancel(jobctx.WithProgress(context.Background()))
	defer cancel()
	var first atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/member/") && !first.Swap(true) {
			cancel()
			<-r.Context().Done()
			return
		}
		f.api.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	f.client = NewClient(testChannel, "synthetic-secret", WithBaseURLs(server.URL, server.URL), WithClock(func() time.Time { return f.now }))
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.enqueue(t, f.message("1", "persisted before cancellation", f.now.UnixMilli()), time.Time{})
	must.ErrorIs(f.processor.Process(ctx), context.Canceled)
	checks.True(jobctx.HasProgress(ctx), "committed text must trigger cache refresh even when enrichment is canceled")
	body := f.archived(t, "1").body
	checks.Equal("persisted before cancellation", body)
	f.processor = NewProcessor(f.st, f.client, f.source.ID, testChannel, f.attachments, WithProcessorClock(func() time.Time { return f.now }))
	f.run(t)
	status, err := f.st.LineInboxStatusContext(context.Background(), f.source.ID)
	must.NoError(err)
	checks.Zero(status.Pending)
	count, err := f.st.CountMessagesForSource(f.source.ID)
	must.NoError(err)
	checks.Equal(int64(1), count)
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
	raw, _, err := f.st.LineProfileContext(context.Background(), f.source.ID, testUser)
	require.NoError(t, err)
	checks.Contains(raw, "pictureUrl")
}

func TestProcessorAnonymousIdentityIsChannelScoped(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	otherClient := NewClient("2000000002", "synthetic-secret", WithBaseURLs(f.client.apiURL, f.client.dataURL))
	other, err := Register(context.Background(), f.st, otherClient, "2000000002")
	must.NoError(err)
	e := f.message("1", "anonymous", f.now.UnixMilli())
	e.Source.UserID = ""
	f.enqueue(t, e, time.Time{})
	f.run(t)
	raw, err := json.Marshal(e)
	must.NoError(err)
	otherBotID, err := f.st.LineBotUserIDContext(context.Background(), other.ID)
	must.NoError(err)
	must.NoError(f.st.EnqueueLineEventsContext(context.Background(), other.ID, other.Identifier, otherBotID, []store.LineWebhookEvent{{WebhookEventID: e.WebhookEventID, EventType: e.Type, TargetMessageID: e.Message.ID, EventTimestamp: e.Timestamp, RawJSON: string(raw), ReceivedAt: f.now}}))
	must.NoError(NewProcessor(f.st, otherClient, other.ID, "2000000002", f.attachments, WithProcessorClock(func() time.Time { return f.now })).Process(context.Background()))
	var a, b int64
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT sender_id FROM messages WHERE source_id=?`), f.source.ID).Scan(&a))
	must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT sender_id FROM messages WHERE source_id=?`), other.ID).Scan(&b))
	checks.NotEqual(a, b)
	var classified int
	must.NoError(f.st.DB().QueryRow(`SELECT COUNT(*) FROM participant_identifiers WHERE identifier_type='line_anonymous' AND service_id IS NOT NULL`).Scan(&classified))
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
	must.NoError(f.st.DB().QueryRow(`SELECT state,raw_json FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state, &raw))
	checks.Equal("failed", state)
	checks.Contains(raw, "synthetic unidentifiable")
}

func TestProcessorUnknownEventPayloadShapeIsIgnored(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	before := len(f.api.Requests())
	raw := `{"type":"futureEvent","timestamp":1,"webhookEventId":"future-shape","source":23,"message":"opaque future payload"}`
	botID, err := f.st.LineBotUserIDContext(context.Background(), f.source.ID)
	must.NoError(err)
	must.NoError(f.st.EnqueueLineEventsContext(context.Background(), f.source.ID, f.source.Identifier, botID, []store.LineWebhookEvent{{WebhookEventID: "future-shape", EventType: "futureEvent", EventTimestamp: 1, RawJSON: raw, ReceivedAt: f.now}}))
	f.run(t)
	var state string
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='future-shape'`).Scan(&state))
	checks.Equal("ignored", state)
	checks.Len(f.api.Requests(), before)
}

func TestProcessorUnknownMessagePayloadRetainsEvidence(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	raw := `{"type":"message","timestamp":1,"webhookEventId":"future-message","source":{"type":"group","groupId":"` + testGroup + `","userId":"` + testUser + `"},"message":{"id":"future-1","type":"future","text":{"new":"shape"},"fileSize":[1,2]}}`
	botID, err := f.st.LineBotUserIDContext(context.Background(), f.source.ID)
	must.NoError(err)
	must.NoError(f.st.EnqueueLineEventsContext(context.Background(), f.source.ID, f.source.Identifier, botID, []store.LineWebhookEvent{{WebhookEventID: "future-message", EventType: "message", TargetMessageID: "future-1", EventTimestamp: 1, RawJSON: raw, ReceivedAt: f.now}}))
	f.run(t)
	body := f.archived(t, "future-1").body
	checks.Equal("[LINE future]", body)
	metadata, err := f.st.LineMessageContext(context.Background(), f.source.ID, "future-1")
	must.NoError(err)
	var native map[string]json.RawMessage
	must.NoError(json.Unmarshal([]byte(metadata.Metadata.String), &native))
	checks.JSONEq(`{"new":"shape"}`, string(native["text"]))
}

func TestProcessorMemberEnrichmentFailureKeepsEveryTransition(t *testing.T) {
	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 503})
	e := Event{Type: "memberJoined", WebhookEventID: "join-many", Timestamp: f.now.UnixMilli(), Source: Source{Type: "group", GroupID: testGroup}, Joined: &Members{Members: []Source{{Type: "user", UserID: testUser}, {Type: "user", UserID: "U00000000000000000000000000000003"}}}}
	f.enqueue(t, e, time.Time{})
	f.run(t)
	var count int
	require.NoError(t, f.st.DB().QueryRow(`SELECT COUNT(*) FROM conversation_participants WHERE joined_at IS NOT NULL AND left_at IS NULL`).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestProcessorProfileRefreshUpdatesObservedPresentation(t *testing.T) {
	for _, curated := range []bool{false, true} {
		t.Run(strconv.FormatBool(curated), func(t *testing.T) {
			must := require.New(t)

			f := newInboxFixture(t)
			f.enqueue(t, f.message("1", "initial", f.now.UnixMilli()), time.Time{})
			f.run(t)
			var pid int64
			must.NoError(f.st.DB().QueryRow(`SELECT sender_id FROM messages WHERE source_message_id='1'`).Scan(&pid))
			if curated {
				_, err := f.st.DB().Exec(f.st.Rebind(`UPDATE participants SET display_name='Chosen label' WHERE id=?`), pid)
				must.NoError(err)
			}
			f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 200, Body: `{"userId":"` + testUser + `","displayName":"Updated user"}`})
			f.now = f.now.Add(25 * time.Hour)
			f.enqueue(t, f.message("2", "refreshed", f.now.UnixMilli()), time.Time{})
			f.run(t)
			var name string
			must.NoError(f.st.DB().QueryRow(f.st.Rebind(`SELECT display_name FROM participants WHERE id=?`), pid).Scan(&name))
			want := "Updated user"
			if curated {
				want = "Chosen label"
			}
			assert.Equal(t, want, name)
		})
	}
}

func TestProcessorCombinedRetryAfterUsesLatestDeadline(t *testing.T) {
	f := newInboxFixture(t)
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/member/"+testUser, lineapi.Response{Status: 429, Header: http.Header{"Retry-After": []string{"30"}}})
	f.api.Enqueue(http.MethodGet, "/v2/bot/group/"+testGroup+"/summary", lineapi.Response{Status: 429, Header: http.Header{"Retry-After": []string{"3600"}}})
	f.enqueue(t, f.message("1", "rate limited enrichments", f.now.UnixMilli()), time.Time{})
	f.run(t)
	due, err := f.st.DueLineEventsContext(context.Background(), f.source.ID, f.now.Add(time.Minute), 10)
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
	require.NoError(t, f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("failed", state)
}

func TestProcessorPersistenceFailureKeepsPending(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)

	f := newInboxFixture(t)
	if f.st.IsPostgreSQL() {
		t.Skip("SQLite trigger injects a real failed archive write; normal processor contracts run on both backends")
	}
	f.enqueue(t, f.message("1", "text must survive a storage failure", f.now.UnixMilli()), f.now.Add(-25*time.Hour))
	_, err := f.st.DB().Exec(`CREATE TRIGGER fail_line_write BEFORE INSERT ON messages WHEN NEW.message_type='line' BEGIN SELECT RAISE(FAIL,'synthetic archive write failure'); END`)
	must.NoError(err)
	must.Error(f.processor.Process(context.Background()))
	var state string
	must.NoError(f.st.DB().QueryRow(`SELECT state FROM line_webhook_events WHERE webhook_event_id='event-1'`).Scan(&state))
	checks.Equal("pending", state)
	_, err = f.st.DB().Exec(`DROP TRIGGER fail_line_write`)
	must.NoError(err)
	f.run(t)
	body := f.archived(t, "1").body
	checks.Equal("text must survive a storage failure", body)
}
