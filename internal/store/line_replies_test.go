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
	require.ErrorIs(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1), store.ErrChatDraftFrozen)
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
	require.ErrorIs(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1), store.ErrChatDraftFrozen)
}

func TestLineReplyRejectedReceiptSurvivesDraftDeletion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	rejected, err := st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{StatusCode: 400, DefiniteRejection: true})
	require.NoError(err)
	assert.Equal(store.LineReplyProviderRejected, rejected.State)
	edited, err := st.UpdateChatDraftContext(t.Context(), draft.DraftID, 1, "edited after rejection")
	require.NoError(err)
	_, err = st.AdmitLineReplyContext(t.Context(), draft.DraftID, edited.Revision, now, allowLineReply)
	require.ErrorIs(err, store.ErrChatDraftRevisionConflict)
	require.NoError(st.DeleteChatDraftContext(t.Context(), draft.DraftID, edited.Revision))
	repeated, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	assert.Equal(rejected, repeated)
}

func TestLineReplyRecoveredUnknownCannotBecomeRejected(t *testing.T) {
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	attempt, err := st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{StatusCode: 400})
	require.NoError(err)
	assert.Equal(t, store.LineReplyRemoteUnknown, attempt.State)
	require.ErrorIs(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1), store.ErrChatDraftFrozen)
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
	assert := assert.New(t)
	require := require.New(t)
	st, draft, parent := replyStoreFixture(t)
	accepted := acceptedReply(t, st, draft)
	sent, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.NoError(err)
	assert.Equal(store.LineReplySent, sent.State)
	require.Positive(sent.MessageID)
	assert.Equal(accepted.RemoteMessageID, sent.RemoteMessageID)
	_, err = st.GetChatDraftContext(t.Context(), draft.DraftID)
	require.ErrorIs(err, store.ErrChatDraftNotFound)
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
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	original, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	expired, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now.Add(23*time.Hour), now.Add(23*time.Hour+30*time.Second))
	require.ErrorIs(err, store.ErrLineReplyExpired)
	assert.Equal(store.LineReplyRemoteUnknownExpired, expired.State)
	repeated, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.ErrorIs(err, store.ErrLineReplyExpired)
	assert.Equal(original.RetryKey, repeated.RetryKey)
	require.ErrorIs(st.DeleteChatDraftContext(t.Context(), draft.DraftID, 1), store.ErrChatDraftFrozen)
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
			require.ErrorIs(st.ValidateLineReplyContext(t.Context(), draft.DraftID), store.ErrChatDraftInvalidDestination)
		})
	}
}

func TestLineReplyAcceptedExpiryPreservesAcceptance(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	_, err := st.AdmitLineReplyContext(t.Context(), draft.DraftID, 1, now, allowLineReply)
	require.NoError(err)
	_, err = st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now, now.Add(30*time.Second))
	require.NoError(err)
	_, err = st.RecordLineReplyOutcomeContext(t.Context(), draft.DraftID, store.LineReplyOutcome{Accepted: true, StatusCode: 200, RequestID: "original-request"})
	require.NoError(err)
	expired, err := st.StartLineReplyRequestContext(t.Context(), draft.DraftID, now.Add(23*time.Hour), now.Add(23*time.Hour+30*time.Second))
	require.ErrorIs(err, store.ErrLineReplyExpired)
	assert.Equal(store.LineReplyAcceptedUnresolvedExpired, expired.State)
	assert.Equal("original-request", expired.RequestID)
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
	// Receipts deliberately outlive drafts. Even a missing local draft cannot
	// make unresolved provider work safe to retire.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`DELETE FROM chat_drafts WHERE draft_id=?`), draft.DraftID)
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

func TestLineReplyCompletionUpdatesConversationSummary(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st, draft, _ := replyStoreFixture(t)
	require.NoError(st.RecomputeConversationStatsForConversationContext(t.Context(), draft.ConversationID))
	accepted := acceptedReply(t, st, draft)
	_, err := st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.NoError(err)
	var count int64
	var latest sql.NullTime
	var preview sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_count,last_message_at,last_message_preview FROM conversations WHERE id=?`), draft.ConversationID).Scan(&count, &latest, &preview))
	assert.Equal(int64(2), count)
	require.True(latest.Valid)
	assert.True(accepted.AdmittedAt.Equal(latest.Time))
	assert.Equal(sql.NullString{String: "Thanks!", Valid: true}, preview)
	_, err = st.CompleteLineReplyContext(t.Context(), draft.DraftID)
	require.NoError(err)
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT message_count FROM conversations WHERE id=?`), draft.ConversationID).Scan(&count))
	assert.Equal(int64(2), count)
}
