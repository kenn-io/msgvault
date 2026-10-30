package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Membership writes its inbox clock before projecting conversation membership.
// Hold that first write while real source removal acquires its maintenance locks.
func TestLineMembershipWriteDoesNotDeadlockWithRemoval(t *testing.T) {
	must := require.New(t)

	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	st.DB().SetMaxOpenConns(4)
	source, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "group:C00000000000000000000000000000001", "group_chat", "")
	must.NoError(err)
	participant, err := st.EnsureParticipantByIdentifier("line_user_id", "channel:2000000001:user:U00000000000000000000000000000001", "")
	must.NoError(err)
	must.NoError(st.SetLineMembershipContext(t.Context(), conversation, participant, "channel:2000000001:user:U00000000000000000000000000000001", true, time.Now().UTC()))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	blocker, err := st.DB().BeginTx(ctx, nil)
	must.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `SET LOCAL deadlock_timeout = '100ms'`)
	must.NoError(err)
	_, err = blocker.ExecContext(ctx, `UPDATE line_webhook_events SET raw_json=raw_json WHERE source_id=$1`, source.ID)
	must.NoError(err)
	var blockerPID int
	must.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))

	done := make(chan error, 1)
	go func() {
		_, _, err := st.RemoveSourceSerialized(ctx, source.ID)
		done <- err
	}()
	waitForPostgresBlocker(t, st.DB(), blockerPID)
	_, err = blocker.ExecContext(ctx, `UPDATE conversation_participants SET role='member' WHERE conversation_id=$1 AND participant_id=$2`, conversation, participant)
	must.NoError(err)
	must.NoError(blocker.Commit())
	must.NoError(waitForSourceOperation(t, done))
}

// A first message has no conversation ID yet. Block its inbox-clock write and
// inspect the live writer: it must not have acquired conversation write locks.
func TestLineFirstGroupMessageLocksClockBeforeConversation(t *testing.T) {
	must := require.New(t)
	checks := assert.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	st.DB().SetMaxOpenConns(4)
	source, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	member := "channel:2000000001:user:U00000000000000000000000000000001"
	participant, err := st.EnsureParticipantByIdentifier("line_user_id", member, "Synthetic sender")
	must.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	blocker, err := st.DB().BeginTx(ctx, nil)
	must.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `LOCK TABLE line_webhook_events IN ACCESS EXCLUSIVE MODE`)
	must.NoError(err)
	var blockerPID int
	must.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	done := make(chan error, 1)
	go func() {
		_, err := st.PersistLineGroupMessageContext(ctx, &MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: "first-group-message", MessageType: "line", SenderID: sql.NullInt64{Int64: participant, Valid: true}, SentAt: sql.NullTime{Time: time.Now().UTC(), Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "group:C00000000000000000000000000000001", ConversationType: "group_chat", Participants: []ConversationParticipantRef{{ParticipantID: participant, Role: "member"}}, PreserveExistingParticipants: true},
			BodyText:     sql.NullString{String: "synthetic group text", Valid: true},
		}, member)
		done <- err
	}()
	waitForPostgresBlocker(t, st.DB(), blockerPID)
	var writerPID int
	must.NoError(st.DB().QueryRowContext(ctx, `SELECT pid FROM pg_stat_activity WHERE backend_type='client backend' AND datname=current_database() AND $1=ANY(pg_blocking_pids(pid))`, blockerPID).Scan(&writerPID))
	for _, table := range []string{"conversations", "conversation_participants"} {
		var writeLock bool
		must.NoError(st.DB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
 WHERE pid=$1 AND relation=to_regclass($2) AND mode='RowExclusiveLock' AND granted)`, writerPID, table).Scan(&writeLock))
		checks.False(writeLock, "the inbox clock precedes writes to %s for a first group message", table)
	}
	must.NoError(blocker.Commit())
	must.NoError(waitForSourceOperation(t, done))
	_, _, err = st.RemoveSourceSerialized(ctx, source.ID)
	must.NoError(err)
}

// Hold retirement's real source-row lock while receipt starts. This forces the
// lifecycle check to run after retirement commits, rather than before a wait.
func TestLineEnqueueSerializesWithRetirement(t *testing.T) {
	must := require.New(t)

	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	st.DB().SetMaxOpenConns(4)
	source, err := st.GetOrCreateSource("line", "2000000000")
	must.NoError(err)
	botID := "U00000000000000000000000000000001"
	must.NoError(st.SetLineBotContext(t.Context(), source.ID, botID))
	into, err := st.GetOrCreateSource("line", "2000000001")
	must.NoError(err)
	blocker, err := st.DB().BeginTx(t.Context(), nil)
	must.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	var blockerPID int
	must.NoError(blocker.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&blockerPID))
	_, err = blocker.ExecContext(t.Context(), `UPDATE sources SET updated_at=updated_at WHERE id=$1`, source.ID)
	must.NoError(err)
	done := make(chan error, 1)
	go func() {
		done <- st.EnqueueLineEventsContext(t.Context(), source.ID, "2000000000", botID, []LineWebhookEvent{{WebhookEventID: "during-retirement", EventType: "message", EventTimestamp: 123, RawJSON: `{"type":"message","timestamp":123,"webhookEventId":"during-retirement","source":{"type":"user","userId":"U00000000000000000000000000000002"},"message":{"id":"1","type":"text","text":"synthetic"}}`}})
	}()
	waitForPostgresBlocker(t, st.DB(), blockerPID)
	_, err = blocker.ExecContext(t.Context(), `INSERT INTO source_settings(source_id,history_only,merged_into_source_id,retired_at) VALUES ($1,TRUE,$2,NOW())`, source.ID, into.ID)
	must.NoError(err)
	must.NoError(blocker.Commit())
	must.ErrorIs(waitForSourceOperation(t, done), ErrSourceRetired)
	status, err := st.LineInboxStatusContext(t.Context(), source.ID)
	must.NoError(err)
	assert.Zero(t, status.Pending)
}
