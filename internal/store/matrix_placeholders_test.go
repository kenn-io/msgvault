package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestMatrixUnsupportedPlaceholderRetainsRawAndInvalidatesIndexes(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(source.ID, "!room:example.org", "Room")
	req.NoError(err)
	raw := []byte(`{"type":"m.room.encrypted","content":{"ciphertext":"opaque"}}`)
	messageID, err := s.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: "$unsupported", MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}},
		BodyText: sql.NullString{String: "placeholder", Valid: true}, RawMIME: raw, RawFormat: "matrix_json", FTS: &store.FTSDoc{Body: "placeholder"},
	})
	req.NoError(err)
	_, err = s.DB().Exec(s.Rebind(`UPDATE messages SET embed_gen = 7 WHERE id = ?`), messageID)
	req.NoError(err)
	revision, err := s.DerivedDataRevision()
	req.NoError(err)
	req.NoError(s.MatrixRecoveryTransaction(t.Context(), func(view *store.Store) error {
		if err := view.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"matrix_event_type":"example.unsupported","matrix_megolm_session_0":"$unsupported"}`, Valid: true}); err != nil {
			return err
		}
		return view.HideMatrixPlaceholder(t.Context(), source.ID, messageID)
	}))
	stored, err := s.GetMessageRaw(messageID)
	req.NoError(err)
	check.Equal(raw, stored)
	var hidden bool
	var batch, embedded sql.NullString
	req.NoError(s.DB().QueryRow(s.Rebind(`SELECT deleted_at IS NOT NULL, delete_batch_id, embed_gen FROM messages WHERE id = ?`), messageID).Scan(&hidden, &batch, &embedded))
	check.True(hidden)
	check.False(batch.Valid)
	check.False(embedded.Valid)
	updated, err := s.DerivedDataRevision()
	req.NoError(err)
	check.Equal(revision+1, updated)
	if s.IsPostgreSQL() {
		var indexed bool
		req.NoError(s.DB().QueryRow(s.Rebind(`SELECT search_fts IS NOT NULL OR indexing_version IS NOT NULL FROM messages WHERE id = ?`), messageID).Scan(&indexed))
		check.False(indexed)
	} else {
		var indexed int
		req.NoError(s.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE rowid = ?`, messageID).Scan(&indexed))
		check.Zero(indexed)
	}
	rooms, err := s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	check.Empty(rooms)
	claims, err := s.MatrixRecoveredEvents(t.Context(), convID)
	req.NoError(err)
	check.Equal("$unsupported", claims["matrix_megolm_session_0"])
}

func TestMatrixPlaceholderDiscoveryBackfillPagingAndRetirement(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(source.ID, "!room:example.org", "Room")
	req.NoError(err)
	var ids []int64
	for _, eventID := range []string{"$first", "$second", "$third", "$plaintext", "$corrupt", "$other-label", "$last"} {
		raw := []byte(`{"type":"m.room.encrypted"}`)
		if eventID == "$plaintext" {
			raw = []byte(fmt.Sprintf(`{"type":"m.room.message","content":{"msgtype":"m.text","body":%q}}`, store.MatrixEncryptedPlaceholder))
		}
		snippet := store.MatrixEncryptedPlaceholder
		if eventID == "$corrupt" {
			raw = []byte("invalid JSON")
		}
		if eventID == "$other-label" {
			snippet = "another label"
		}
		messageID, err := s.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:  &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: eventID, MessageType: "matrix", Snippet: sql.NullString{String: snippet, Valid: true}},
			BodyText: sql.NullString{String: "placeholder", Valid: true}, RawMIME: raw, RawFormat: "matrix_json", FTS: &store.FTSDoc{Body: "placeholder"},
		})
		req.NoError(err)
		ids = append(ids, messageID)
	}
	req.NoError(s.MarkMessageDeleted(source.ID, "$second"))
	rooms, err := s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(rooms, 1)
	check.Equal(int64(4), rooms[0].Count)
	check.Equal(ids[6], rooms[0].MaxMessageID)
	check.Equal("$last", rooms[0].MaxEventID)
	for i, eventType := range []string{"m.room.encrypted", "m.room.encrypted", "m.room.encrypted", "m.room.message", ""} {
		metadata, err := s.GetMessageMetadata(ids[i])
		req.NoError(err)
		check.JSONEq(fmt.Sprintf(`{"matrix_event_type":%q}`, eventType), metadata.String)
	}
	_, err = s.DB().Exec(s.Rebind(`UPDATE message_raw SET raw_data = ? WHERE message_id = ?`), []byte("invalid JSON"), ids[0])
	req.NoError(err)
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(rooms, 1)
	check.Equal(int64(4), rooms[0].Count)
	check.Equal("!room:example.org", rooms[0].RoomID)
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), source.ID, []string{"!other:example.org"}, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	check.Empty(rooms)
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, []string{"!room:example.org"}, attachmentpolicy.Policy{})
	req.NoError(err)
	check.Empty(rooms)
	page, scanned, err := s.MatrixPlaceholders(t.Context(), convID, ids[0], 1)
	req.NoError(err)
	req.Len(page, 1)
	check.Equal(ids[1], scanned)
	check.Equal(ids[1], page[0].MessageID)
	check.True(page[0].Deleted)
	check.JSONEq(`{"type":"m.room.encrypted"}`, string(page[0].Raw))
	page, scanned, err = s.MatrixPlaceholders(t.Context(), convID, ids[1], 1)
	req.NoError(err)
	req.Len(page, 1)
	check.Equal(ids[2], scanned)
	check.Equal(ids[2], page[0].MessageID)
	check.False(page[0].Deleted)
	check.JSONEq(`{"type":"m.room.encrypted"}`, string(page[0].Raw))
	for _, messageID := range ids[3:5] {
		page, scanned, err = s.MatrixPlaceholders(t.Context(), convID, scanned, 1)
		req.NoError(err)
		check.Empty(page)
		check.Equal(messageID, scanned)
	}
	page, scanned, err = s.MatrixPlaceholders(t.Context(), convID, scanned, 1)
	req.NoError(err)
	req.Len(page, 1)
	check.Equal(ids[6], scanned)
	check.Equal(ids[6], page[0].MessageID)
	page, scanned, err = s.MatrixPlaceholders(t.Context(), convID, scanned, 1)
	req.NoError(err)
	check.Empty(page)
	check.Equal(ids[6], scanned)
	req.NoError(s.SetMessageMetadataContext(t.Context(), ids[3], sql.NullString{String: `{"matrix_event_type":"m.room.message"}`, Valid: true}))
	_, err = s.DB().Exec(s.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`), []byte("invalid compressed data"), ids[4])
	req.NoError(err)
	req.NoError(s.SetMessageMetadataContext(t.Context(), ids[4], sql.NullString{String: `{"matrix_event_type":"m.room.encrypted"}`, Valid: true}))
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, nil, attachmentpolicy.Policy{})
	req.NoError(err)
	req.Len(rooms, 1)
	check.Equal(int64(5), rooms[0].Count)
	page, scanned, err = s.MatrixPlaceholders(t.Context(), convID, ids[3], 1)
	req.NoError(err)
	check.Empty(page)
	check.Equal(ids[4], scanned)
	req.NoError(s.SetMessageReplyContext(t.Context(), ids[0], ids[2]))
	revision, err := s.DerivedDataRevision()
	req.NoError(err)
	req.NoError(s.DeleteMatrixPlaceholder(t.Context(), source.ID+1, ids[2]))
	req.NoError(s.SetConversationMetadata(convID, sql.NullString{String: `{"member_count":2}`, Valid: true}))
	req.NoError(s.DeleteMatrixPlaceholder(t.Context(), source.ID, ids[2]))
	metadata, err := s.GetConversationMetadata(convID)
	req.NoError(err)
	check.JSONEq(`{"member_count":2}`, metadata.String)
	updated, err := s.DerivedDataRevision()
	req.NoError(err)
	check.Equal(revision+1, updated)
	var reply sql.NullInt64
	req.NoError(s.DB().QueryRow(s.Rebind(`SELECT reply_to_message_id FROM messages WHERE id = ?`), ids[0]).Scan(&reply))
	check.False(reply.Valid)
	if !s.IsPostgreSQL() {
		var indexed int
		req.NoError(s.DB().QueryRow(`SELECT count(*) FROM messages_fts WHERE rowid = ?`, ids[2]).Scan(&indexed))
		check.Zero(indexed)
	}
	for _, table := range []string{"message_bodies", "message_raw"} {
		var count int
		req.NoError(s.DB().QueryRow(s.Rebind(`SELECT count(*) FROM `+table+` WHERE message_id = ?`), ids[2]).Scan(&count))
		check.Zero(count)
	}
	req.NoError(s.DeleteMatrixPlaceholder(t.Context(), source.ID, ids[2]))
	updated, err = s.DerivedDataRevision()
	req.NoError(err)
	check.Equal(revision+1, updated)
}

func TestMatrixRecoveryTransactionKeepsStoreConfigurationAndScope(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(source.ID, "!room:example.org", "Room")
	req.NoError(err)
	messageID, err := s.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: "$original", MessageType: "matrix"}})
	req.NoError(err)
	f := newAttrFixtureOn(t, s, "mbox", attrSink)
	f.confirm(attrSink)
	labels, err := s.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{"Outbox": {Name: "Outbox", Type: "system"}})
	req.NoError(err)
	id := f.persist(attrMail{raw: "Delivered-To: " + attrSink + "\r\n\r\nbody", labels: []int64{labels["Outbox"]}})
	syncID, err := s.StartSync(source.ID, "matrix")
	req.NoError(err)
	scoped := s.ScopedToSync(source.ID, syncID)
	req.NoError(scoped.MatrixRecoveryTransaction(t.Context(), func(view *store.Store) error {
		check.Equal(store.DBPathForTest(s), store.DBPathForTest(view))
		check.Equal(s.IsPostgreSQL(), view.IsPostgreSQL())
		check.Error(view.WithExclusiveLock(t.Context(), func() error { return nil }))
		_, err := view.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{"Outbox": {Name: "Outbox", Type: "system", SystemRole: store.LabelSystemRoleSent}})
		if err != nil {
			return err
		}
		check.NotContains(searchIDs(t, view, "received:"+attrSink), id)
		_, err = view.EnsureLabelsBatch(f.source.ID, map[string]store.LabelInfo{"Outbox": {Name: "Outbox", Type: "system"}})
		if err != nil {
			return err
		}
		check.Contains(searchIDs(t, view, "received:"+attrSink), id)
		return view.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"saved":true}`, Valid: true})
	}))
	check.Contains(searchIDs(t, s, "received:"+attrSink), id)
	req.NoError(scoped.FailSync(syncID, "interrupted"))
	err = scoped.MatrixRecoveryTransaction(t.Context(), func(view *store.Store) error {
		return view.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{})
	})
	req.ErrorIs(err, store.ErrSyncRunSuperseded)
	metadata, err := s.GetMessageMetadata(messageID)
	req.NoError(err)
	check.JSONEq(`{"saved":true}`, metadata.String)
}

func TestMatrixPlaceholderRoomsUsesPolicyParticipantCount(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	room, err := s.EnsureConversationWithType(source.ID, "!room:example.org", "group_chat", "Room")
	req.NoError(err)
	_, err = s.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message: &store.Message{SourceID: source.ID, ConversationID: room, SourceMessageID: "$encrypted", MessageType: "matrix", Snippet: sql.NullString{String: store.MatrixEncryptedPlaceholder, Valid: true}},
		RawMIME: []byte(`{"type":"m.room.encrypted"}`), RawFormat: "matrix_json",
	})
	req.NoError(err)
	policy := attachmentpolicy.Policy{MaxParticipants: 20}
	for _, roster := range []string{"missing", "unknown"} {
		if roster == "unknown" {
			req.NoError(s.MarkConversationMemberCountUnknown(room))
		}
		rooms, err := s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, nil, policy)
		req.NoError(err)
		req.Len(rooms, 1)
		check.Equal(21, rooms[0].ParticipantCount, roster)
	}
}
