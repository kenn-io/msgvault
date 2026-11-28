package whatsapp

import (
	"database/sql"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const appleReplySchema = `
	ALTER TABLE ZWAMESSAGE ADD COLUMN ZMEDIAITEM INTEGER;
	CREATE TABLE ZWAMEDIAITEM (Z_PK INTEGER PRIMARY KEY, Z_OPT INTEGER DEFAULT 1, ZMETADATA BLOB);
`

func TestAppleQuotedStanza(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		want     string
	}{
		{name: "other wire fields", metadata: "\x08\x96\x01\x11abcdefgh\x1a\x03abc\x25abcd\x2a\x03abc", want: "abc"},
		{name: "field 5 varint", metadata: "\x28\x01"},
		{name: "empty stanza", metadata: "\x2a\x00"},
		{name: "invalid stanza", metadata: "\x2a\x02a\x00"},
		{name: "truncated tag", metadata: "\x80"},
		{name: "truncated length", metadata: "\x2a\x80"},
		{name: "length exceeds remaining data", metadata: "\x2a\x04abc"},
		{name: "length exceeds platform int", metadata: "\x2a\xff\xff\xff\xff\xff\xff\xff\xff\xff\x01"},
		{name: "overflowing length", metadata: "\x2a\xff\xff\xff\xff\xff\xff\xff\xff\xff\x02"},
		{name: "truncated varint after quote", metadata: "\x2a\x03abc\x08\x80"},
		{name: "truncated fixed32 after quote", metadata: "\x2a\x03abc\x25ab"},
		{name: "truncated fixed64 after quote", metadata: "\x2a\x03abc\x11ab"},
		{name: "invalid field number", metadata: "\x2a\x03abc\x00\x01"},
		{name: "unsupported wire type", metadata: "\x2a\x03abc\x0e"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, appleQuotedStanza([]byte(tt.metadata)))
		})
	}
}

func TestImportAppleReplyLinks(t *testing.T) {
	path := createAppleMarkerFixture(t)
	execAppleFixture(t, path, appleReplySchema+`
		INSERT INTO ZWAMEDIAITEM (Z_PK, ZMETADATA) VALUES
			(1, X'2A096469726563742D696E'),
			(2, X'2A0C6C617465722D746172676574'),
			(3, X'2A076D697373696E67'),
			(4, X'2A0B67726F75702D696D616765'),
			(5, X'2A096469726563742D696E80'),
			(6, X'2A0E6475706C69636174652D74657874'),
			(7, X'2A0867726F75702D696E');
		UPDATE ZWAMESSAGE SET ZMEDIAITEM = 1 WHERE Z_PK = 2;
		UPDATE ZWAMESSAGE SET ZMEDIAITEM = 7 WHERE Z_PK = 4;
		INSERT INTO ZWAMESSAGE VALUES
			(11, 1, NULL, 'forward-reply', 1, 700000011, 'reply to a later row', 0, '', 1, 2),
			(12, 1, NULL, 'later-target', 1, 700000012, 'later row', 0, '', 1, NULL),
			(13, 1, NULL, 'missing-reply', 1, 700000013, 'reply to a missing row', 0, '', 1, 3),
			(14, 2, NULL, 'image-reply', 1, 700000014, 'reply to an image', 0, '', 1, 4),
			(15, 1, NULL, 'malformed-reply', 1, 700000015, 'reply with malformed metadata', 0, '', 1, 5),
			(16, 1, NULL, 'duplicate-reply', 1, 700000016, 'reply to a duplicate', 0, '', 1, 6),
			(17, 1, NULL, 'other-chat-reply', 1, 700000017, 'https://example.com', 7, '', 1, 7);
	`)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	summary, err := importer.Import(t.Context(), path, appleTestOptions())
	require.NoError(t, err)
	replies := appleStoredReplyLinks(t, st, summary.SourceID)
	assert.Equal(t, sql.NullString{String: "direct-in", Valid: true}, replies["direct-out"])
	assert.Equal(t, sql.NullString{String: "later-target", Valid: true}, replies["forward-reply"])
	assert.Equal(t, sql.NullString{String: "group-in", Valid: true}, replies["other-chat-reply"])
	for _, stanza := range []string{"missing-reply", "image-reply", "malformed-reply", "duplicate-reply"} {
		assert.Contains(t, replies, stanza)
		assert.False(t, replies[stanza].Valid, stanza)
	}
	assert.NotContains(t, replies, "group-image")

	rerun, err := importer.Import(t.Context(), path, appleTestOptions())
	require.NoError(t, err)
	assert.Zero(t, rerun.MessagesProcessed)
}

func TestImportAppleReplyIgnoresAndroidTarget(t *testing.T) {
	path := createAppleMarkerFixture(t)
	execAppleFixture(t, path, appleReplySchema+`
		INSERT INTO ZWAMEDIAITEM (Z_PK, ZMETADATA)
		VALUES (1, CAST(X'2A10' || 'message-incoming' AS BLOB));
		UPDATE ZWAMESSAGE SET ZMEDIAITEM = 1 WHERE Z_PK = 2;
	`)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	opts := appleTestOptions()
	android, err := importer.Import(t.Context(), createGroupParticipantsImportFixture(t, nil), opts)
	require.NoError(t, err)
	targets, err := st.StoredMessagesContext(t.Context(), android.SourceID, "whatsapp_json", []string{"message-incoming"})
	require.NoError(t, err)
	require.NotEmpty(t, targets["message-incoming"].Raw)

	apple, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)
	require.Equal(t, android.SourceID, apple.SourceID)
	replies := appleStoredReplyLinks(t, st, apple.SourceID)
	require.Contains(t, replies, "direct-out")
	assert.False(t, replies["direct-out"].Valid)
}

func TestImportAppleReplyToLaterChat(t *testing.T) {
	for _, tt := range []struct {
		name  string
		limit int
	}{
		{name: "unlimited"},
		{name: "stopped before the target", limit: 3},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := createAppleMarkerFixture(t)
			// The fixture reads the group chat before the direct chat.
			execAppleFixture(t, path, appleReplySchema+`
				INSERT INTO ZWAMEDIAITEM (Z_PK, ZMETADATA) VALUES (1, X'2A0A6469726563742D6F7574');
				UPDATE ZWAMESSAGE SET ZMEDIAITEM = 1 WHERE Z_PK = 3;
			`)
			st := testutil.NewTestStore(t)
			importer := NewImporter(st, nil)
			opts := appleTestOptions()
			opts.Limit = tt.limit
			first, err := importer.Import(t.Context(), path, opts)
			require.NoError(t, err)
			replies := appleStoredReplyLinks(t, st, first.SourceID)
			require.Contains(t, replies, "group-in")
			want := sql.NullString{String: "direct-out", Valid: true}
			if tt.limit == 0 {
				assert.Equal(t, want, replies["group-in"])
				return
			}
			assert.NotContains(t, replies, "direct-out")
			assert.False(t, replies["group-in"].Valid)

			opts.Limit = 0
			_, err = importer.Import(t.Context(), path, opts)
			require.NoError(t, err)
			assert.Equal(t, want, appleStoredReplyLinks(t, st, first.SourceID)["group-in"])

			third, err := importer.Import(t.Context(), path, opts)
			require.NoError(t, err)
			assert.Zero(t, third.MessagesProcessed)
		})
	}
}

func TestImportAppleReplyBackfill(t *testing.T) {
	path := createAppleMarkerFixture(t)
	execAppleFixture(t, path, appleReplySchema+`
		INSERT INTO ZWAMEDIAITEM (Z_PK, ZMETADATA) VALUES (1, X'2A096469726563742D696E');
		UPDATE ZWAMESSAGE SET ZMEDIAITEM = 1 WHERE Z_PK = 2;
	`)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	opts := appleTestOptions()
	first, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)

	// Recreate the archive state left by an import from before reply support.
	source, err := st.GetSourceByID(first.SourceID)
	require.NoError(t, err)
	var marker appleChatMarkerState
	require.NoError(t, json.Unmarshal([]byte(source.SyncCursor.String), &marker))
	marker.Version = 1
	oldCursor, err := json.Marshal(marker)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncState(t.Context(), source.ID, string(oldCursor)))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE messages SET reply_to_message_id = NULL WHERE source_id = ?
	`), source.ID)
	require.NoError(t, err)
	markAppleMessagesUnwritten(t, st)

	second, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)
	assert.Positive(t, second.MessagesProcessed)
	assert.Zero(t, second.MessagesAdded)
	assert.Zero(t, second.MessagesUpdated)
	assert.Equal(t, []string{"direct-out"}, rewrittenAppleMessages(t, st))
	want := sql.NullString{String: "direct-in", Valid: true}
	assert.Equal(t, want, appleStoredReplyLinks(t, st, source.ID)["direct-out"])

	third, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)
	assert.Zero(t, third.MessagesProcessed)
	assert.Equal(t, want, appleStoredReplyLinks(t, st, source.ID)["direct-out"])
}

func TestImportAppleReplyMetadataChange(t *testing.T) {
	path := createAppleMarkerFixture(t)
	execAppleFixture(t, path, appleReplySchema+`
		INSERT INTO ZWAMEDIAITEM (Z_PK, ZMETADATA) VALUES (1, X'2A096469726563742D696E');
		UPDATE ZWAMESSAGE SET ZMEDIAITEM = 1 WHERE Z_PK = 2;
	`)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, nil)
	opts := appleTestOptions()
	first, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)
	want := sql.NullString{String: "direct-in", Valid: true}
	require.Equal(t, want, appleStoredReplyLinks(t, st, first.SourceID)["direct-out"])

	// A media-only revision leaves the chat marker valid.
	execAppleFixture(t, path, `
		UPDATE ZWAMEDIAITEM SET ZMETADATA = X'2A0867726F75702D696E', Z_OPT = Z_OPT + 1 WHERE Z_PK = 1;
	`)
	second, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)
	assert.Zero(t, second.MessagesProcessed)

	// Re-reading the changed quote leaves the existing link intact.
	opts.Full = true
	third, err := importer.Import(t.Context(), path, opts)
	require.NoError(t, err)
	assert.Positive(t, third.MessagesProcessed)
	assert.Equal(t, want, appleStoredReplyLinks(t, st, first.SourceID)["direct-out"])
}

func appleStoredReplyLinks(t *testing.T, st *store.Store, sourceID int64) map[string]sql.NullString {
	t.Helper()
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT m.source_message_id, parent.source_message_id
		FROM messages m LEFT JOIN messages parent ON parent.id = m.reply_to_message_id
		WHERE m.source_id = ?
	`), sourceID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	replies := make(map[string]sql.NullString)
	for rows.Next() {
		var stanza string
		var parent sql.NullString
		require.NoError(t, rows.Scan(&stanza, &parent))
		replies[stanza] = parent
	}
	require.NoError(t, rows.Err())
	return replies
}
