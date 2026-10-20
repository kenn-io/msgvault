package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestReplaceMessageRecipientsPostgresAcquiresEmbeddingClockBeforeMessage(t *testing.T) {
	requirements := require.New(t)
	s := testutil.NewTestStore(t)
	if !s.IsPostgreSQL() {
		t.Skip("PostgreSQL lock-order regression; set MSGVAULT_TEST_DB")
	}
	s.DB().SetMaxOpenConns(4)

	source, err := s.GetOrCreateSource("beeper", "recipient-lock-order")
	requirements.NoError(err)
	room, err := s.EnsureConversation(source.ID, "room", "Room")
	requirements.NoError(err)
	participantID, err := s.EnsureParticipantByIdentifier(
		"beeper", "synthetic:lock-order-recipient", "Synthetic Lock Order Recipient")
	requirements.NoError(err)
	messageID, err := s.UpsertMessage(&store.Message{
		SourceID: source.ID, ConversationID: room,
		SourceMessageID: "recipient-lock-order", MessageType: "beeper",
	})
	requirements.NoError(err)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	blocker, err := s.DB().BeginTx(ctx, nil)
	requirements.NoError(err)
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = blocker.Rollback()
		}
	})
	_, err = blocker.ExecContext(ctx, `SELECT pg_advisory_xact_lock(
		hashtextextended('msgvault.embedding_change_clock', 0))`)
	requirements.NoError(err)

	replacement := make(chan error, 1)
	go func() {
		replacement <- s.ReplaceMessageRecipientsContext(
			ctx, messageID, "to", []int64{participantID}, []string{"Synthetic Lock Order Recipient"})
	}()

	requirements.Eventually(func() bool {
		var waiting bool
		err := s.DB().QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname=current_database()
			  AND pid<>pg_backend_pid()
			  AND wait_event_type='Lock'
			  AND (query LIKE '%msgvault.embedding_change_clock%'
			       OR query LIKE '%WITH affected_participants%'))`).Scan(&waiting)
		return err == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "recipient replacement did not reach the embedding/message lock wait")

	_, err = blocker.ExecContext(ctx, `SAVEPOINT recipient_message_lock_probe`)
	requirements.NoError(err)
	var lockedID int64
	lockErr := blocker.QueryRowContext(ctx, s.Rebind(
		`SELECT id FROM messages WHERE id=? FOR UPDATE NOWAIT`), messageID).Scan(&lockedID)
	if lockErr != nil {
		_, err = blocker.ExecContext(ctx, `ROLLBACK TO SAVEPOINT recipient_message_lock_probe`)
		requirements.NoError(err)
	}
	commitErr := blocker.Commit()
	committed = true
	requirements.NoError(commitErr)

	select {
	case err := <-replacement:
		requirements.NoError(err, "recipient replacement completes after the embedding lock is released")
	case <-ctx.Done():
		requirements.NoError(ctx.Err(), "recipient replacement did not resume after the embedding lock was released")
	}
	requirements.NoError(lockErr, "recipient replacement must acquire the embedding-change lock before its message row")
	requirements.Equal(messageID, lockedID)
}
