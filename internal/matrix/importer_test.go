package matrix

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestImporterBackfillsJoinedRoomAndPersistsCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("offline", r.URL.Query().Get("set_presence"),
			"every Matrix sync request must keep the archival account offline")
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[{"type":"m.room.name","state_key":"","content":{"name":"Example room"}}]},"timeline":{"events":[{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* updated","m.new_content":{"msgtype":"m.text","body":"updated"},"m.relates_to":{"rel_type":"m.replace","event_id":"$one"}}},{"type":"m.room.message","event_id":"$foreign-edit","sender":"@intruder:example.org","origin_server_ts":3500,"content":{"msgtype":"m.text","body":"* replaced by someone else","m.new_content":{"msgtype":"m.text","body":"replaced by someone else"},"m.relates_to":{"rel_type":"m.replace","event_id":"$one"}}},{"type":"m.reaction","event_id":"$reaction","sender":"@archive:example.org","origin_server_ts":4000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$one","key":"👍"}}},{"type":"m.reaction","event_id":"$reaction-removed","sender":"@archive:example.org","origin_server_ts":4250,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$one","key":"👎"}}},{"type":"m.room.redaction","event_id":"$redact-reaction","sender":"@archive:example.org","origin_server_ts":4500,"redacts":"$reaction-removed","content":{}},{"type":"m.room.redaction","event_id":"$redact-message","sender":"@archive:example.org","origin_server_ts":4750,"redacts":"$delete-me","content":{}},{"type":"org.matrix.msc4075.rtc.notification","event_id":"$unsupported-call","sender":"@member:example.org","origin_server_ts":4900,"content":{"notification_type":"ring"}},{"type":"m.room.encrypted","event_id":"$encrypted","sender":"@member:example.org","origin_server_ts":5000,"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque","session_id":"session","sender_key":"key"}},{"type":"m.room.message","event_id":"$reply","sender":"@archive:example.org","origin_server_ts":5500,"content":{"msgtype":"m.text","body":"reply","m.relates_to":{"m.in_reply_to":{"event_id":"$one"}}}},{"type":"m.room.message","event_id":"$image","sender":"@member:example.org","origin_server_ts":6000,"content":{"msgtype":"m.image","body":"photo.jpg","url":"mxc://example.org/image","info":{"mimetype":"image/jpeg","size":1234}}},{"type":"m.room.message","event_id":"$orphan-reply","sender":"@member:example.org","origin_server_ts":6500,"content":{"msgtype":"m.text","body":"visible reply","m.relates_to":{"m.in_reply_to":{"event_id":"$not-visible"}}}}],"prev_batch":"older-1"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("older-1", r.URL.Query().Get("from"))
		assert.Equal("b", r.URL.Query().Get("dir"))
		_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$delete-me","sender":"@member:example.org","origin_server_ts":1500,"content":{"msgtype":"m.text","body":"remove this"}},{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}],"end":""}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"@member:example.org":["!room:example.org"]}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Equal(int64(6), sum.MessagesAdded)
	assert.Equal(int64(1), sum.Undecryptable)
	assert.Equal(int64(1), sum.EventsSkipped)
	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(5), count)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$one", "$delete-me", "$encrypted", "$reply", "$image", "$orphan-reply"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$one"])
	require.NoError(err)
	assert.Equal("updated", body)
	originalRaw, err := st.GetMessageRaw(messageIDs["$one"])
	require.NoError(err)
	var original event.Event
	require.NoError(json.Unmarshal(originalRaw, &original))
	require.NoError(original.Content.ParseRaw(original.Type))
	assert.Equal(id.EventID("$one"), original.ID)
	assert.Equal("original", original.Content.AsMessage().Body)
	body, err = st.GetMessageBodyText(messageIDs["$encrypted"])
	require.NoError(err)
	assert.Equal(encryptedPlaceholder, body)
	rawRows, err := st.ScanArchivedRawMessages(source.ID, rawFormat, 0, 10)
	require.NoError(err)
	var encrypted event.Event
	for _, row := range rawRows {
		var evt event.Event
		require.NoError(json.Unmarshal(row.RawData, &evt))
		if evt.ID == "$encrypted" {
			encrypted = evt
		}
	}
	assert.Equal(id.RoomID("!room:example.org"), encrypted.RoomID)
	var replyToMessageID int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), messageIDs["$reply"]).Scan(&replyToMessageID))
	assert.Equal(messageIDs["$one"], replyToMessageID)
	var orphanReplyTargetValid bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$orphan-reply"]).Scan(&orphanReplyTargetValid))
	assert.False(orphanReplyTargetValid)
	var deletedAtValid bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$delete-me"]).Scan(&deletedAtValid))
	assert.True(deletedAtValid)
	var reactions int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM reactions WHERE message_id = ?`), messageIDs["$one"]).Scan(&reactions))
	assert.Equal(1, reactions)
	var sourceReactionID string
	require.NoError(st.DB().QueryRow(st.Rebind(`
		SELECT rse.source_reaction_id
		FROM reaction_source_events rse
		JOIN reactions r ON r.id = rse.reaction_id
		WHERE r.message_id = ?`), messageIDs["$one"]).Scan(&sourceReactionID))
	assert.Equal("$reaction", sourceReactionID)
	assert.Equal(int64(1), sum.RelationsUnresolved)
	var conversationType string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type FROM conversations WHERE source_id = ?`), source.ID).Scan(&conversationType))
	assert.Equal("direct_chat", conversationType)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := loadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal("next-1", state.NextBatch)
	assert.True(state.Rooms["!room:example.org"].Backfilled)

	fullSummary, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: "@archive:example.org", Full: true,
	})
	require.NoError(err)
	assert.Equal(int64(0), fullSummary.MessagesAdded)
	assert.Equal(int64(1), fullSummary.RelationsUnresolved, "the orphan reply still has no target")
	body, err = st.GetMessageBodyText(messageIDs["$one"])
	require.NoError(err)
	assert.Equal("updated", body)
}

func TestImporterFullSyncIgnoresMalformedSavedCursor(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(r.URL.Query().Get("since"))
		_, _ = w.Write([]byte(`{"next_batch":"fresh","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	syncID, err := st.StartSync(source.ID, SourceType)
	require.NoError(err)
	require.NoError(st.CompleteSync(syncID, "malformed saved cursor"))

	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: source.Identifier, Full: true,
	})
	require.NoError(err)
}

func TestImporterDoesNotRecreateRemovedSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, _, err = st.RemoveSourceSerialized(t.Context(), source.ID)
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID(source.Identifier), "token")
	require.NoError(err)

	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{
		UserID: source.Identifier,
	})

	require.ErrorIs(err, store.ErrSourceNotFound)
	sources, listErr := st.ListSources(SourceType)
	require.NoError(listErr)
	assert.Empty(sources)
}

func TestRoomIncluded(t *testing.T) {
	tests := []struct {
		name    string
		include []string
		exclude []string
		want    bool
	}{
		{name: "all rooms by default", want: true},
		{name: "included room", include: []string{"!room:example.org"}, want: true},
		{name: "not in include list", include: []string{"!other:example.org"}},
		{name: "excluded room", exclude: []string{"!room:example.org"}},
		{name: "exclude wins", include: []string{"!room:example.org"}, exclude: []string{"!room:example.org"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, roomIncluded("!room:example.org", tt.include, tt.exclude))
		})
	}
}

func TestRoomTitleUsesLatestTimelineRename(t *testing.T) {
	assert := assert.New(t)
	first := matrixTestEvent(t, `{"type":"m.room.name","event_id":"$name-1","state_key":"","content":{"name":"First"}}`)
	second := matrixTestEvent(t, `{"type":"m.room.name","event_id":"$name-2","state_key":"","content":{"name":"Second"}}`)
	title, present := roomTitle([]*event.Event{first, second})
	assert.True(present)
	assert.Equal("Second", title)
	empty, present := roomTitle([]*event.Event{matrixTestEvent(t, `{"type":"m.room.name","event_id":"$name-3","state_key":"","content":{"name":""}}`)})
	assert.True(present)
	assert.Empty(empty)
}

func TestImporterReclassifiesInactiveRoomFromDirectAccountData(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		if syncCalls == 1 {
			_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[]}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		if syncCalls == 1 {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`{"@member:example.org":["!room:example.org"]}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	importer := NewImporter(st, &Runtime{Client: client})
	_, err = importer.Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	var conversationType string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT conversation_type FROM conversations WHERE source_conversation_id = ?`), "!room:example.org").Scan(&conversationType))
	assert.Equal("direct_chat", conversationType)
}

func TestImporterExplicitEmptyRoomNameClearsArchivedTitle(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		name := "Named room"
		if syncCalls > 1 {
			name = ""
		}
		_, _ = fmt.Fprintf(w, `{"next_batch":"next-%d","rooms":{"join":{"!room:example.org":{"state":{"events":[{"type":"m.room.name","state_key":"","content":{"name":%q}}]},"timeline":{"events":[]}}}}}`, syncCalls, name)
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member fallback"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	var title string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT title FROM conversations WHERE source_conversation_id = ?`), "!room:example.org").Scan(&title))
	assert.Empty(title)
}

func TestLatestEditWinsOriginalAtEqualTimestamp(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$z-original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$a-edit","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$z-original"}}}`), sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$z-original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$z-original"])
	require.NoError(err)
	assert.Equal("edited", body)
}

func TestImporterSkipsFirstSeenStrippedMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$already-redacted","sender":"@member:example.org","origin_server_ts":1000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","content":{}}}}`), &ImportSummary{}))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$already-redacted"})
	require.NoError(err)
	assert.Zero(messageIDs["$already-redacted"])
}

func TestImporterKeepsNewestEditAndRecomputesAfterRedaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, map[id.EventID]string{
		"$original": `{"type":"m.room.message","event_id":"$older","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* old","m.new_content":{"msgtype":"m.text","body":"old"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
	})})
	sum := &ImportSummary{}

	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`)
	newer := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$newer","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* newer version","m.new_content":{"msgtype":"m.text","body":"newer version"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)
	older := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$older","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* old","m.new_content":{"msgtype":"m.text","body":"old"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)
	redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","origin_server_ts":4000,"redacts":"$newer","content":{}}`)
	for _, evt := range []*event.Event{original, newer, older} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, sum))
	}
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("newer version", body, "an older edit from a later sync must not win")
	var sizeEstimate int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messageIDs["$original"]).Scan(&sizeEstimate))
	assert.Equal(int64(len("newer version")), sizeEstimate)
	labelID, err := st.EnsureLabel(source.ID, "local-review", "Local review", "user")
	require.NoError(err)
	require.NoError(st.AddMessageLabels(messageIDs["$original"], []int64{labelID}))
	_, err = st.DB().Exec(st.Rebind(`UPDATE messages SET embed_gen = 7 WHERE id = ?`), messageIDs["$original"])
	require.NoError(err)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, original, sum))
	var embedGen int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT embed_gen FROM messages WHERE id = ?`), messageIDs["$original"]).Scan(&embedGen))
	assert.Equal(int64(7), embedGen, "unchanged full replay must preserve the selected edit's embedding generation")
	labelIDs, err := st.MessageLabelIDsContext(t.Context(), messageIDs["$original"])
	require.NoError(err)
	assert.Equal([]int64{labelID}, labelIDs, "provider replay must preserve locally assigned labels")
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction, sum))
	body, err = st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("old", body, "redacting the winning edit selects the latest survivor")
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT size_estimate FROM messages WHERE id = ?`), messageIDs["$original"]).Scan(&sizeEstimate))
	assert.Equal(int64(len("old")), sizeEstimate)
}

func TestImporterRejectsCrossRoomRelations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	roomA, err := st.EnsureConversationWithType(source.ID, "!room-a:example.org", "group_chat", "Room A")
	require.NoError(err)
	roomB, err := st.EnsureConversationWithType(source.ID, "!room-b:example.org", "group_chat", "Room B")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$room-a-message","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"room A"}}`)
	edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$room-a-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$room-a-message"}}}`)
	reaction := matrixTestEvent(t, `{"type":"m.reaction","event_id":"$room-a-reaction","sender":"@member:example.org","origin_server_ts":3000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$room-a-message","key":"ok"}}}`)
	for _, evt := range []*event.Event{original, edit, reaction} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, roomA, evt, sum))
	}
	crossRoomReaction := matrixTestEvent(t, `{"type":"m.reaction","event_id":"$room-b-reaction","sender":"@member:example.org","origin_server_ts":4000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$room-a-message","key":"no"}}}`)
	require.NoError(imp.persistEvent(t.Context(), source.ID, roomB, crossRoomReaction, sum))
	for i, target := range []string{"$room-a-message", "$room-a-edit", "$room-a-reaction"} {
		redaction := matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.redaction","event_id":"$room-b-redaction-%d","sender":"@member:example.org","origin_server_ts":%d,"redacts":%q,"content":{}}`, i, 5000+i, target))
		if err := imp.persistEvent(t.Context(), source.ID, roomB, redaction, sum); err != nil {
			require.ErrorIs(err, errRelationTargetMissing)
		}
	}
	var reactions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Equal(1, reactions)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$room-a-message"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$room-a-message"])
	require.NoError(err)
	assert.Equal("edited", body)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`),
		messageIDs["$room-a-message"]).Scan(&deleted))
	assert.False(deleted)
}

func TestImporterDoesNotRestoreRedactedRelationPayloads(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	original := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`)
	reaction := matrixTestEvent(t, `{"type":"m.reaction","event_id":"$reaction","sender":"@member:example.org","origin_server_ts":2000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$original","key":"ok"}}}`)
	edit := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`)
	for _, evt := range []*event.Event{original, reaction, edit} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, evt, sum))
	}
	for _, target := range []string{"$reaction", "$edit"} {
		redaction := matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","origin_server_ts":4000,"redacts":"`+target+`","content":{}}`)
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, redaction, sum))
	}
	// The homeserver serves redacted events stripped, with the redaction attached.
	for _, eventID := range []string{"$reaction", "$edit"} {
		stripped := matrixTestEvent(t, `{"type":"m.room.message","event_id":"`+eventID+`","sender":"@member:example.org","origin_server_ts":2000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","content":{}}}}`)
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, stripped, sum))
	}
	var reactions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Zero(reactions)
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("original", body)
}

func TestImporterFullReplayPreservesRedactedMessageContent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", id.UserID("@archive:example.org"), "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"retained"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","content":{}}}}`), sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$one"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$one"])
	require.NoError(err)
	assert.Equal("retained", body)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$one"]).Scan(&deleted))
	assert.True(deleted)
}

func TestImporterAppliesTargetOfStrippedRedaction(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	client, err := mautrix.NewClient("https://example.invalid", "@archive:example.org", "token")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: client})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$message","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"retained"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","origin_server_ts":2000,"content":{"redacts":"$message"},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redact-redaction","sender":"@member:example.org","content":{}}}}`), sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$message"})
	require.NoError(err)
	var deleted bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at IS NOT NULL FROM messages WHERE id = ?`), messageIDs["$message"]).Scan(&deleted))
	assert.True(deleted)
}

func TestImporterFullReplayRedactedEditDoesNotCreateMessage(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original-edit-target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$stripped-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original-edit-target"}}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$stripped-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{},"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$redaction","sender":"@member:example.org","content":{}}}}`), sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original-edit-target", "$stripped-edit"})
	require.NoError(err)
	assert.Zero(messageIDs["$stripped-edit"])
	body, err := st.GetMessageBodyText(messageIDs["$original-edit-target"])
	require.NoError(err)
	assert.Equal("original", body)
}

func matrixTestEvent(t *testing.T, raw string) *event.Event {
	t.Helper()
	var evt event.Event
	require.NoError(t, json.Unmarshal([]byte(raw), &evt))
	evt.RoomID = "!room:example.org"
	return &evt
}

func TestMessageBodyStripsMatrixReplyFallback(t *testing.T) {
	assert := assert.New(t)
	t.Parallel()
	content := &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          "> <@other:example.org> quoted text\n>\nreply text",
		Format:        event.FormatHTML,
		FormattedBody: "<mx-reply><blockquote>quoted text</blockquote></mx-reply><p>reply text</p>",
		RelatesTo:     &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$target"}},
	}
	originalBody := content.Body
	originalFormattedBody := content.FormattedBody

	body := messageBody(content)

	assert.Equal("reply text", body)
	assert.NotContains(body, "quoted text")
	assert.Equal(originalBody, content.Body, "raw event content must remain unchanged")
	assert.Equal(originalFormattedBody, content.FormattedBody,
		"raw formatted event content must remain unchanged")
}

func TestOnlyMutatingMessageRelationsAreDeferred(t *testing.T) {
	assert := assert.New(t)
	reply := &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{
		MsgType:   event.MsgText,
		RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$target"}},
	}}}
	assert.False(shouldDeferRelation(reply), "reply messages must exist before reactions are replayed")

	edit := &event.Event{Type: event.EventMessage, Content: event.Content{Parsed: &event.MessageEventContent{
		MsgType:   event.MsgText,
		RelatesTo: &event.RelatesTo{Type: event.RelReplace, EventID: "$target"},
	}}}
	assert.True(shouldDeferRelation(edit))
}

func TestDeferredReplyResolutionDoesNotOverwriteEditedBody(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversation(source.ID, "!room:example.org", "Example room")
	require.NoError(err)
	targetID, err := st.UpsertMessage(&store.Message{
		ConversationID: conversationID, SourceID: source.ID,
		SourceMessageID: "$target", MessageType: SourceType,
	})
	require.NoError(err)
	replyID, err := st.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{
			ConversationID: conversationID, SourceID: source.ID,
			SourceMessageID: "$reply", MessageType: SourceType,
		},
		BodyText: sql.NullString{String: "newest edit", Valid: true},
	})
	require.NoError(err)
	reply := &event.Event{
		ID: "$reply", RoomID: "!room:example.org", Type: event.EventMessage,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText, Body: "original reply",
			RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$target"}},
		}},
	}
	rawReply, err := json.Marshal(reply, json.Deterministic(true))
	require.NoError(err)
	var restoredReply event.Event
	require.NoError(json.Unmarshal(rawReply, &restoredReply))
	assert.Nil(restoredReply.Content.Parsed)

	require.NoError(NewImporter(st, nil).replayDeferredRelation(
		t.Context(), source.ID, conversationID, &restoredReply, &ImportSummary{},
	))
	body, err := st.GetMessageBodyText(replyID)
	require.NoError(err)
	assert.Equal("newest edit", body)
	var replyTo int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), replyID).Scan(&replyTo))
	assert.Equal(targetID, replyTo)
}

func TestImporterResumesFailedRoomBackfillCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var requestedFrom, requestedTo []string
	failedOnce := false
	var syncCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		syncCalls++
		if syncCalls == 1 {
			_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$recent","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"recent"}},{"type":"m.room.message","event_id":"$edit-oldest","sender":"@member:example.org","origin_server_ts":3500,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$oldest"}}}],"prev_batch":"older-1"}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$new","sender":"@member:example.org","origin_server_ts":5000,"content":{"msgtype":"m.text","body":"new"}}],"limited":true,"prev_batch":"fresh-gap"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		from := r.URL.Query().Get("from")
		requestedFrom = append(requestedFrom, from)
		requestedTo = append(requestedTo, r.URL.Query().Get("to"))
		if from == "fresh-gap" {
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$between","sender":"@member:example.org","origin_server_ts":4000,"content":{"msgtype":"m.text","body":"between"}}]}`))
			return
		}
		if from == "older-1" {
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$middle","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"middle"}}],"end":"older-2"}`))
			return
		}
		if !failedOnce {
			failedOnce = true
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"temporary test interruption"}`))
			return
		}
		_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$oldest","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"oldest"}}],"end":""}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	options := ImportOptions{UserID: "@archive:example.org"}
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), options)
	require.Error(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), options)
	require.NoError(err)
	require.Equal([]string{"older-1", "older-2", "fresh-gap", "older-2"}, requestedFrom)
	assert.Equal([]string{"", "", "next-1", ""}, requestedTo,
		"the gap after an interrupted first run stops at that run's sync token")

	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(5), count)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$oldest"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$oldest"])
	require.NoError(err)
	assert.Equal("edited", body, "an edit deferred before the interruption applies once its target arrives")
}

func TestImporterAppliesBackfillEditsOldestFirst(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"next_batch":"next","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[],"prev_batch":"newer-page"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("from") == "newer-page" {
			_, _ = w.Write([]byte(`{"chunk":[],"end":"middle-page"}`))
			return
		}
		if r.URL.Query().Get("from") == "middle-page" {
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$latest-edit","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* latest","m.new_content":{"msgtype":"m.text","body":"latest"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}],"end":"older-page"}`))
			return
		}
		_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$older-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* older","m.new_content":{"msgtype":"m.text","body":"older"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}},{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}],"end":""}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	messages, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messages["$original"])
	require.NoError(err)
	assert.Equal("latest", body)
}

func TestImporterFillsLimitedIncrementalTimelineGap(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var gapRequests []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") == "next-1" {
			_, _ = w.Write([]byte(`{"next_batch":"next-2","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[{"type":"m.room.message","event_id":"$new","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"new"}}],"prev_batch":"gap-1"}}}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$known","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"known"}}],"prev_batch":"initial-backfill"}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		switch query.Get("from") {
		case "initial-backfill":
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$old","sender":"@member:example.org","origin_server_ts":500,"content":{"msgtype":"m.text","body":"old"}}]}`))
		case "gap-1":
			gapRequests = append(gapRequests, query.Get("to"))
			// An empty page with an end token is not the end of the gap.
			_, _ = w.Write([]byte(`{"chunk":[],"end":"gap-2"}`))
		case "gap-2":
			gapRequests = append(gapRequests, query.Get("to"))
			_, _ = w.Write([]byte(`{"chunk":[{"type":"m.room.message","event_id":"$gap","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"gap"}}]}`))
		default:
			http.Error(w, "unexpected history cursor", http.StatusBadRequest)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	opts := ImportOptions{UserID: "@archive:example.org"}
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	second, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal(int64(2), second.MessagesAdded)
	assert.Equal([]string{"next-1", "next-1"}, gapRequests, "the gap walk stops at the previous sync token")

	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(4), count)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := loadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal(&RoomState{Backfilled: true, SyncedTo: "next-2", EventsCovered: true, EventsSince: "next-1", EventsMode: store.IngestLive, GapMode: store.IngestLive}, state.Rooms["!room:example.org"])
}

func TestImporterResumesInterruptedGapFromSavedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	msg := func(eventID string, ts int) string {
		return fmt.Sprintf(`{"type":"m.room.message","event_id":"%s","sender":"@member:example.org","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"%s"}}`, eventID, ts, eventID)
	}
	var historyRequests []string
	var resumedSyncs int
	failed := map[string]bool{}
	interruptOnce := func(w http.ResponseWriter, page string) bool {
		if failed[page] {
			return false
		}
		failed[page] = true
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errcode":"M_FORBIDDEN","error":"temporary test interruption"}`))
		return true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("since") != "next-1" {
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[%s],"prev_batch":"initial-backfill"}}}}}`, msg("$known", 1000))
			return
		}
		// A failed run never saves its token, so every later run syncs from next-1.
		resumedSyncs++
		switch resumedSyncs {
		case 1:
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-2","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[%s],"prev_batch":"gap-1"}}}}}`, msg("$new", 4000))
		case 2:
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-3","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[%s],"prev_batch":"fresh-gap"}}}}}`, msg("$newer", 6000))
		default:
			_, _ = fmt.Fprintf(w, `{"next_batch":"next-4","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"limited":true,"events":[%s],"prev_batch":"last-gap"}}}}}`, msg("$newest", 8000))
		}
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@archive:example.org":{"display_name":"Archive"},"@member:example.org":{"display_name":"Member"}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/messages", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		from := query.Get("from")
		if from != "initial-backfill" {
			historyRequests = append(historyRequests, from+"->"+query.Get("to"))
		}
		switch from {
		case "initial-backfill":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s]}`, msg("$old", 500))
		case "gap-1":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s],"end":"gap-2"}`, msg("$gap-newer", 3000))
		case "gap-2":
			if !interruptOnce(w, from) {
				_, _ = fmt.Fprintf(w, `{"chunk":[%s]}`, msg("$gap-older", 2000))
			}
		case "fresh-gap":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s],"end":"fresh-2"}`, msg("$fresh-newer", 5500))
		case "fresh-2":
			if !interruptOnce(w, from) {
				_, _ = fmt.Fprintf(w, `{"chunk":[%s,%s]}`, msg("$fresh-older", 5000), msg("$new", 4000))
			}
		case "last-gap":
			_, _ = fmt.Fprintf(w, `{"chunk":[%s,%s]}`, msg("$late", 7000), msg("$newer", 6000))
		default:
			http.Error(w, "unexpected history cursor", http.StatusBadRequest)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	opts := ImportOptions{UserID: "@archive:example.org"}
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.Error(err)
	checkpoint, err := st.GetLatestCheckpointedSyncByType(source.ID, SourceType)
	require.NoError(err)
	interrupted, err := loadSyncState(checkpoint.CursorBefore.String)
	require.NoError(err)
	assert.Equal("gap-2", interrupted.Rooms["!room:example.org"].GapFrom)
	assert.Equal("next-2", interrupted.Rooms["!room:example.org"].GapTo)

	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.Error(err, "the next gap is interrupted too")
	_, err = NewImporter(st, &Runtime{Client: client}).Import(t.Context(), opts)
	require.NoError(err)
	assert.Equal([]string{
		"gap-1->next-1", "gap-2->next-1",
		"gap-2->next-1", "fresh-gap->next-2", "fresh-2->next-2",
		"fresh-2->next-2", "last-gap->next-3",
	}, historyRequests, "each run finishes the saved gap from its page before walking newer events")

	ids := []string{"$known", "$old", "$new", "$gap-newer", "$gap-older", "$fresh-newer", "$fresh-older", "$newer", "$late", "$newest"}
	found, err := st.MessageExistsBatch(source.ID, ids)
	require.NoError(err)
	for _, eventID := range ids {
		assert.NotZero(found[eventID], eventID)
	}
	count, err := st.CountMessagesForSource(source.ID)
	require.NoError(err)
	assert.Equal(int64(len(ids)), count)
	run, err := st.GetLastSuccessfulSync(source.ID)
	require.NoError(err)
	state, err := loadSyncState(run.CursorAfter.String)
	require.NoError(err)
	assert.Equal(&RoomState{Backfilled: true, SyncedTo: "next-4", EventsCovered: true, EventsSince: "next-1", EventsMode: store.IngestLive, GapMode: store.IngestLive}, state.Rooms["!room:example.org"])
}

func TestImporterEditMustKeepEventType(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.sticker","event_id":"$sticker","sender":"@member:example.org","origin_server_ts":1000,"content":{"body":"party parrot","url":"mxc://example.org/parrot"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$text-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* replaced","m.new_content":{"msgtype":"m.text","body":"replaced"},"m.relates_to":{"rel_type":"m.replace","event_id":"$sticker"}}}`), sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$sticker"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(messageIDs["$sticker"])
	require.NoError(err)
	assert.Equal("party parrot", body)
	var edited bool
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT is_edited FROM messages WHERE id = ?`), messageIDs["$sticker"]).Scan(&edited))
	assert.False(edited)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$file","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.file","body":"report.pdf","url":"mxc://example.org/report"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$file-edit","sender":"@member:example.org","origin_server_ts":4000,"content":{"msgtype":"m.text","body":"* gone","m.new_content":{"msgtype":"m.text","body":"gone"},"m.relates_to":{"rel_type":"m.replace","event_id":"$file"}}}`), sum))
	messageIDs, err = st.MessageExistsBatch(source.ID, []string{"$file"})
	require.NoError(err)
	body, err = st.GetMessageBodyText(messageIDs["$file"])
	require.NoError(err)
	assert.Equal("gone", body, "Matrix lets an edit change a file into text")
}

// relationsClient serves each original's surviving m.replace events.
func relationsClient(t *testing.T, edits map[id.EventID]string) *mautrix.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v1/rooms/{room}/relations/{event}/m.replace", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"chunk":[` + edits[id.EventID(r.PathValue("event"))] + `]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(t, err)
	return client
}

func TestImporterReplaysRedactionsAfterReactionsDespiteClockSkew(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /_matrix/client/v3/sync", func(w http.ResponseWriter, _ *http.Request) {
		// The redaction's server timestamp is older than the reaction it removes.
		_, _ = w.Write([]byte(`{"next_batch":"next-1","rooms":{"join":{"!room:example.org":{"state":{"events":[]},"timeline":{"events":[{"type":"m.room.message","event_id":"$one","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"one"}},{"type":"m.reaction","event_id":"$late-reaction","sender":"@member:example.org","origin_server_ts":5000,"content":{"m.relates_to":{"rel_type":"m.annotation","event_id":"$one","key":"ok"}}},{"type":"m.room.redaction","event_id":"$early-redaction","sender":"@member:example.org","origin_server_ts":4000,"redacts":"$late-reaction","content":{}}]}}}}}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/user/@archive:example.org/account_data/m.direct", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("GET /_matrix/client/v3/rooms/!room:example.org/joined_members", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"joined":{"@member:example.org":{"display_name":"Member"}}}`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, id.UserID("@archive:example.org"), "token")
	require.NoError(err)

	st := testutil.NewTestStore(t)
	_, err = st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	sum, err := NewImporter(st, &Runtime{Client: client}).Import(t.Context(), ImportOptions{UserID: "@archive:example.org"})
	require.NoError(err)
	assert.Zero(sum.RelationsUnresolved)
	var reactions int
	require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&reactions))
	assert.Zero(reactions)
}

func TestImporterReappliesEditWhosePointerWasNotSaved(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`), sum))
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original"})
	require.NoError(err)
	messageID := messageIDs["$original"]
	// An interruption after the text and flag but before the pointer.
	require.NoError(imp.setBody(messageID, "edited"))
	require.NoError(st.SetMessageEdited(messageID))

	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* edited","m.new_content":{"msgtype":"m.text","body":"edited"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`), sum))
	edit, err := imp.appliedEdit(messageID)
	require.NoError(err)
	assert.Equal(appliedEdit{EventID: "$edit", TS: 2000}, edit)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.redaction","event_id":"$redact","sender":"@member:example.org","origin_server_ts":3000,"redacts":"$edit","content":{}}`), sum))
	body, err := st.GetMessageBodyText(messageID)
	require.NoError(err)
	assert.Equal("original", body)
}

func TestImporterIgnoresInvalidReplacements(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	for _, raw := range []string{
		`{"type":"m.room.message","event_id":"$original","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"original"}}`,
		`{"type":"m.room.message","event_id":"$bare-edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* bare","m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
		`{"type":"m.room.message","event_id":"$state-edit","state_key":"","sender":"@member:example.org","origin_server_ts":3000,"content":{"msgtype":"m.text","body":"* state","m.new_content":{"msgtype":"m.text","body":"state"},"m.relates_to":{"rel_type":"m.replace","event_id":"$original"}}}`,
	} {
		require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, raw), sum))
	}
	messageIDs, err := st.MessageExistsBatch(source.ID, []string{"$original", "$bare-edit"})
	require.NoError(err)
	assert.Zero(messageIDs["$bare-edit"], "a replacement without new content is not a message")
	body, err := st.GetMessageBodyText(messageIDs["$original"])
	require.NoError(err)
	assert.Equal("original", body)
}

func TestImporterReplayRecoversReplyLinkLostBeforeCheckpoint(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	reply := `{"type":"m.room.message","event_id":"$reply","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"reply","m.relates_to":{"m.in_reply_to":{"event_id":"$target"}}}}`
	// The reply was saved, but the run stopped before its deferral was checkpointed.
	require.ErrorIs(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, reply), sum), errRelationTargetMissing)
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$target","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"target"}}`), sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, reply), sum))
	ids, err := st.MessageExistsBatch(source.ID, []string{"$reply", "$target"})
	require.NoError(err)
	var replyTo int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids["$reply"]).Scan(&replyTo))
	assert.Equal(ids["$target"], replyTo)
}

func TestImporterSkipsMalformedEventAndKeepsGoing(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	malformed := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$bad","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":42}}`)
	assert.False(shouldDeferRelation(malformed))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, malformed, sum))
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID,
		matrixTestEvent(t, `{"type":"m.room.message","event_id":"$good","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"good"}}`), sum))
	assert.Equal(int64(1), sum.EventsSkipped)
	ids, err := st.MessageExistsBatch(source.ID, []string{"$bad", "$good"})
	require.NoError(err)
	assert.Zero(ids["$bad"])
	assert.NotZero(ids["$good"])
}

func TestMessageBodyKeepsCaptionsAndDropsReplyFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want string
	}{
		"image caption":        {`{"msgtype":"m.image","body":"Meter reading 12345","filename":"photo.jpg","url":"mxc://example.org/p"}`, "[image] Meter reading 12345"},
		"file without caption": {`{"msgtype":"m.file","body":"report.pdf","url":"mxc://example.org/r"}`, "[file: report.pdf]"},
		"plain reply fallback": {`{"msgtype":"m.text","body":"> <@other:example.org> their words\n\nmy answer","m.relates_to":{"m.in_reply_to":{"event_id":"$x"}}}`, "my answer"},
		"html reply fallback":  {`{"msgtype":"m.text","body":"> <@other:example.org> their words\n\nmy answer","format":"org.matrix.custom.html","formatted_body":"<mx-reply><blockquote>their words</blockquote></mx-reply>my answer","m.relates_to":{"m.in_reply_to":{"event_id":"$x"}}}`, "my answer"},
	} {
		t.Run(name, func(t *testing.T) {
			evt := matrixTestEvent(t, `{"type":"m.room.message","event_id":"$e","sender":"@member:example.org","origin_server_ts":1,"content":`+tc.raw+`}`)
			require.NoError(t, evt.Content.ParseRaw(evt.Type))
			assert.Equal(t, tc.want, messageBody(evt.Content.AsMessage()))
		})
	}
}

func TestImporterEditedReplyKeepsQuotedFallbackOut(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(err)
	conversationID, err := st.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Example")
	require.NoError(err)
	imp := NewImporter(st, &Runtime{Client: relationsClient(t, nil)})
	sum := &ImportSummary{}
	reply := `{"type":"m.room.message","event_id":"$reply","sender":"@member:example.org","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"answer","m.relates_to":{"m.in_reply_to":{"event_id":"$quoted"}}}}`
	edit := `{"type":"m.room.message","event_id":"$edit","sender":"@member:example.org","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"* fixed","m.new_content":{"msgtype":"m.text","body":"> <@other:example.org> their words\n\nfixed answer"},"m.relates_to":{"rel_type":"m.replace","event_id":"$reply"}}}`
	if err := imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, reply), sum); err != nil {
		require.ErrorIs(err, errRelationTargetMissing)
	}
	require.NoError(imp.persistEvent(t.Context(), source.ID, conversationID, matrixTestEvent(t, edit), sum))
	ids, err := st.MessageExistsBatch(source.ID, []string{"$reply"})
	require.NoError(err)
	body, err := st.GetMessageBodyText(ids["$reply"])
	require.NoError(err)
	assert.Equal("fixed answer", body)
}
