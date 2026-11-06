package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/attachmentpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestLineMediaCacheAppend(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "archive.db")
	analyticsDir := t.TempDir()
	st := testutil.NewSQLiteTestStoreAt(t, dbPath)
	src, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	chat, err := st.EnsureConversationWithType(src.ID, "group:synthetic", "group_chat", "Example")
	must.NoError(err)
	sentAt := sql.NullTime{Time: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC), Valid: true}
	for _, id := range []string{"1", "2"} {
		_, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: chat, SourceMessageID: id, MessageType: "line", SentAt: sentAt})
		must.NoError(err)
	}
	_, err = buildCache(dbPath, analyticsDir, true)
	must.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  &store.Message{SourceID: src.ID, ConversationID: chat, SourceMessageID: "3", MessageType: "line", SentAt: sentAt},
		BodyText: sql.NullString{String: "file", Valid: true},
	})
	must.NoError(err)
	must.NoError(st.UpsertLineAttachmentContext(t.Context(), id, store.AttachmentWrite{
		SourceAttachmentID: "line:3", Filename: "example.bin", ContentHash: "synthetic-hash", StoragePath: "synthetic-path", Size: 12,
		State: attachmentpolicy.StateStored,
	}))
	result, err := buildCacheAuto(dbPath, analyticsDir)
	must.NoError(err)
	checks.Equal(int64(1), result.StagedCount, "new media must append only the new message")
}
