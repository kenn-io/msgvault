package meta

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/metawebhook"
	"go.kenn.io/msgvault/internal/store"
)

func dmTestHandler(t *testing.T, w *dmWriter) *DMHandler {
	t.Helper()
	return NewDMHandler(w.store, func(ctx context.Context, app, object, entry string) (DMSource, *store.Source, error) {
		wantObject := "page"
		if w.src.Platform == PlatformInstagram {
			wantObject = "instagram"
		}
		if app != w.src.AppID || object != wantObject || entry != dmOwner(w.src) {
			return DMSource{}, nil, ErrDMSourceUnrouted
		}
		s, err := w.store.GetSourceByIDContext(ctx, w.sourceID)
		return w.src, s, err
	})
}
func dmItem(w *dmWriter, field, payload string) metawebhook.Item {
	object := "page"
	if w.src.Platform == PlatformInstagram {
		object = "instagram"
	}
	return metawebhook.Item{Object: object, EntryID: dmOwner(w.src), AppID: w.src.AppID, Field: field, Value: []byte(payload)}
}
func dmMessageItem(w *dmWriter, echo bool) metawebhook.Item {
	sender, recipient := "300", dmOwner(w.src)
	field, extra := "messages", ""
	if echo {
		sender, recipient = recipient, sender
		extra = `,"is_echo":true,"app_id":"9"`
		if w.src.Platform == PlatformMessenger {
			field = "message_echoes"
		}
	}
	return dmItem(w, field, fmt.Sprintf(`{"sender":{"id":%q},"recipient":{"id":%q},"timestamp":1793577600123,"message":{"mid":"m_1","text":"hello inbox"%s}}`, sender, recipient, extra))
}
func dmApply(t *testing.T, h *DMHandler, item metawebhook.Item) metawebhook.ItemOutcome {
	t.Helper()
	require := require.New(t)

	result := h.Handle(t.Context(), "9", []metawebhook.Item{item})
	require.Len(result, 1)
	return result[0]
}
func TestDMHandleMessengerMessage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	out := dmApply(t, dmTestHandler(t, w), dmMessageItem(w, false))
	require.Equal(metawebhook.Applied, out.Class)
	m := message(t, w.store, w.sourceID, "m_1")
	body, err := w.store.GetMessageBodyText(m.ID)
	require.NoError(err)
	assert.Equal("hello inbox", body)
}

func TestDMWebhookRunPersistsMessageCounters(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	first := dmMessageItem(w, false)
	updated := first
	updated.Value = []byte(strings.ReplaceAll(string(updated.Value), "hello inbox", "updated inbox"))
	results := h.Handle(t.Context(), "9", []metawebhook.Item{first, updated})
	require.Len(results, 2)
	assert.Equal(metawebhook.Applied, results[0].Class)
	assert.Equal(metawebhook.Applied, results[1].Class)
	run, err := w.store.GetLatestSync(w.sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusCompleted, run.Status)
	assert.Equal(int64(2), run.MessagesProcessed)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Equal(int64(1), run.MessagesUpdated)
	assert.Zero(run.ErrorsCount)
}

func TestDMWebhookRunRetainsCommittedCountersWhenLaterItemFails(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	w := dmTestWriter(t, PlatformMessenger)
	_, err := w.store.DB().Exec(`CREATE TRIGGER reject_second_dm_message BEFORE INSERT ON messages WHEN NEW.source_message_id = 'm_2' BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`)
	require.NoError(err)
	first := dmMessageItem(w, false)
	second := first
	second.Value = []byte(strings.ReplaceAll(string(second.Value), `"m_1"`, `"m_2"`))
	results := dmTestHandler(t, w).Handle(t.Context(), "9", []metawebhook.Item{first, second})
	require.Len(results, 2)
	assert.Equal(metawebhook.Applied, results[0].Class)
	assert.Equal(metawebhook.Retry, results[1].Class)
	run, err := w.store.GetLatestSync(w.sourceID)
	require.NoError(err)
	assert.Equal(store.SyncStatusFailed, run.Status)
	assert.Equal(int64(1), run.MessagesProcessed)
	assert.Equal(int64(1), run.MessagesAdded)
	assert.Zero(run.MessagesUpdated)
	assert.Equal(int64(1), run.ErrorsCount)
	rows, err := w.store.StoredMessagesContext(t.Context(), w.sourceID, dmRawFormat, []string{"m_1", "m_2"})
	require.NoError(err)
	assert.Contains(rows, "m_1")
	assert.NotContains(rows, "m_2")
}

func TestDMHandleRetryPreservesCommittedWrite(t *testing.T) {
	for _, tc := range []struct {
		name, trigger string
		changed       bool
	}{
		{"message rejected", "BEFORE INSERT ON messages", false},
		{"conversation metadata rejected", "BEFORE UPDATE OF metadata ON conversations", true},
		{"reply resolution rejected", "BEFORE UPDATE OF reply_to_message_id ON messages", true},
		{"sync completion rejected", "BEFORE UPDATE OF status ON sync_runs WHEN NEW.status = 'completed'", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			w := dmTestWriter(t, PlatformMessenger)
			item := dmMessageItem(w, false)
			if tc.name == "reply resolution rejected" {
				_, _, err := w.persist(t.Context(), dmTestRecord(t, w, "parent", "earlier message"))
				require.NoError(err)
				item.Value = []byte(strings.Replace(string(item.Value), `"text":"hello inbox"`, `"text":"hello inbox","reply_to":{"mid":"parent"}`, 1))
			}
			_, err := w.store.DB().Exec("CREATE TRIGGER reject_dm_write " + tc.trigger + " BEGIN SELECT RAISE(ABORT, 'fixture failure'); END")
			require.NoError(err)
			out := dmApply(t, dmTestHandler(t, w), item)
			assert.Equal(metawebhook.Retry, out.Class)
			assert.True(out.CountAttempt)
			rows, err := w.store.StoredMessagesContext(t.Context(), w.sourceID, dmRawFormat, []string{"m_1"})
			require.NoError(err)
			_, committed := rows["m_1"]
			assert.Equal(tc.changed, committed)
			assert.Equal(tc.changed, out.ArchiveChanged, "committed writes must refresh cached search even on retry")
		})
	}
}

func TestDMHandleCancellationPreservesItemProgress(t *testing.T) {
	for _, firstFails := range []bool{false, true} {
		t.Run(strconv.FormatBool(firstFails), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			w := dmTestWriter(t, PlatformMessenger)
			w.store.DB().SetMaxOpenConns(1)
			ctx, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			conn, err := w.store.DB().Conn(t.Context())
			require.NoError(err)
			err = conn.Raw(func(raw any) error {
				sqliteConn, ok := raw.(*sqlite3.SQLiteConn)
				require.True(ok)
				return sqliteConn.RegisterFunc("cancel_inbox_write", func() int {
					cancel()
					return 0
				}, false)
			})
			require.NoError(err)
			require.NoError(conn.Close())
			// Cancel at a real write boundary in the second event's transaction.
			_, err = w.store.DB().Exec(`CREATE TRIGGER cancel_second_inbox_write BEFORE INSERT ON messages WHEN NEW.source_message_id = 'm_2' BEGIN SELECT cancel_inbox_write(); END`)
			require.NoError(err)
			if firstFails {
				_, err = w.store.DB().Exec(`CREATE TRIGGER reject_first_inbox_write BEFORE INSERT ON messages WHEN NEW.source_message_id = 'm_1' BEGIN SELECT RAISE(ABORT, 'independent fixture failure'); END`)
				require.NoError(err)
			}
			items := []metawebhook.Item{dmMessageItem(w, false), dmMessageItem(w, false), dmMessageItem(w, false)}
			for i := 1; i < len(items); i++ {
				items[i].Value = []byte(strings.Replace(string(items[i].Value), `"m_1"`, fmt.Sprintf(`"m_%d"`, i+1), 1))
			}
			out := dmTestHandler(t, w).Handle(ctx, "9", items)
			require.Len(out, len(items))
			require.ErrorIs(ctx.Err(), context.Canceled)
			if firstFails {
				assert.Equal(metawebhook.Retry, out[0].Class)
				assert.Equal("archive_unavailable", out[0].ErrorClass)
				assert.True(out[0].CountAttempt, "an independent failure must keep its attempt")
			} else {
				assert.Equal(metawebhook.Applied, out[0].Class)
				assert.True(out[0].ArchiveChanged)
			}
			for _, pending := range out[1:] {
				assert.Equal(metawebhook.Retry, pending.Class)
				assert.Equal(string(ClassCancelled), pending.ErrorClass)
				assert.False(pending.CountAttempt)
			}
			rows, err := w.store.StoredMessagesContext(t.Context(), w.sourceID, dmRawFormat, []string{"m_1", "m_2", "m_3"})
			require.NoError(err)
			assert.Equal(!firstFails, rows["m_1"].ID != 0)
			assert.NotContains(rows, "m_2")
			assert.NotContains(rows, "m_3")
		})
	}
}

func TestDMHandleMessengerEcho(t *testing.T)                { testDMHandleEcho(t, PlatformMessenger) }
func TestDMHandleInstagramEchoOnMessagesField(t *testing.T) { testDMHandleEcho(t, PlatformInstagram) }
func testDMHandleEcho(t *testing.T, p DMPlatform) {
	t.Helper()
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, p)
	require.Equal(metawebhook.Applied, dmApply(t, dmTestHandler(t, w), dmMessageItem(w, true)).Class)
	m := message(t, w.store, w.sourceID, "m_1")
	mine, err := w.store.GetMessageIsFromMe(m.ID)
	require.NoError(err)
	assert.True(mine)
	ref, err := w.store.MessageConversation(m.ID)
	require.NoError(err)
	assert.True(strings.HasSuffix(ref.SourceConversationID, ":300"))
	meta, _ := dmSavedMeta(t, w, m.ID)
	assert.Equal("9", meta.EchoAppID)
}
func TestDMHandleStandbyArchived(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	item := dmMessageItem(w, false)
	item.Field = "standby"
	require.Equal(metawebhook.Applied, dmApply(t, h, item).Class)
	m := message(t, w.store, w.sourceID, "m_1")
	meta, _ := dmSavedMeta(t, w, m.ID)
	assert.Equal("standby", meta.Channel)
	item.Value = []byte(`{`)
	assert.Equal(metawebhook.Invalid, dmApply(t, h, item).Class)
}
func TestDMHandleDuplicateIdempotent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	item := dmMessageItem(w, false)
	require.Equal(metawebhook.Applied, dmApply(t, h, item).Class)
	require.Equal(metawebhook.Applied, dmApply(t, h, item).Class)
	_, added, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "hello inbox"))
	require.NoError(err)
	assert.False(added)
	n, err := w.store.CountMessagesWithRaw(w.sourceID)
	require.NoError(err)
	assert.EqualValues(1, n)
}
func dmReactionItem(w *dmWriter, action, emoji string) metawebhook.Item {
	return dmItem(w, "message_reactions", fmt.Sprintf(`{"sender":{"id":"300"},"recipient":{"id":%q},"timestamp":1793577600123,"reaction":{"mid":"m_1","action":%q,"reaction":"love","emoji":%q}}`, dmOwner(w.src), action, emoji))
}
func TestDMHandleReactAndUnreact(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmReactionItem(w, "react", "❤")).Class)
	var count int
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM reactions").Scan(&count))
	assert.Equal(1, count)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmReactionItem(w, "unreact", "")).Class)
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM reactions").Scan(&count))
	assert.Zero(count)
}
func TestDMHandleReactReplacesPrevious(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformInstagram)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	for _, emoji := range []string{"❤", "👍", "👍"} {
		require.Equal(metawebhook.Applied, dmApply(t, h, dmReactionItem(w, "react", emoji)).Class)
	}
	var value string
	var count int
	require.NoError(w.store.DB().QueryRow("SELECT count(*),min(reaction_value) FROM reactions").Scan(&count, &value))
	assert.Equal(1, count)
	assert.Equal("👍", value)
}
func TestDMHandleReactionMissingTargetRetry(t *testing.T) {
	assert := assert.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	assert.Equal(metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "reaction_target_missing", CountAttempt: true}, dmApply(t, dmTestHandler(t, w), dmReactionItem(w, "react", "❤")))
}
func dmEditItem(w *dmWriter, mid, text string, count int) metawebhook.Item {
	return dmItem(w, "message_edits", fmt.Sprintf(`{"sender":{"id":"300"},"recipient":{"id":%q},"timestamp":1793577601123,"message_edit":{"mid":%q,"text":%q,"num_edit":%d}}`, dmOwner(w.src), mid, text, count))
}
func TestDMHandleMessengerEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmEditItem(w, "m_1", "edited inbox", 2)).Class)
	for _, n := range []int{2, 1} {
		assert.Equal(metawebhook.Ignored, dmApply(t, h, dmEditItem(w, "m_1", "stale text", n)).Class)
	}
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	m := message(t, w.store, w.sourceID, "m_1")
	body, err := w.store.GetMessageBodyText(m.ID)
	require.NoError(err)
	assert.Equal("edited inbox", body)
	var edited bool
	require.NoError(w.store.DB().QueryRow("SELECT is_edited FROM messages WHERE id=?", m.ID).Scan(&edited))
	assert.True(edited)
	var count int
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'edited'").Scan(&count))
	assert.Equal(1, count)
	meta, _ := dmSavedMeta(t, w, m.ID)
	assert.Equal(2, meta.EditCount)
	assert.Equal(metawebhook.ItemOutcome{Class: metawebhook.Retry, ErrorClass: "edit_target_missing", CountAttempt: true}, dmApply(t, h, dmEditItem(w, "missing", "new", 1)))
}

func TestDMWebhookReplayAfterSourceRemovalAndReregistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remove func(context.Context, *store.Store, int64) error
	}{
		{
			name: "maintenance",
			remove: func(_ context.Context, st *store.Store, sourceID int64) error {
				return st.RemoveSource(sourceID)
			},
		},
		{
			name: "serialized",
			remove: func(ctx context.Context, st *store.Store, sourceID int64) error {
				_, _, err := st.RemoveSourceSerialized(ctx, sourceID)
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			w := dmTestWriter(t, PlatformMessenger)
			h := dmTestHandler(t, w)

			original := dmMessageItem(w, false)
			original.Value = []byte(strings.ReplaceAll(string(original.Value), "1793577600123", "1793577600122"))
			require.Equal(metawebhook.Applied, dmApply(t, h, original).Class)

			graph, err := recordFromNode(w.src, DMMessage{
				ID: "m_1", From: DMParticipant{ID: "300"},
				Message: "reconciled after missed edit", MessagePresent: true,
				Raw: []byte(`{"id":"m_1","message":"reconciled after missed edit"}`),
			})
			require.NoError(err)
			graph.GraphObservedAt = 1793577600124
			_, _, err = w.persist(t.Context(), graph)
			require.NoError(err)

			oldSourceID := w.sourceID
			require.NoError(tc.remove(t.Context(), w.store, oldSourceID))
			_, found, err := w.store.GetArchiveMarker(t.Context(), dmGraphObservedKey(oldSourceID, "m_1"))
			require.NoError(err)
			assert.False(found, "source removal must delete its Meta Graph observation markers")

			source, err := w.store.GetOrCreateSource(w.src.Platform.SourceType(), w.src.Identifier())
			require.NoError(err)
			require.Equal(oldSourceID, source.ID, "SQLite should reuse the removed source ID")
			reRegistered := &dmWriter{store: w.store, sourceID: source.ID, src: w.src}
			h = dmTestHandler(t, reRegistered)

			require.Equal(metawebhook.Applied, dmApply(t, h, original).Class)
			edit := dmEditItem(reRegistered, "m_1", "edited after re-registration", 1)
			edit.Value = []byte(strings.ReplaceAll(string(edit.Value), "1793577601123", "1793577600123"))
			require.Equal(metawebhook.Applied, dmApply(t, h, edit).Class)

			archived := message(t, w.store, source.ID, "m_1")
			body, err := w.store.GetMessageBodyText(archived.ID)
			require.NoError(err)
			assert.Equal("edited after re-registration", body)
		})
	}
}

func TestDMDelayedEditAfterGraphReconciliationIsIgnored(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmEditItem(w, "m_1", "first edit", 1)).Class)

	graph, err := recordFromNode(w.src, DMMessage{ID: "m_1", From: DMParticipant{ID: "300"}, Message: "latest polled edit", MessagePresent: true, Raw: []byte(`{"id":"m_1","message":"latest polled edit"}`)})
	require.NoError(err)
	graph.GraphObservedAt = 1793577602123
	id, _, err := w.persist(t.Context(), graph)
	require.NoError(err)
	meta, _ := dmSavedMeta(t, w, id)
	assert.Equal(1, meta.EditCount)
	observedAt, found, err := w.loadGraphObservedAt(t.Context(), "m_1", 0)
	require.NoError(err)
	assert.True(found)
	assert.Equal(int64(1793577602123), observedAt)

	delayed := dmEditItem(w, "m_1", "stale second edit", 2)
	delayed.Value = []byte(strings.ReplaceAll(string(delayed.Value), "1793577601123", "1793577602122"))
	assert.Equal(metawebhook.Ignored, dmApply(t, h, delayed).Class)
	body, err := w.store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("latest polled edit", body)
	meta, _ = dmSavedMeta(t, w, id)
	assert.Equal(1, meta.EditCount)
}

func TestDMEditKeepsSenderSearch(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	id, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "original"))
	require.NoError(err)
	require.Equal(metawebhook.Applied, dmApply(t, dmTestHandler(t, w), dmEditItem(w, "m_1", "edited", 1)).Class)
	var count int
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM messages_fts WHERE rowid=? AND messages_fts MATCH 'Customer'", id).Scan(&count))
	assert.Equal(1, count)
}

func TestDMEditFailureRollsBack(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	id, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "original"))
	require.NoError(err)
	_, err = w.store.DB().Exec(`CREATE TRIGGER reject_dm_metadata BEFORE UPDATE OF metadata ON messages BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`)
	require.NoError(err)
	before, err := w.store.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(metawebhook.Retry, dmApply(t, dmTestHandler(t, w), dmEditItem(w, "m_1", "edited", 1)).Class)
	after, err := w.store.DerivedDataRevisionContext(t.Context())
	require.NoError(err)
	assert.Equal(before, after, "the cache revision rolls back with the failed edit")
	body, err := w.store.GetMessageBodyText(id)
	require.NoError(err)
	assert.Equal("original", body)
	var edited bool
	require.NoError(w.store.DB().QueryRow("SELECT is_edited FROM messages WHERE id=?", id).Scan(&edited))
	assert.False(edited)
	_, err = w.store.DB().Exec(`DROP TRIGGER reject_dm_metadata`)
	require.NoError(err)
	require.Equal(metawebhook.Applied, dmApply(t, dmTestHandler(t, w), dmEditItem(w, "m_1", "edited", 1)).Class)
	meta, _ := dmSavedMeta(t, w, id)
	assert.Equal(1, meta.EditCount)
}

func TestDMReactionReplayKeepsRemoval(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	react := dmReactionItem(w, "react", "❤")
	unreact := dmReactionItem(w, "unreact", "")
	unreact.Value = []byte(strings.ReplaceAll(string(unreact.Value), "1793577600123", "1793577601123"))
	require.Equal(metawebhook.Applied, dmApply(t, h, react).Class)
	require.Equal(metawebhook.Applied, dmApply(t, h, unreact).Class)
	// Restart the handler, and replay both polling and the older delivery.
	_, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "hello inbox"))
	require.NoError(err)
	assert.Equal(metawebhook.Ignored, dmApply(t, dmTestHandler(t, w), react).Class)
	var count int
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM reactions").Scan(&count))
	assert.Zero(count)
}
func TestDMHandleInstagramDeleteKeepsBody(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformInstagram)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	item := dmItem(w, "messages", `{"sender":{"id":"300"},"recipient":{"id":"1784"},"timestamp":1793577601123,"message":{"mid":"m_1","is_deleted":true}}`)
	require.Equal(metawebhook.Applied, dmApply(t, h, item).Class)
	_, _, err := w.persist(t.Context(), dmTestRecord(t, w, "m_1", "hello inbox"))
	require.NoError(err)
	m := message(t, w.store, w.sourceID, "m_1")
	body, err := w.store.GetMessageBodyText(m.ID)
	require.NoError(err)
	assert.Equal("hello inbox", body)
	var deleted bool
	require.NoError(w.store.DB().QueryRow("SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id=?", m.ID).Scan(&deleted))
	assert.True(deleted)
	item.Value = []byte(strings.ReplaceAll(string(item.Value), "m_1", "missing"))
	assert.Equal(metawebhook.Ignored, dmApply(t, h, item).Class)
}
func TestDMHandleUnroutedEntry(t *testing.T) {
	for _, kind := range []string{"page", "disabled", "retired"} {
		t.Run(kind, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			w := dmTestWriter(t, PlatformMessenger)
			h := dmTestHandler(t, w)
			item := dmMessageItem(w, false)
			switch kind {
			case "page":
				item.EntryID = "101"
			case "disabled":
				h.route = func(context.Context, string, string, string) (DMSource, *store.Source, error) {
					return DMSource{}, nil, ErrDMSourceUnrouted
				}
			case "retired":
				b, err := w.store.GetOrCreateSource(SourceTypeFacebookMessages, "app:99:page:100")
				require.NoError(err)
				_, err = w.store.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: w.sourceID, IntoSourceID: b.ID})
				require.NoError(err)
			}
			assert.Equal(metawebhook.Unrouted, dmApply(t, h, item).Class)
			n, err := w.store.CountMessagesWithRaw(w.sourceID)
			require.NoError(err)
			assert.Zero(n)
		})
	}
}
func TestDMHandleMisroutedRecipient(t *testing.T) {
	for _, echo := range []bool{false, true} {
		t.Run(strconv.FormatBool(echo), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			w := dmTestWriter(t, PlatformMessenger)
			item := dmMessageItem(w, echo)
			item.Value = []byte(strings.ReplaceAll(string(item.Value), `"100"`, `"101"`))
			assert.Equal(metawebhook.ItemOutcome{Class: metawebhook.Invalid, ErrorClass: "misrouted"}, dmApply(t, dmTestHandler(t, w), item))
			n, err := w.store.CountMessagesWithRaw(w.sourceID)
			require.NoError(err)
			assert.Zero(n)
		})
	}
}
func TestDMHandleLockBusyRetry(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	exec, err := w.store.AcquireSyncExecutionContext(t.Context(), w.sourceID)
	require.NoError(err)
	defer func() { require.NoError(exec.Release()) }()
	out := dmApply(t, dmTestHandler(t, w), dmMessageItem(w, false))
	assert.Equal(metawebhook.Retry, out.Class)
	assert.False(out.CountAttempt)
	n, err := w.store.CountMessagesWithRaw(w.sourceID)
	require.NoError(err)
	assert.Zero(n)
}
func TestDMHandleOneRunPerSource(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformMessenger)
	other := DMSource{AppID: "9", PageID: "101", Platform: PlatformMessenger}
	source, err := w.store.GetOrCreateSource(SourceTypeFacebookMessages, other.Identifier())
	require.NoError(err)
	w2 := &dmWriter{store: w.store, sourceID: source.ID, src: other}
	h := NewDMHandler(w.store, func(ctx context.Context, app, object, entry string) (DMSource, *store.Source, error) {
		use := w
		if entry == "101" {
			use = w2
		}
		s, err := w.store.GetSourceByIDContext(ctx, use.sourceID)
		return use.src, s, err
	})
	run, err := w.store.StartSync(w.sourceID, SourceTypeFacebookMessages)
	require.NoError(err)
	require.NoError(w.store.PauseSyncWithCheckpoint(run, &store.Checkpoint{PageToken: "preserved polling checkpoint"}))
	items := []metawebhook.Item{dmMessageItem(w, false), dmMessageItem(w2, false), dmMessageItem(w, false)}
	out := h.Handle(t.Context(), "9", items)
	require.Len(out, 3)
	for _, o := range out {
		require.Equal(metawebhook.Applied, o.Class)
	}
	for _, id := range []int64{w.sourceID, w2.sourceID} {
		var count int
		require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM sync_runs WHERE source_id=? AND sync_type=?", id, SourceTypeFacebookMessages+"_webhook").Scan(&count))
		assert.Equal(1, count)
	}
	prior, err := w.store.GetLatestCheckpointedSyncByTypeContext(t.Context(), w.sourceID, SourceTypeFacebookMessages)
	require.NoError(err)
	assert.Equal("preserved polling checkpoint", prior.CursorBefore.String)
}
func TestDMHandleOutOfScopeShapes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	w := dmTestWriter(t, PlatformInstagram)
	h := dmTestHandler(t, w)
	assert.Equal(metawebhook.Unhandled, dmApply(t, h, dmEditItem(w, "m_1", "edit", 1)).Class)
	assert.Equal(metawebhook.Unhandled, dmApply(t, h, dmItem(w, "messages", `{"unknown":true}`)).Class)
	assert.Equal(metawebhook.Invalid, dmApply(t, h, dmItem(w, "messages", strings.Repeat(" ", 256*1024+1))).Class)
	var event map[string]any
	require.NoError(json.Unmarshal(dmMessageItem(w, false).Value, &event))
	event["message"] = map[string]any{"mid": "m_1", "text": strings.Repeat("x", 256*1024)}
	raw, err := json.Marshal(event)
	require.NoError(err)
	item := dmItem(w, "messages", string(raw))
	assert.Equal(metawebhook.Invalid, dmApply(t, h, item).Class)
}

func TestDMReactionFailureRollsBack(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	w := dmTestWriter(t, PlatformMessenger)
	h := dmTestHandler(t, w)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmMessageItem(w, false)).Class)
	require.Equal(metawebhook.Applied, dmApply(t, h, dmReactionItem(w, "react", "❤")).Class)
	id := message(t, w.store, w.sourceID, "m_1").ID
	prior, err := w.store.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	_, err = w.store.DB().Exec(`CREATE TRIGGER reject_reaction_metadata BEFORE UPDATE OF metadata ON messages BEGIN SELECT RAISE(FAIL, 'fixture failure'); END`)
	require.NoError(err)
	assert.Equal(metawebhook.Retry, dmApply(t, h, dmReactionItem(w, "unreact", "")).Class)
	var count int
	require.NoError(w.store.DB().QueryRow("SELECT count(*) FROM reactions").Scan(&count))
	assert.Equal(1, count)
	after, err := w.store.GetMessageMetadataContext(t.Context(), id)
	require.NoError(err)
	assert.Equal(prior, after)
}

func TestDMStoryProjectionsMerge(t *testing.T) {
	for _, nodeFirst := range []bool{false, true} {
		t.Run(strconv.FormatBool(nodeFirst), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			w := dmTestWriter(t, PlatformInstagram)
			h := dmTestHandler(t, w)
			mention := dmItem(w, "messages", `{"sender":{"id":"300"},"recipient":{"id":"1784"},"timestamp":1793577600123,"message":{"mid":"m_1","attachments":[{"type":"story_mention","payload":{"url":"https://cdn.example/story"}}]}}`)
			node := DMMessage{ID: "m_1", From: DMParticipant{ID: "300"}, Story: &DMStory{ID: "s_1"}, Raw: []byte(`{}`)}
			persistNode := func() {
				rec, err := recordFromNode(w.src, node)
				require.NoError(err)
				_, _, err = w.persist(t.Context(), rec)
				require.NoError(err)
			}
			if nodeFirst {
				persistNode()
			}
			require.Equal(metawebhook.Applied, dmApply(t, h, mention).Class)
			if !nodeFirst {
				persistNode()
			}
			id := message(t, w.store, w.sourceID, "m_1").ID
			metadata, _ := dmSavedMeta(t, w, id)
			require.NotNil(metadata.Story)
			assert.Equal("s_1", metadata.Story.ID)
			assert.Equal("mention", metadata.Story.Kind)
		})
	}
}
