package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/synctechstate"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestMigrationMarksExistingSynctechDraftsAndUnsentMessages(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("synctech_sms", "+15550000001")
	require.NoError(err)
	conversationID, err := f.Store.EnsureConversationWithType(source.ID, "legacy-unsent", "direct_chat", "Alice")
	require.NoError(err)

	tests := []struct {
		messageType string
		raw         string
		wantSent    bool
		corrupt     bool
	}{
		{messageType: "sms", raw: `{"Type":2}`, wantSent: true, corrupt: true},
		{messageType: "sms", wantSent: true},
		{messageType: "mms", raw: `broken JSON`, wantSent: true},
		{messageType: "sms", raw: `{"Type":2}`, wantSent: true},
		{messageType: "sms", raw: `{"Type":3}`, wantSent: false},
		{messageType: "sms", raw: `{"Type":4}`, wantSent: false},
		{messageType: "sms", raw: `{"Type":5}`, wantSent: false},
		{messageType: "sms", raw: `{"Type":6}`, wantSent: false},
		{messageType: "mms", raw: `{"MessageBox":2}`, wantSent: true},
		{messageType: "mms", raw: `{"MessageBox":3}`, wantSent: false},
		{messageType: "mms", raw: `{"MessageBox":4}`, wantSent: false},
	}
	// Cross the migration's 500-record page boundary with both sent and unsent
	// rows, including originals that cannot be classified.
	ids := make([]int64, 510)
	for i := range ids {
		tt := tests[i%len(tests)]
		messageID, err := f.Store.UpsertMessage(&store.Message{
			ConversationID:  conversationID,
			SourceID:        source.ID,
			SourceMessageID: fmt.Sprintf("legacy-%d", i),
			MessageType:     tt.messageType,
			SentAt:          sql.NullTime{Time: time.Date(2024, 6, 1, 12, i, 0, 0, time.UTC), Valid: true},
			IsFromMe:        true,
		})
		require.NoError(err)
		ids[i] = messageID
		if tt.raw != "" {
			require.NoError(f.Store.UpsertMessageRawWithFormat(messageID, []byte(tt.raw), synctechstate.RawFormat))
		}
		if tt.corrupt {
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE message_raw SET raw_data = ?, compression = ? WHERE message_id = ?`), []byte("broken compressed block"), "zlib", messageID)
			require.NoError(err)
		}
		var isSent bool
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
			`SELECT is_sent FROM messages WHERE id = ?`), messageID).Scan(&isSent))
		assert.True(isSent, "legacy message starts with the old default: %s %s", tt.messageType, tt.raw)
	}

	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`DELETE FROM applied_migrations WHERE name = ?`), "synctech_unsent_messages_v1")
	require.NoError(err)
	require.NoError(f.Store.InitSchemaContext(t.Context()))

	for i, id := range ids {
		tt := tests[i%len(tests)]
		var got bool
		require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
			`SELECT is_sent FROM messages WHERE id = ?`), id).Scan(&got))
		assert.Equal(tt.wantSent, got, "message %d: %s %s", i, tt.messageType, tt.raw)
	}
}
