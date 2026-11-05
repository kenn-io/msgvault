package store_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const replyBotID = "U00000000000000000000000000000001"

func allowLineReply(store.LineReplyAttempt) error { return nil }

func replyStoreFixture(t *testing.T) (*store.Store, store.ChatDraft, int64) {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("line", "2000000001")
	require.NoError(t, err)
	require.NoError(t, st.SetLineBotContext(t.Context(), source.ID, replyBotID))
	_, err = st.EnsureParticipantByIdentifierContext(t.Context(), "line_user_id", "channel:2000000001:user:"+replyBotID, "Example Bot")
	require.NoError(t, err)
	chat, err := st.EnsureConversationWithType(source.ID, "user:U00000000000000000000000000000002", "direct_chat", "Example")
	require.NoError(t, err)
	metadata := sql.NullString{String: `{"quoteToken":"synthetic-parent-quote"}`, Valid: true}
	parent, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:  &store.Message{SourceID: source.ID, ConversationID: chat, SourceMessageID: "100", MessageType: "line", SentAt: sql.NullTime{Time: time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), Valid: true}, Snippet: sql.NullString{String: "Incoming", Valid: true}},
		BodyText: sql.NullString{String: "Incoming", Valid: true}, Metadata: &metadata, FTS: &store.FTSDoc{Body: "Incoming"},
	})
	require.NoError(t, err)
	draft, err := st.CreateChatDraftContext(t.Context(), chat, parent, "Thanks!", func(string, string) error { return nil })
	require.NoError(t, err)
	return st, draft, parent
}

func TestLineReplyAdmissionFreezesRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, parent := replyStoreFixture(t)
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	calls := 0
	attempt, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, func(candidate store.LineReplyAttempt) error {
		calls++
		assert.Equal("Thanks!", candidate.Body)
		assert.Equal("synthetic-parent-quote", candidate.QuoteToken)
		return nil
	})
	require.NoError(err)
	require.NoError(uuid.Validate(attempt.RetryKey))
	assert.Equal(store.LineReplyAdmitted, attempt.State)
	assert.Equal(draft.SourceID, attempt.SourceID)
	assert.Equal(draft.ConversationID, attempt.ConversationID)
	assert.Equal(parent, attempt.ReplyToMessageID)
	assert.Equal("100", attempt.ReplyToSourceMessageID)
	assert.Equal(now.Add(23*time.Hour), attempt.RetryUntil)
	repeated, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now.Add(time.Hour), func(store.LineReplyAttempt) error { calls++; return errors.New("must not readmit") })
	require.NoError(err)
	assert.Equal(1, calls)
	assert.Equal(attempt.RetryKey, repeated.RetryKey)
	_, err = st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "changed")
	require.ErrorIs(err, store.ErrChatDraftFrozen)
	_, err = st.UpdateChatDraftContext(t.Context(), draft.DraftID, 2, "stale")
	require.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	_, err = st.AdmitLineReplyContext(t.Context(), draft.DraftID, 2, now, allowLineReply)
	require.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	unchanged, err := st.GetChatDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(draft, unchanged)
}

func TestLineReplyAdmissionGateFailureLeavesDraftEditable(t *testing.T) {
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	denied := errors.New("synthetic policy denial")
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, time.Now(), func(store.LineReplyAttempt) error { return denied })
	require.ErrorIs(err, denied)
	_, err = st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
	require.ErrorIs(err, sql.ErrNoRows)
	edited, err := st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "editable")
	require.NoError(err)
	require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, edited.Revision))
}

func TestLineReplyAcceptanceAndBackoffNeverRegress(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	attempt, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	assert.Equal(store.LineReplyRemoteUnknown, attempt.State)
	attempt, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{Accepted: true, RequestID: "original-request", StatusCode: 200, NextRetryAt: now.Add(time.Minute)})
	require.NoError(err)
	assert.Equal(store.LineReplyAcceptedUnresolved, attempt.State)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.ErrorIs(err, store.ErrLineReplyRetryWait)
	attempt, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now.Add(time.Minute), now.Add(90*time.Second))
	require.NoError(err)
	assert.Equal(store.LineReplyAcceptedUnresolved, attempt.State)
	_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{StatusCode: 503, NextRetryAt: now.Add(3 * time.Minute)})
	require.NoError(err)
	attempt, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{StatusCode: 400, DefiniteRejection: true, NextRetryAt: now.Add(time.Minute)})
	require.NoError(err)
	assert.Equal(store.LineReplyAcceptedUnresolved, attempt.State)
	assert.Equal("original-request", attempt.RequestID)
	assert.Equal(now.Add(3*time.Minute), attempt.NextRetryAt)
}

func TestLineReplyRejectionFlagDeterminesReceipt(t *testing.T) {
	for _, definite := range []bool{true, false} {
		t.Run(map[bool]string{true: "definite rejection", false: "no definite rejection flag"}[definite], func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, draft, _ := replyStoreFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
			require.NoError(err)
			_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
			require.NoError(err)
			receipt, err := st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{StatusCode: 400, DefiniteRejection: definite})
			require.NoError(err)
			want := store.LineReplyRemoteUnknown
			if definite {
				want = store.LineReplyProviderRejected
			}
			assert.Equal(want, receipt.State)
		})
	}
}

func acceptedReply(t *testing.T, st *store.Store, draft store.ChatDraft) store.LineReplyAttempt {
	t.Helper()
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(t, err)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(t, err)
	attempt, err := st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{Accepted: true, MessageID: "900000000000000001", QuoteToken: "synthetic-sent-quote", RequestID: "original-request", StatusCode: 200})
	require.NoError(t, err)
	return attempt
}

func TestLineReplyCompletionArchivesReceiptOnce(t *testing.T) {
	t.Run("receipt and conversation summary", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st, draft, parent := replyStoreFixture(t)
		require.NoError(st.RecomputeConversationStatsForConversationContext(t.Context(), draft.ConversationID))
		accepted := acceptedReply(t, st, draft)
		sent, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
		require.NoError(err)
		assert.Equal(store.LineReplySent, sent.State)
		require.Positive(sent.MessageID)
		assert.Equal(accepted.RemoteMessageID, sent.RemoteMessageID)
		_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
		require.ErrorIs(err, store.ErrChatDraftNotFound)
		var summaryCount int64
		var latest sql.NullTime
		var preview sql.NullString
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_count,last_message_at,last_message_preview FROM conversations WHERE id=?`), draft.ConversationID).Scan(&summaryCount, &latest, &preview))
		assert.Equal(int64(2), summaryCount)
		require.True(latest.Valid)
		assert.True(accepted.AdmittedAt.Equal(latest.Time))
		assert.Equal(sql.NullString{String: "Thanks!", Valid: true}, preview)
		repeated, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
		require.NoError(err)
		assert.Equal(sent, repeated)
		var conversation, reply, sender int64
		var fromMe bool
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT conversation_id,reply_to_message_id,sender_id,is_from_me FROM messages WHERE id=?`), sent.MessageID).Scan(&conversation, &reply, &sender, &fromMe))
		assert.Equal(draft.ConversationID, conversation)
		assert.Equal(parent, reply)
		assert.True(fromMe)
		assert.Positive(sender)
		body, raw, err := st.GetMessageBodyAndRawContext(t.Context(), sent.MessageID)
		require.NoError(err)
		assert.Equal("Thanks!", body)
		assert.JSONEq(`{"status_code":200,"request_id":"original-request","sentMessages":[{"id":"900000000000000001","quoteToken":"synthetic-sent-quote"}]}`, string(raw))
		count, err := st.CountMessagesForSourceContext(t.Context(), draft.SourceID)
		require.NoError(err)
		assert.Equal(int64(2), count)
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), draft.ConversationID).Scan(&summaryCount))
		assert.Equal(int64(2), summaryCount)
	})
	t.Run("earlier incoming quote", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		st, draft, _ := replyStoreFixture(t)
		quoteMetadata := sql.NullString{String: `{"quotedMessageId":"900000000000000001"}`, Valid: true}
		quotedMessageID, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
			Message:  &store.Message{SourceID: draft.SourceID, ConversationID: draft.ConversationID, SourceMessageID: "200", MessageType: "line"},
			BodyText: sql.NullString{String: "Reply received before local recovery", Valid: true}, Metadata: &quoteMetadata,
			FTS: &store.FTSDoc{Body: "Reply received before local recovery"},
		})
		require.NoError(err)
		var parent sql.NullInt64
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), quotedMessageID).Scan(&parent))
		assert.False(parent.Valid)

		acceptedReply(t, st, draft)
		sent, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
		require.NoError(err)
		require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), quotedMessageID).Scan(&parent))
		require.True(parent.Valid)
		assert.Equal(sent.MessageID, parent.Int64)
	})
}

func TestLineReplyCompletionFailureKeepsAcceptedDraft(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	acceptedReply(t, st, draft)
	var install, remove []string
	if st.IsPostgreSQL() {
		install = []string{`CREATE FUNCTION fail_line_reply() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN IF NEW.is_from_me THEN RAISE EXCEPTION 'synthetic archive failure'; END IF; RETURN NEW; END$$`, `CREATE TRIGGER fail_line_reply BEFORE INSERT ON messages FOR EACH ROW EXECUTE FUNCTION fail_line_reply()`}
		remove = []string{`DROP TRIGGER fail_line_reply ON messages`, `DROP FUNCTION fail_line_reply()`}
	} else {
		install = []string{`CREATE TRIGGER fail_line_reply BEFORE INSERT ON messages WHEN NEW.is_from_me=1 BEGIN SELECT RAISE(ABORT,'synthetic archive failure'); END`}
		remove = []string{`DROP TRIGGER fail_line_reply`}
	}
	for _, statement := range install {
		_, err := st.DB().ExecContext(t.Context(), statement)
		require.NoError(err)
	}
	_, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.Error(err)
	pending, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(store.LineReplyRemoteAccepted, pending.State)
	_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
	require.NoError(err)
	count, err := st.CountMessagesForSourceContext(t.Context(), draft.SourceID)
	require.NoError(err)
	assert.Equal(int64(1), count)
	for _, statement := range remove {
		_, err := st.DB().ExecContext(t.Context(), statement)
		require.NoError(err)
	}
	sent, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(store.LineReplySent, sent.State)
}

func TestLineReplyExpiryCannotBeReopenedByClockRollback(t *testing.T) {
	for _, phase := range []string{"delayed first push", "immediate first push", "accepted"} {
		t.Run(phase, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, draft, _ := replyStoreFixture(t)
			now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
			original, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
			require.NoError(err)
			firstPush := now
			if phase == "delayed first push" {
				firstPush = now.Add(2 * time.Hour)
			}
			nextRetry := firstPush.Add(time.Minute)
			if phase == "accepted" {
				nextRetry = firstPush.Add(30 * time.Second)
			}
			started, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, firstPush, nextRetry)
			require.NoError(err)
			assert.Equal(firstPush, started.AdmittedAt)
			if phase == "accepted" {
				_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{Accepted: true, StatusCode: 200, RequestID: "original-request"})
				require.NoError(err)
			}
			expired, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, firstPush.Add(23*time.Hour), firstPush.Add(23*time.Hour+30*time.Second))
			require.ErrorIs(err, store.ErrLineReplyExpired)
			if phase == "accepted" {
				assert.Equal(store.LineReplyAcceptedUnresolvedExpired, expired.State)
				assert.Equal("original-request", expired.RequestID)
			} else {
				assert.Equal(store.LineReplyRemoteUnknownExpired, expired.State)
			}
			repeated, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
			require.ErrorIs(err, store.ErrLineReplyExpired)
			assert.Equal(original.RetryKey, repeated.RetryKey)
			if phase == "accepted" {
				_, err = st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "changed")
				require.ErrorIs(err, store.ErrChatDraftFrozen)
				require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1))
				_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
				require.ErrorIs(err, store.ErrChatDraftNotFound)
				receipt, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
				require.NoError(err)
				assert.Equal(expired, receipt)
			}
		})
	}
}

func TestLineReplyBindingChangesFailClosed(t *testing.T) {
	for _, change := range []string{"conversation", "parent", "bot"} {
		t.Run(change, func(t *testing.T) {
			require := require.New(t)
			st, draft, parent := replyStoreFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
			require.NoError(err)
			switch change {
			case "conversation":
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE conversations SET source_conversation_id=? WHERE id=?`), "user:U00000000000000000000000000000009", draft.ConversationID)
			case "parent":
				other, createErr := st.EnsureConversationWithType(draft.SourceID, "group:C00000000000000000000000000000009", "group_chat", "")
				require.NoError(createErr)
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET conversation_id=? WHERE id=?`), other, parent)
			case "bot":
				err = st.SetLineBotContext(t.Context(), draft.SourceID, "U00000000000000000000000000000009")
			}
			require.NoError(err)
			_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(time.Minute))
			require.ErrorIs(err, store.ErrChatDraftInvalidDestination)
		})
	}
}

func TestLineReplyReceiptCannotOverwriteInboundMessage(t *testing.T) {
	require := require.New(t)
	st, draft, parent := replyStoreFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{Accepted: true, StatusCode: 200, MessageID: "100", RequestID: "original-request"})
	require.NoError(err)
	_, err = st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.ErrorIs(err, store.ErrLineReplyReceiptConflict)
	body, err := st.GetMessageBodyText(parent)
	require.NoError(err)
	assert.Equal(t, "Incoming", body)
}

func TestLineReplyUnresolvedAttemptPreventsSourceRetirement(t *testing.T) {
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, time.Now(), allowLineReply)
	require.NoError(err)
	target, err := st.GetOrCreateSource("line", "2000000002")
	require.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: draft.SourceID, IntoSourceID: target.ID})
	require.ErrorIs(err, store.ErrSourceMergeInvalid)
}

func TestLineReplyAdmissionHonorsSyncSourceScope(t *testing.T) {
	st, draft, _ := replyStoreFixture(t)
	other, err := st.GetOrCreateSource("line", "2000000002")
	require.NoError(t, err)
	run, err := st.StartSyncContext(t.Context(), other.ID, "full")
	require.NoError(t, err)
	scoped := st.ScopedToSync(other.ID, run)
	_, err = scoped.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, time.Now(), allowLineReply)
	require.ErrorContains(t, err, "scoped to source")
}

func TestLineReplyMissingParentAdmissionAndRecovery(t *testing.T) {
	for _, phase := range []string{"before admission", "lost response", "known receipt"} {
		t.Run(phase, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, draft, parent := replyStoreFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			if phase != "before admission" {
				_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
				require.NoError(err)
				_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(time.Minute))
				require.NoError(err)
			}
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM messages WHERE id=?`), parent)
			require.NoError(err)
			if phase == "before admission" {
				_, err = st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
				assert.ErrorIs(err, store.ErrChatDraftInvalidDestination)
				return
			}
			if phase == "lost response" {
				_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now.Add(time.Minute), now.Add(2*time.Minute))
				require.NoError(err)
			}
			_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{Accepted: true, StatusCode: 200, MessageID: "200"})
			require.NoError(err)
			sent, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
			require.NoError(err)
			assert.Equal(store.LineReplySent, sent.State)
			var reply sql.NullInt64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT reply_to_message_id FROM messages WHERE id=?`), sent.MessageID).Scan(&reply))
			assert.False(reply.Valid)
			durable, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
			require.NoError(err)
			assert.Equal(parent, durable.ReplyToMessageID, "the admitted snapshot stays frozen")
		})
	}
}

func TestLineReplyTerminalReceiptAllowsSourceMerge(t *testing.T) {
	for _, state := range []string{store.LineReplyAdmitted, store.LineReplyProviderRejected, store.LineReplyRemoteUnknownExpired, store.LineReplyAcceptedUnresolvedExpired, store.LineReplySent} {
		t.Run(state, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, draft, _ := replyStoreFixture(t)
			now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
			attempt, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
			require.NoError(err)
			if state != store.LineReplyAdmitted {
				_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(time.Minute))
				require.NoError(err)
				outcome := store.LineReplyOutcome{StatusCode: 400, DefiniteRejection: true}
				if state == store.LineReplyAcceptedUnresolvedExpired || state == store.LineReplySent {
					outcome = store.LineReplyOutcome{StatusCode: 200, Accepted: true}
					if state == store.LineReplySent {
						outcome.MessageID = "200"
					}
				}
				if state != store.LineReplyRemoteUnknownExpired {
					_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, outcome)
					require.NoError(err)
				}
				if state == store.LineReplySent {
					_, err = st.CompleteLineReplyContext(t.Context(), draft.DraftID)
					require.NoError(err)
				} else if state != store.LineReplyProviderRejected {
					_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now.Add(23*time.Hour), now.Add(24*time.Hour))
					require.ErrorIs(err, store.ErrLineReplyExpired)
				}
			}
			if state != store.LineReplySent {
				require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1))
			}
			target, err := st.GetOrCreateSource("line", "2000000002")
			require.NoError(err)
			if state == store.LineReplyAdmitted {
				_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: draft.SourceID, IntoSourceID: target.ID, DryRun: true})
				require.NoError(err)
				previewed, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
				require.NoError(err)
				assert.Equal(store.LineReplyAdmitted, previewed.State)
			}
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: draft.SourceID, IntoSourceID: target.ID})
			require.NoError(err)
			receipt, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
			require.NoError(err)
			want := state
			if state == store.LineReplyAdmitted {
				want = store.LineReplyRemoteUnknownExpired
			}
			assert.Equal(want, receipt.State)
			assert.Equal(attempt.RetryKey, receipt.RetryKey)
		})
	}
}

func TestLineReplyDraftDeletionPreservesEveryAttemptState(t *testing.T) {
	for _, state := range []string{store.LineReplyAdmitted, store.LineReplyRemoteUnknown, store.LineReplyAcceptedUnresolved, store.LineReplyRemoteAccepted, store.LineReplyProviderRejected, store.LineReplyRemoteUnknownExpired, store.LineReplyAcceptedUnresolvedExpired} {
		t.Run(state, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st, draft, _ := replyStoreFixture(t)
			now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
			attempt, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
			require.NoError(err)
			if state != store.LineReplyAdmitted {
				_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(time.Minute))
				require.NoError(err)
				outcome := store.LineReplyOutcome{StatusCode: 503}
				switch state {
				case store.LineReplyAcceptedUnresolved, store.LineReplyAcceptedUnresolvedExpired:
					outcome = store.LineReplyOutcome{Accepted: true, StatusCode: 200}
				case store.LineReplyRemoteAccepted:
					outcome = store.LineReplyOutcome{Accepted: true, StatusCode: 200, MessageID: "200"}
				case store.LineReplyProviderRejected:
					outcome = store.LineReplyOutcome{StatusCode: 400, DefiniteRejection: true}
				}
				attempt, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, outcome)
				require.NoError(err)
				if state == store.LineReplyRemoteUnknownExpired || state == store.LineReplyAcceptedUnresolvedExpired {
					attempt, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now.Add(23*time.Hour), now.Add(24*time.Hour))
					require.ErrorIs(err, store.ErrLineReplyExpired)
				}
			}
			assert.Equal(state, attempt.State)
			_, err = st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "changed")
			require.ErrorIs(err, store.ErrChatDraftFrozen)
			require.ErrorIs(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 2), store.ErrChatDraftRevisionConflict)
			require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1))
			_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
			require.ErrorIs(err, store.ErrChatDraftNotFound)
			receipt, err := st.GetLineReplyAttemptContext(t.Context(), draft.DraftID)
			require.NoError(err)
			assert.Equal(attempt, receipt)
		})
	}
}
