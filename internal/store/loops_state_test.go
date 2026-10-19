package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLoopReadyQueueWaitsForSynctechBackfillCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	source, err := f.Store.GetOrCreateSource("synctech_sms", "+15551234567")
	requirements.NoError(err)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	conversationIDs := make([]int64, 0, 2)
	for i := range 2 {
		conversationID, err := f.Store.EnsureConversation(source.ID, fmt.Sprintf("thread-%d", i), fmt.Sprintf("Thread %d", i))
		requirements.NoError(err)
		conversationIDs = append(conversationIDs, conversationID)
		messageID, err := f.Store.UpsertMessage(&store.Message{
			SourceID: source.ID, ConversationID: conversationID,
			SourceMessageID: fmt.Sprintf("sms-%d", i), MessageType: "sms",
			IsFromMe: true, IsSent: sql.NullBool{Bool: true, Valid: true},
		})
		requirements.NoError(err)
		loopExec(t, f.Store, `INSERT INTO message_raw(message_id,raw_data,raw_format)
			VALUES(?,?,'synctech_sms_xml_json')`, messageID, []byte(`{"Type":2}`))
		requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{conversationID}, "backfill", now))
	}

	ready, err := f.Store.PendingLoopConversationsReadyForDerivationContext(t.Context(), 10)
	requirements.NoError(err)
	assertions.Empty(ready, "unverified Synctech messages remain queued until their send state is checked")

	inspect := func(store.SynctechSMSStateBackfillCandidate) (bool, error) { return true, nil }
	progress, err := f.Store.ProcessSynctechSMSNativeStateBackfillBatchContext(t.Context(), 1, inspect)
	requirements.NoError(err)
	assertions.Equal(1, progress.Processed)
	assertions.False(progress.Complete)
	ready, err = f.Store.PendingLoopConversationsReadyForDerivationContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(ready, 1)
	assertions.Equal(conversationIDs[0], ready[0].ConversationID, "rows through the durable cursor can derive while later rows stay blocked")

	progress, err = f.Store.ProcessSynctechSMSNativeStateBackfillBatchContext(t.Context(), 1, inspect)
	requirements.NoError(err)
	assertions.Equal(1, progress.Processed)
	assertions.True(progress.Complete)
	ready, err = f.Store.PendingLoopConversationsReadyForDerivationContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(ready, 2)
}

func TestLoopStateFeedUsesPriorFrontierAndAtomicCursor(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	before := now.Add(-time.Hour)
	loopExec(t, f.Store, "UPDATE loop_derivation_state SET feed_frontier=?", loopTestStamp(f.Store, before))
	requirements.NoError(f.Store.CommitLoopFeedContext(t.Context(), []int64{f.ConvID}, store.ChangedMessagesAfter(now, 0), now, now))
	state, err := f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	requirements.NotNil(state.FeedCursorAt)
	assertions.True(now.Equal(*state.FeedCursorAt))
	assertions.Zero(state.FeedCursorMessageID)
	queue, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(queue, 1)
	requirements.NotNil(queue[0].FeedBound)
	assertions.True(before.Equal(*queue[0].FeedBound))
	// A failed state write must roll back the queue generation in the same transaction.
	if f.Store.IsPostgreSQL() {
		loopExec(t, f.Store, `CREATE FUNCTION fail_loop_state() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected state failure'; END $$`)
		loopExec(t, f.Store, `CREATE TRIGGER fail_loop_state BEFORE UPDATE ON loop_derivation_state FOR EACH ROW WHEN (OLD.feed_cursor_at IS DISTINCT FROM NEW.feed_cursor_at) EXECUTE FUNCTION fail_loop_state()`)
	} else {
		loopExec(t, f.Store, `CREATE TRIGGER fail_loop_state BEFORE UPDATE ON loop_derivation_state WHEN OLD.feed_cursor_at IS NOT NEW.feed_cursor_at BEGIN SELECT RAISE(ABORT,'injected state failure'); END`)
	}
	err = f.Store.CommitLoopFeedContext(t.Context(), []int64{f.ConvID}, store.ChangedMessagesAfter(now.Add(time.Hour), 1), now.Add(time.Hour), now)
	requirements.Error(err)
	after, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	assertions.Equal(queue, after)
	state, err = f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	assertions.True(now.Equal(*state.FeedFrontier))
}

func TestLoopStatePendingBoundsEpochAndCertification(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	older := now.Add(-time.Hour)
	epoch, err := f.Store.CurrentLoopEpochContext(t.Context(), 1, "config")
	requirements.NoError(err)
	loopExec(t, f.Store, "UPDATE loop_derivation_state SET feed_frontier=?,applied_through=?", loopTestStamp(f.Store, now), loopTestStamp(f.Store, older))
	for _, reason := range []string{"changed", "labels", "recipients", "moved", "ownership", "signals", "unmute", "rescan"} {
		t.Run(reason, func(t *testing.T) {
			requirements := require.New(t)
			loopExec(t, f.Store, "DELETE FROM loop_conversation_queue")
			loopExec(t, f.Store, "INSERT INTO loop_conversation_queue(conversation_id,feed_bound,reason) VALUES(?,?,?)", f.ConvID, loopTestStamp(f.Store, older), reason)
			requirements.NoError(f.Store.FinalizeLoopProgressContext(t.Context(), epoch, time.Time{}, now))
			state, err := f.Store.LoopDerivationStateContext(t.Context())
			requirements.NoError(err)
			requirements.NotNil(state.AppliedThrough)
			assert.True(t, older.Equal(*state.AppliedThrough))
		})
	}
	loopExec(t, f.Store, "UPDATE loop_conversation_queue SET feed_bound=NULL")
	requirements.NoError(f.Store.BeginLoopRescanContext(t.Context(), epoch, "identity", nil, now))
	n, err := f.Store.EnqueueLoopRescanContext(t.Context(), epoch, time.Time{}, 1, now)
	requirements.NoError(err)
	assertions.Equal(1, n)
	requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{f.ConvID}, "signals", now))
	rows, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	assertions.Nil(rows[0].FeedBound)
	assertions.Equal(epoch.String(), rows[0].Epoch)
	requirements.NoError(f.Store.FinalizeLoopProgressContext(t.Context(), epoch, time.Time{}, now))
	state, err := f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	assertions.Nil(state.AppliedThrough)
	assertions.Equal("identity", state.RescanReason)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: rows[0].Revision - 1, Epoch: epoch.String(), Now: now}))
	pending, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(pending, 1)
	assertions.Nil(pending[0].FeedBound)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: rows[0].Revision, Epoch: "stale-epoch", Now: now}))
	pending, err = f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(pending, 1)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: rows[0].Revision, Epoch: epoch.String(), Now: now}))
	requirements.NoError(f.Store.FinalizeLoopProgressContext(t.Context(), epoch, time.Time{}, now))
	state, err = f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	assertions.Empty(state.RescanReason)
	assertions.Equal(1, state.RulesetVersion)
	requirements.NotNil(state.AppliedThrough)
	assertions.True(now.Equal(*state.AppliedThrough))
	requirements.NoError(f.Store.EnqueueLoopConversationsContext(t.Context(), []int64{f.ConvID}, "changed", now))
	rows, err = f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.NotNil(rows[0].FeedBound)
	assertions.True(now.Equal(*rows[0].FeedBound))
	assertions.Empty(rows[0].Epoch)
}

func TestLoopStateRescanHoldsProgressAndRechecksActualEpoch(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	epoch, err := f.Store.CurrentLoopEpochContext(t.Context(), 1, "first")
	requirements.NoError(err)
	loopExec(t, f.Store, "UPDATE loop_derivation_state SET feed_frontier=?", loopTestStamp(f.Store, now))
	requirements.NoError(f.Store.BeginLoopRescanContext(t.Context(), epoch, "config", nil, now))
	requirements.NoError(f.Store.FinalizeLoopProgressContext(t.Context(), epoch, time.Time{}, now))
	state, err := f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	assertions.Nil(state.AppliedThrough)
	assertions.Equal("config", state.RescanReason)
	n, err := f.Store.EnqueueLoopRescanContext(t.Context(), epoch, time.Time{}, 1, now)
	requirements.NoError(err)
	assertions.Equal(1, n, "initial cursor must include conversation 1")
	rows, err := f.Store.PendingLoopConversationsContext(t.Context(), 10)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: rows[0].Revision, Epoch: rows[0].Epoch, Now: now}))
	requirements.NoError(f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	requirements.NoError(f.Store.FinalizeLoopProgressContext(t.Context(), epoch, time.Time{}, now))
	state, err = f.Store.LoopDerivationStateContext(t.Context())
	requirements.NoError(err)
	assertions.Equal("config", state.RescanReason)
	assertions.Zero(state.RulesetVersion)
	assertions.Nil(state.AppliedThrough)
}

func TestLoopRescanIncludesRecentSentMessageWhenConversationTimestampIsNull(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	message := f.CreateMessage("recent-null-conversation-time")
	loopExec(t, f.Store, "UPDATE messages SET sent_at=? WHERE id=?", loopTestStamp(f.Store, now), message)
	loopExec(t, f.Store, "UPDATE conversations SET last_message_at=NULL WHERE id=?", f.ConvID)
	epoch, err := f.Store.CurrentLoopEpochContext(t.Context(), 1, "test")
	requirements.NoError(err)
	requirements.NoError(f.Store.BeginLoopRescanContext(t.Context(), epoch, "config", nil, now))

	count, err := f.Store.EnqueueLoopRescanContext(t.Context(), epoch, now.Add(-time.Hour), 10, now)
	requirements.NoError(err)
	assertions.Equal(1, count)
}

func TestLoopStateRescanIncludesRecentMessageWithoutConversationTimestamp(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	messageAt := now.Add(-time.Hour)
	f.NewMessage().WithReceivedAt(messageAt).Create(t, f.Store)
	loopExec(t, f.Store, "UPDATE conversations SET last_message_at=NULL WHERE id=?", f.ConvID)
	epoch, err := f.Store.CurrentLoopEpochContext(t.Context(), 1, "config")
	requirements.NoError(err)
	requirements.NoError(f.Store.BeginLoopRescanContext(t.Context(), epoch, "config", nil, now))

	n, err := f.Store.EnqueueLoopRescanContext(t.Context(), epoch, now.Add(-24*time.Hour), 10, now)
	requirements.NoError(err)
	assertions.Equal(1, n)
}
