package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func lineReplyPGFixture(t *testing.T) (*Store, ChatDraft, int64) {
	t.Helper()
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	source, err := st.GetOrCreateSource("line", "2000000001")
	require.NoError(t, err)
	require.NoError(t, st.SetLineBotContext(t.Context(), source.ID, "U00000000000000000000000000000001"))
	bot, err := st.EnsureParticipantByIdentifierContext(t.Context(), "line_user_id", "channel:2000000001:user:U00000000000000000000000000000001", "Example Bot")
	require.NoError(t, err)
	chat, err := st.EnsureConversationWithType(source.ID, "user:U00000000000000000000000000000002", "direct_chat", "Example")
	require.NoError(t, err)
	parent, err := st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "100", MessageType: "line"})
	require.NoError(t, err)
	draft, err := st.CreateChatDraftContext(t.Context(), chat, parent, "Thanks!", func(string, string) error { return nil })
	require.NoError(t, err)
	return st, draft, bot
}

// Two transactions compete for the same draft while admission is uncommitted.
// The third connection only observes the real server lock wait.
func TestLineReplyAdmissionBlocksStartedDraftMutation(t *testing.T) {
	for _, operation := range []string{"edit", "delete"} {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			st, draft, _ := lineReplyPGFixture(t)
			st.DB().SetMaxOpenConns(4)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			done := make(chan error, 1)
			require.NoError(st.withTxContext(ctx, func(tx *loggedTx) error {
				var blocker int
				if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blocker); err != nil {
					return err
				}
				_, err := st.admitLineReplyTx(ctx, tx, draft.SourceID, draft.DraftID, 1, time.Now(), func(LineReplyAttempt) error { return nil })
				if err != nil {
					return err
				}
				go func() {
					if operation == "edit" {
						_, err := st.UpdateChatDraftContext(ctx, draft.DraftID, 1, "late edit")
						done <- err
						return
					}
					done <- st.DeleteChatDraftContext(ctx, draft.DraftID, 1)
				}()
				waitForPostgresBlocker(t, st.DB(), blocker)
				return nil
			}))
			require.ErrorIs(waitForSourceOperation(t, done), ErrChatDraftFrozen)
			got, err := st.GetChatDraftContext(ctx, draft.DraftID)
			require.NoError(err)
			assert.Equal(t, draft, got)
		})
	}
}

func TestLineReplyAdoptionResolvesMergedBotWithTwoConnections(t *testing.T) {
	require := require.New(t)
	st, draft, oldBot := lineReplyPGFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := st.AdmitLineReplyContext(ctx, draft.DraftID, 1, now, func(LineReplyAttempt) error { return nil })
	require.NoError(err)
	_, err = st.StartLineReplyRequestContext(ctx, draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	_, err = st.RecordLineReplyOutcomeContext(ctx, draft.DraftID, LineReplyOutcome{Accepted: true, MessageID: "900000000000000001", RequestID: "original-request", StatusCode: 200})
	require.NoError(err)
	replacement, err := st.EnsureParticipantByIdentifierContext(ctx, "line_user_id", "channel:2000000001:user:U00000000000000000000000000000009", "Example merged bot")
	require.NoError(err)
	require.NoError(st.MergeParticipantsContext(ctx, oldBot, replacement))
	st.DB().SetMaxOpenConns(2)
	execution, err := st.AcquireSyncExecutionContext(ctx, draft.SourceID)
	require.NoError(err)
	defer func() { require.NoError(execution.Release()) }()
	sent, err := st.CompleteLineReplyContext(ctx, draft.DraftID)
	require.NoError(err)
	var sender int64
	require.NoError(st.DB().QueryRowContext(ctx, st.Rebind(`SELECT sender_id FROM messages WHERE id=?`), sent.MessageID).Scan(&sender))
	assert.Equal(t, replacement, sender)
}

func TestLineReplyCommitFailureDoesNotClaimSent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := lineReplyPGFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, func(LineReplyAttempt) error { return nil })
	require.NoError(err)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, LineReplyOutcome{Accepted: true, MessageID: "900000000000000001", RequestID: "original-request", StatusCode: 200})
	require.NoError(err)
	for _, statement := range []string{
		`CREATE FUNCTION fail_line_reply_commit() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.state='sent' THEN RAISE EXCEPTION 'synthetic commit failure'; END IF; RETURN NEW; END$$`,
		`CREATE CONSTRAINT TRIGGER fail_line_reply_commit AFTER UPDATE ON line_reply_attempts DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_line_reply_commit()`,
	} {
		_, err := st.DB().ExecContext(t.Context(), statement)
		require.NoError(err)
	}
	result, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.Error(err)
	assert.Equal(LineReplyRemoteAccepted, result.State)
	assert.Zero(result.MessageID)
	durable, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(LineReplyRemoteAccepted, durable.State)
	_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
}
