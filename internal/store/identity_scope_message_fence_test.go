package store

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityScopeMessageWritersSerialize(t *testing.T) {
	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("requires PostgreSQL to observe independent transaction row locks")
	}
	st := newPGStoreInternal(t, os.Getenv("MSGVAULT_TEST_DB"))
	require.True(t, st.IsPostgreSQL())
	a, err := st.EnsureParticipant("sender@example.test", "Synthetic Sender", "example.test")
	require.NoError(t, err)
	b, err := st.EnsureParticipant("recipient@example.test", "Synthetic Recipient", "example.test")
	require.NoError(t, err)
	for _, operation := range []string{"upsert sender", "persist recipients", "replace recipients", "scoped upsert sender", "scoped persist recipients", "scoped replace recipients"} {
		t.Run(operation, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			source, err := st.GetOrCreateSource("gmail", strings.ReplaceAll(operation, " ", "-")+"@example.test")
			requirements.NoError(err)
			conversationID, err := st.EnsureConversation(source.ID, "synthetic-scope-thread", "Synthetic Thread")
			requirements.NoError(err)
			message := &Message{SourceID: source.ID, ConversationID: conversationID, SourceMessageID: "synthetic-scope-message", MessageType: "email", SenderID: sql.NullInt64{Int64: a, Valid: true}}
			messageID, err := st.UpsertMessage(message)
			requirements.NoError(err)
			writer := st
			if strings.HasPrefix(operation, "scoped ") {
				runID, err := st.StartSync(source.ID, "full")
				requirements.NoError(err)
				writer = st.ScopedToSync(source.ID, runID)
			}
			writeOperation := strings.TrimPrefix(operation, "scoped ")
			before, err := st.IdentityRevision()
			requirements.NoError(err)

			tx, err := st.db.BeginTx(t.Context(), nil)
			requirements.NoError(err)
			defer func() { _ = tx.Rollback() }()
			requirements.NoError(st.lockIdentityMutationTxContext(t.Context(), tx))
			var blockerPID int
			requirements.NoError(tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID))
			done := make(chan error, 1)
			go func() {
				switch writeOperation {
				case "upsert sender":
					updatedMessage := *message
					updatedMessage.SenderID = sql.NullInt64{Int64: b, Valid: true}
					_, err := writer.UpsertMessage(&updatedMessage)
					done <- err
				case "persist recipients":
					_, err := writer.PersistMessageWithParticipantsContext(t.Context(), []ParticipantPersistData{{EmailAddress: "recipient@example.test", DisplayName: "Synthetic Recipient", Domain: "example.test"}}, func(ids []int64) *MessagePersistData {
						return &MessagePersistData{Message: message, Recipients: []RecipientSet{{Type: "to", ParticipantIDs: ids}}}
					})
					done <- err
				case "replace recipients":
					done <- writer.ReplaceMessageRecipients(messageID, "to", []int64{b}, nil)
				}
			}()

			// Wait for an observable database lock transition or a completed
			// writer. The budget bounds external PostgreSQL work, not throughput.
			completed := false
			var writeErr error
			const lockTransitionBudget = 15 * time.Second
			assertions.Eventually(func() bool {
				select {
				case writeErr = <-done:
					completed = true
					return true
				default:
				}
				var waiting bool
				err := st.db.QueryRowContext(t.Context(), `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity
					WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&waiting)
				return err == nil && waiting
			}, lockTransitionBudget, 10*time.Millisecond)
			assertions.False(completed, "native writer changed source support while authorization held its fence")
			if !completed {
				var writerPID int
				requirements.NoError(st.db.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity
					WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)) LIMIT 1`, blockerPID).Scan(&writerPID))
				var priorTableWrites int
				requirements.NoError(st.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pg_locks l
					JOIN pg_class c ON c.oid = l.relation
					WHERE l.pid = ? AND l.granted AND l.mode = 'RowExclusiveLock'
					AND c.relname IN ('messages', 'message_recipients', 'participants', 'sync_runs', 'gmail_drafts', 'imap_drafts')`, writerPID).Scan(&priorTableWrites))
				assertions.Zero(priorTableWrites, "identity fence must precede provenance and sync-generation table writes")
			}
			requirements.NoError(tx.Commit())
			if !completed {
				select {
				case writeErr = <-done:
				case <-time.After(lockTransitionBudget):
					requirements.FailNow("native writer did not finish after releasing the fence")
				}
			}
			requirements.NoError(writeErr)
			after, err := st.IdentityRevision()
			requirements.NoError(err)
			assertions.Equal(before, after, "source-support synchronization must not change semantic identity revision")
			if writeOperation == "upsert sender" {
				var senderID int64
				requirements.NoError(st.db.QueryRow(`SELECT sender_id FROM messages WHERE id = ?`, messageID).Scan(&senderID))
				assertions.Equal(b, senderID)
			} else {
				var recipientID int64
				requirements.NoError(st.db.QueryRow(`SELECT participant_id FROM message_recipients WHERE message_id = ? AND recipient_type = 'to'`, messageID).Scan(&recipientID))
				assertions.Equal(b, recipientID)
			}
		})
	}
}
