package store_test

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	rooms, err := s.MatrixPlaceholderRooms(t.Context(), source.ID, nil, nil)
	req.NoError(err)
	check.Empty(rooms)
}

func setupMatrixPlaceholders(t *testing.T) (*store.Store, int64, int64, []int64) {
	t.Helper()
	req := require.New(t)
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
	return s, source.ID, convID, ids
}

func setCorruptCompressedPlaceholder(t *testing.T, s *store.Store, messageID int64) {
	t.Helper()
	req := require.New(t)
	_, err := s.DB().Exec(s.Rebind(`UPDATE message_raw SET raw_data = ?, compression = 'zlib' WHERE message_id = ?`), []byte("invalid compressed data"), messageID)
	req.NoError(err)
	req.NoError(s.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"matrix_event_type":"m.room.encrypted"}`, Valid: true}))
}

func TestMatrixPlaceholderDiscoveryAndBackfill(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s, sourceID, _, ids := setupMatrixPlaceholders(t)
	rooms, err := s.MatrixPlaceholderRooms(t.Context(), sourceID, nil, nil)
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
	check.Equal("!room:example.org", rooms[0].RoomID)
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), sourceID, []string{"!other:example.org"}, nil)
	req.NoError(err)
	check.Empty(rooms)
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), sourceID, nil, []string{"!room:example.org"})
	req.NoError(err)
	check.Empty(rooms)
	setCorruptCompressedPlaceholder(t, s, ids[4])
	rooms, err = s.MatrixPlaceholderRooms(t.Context(), sourceID, nil, nil)
	req.NoError(err)
	req.Len(rooms, 1)
	check.Equal(int64(5), rooms[0].Count)
	t.Run("paging", func(t *testing.T) {
		req, check := require.New(t), assert.New(t)
		s, _, convID, ids := setupMatrixPlaceholders(t)
		for i, eventType := range []string{"m.room.encrypted", "m.room.encrypted", "m.room.encrypted", "m.room.message", "", "m.room.encrypted", "m.room.encrypted"} {
			req.NoError(s.SetMessageMetadataContext(t.Context(), ids[i], sql.NullString{String: fmt.Sprintf(`{"matrix_event_type":%q}`, eventType), Valid: true}))
		}
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
		setCorruptCompressedPlaceholder(t, s, ids[4])
		page, scanned, err = s.MatrixPlaceholders(t.Context(), convID, ids[3], 1)
		req.NoError(err)
		check.Empty(page)
		check.Equal(ids[4], scanned)
	})
}

func TestDeleteMatrixPlaceholderRetiresRelatedData(t *testing.T) {
	req, check := require.New(t), assert.New(t)
	s, sourceID, _, ids := setupMatrixPlaceholders(t)
	req.NoError(s.SetMessageReplyContext(t.Context(), ids[0], ids[2]))
	revision, err := s.DerivedDataRevision()
	req.NoError(err)
	req.NoError(s.DeleteMatrixPlaceholder(t.Context(), sourceID, ids[2]))
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
	req.NoError(s.DeleteMatrixPlaceholder(t.Context(), sourceID, ids[2]))
	updated, err = s.DerivedDataRevision()
	req.NoError(err)
	check.Equal(revision+1, updated)
}

func TestMatrixRecoveryTransactionRejectsNestedLocksAndRollsBackSupersededSync(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	s := testutil.NewTestStore(t)
	source, err := s.GetOrCreateSource("matrix", "@archive:example.org")
	req.NoError(err)
	convID, err := s.EnsureConversation(source.ID, "!room:example.org", "Room")
	req.NoError(err)
	messageID, err := s.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, ConversationID: convID, SourceMessageID: "$original", MessageType: "matrix"}})
	req.NoError(err)
	syncID, err := s.StartSync(source.ID, "matrix")
	req.NoError(err)
	scoped := s.ScopedToSync(source.ID, syncID)
	req.NoError(scoped.MatrixRecoveryTransaction(t.Context(), func(view *store.Store) error {
		check.Error(view.WithExclusiveLock(t.Context(), func() error { return nil }))
		return view.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{String: `{"saved":true}`, Valid: true})
	}))
	req.NoError(scoped.FailSync(syncID, "interrupted"))
	err = scoped.MatrixRecoveryTransaction(t.Context(), func(view *store.Store) error {
		return view.SetMessageMetadataContext(t.Context(), messageID, sql.NullString{})
	})
	req.ErrorIs(err, store.ErrSyncRunSuperseded)
	metadata, err := s.GetMessageMetadata(messageID)
	req.NoError(err)
	check.JSONEq(`{"saved":true}`, metadata.String)
}
