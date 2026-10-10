package store

import (
	"database/sql"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityScopePhysicalDeletionSerializes(t *testing.T) {
	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	require.True(t, st.IsPostgreSQL())
	for _, operation := range []string{"batch", "all", "source"} {
		t.Run(operation, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			source, err := st.GetOrCreateSource("gmail", operation+"-deletion@example.test")
			requirements.NoError(err)
			conversationID, err := st.EnsureConversation(source.ID, "synthetic-deletion-thread", "Synthetic Thread")
			requirements.NoError(err)
			senderID, err := st.EnsureParticipant(operation+"-sender@example.test", "Synthetic Sender", "example.test")
			requirements.NoError(err)
			var ids []int64
			for _, key := range []string{"survivor", "duplicate"} {
				id, err := st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversationID,
					SourceMessageID: key, MessageType: "email", SenderID: sql.NullInt64{Int64: senderID, Valid: true}})
				requirements.NoError(err)
				ids = append(ids, id)
			}
			batch := "synthetic-deletion-" + operation
			_, err = st.MergeDuplicates(ids[0], ids[1:], batch)
			requirements.NoError(err)
			var deleted, batches int64
			observeIdentitySupportWriter(t, st, func() error {
				var err error
				switch operation {
				case "batch":
					deleted, err = st.DeleteDedupedBatchesContext(t.Context(), []string{batch})
				case "all":
					deleted, batches, err = st.DeleteAllDedupedContext(t.Context())
				case "source":
					err = st.RemoveSource(source.ID)
				}
				return err
			})
			var remaining int
			requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, source.ID).Scan(&remaining))
			if operation == "source" {
				assertions.Zero(remaining)
			} else {
				assertions.Equal(int64(1), deleted)
				assertions.Equal(1, remaining)
			}
			if operation == "all" {
				assertions.Equal(int64(1), batches)
			}
		})
	}
}
