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

func loopOpen(t *testing.T, f *storetest.Fixture, anchor, last int64, now time.Time) store.LoopOpenDecision {
	t.Helper()
	return store.LoopOpenDecision{TurnKey: fmt.Sprintf("turn:%d:reply_owed:%d", f.ConvID, anchor), Kind: "reply_owed", Status: "open", Channel: "email", ConversationID: f.ConvID, SourceID: f.Source.ID, AnchorMessageID: anchor, LastActivityMessageID: last, OwedSinceAt: now.Add(-48 * time.Hour), LastActivityAt: now, OpensAt: now.Add(-24 * time.Hour), EvidenceJSON: []byte(fmt.Sprintf(`[{"message_id":%d,"quote":"question","quote_start_rune":0,"quote_end_rune":8}]`, anchor)), RulesetVersion: 1, TurnMessageIDs: []int64{anchor, last}}
}

func loopMessageAt(t *testing.T, f *storetest.Fixture, sentAt time.Time, fromMe bool) int64 {
	t.Helper()
	return f.NewMessage().WithSentAt(sentAt).WithIsFromMe(fromMe).Create(t, f.Store)
}

func loopOpenChronology(t *testing.T, f *storetest.Fixture, anchor, last int64, anchorAt, lastActivityAt time.Time) store.LoopOpenDecision {
	t.Helper()
	open := loopOpen(t, f, anchor, last, lastActivityAt)
	open.OwedSinceAt = anchorAt
	open.LastActivityAt = lastActivityAt
	open.OpensAt = anchorAt.Add(24 * time.Hour)
	return open
}

func applyLoop(t *testing.T, f *storetest.Fixture, open store.LoopOpenDecision, now time.Time) {
	t.Helper()
	require.NoError(t, f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Open: &open}}))
}
func TestLoopApplyDoneWinsRewrittenAnchorAndWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	timeline := []time.Time{
		now.Add(-72 * time.Hour),
		now.Add(-71 * time.Hour),
		now.Add(-70 * time.Hour),
		now.Add(-69 * time.Hour),
		now.Add(-68 * time.Hour),
		now.Add(-67 * time.Hour),
	}
	ids := []int64{
		loopMessageAt(t, f, timeline[0], true),  // earlier message in the same owner turn
		loopMessageAt(t, f, timeline[1], true),  // anchor first seen by detection
		loopMessageAt(t, f, timeline[2], true),  // latest activity in that turn
		loopMessageAt(t, f, timeline[3], false), // opposite turn before a later obligation
		loopMessageAt(t, f, timeline[4], true),  // anchor of the later owner turn
		loopMessageAt(t, f, timeline[5], true),  // latest activity in the later turn
	}
	open := loopOpenChronology(t, f, ids[1], ids[2], timeline[1], timeline[2])
	applyLoop(t, f, open, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	done, err := f.Store.CloseLoopContext(t.Context(), rows[0].ID, "handled", now)
	requirements.NoError(err)
	assertions.Equal("done", *done.ClosedReason)
	assertions.JSONEq(fmt.Sprintf(`[{"message_id":%d,"quote":"question","quote_start_rune":0,"quote_end_rune":8,"owner_note":"handled"}]`, ids[1]), string(done.EvidenceJSON))
	_, err = f.Store.CloseLoopContext(t.Context(), done.ID, "", now)
	requirements.ErrorIs(err, store.ErrLoopAlreadyClosed)
	rewritten := loopOpenChronology(t, f, ids[0], ids[2], timeline[0], timeline[2])
	rewritten.TurnMessageIDs = ids[:3]
	applyLoop(t, f, rewritten, now)
	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(rows)
	outside := loopOpenChronology(t, f, ids[4], ids[5], timeline[4], timeline[5])
	applyLoop(t, f, outside, now)
	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(rows, "done outside window retains authority")
	outside.PreviousTurnKnown = true
	outside.PreviousTurnAt = timeline[3]
	applyLoop(t, f, outside, now)
	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Len(rows, 1, "observed opposite boundary proves a later turn")
}
func TestLoopApplyRewriteCyclePreservesOwnerStateAndCAS(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(3)
	old := loopOpen(t, f, ids[0], ids[0], now)
	applyLoop(t, f, old, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Close: []store.LoopCloseDecision{{ID: rows[0].ID, Reason: "withdrawn", At: now}}}}))
	newer := loopOpen(t, f, ids[1], ids[2], now)
	newer.PreviousTurnKnown = true
	applyLoop(t, f, newer, now)
	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	live := rows[0]
	_, err = f.Store.SnoozeLoopContext(t.Context(), live.ID, now.Add(time.Hour), now)
	requirements.NoError(err)
	loopExec(t, f.Store, "UPDATE open_loops SET agenda_state='dismissed',fulfilment_suggested_at=? WHERE id=?", now, live.ID)
	loopExec(t, f.Store, "INSERT INTO loop_conversation_queue(conversation_id,revision,feed_bound,epoch) VALUES(?,3,?,'scan') ON CONFLICT(conversation_id) DO UPDATE SET revision=3,processed_revision=0,feed_bound=excluded.feed_bound,epoch=excluded.epoch", f.ConvID, now)
	old.TurnMessageIDs = ids
	old.PreviousTurnKnown = true
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: 2, Epoch: "scan", Now: now, Verdict: store.LoopVerdict{Open: &old}}))
	got, err := f.Store.GetOpenLoopContext(t.Context(), live.ID)
	requirements.NoError(err)
	assertions.Equal(int64(2), got.Cycle)
	assertions.Equal(old.TurnKey, got.TurnKey)
	requirements.NotNil(got.SnoozedUntil)
	assertions.Equal("dismissed", got.AgendaState)
	assertions.NotNil(got.FulfilmentSuggestedAt)
	var revision, processed int64
	var epoch string
	var bound sql.NullTime
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT revision,processed_revision,feed_bound,epoch FROM loop_conversation_queue WHERE conversation_id=?"), f.ConvID).Scan(&revision, &processed, &bound, &epoch))
	assertions.Equal(int64(3), revision)
	assertions.Equal(int64(2), processed)
	assertions.True(bound.Valid)
	assertions.Equal("scan", epoch)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: 3, Epoch: "scan", Now: now}))
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT revision,processed_revision,feed_bound,epoch FROM loop_conversation_queue WHERE conversation_id=?"), f.ConvID).Scan(&revision, &processed, &bound, &epoch))
	assertions.Equal(int64(3), processed)
	assertions.False(bound.Valid)
	assertions.Empty(epoch)
}

func TestLoopSuppressionsDryRunCloseAndUnmuteMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	id := f.CreateMessage("muted")
	loopExec(t, f.Store, "UPDATE messages SET sender_id=?,sent_at=? WHERE id=?", peer, now, id)
	open := loopOpen(t, f, id, id, now)
	open.CounterpartyParticipantID = &peer
	applyLoop(t, f, open, now)
	noHistory, err := f.Store.EnsureConversation(f.Source.ID, "no-history", "No history")
	requirements.NoError(err)
	loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", noHistory, peer)
	loopExec(t, f.Store, "UPDATE conversations SET last_message_at=? WHERE id=?", now, noHistory)
	request := store.LoopSuppressionRequest{TargetKind: "participant", ParticipantID: &peer, DryRun: true}
	result, err := f.Store.CreateLoopSuppressionContext(t.Context(), request, now)
	requirements.NoError(err)
	assertions.Equal(int64(1), result.WouldClose)
	assertions.Nil(result.Suppression)
	assertions.Zero(loopCount(t, f.Store, "loop_suppressions"))
	request.DryRun = false
	result, err = f.Store.CreateLoopSuppressionContext(t.Context(), request, now)
	requirements.NoError(err)
	requirements.NotNil(result.Suppression)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	assertions.Empty(rows)
	loopExec(t, f.Store, "DELETE FROM loop_conversation_queue")
	requirements.NoError(f.Store.DeleteLoopSuppressionContext(t.Context(), result.Suppression.ID, 365*24*time.Hour, now))
	var queued int
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT COUNT(*) FROM loop_conversation_queue WHERE conversation_id IN (?,?) AND revision>processed_revision"), f.ConvID, noHistory).Scan(&queued))
	assertions.Equal(2, queued)
}
func TestLoopPersonMuteUsesCurrentParticipantMembership(t *testing.T) {
	for _, linked := range []bool{true, false} {
		t.Run(fmt.Sprintf("linked_%t", linked), func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
			target := f.EnsureParticipant("target@example.test", "Target", "example.test")
			counterparty := target
			if !linked {
				counterparty = f.EnsureParticipant("other@example.test", "Other", "example.test")
			}
			message := f.CreateMessage("before-person-link")
			loopExec(t, f.Store, "UPDATE messages SET sender_id=? WHERE id=?", counterparty, message)
			open := loopOpen(t, f, message, message, now)
			open.CounterpartyParticipantID = &counterparty
			applyLoop(t, f, open, now)
			person, _, err := f.Store.CreatePersonFromParticipant(target)
			requirements.NoError(err)
			wantClosed, wantLive := int64(0), 1
			if linked {
				wantClosed, wantLive = 1, 0
			}
			request := store.LoopSuppressionRequest{TargetKind: "person", PersonID: &person.ID, DryRun: true}
			preview, err := f.Store.CreateLoopSuppressionContext(t.Context(), request, now)
			requirements.NoError(err)
			assertions.Equal(wantClosed, preview.WouldClose)
			assertions.Nil(preview.Suppression)
			request.DryRun = false
			result, err := f.Store.CreateLoopSuppressionContext(t.Context(), request, now)
			requirements.NoError(err)
			requirements.NotNil(result.Suppression)
			assertions.Equal(wantClosed, result.WouldClose)
			live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
			requirements.NoError(err)
			assertions.Len(live, wantLive)
			// A decision loaded before the person link must respect the mute too.
			applyLoop(t, f, open, now)
			live, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
			requirements.NoError(err)
			assertions.Len(live, wantLive)
		})
	}
}

func TestLoopApplyRollbackAndSnoozeValidation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(2)
	open := loopOpen(t, f, ids[0], ids[0], now)
	applyLoop(t, f, open, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	_, err = f.Store.SnoozeLoopContext(t.Context(), rows[0].ID, now, now)
	requirements.ErrorIs(err, store.ErrInvalidLoop)
	_, err = f.Store.SnoozeLoopContext(t.Context(), rows[0].ID, now.Add(time.Hour), now)
	requirements.NoError(err)
	got, err := f.Store.UnsnoozeLoopContext(t.Context(), rows[0].ID, now)
	requirements.NoError(err)
	assertions.Nil(got.SnoozedUntil)
	invalid := loopOpen(t, f, ids[1], 999999, now)
	invalid.PreviousTurnKnown = true
	err = f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Close: []store.LoopCloseDecision{{ID: rows[0].ID, Reason: "turn_flip", At: now}}, Open: &invalid}})
	requirements.Error(err)
	got, err = f.Store.GetOpenLoopContext(t.Context(), rows[0].ID)
	requirements.NoError(err)
	assertions.Equal("open", got.Status)
}

func TestLoopApplyRegenerationReasons(t *testing.T) {
	for _, reason := range []string{"done", "fulfilled", "expired", "muted", "withdrawn", "turn_flip"} {
		t.Run(reason, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			f := storetest.New(t)
			now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
			id := f.CreateMessage("regen")
			open := loopOpen(t, f, id, id, now)
			applyLoop(t, f, open, now)
			rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
			requirements.NoError(err)
			requirements.Len(rows, 1)
			loopExec(t, f.Store, "UPDATE open_loops SET status='closed',closed_at=?,closed_reason=?,closed_by='system' WHERE id=?", loopTestStamp(f.Store, now), reason, rows[0].ID)
			applyLoop(t, f, open, now)
			rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
			requirements.NoError(err)
			if reason == "done" || reason == "fulfilled" || reason == "expired" {
				assertions.Empty(rows)
			} else {
				requirements.Len(rows, 1)
				assertions.Equal(int64(2), rows[0].Cycle)
			}
		})
	}
}
func TestLoopApplyMismatchedEpochRemainsPending(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	loopExec(t, f.Store, "INSERT INTO loop_conversation_queue(conversation_id,revision,processed_revision,feed_bound,epoch) VALUES(?,4,1,?,'new-epoch')", f.ConvID, loopTestStamp(f.Store, now))
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, ObservedRevision: 4, Epoch: "old-epoch", Now: now}))
	var processed int64
	var bound sql.NullTime
	var epoch string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT processed_revision,feed_bound,epoch FROM loop_conversation_queue WHERE conversation_id=?"), f.ConvID).Scan(&processed, &bound, &epoch))
	assertions.Equal(int64(1), processed)
	assertions.True(bound.Valid)
	assertions.Equal("new-epoch", epoch)
}
func TestLoopUnmuteDiscoversPersonSenderRecipientAndMember(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	peer := f.EnsureParticipant("peer@example.test", "Peer", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(peer)
	requirements.NoError(err)
	var conversations []int64
	for _, mode := range []string{"sender", "recipient", "member"} {
		conversation, err := f.Store.EnsureConversation(f.Source.ID, mode, mode)
		requirements.NoError(err)
		conversations = append(conversations, conversation)
		loopExec(t, f.Store, "UPDATE conversations SET last_message_at=? WHERE id=?", loopTestStamp(f.Store, now), conversation)
		if mode == "member" {
			loopExec(t, f.Store, "INSERT INTO conversation_participants(conversation_id,participant_id) VALUES(?,?)", conversation, peer)
			continue
		}
		id, err := f.Store.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: f.Source.ID, SourceMessageID: mode, MessageType: "email", SentAt: sql.NullTime{Time: now, Valid: true}})
		requirements.NoError(err)
		if mode == "sender" {
			loopExec(t, f.Store, "UPDATE messages SET sender_id=? WHERE id=?", peer, id)
		} else {
			loopExec(t, f.Store, "INSERT INTO message_recipients(message_id,participant_id,recipient_type) VALUES(?,?,'to')", id, peer)
		}
	}
	result, err := f.Store.CreateLoopSuppressionContext(t.Context(), store.LoopSuppressionRequest{TargetKind: "person", PersonID: &person.ID}, now)
	requirements.NoError(err)
	requirements.NotNil(result.Suppression)
	loopExec(t, f.Store, "DELETE FROM loop_conversation_queue")
	requirements.NoError(f.Store.DeleteLoopSuppressionContext(t.Context(), result.Suppression.ID, 24*time.Hour, now))
	for _, conversation := range conversations {
		var reason string
		requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind("SELECT reason FROM loop_conversation_queue WHERE conversation_id=?"), conversation).Scan(&reason))
		assert.Equal(t, "unmute", reason)
	}
}

func TestLoopCoverageGapSurvivesOpenReconciliation(t *testing.T) {
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	ids := f.CreateMessages(2)
	gap := loopOpen(t, f, ids[0], ids[0], now)
	gap.Kind = "awaiting_reply"
	gap.TurnKey = fmt.Sprintf("turn:%d:awaiting_reply:%d", f.ConvID, ids[0])
	applyLoop(t, f, gap, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	open := loopOpen(t, f, ids[1], ids[1], now)
	requirements.NoError(f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{ConversationID: f.ConvID, Now: now, Verdict: store.LoopVerdict{Open: &open, CoverageGaps: []int64{rows[0].ID}}}))
	got, err := f.Store.GetOpenLoopContext(t.Context(), rows[0].ID)
	requirements.NoError(err)
	assert.JSONEq(t, fmt.Sprintf(`[{"message_id":%d,"quote":"question","quote_start_rune":0,"quote_end_rune":8,"coverage_gap":true}]`, ids[0]), string(got.EvidenceJSON))
	live, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(live, 2)
}
func TestLoopTerminalHistoryCollisionWithdrawsLive(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	timeline := []time.Time{
		now.Add(-72 * time.Hour),
		now.Add(-71 * time.Hour),
		now.Add(-70 * time.Hour),
		now.Add(-69 * time.Hour),
		now.Add(-68 * time.Hour),
		now.Add(-67 * time.Hour),
	}
	ids := []int64{
		loopMessageAt(t, f, timeline[0], false), // prior turn before the old obligation
		loopMessageAt(t, f, timeline[1], true),  // old owner anchor
		loopMessageAt(t, f, timeline[2], true),  // old owner turn activity
		loopMessageAt(t, f, timeline[3], false), // new opposite turn after terminal history
		loopMessageAt(t, f, timeline[4], true),  // newer owner anchor
		loopMessageAt(t, f, timeline[5], true),  // newer owner turn activity
	}
	old := loopOpenChronology(t, f, ids[1], ids[2], timeline[1], timeline[2])
	old.PreviousTurnKnown = true
	old.PreviousTurnAt = timeline[0]
	applyLoop(t, f, old, now)
	rows, err := f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	_, err = f.Store.CloseLoopContext(t.Context(), rows[0].ID, "", now)
	requirements.NoError(err)
	newer := loopOpenChronology(t, f, ids[4], ids[5], timeline[4], timeline[5])
	newer.PreviousTurnKnown = true
	newer.PreviousTurnAt = timeline[3]
	applyLoop(t, f, newer, now)
	rows, err = f.Store.ListLiveConversationLoopsContext(t.Context(), f.ConvID)
	requirements.NoError(err)
	requirements.Len(rows, 1)
	live := rows[0]
	// The delayed old verdict stays within its original same-side turn.
	applyLoop(t, f, old, now)
	got, err := f.Store.GetOpenLoopContext(t.Context(), live.ID)
	requirements.NoError(err)
	assertions.Equal("closed", got.Status)
	requirements.NotNil(got.ClosedReason)
	assertions.Equal("withdrawn", *got.ClosedReason)
}

func TestLoopApplyMissingCloseReturnsNotFound(t *testing.T) {
	f := storetest.New(t)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	err := f.Store.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: f.ConvID, Now: now,
		Verdict: store.LoopVerdict{Close: []store.LoopCloseDecision{{ID: 999999, Reason: "withdrawn", At: now}}},
	})
	assert.ErrorIs(t, err, store.ErrLoopNotFound)
}
