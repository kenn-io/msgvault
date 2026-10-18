package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func sourceMergeLoopOpen(conversationID, sourceID, messageID int64, kind string, now time.Time) store.LoopOpenDecision {
	return store.LoopOpenDecision{
		TurnKey:               fmt.Sprintf("turn:%d:%s:%d", conversationID, kind, messageID),
		Kind:                  kind,
		Status:                "open",
		Channel:               "chat",
		ConversationID:        conversationID,
		SourceID:              sourceID,
		AnchorMessageID:       messageID,
		LastActivityMessageID: messageID,
		OwedSinceAt:           now.Add(-48 * time.Hour),
		LastActivityAt:        now.Add(-time.Hour),
		OpensAt:               now.Add(-time.Hour),
		EvidenceJSON:          []byte("[]"),
		RulesetVersion:        1,
		TurnMessageIDs:        []int64{messageID},
	}
}

func TestMergeSourcesRemapsLoopReferencesForHiddenMessages(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-loop-references-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-loop-references-example")
	require.NoError(err)

	survivor, destinationConversation := mergeFixtureMessage(t, st, into.ID, "shared-loop-reference", "live-message", "same payload", 1)
	duplicate, sourceConversation := mergeFixtureMessage(t, st, from.ID, "shared-loop-reference", "history-message", "same payload", 1)
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	open := sourceMergeLoopOpen(sourceConversation, from.ID, duplicate, "reply_owed", now)
	open.OwnerAddress = "owner@example.test"
	open.EvidenceJSON = []byte(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"quote":"history question","owner_note":"keep this note"}]`, duplicate, sourceConversation))
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: sourceConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &open},
	}))
	openRows, err := st.ListLiveConversationLoopsContext(t.Context(), sourceConversation)
	require.NoError(err)
	require.Len(openRows, 1)

	closed := sourceMergeLoopOpen(sourceConversation, from.ID, duplicate, "awaiting_reply", now)
	closed.EvidenceJSON = []byte(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"quote":"closed history"}]`, duplicate, sourceConversation))
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: sourceConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &closed},
	}))
	closedRows, err := st.ListLiveConversationLoopsContext(t.Context(), sourceConversation)
	require.NoError(err)
	require.Len(closedRows, 2)
	var closedLoopID int64
	for _, row := range closedRows {
		if row.Kind == "awaiting_reply" {
			closedLoopID = row.ID
		}
	}
	require.NotZero(closedLoopID)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: sourceConversation,
		Now:            now,
		Verdict: store.LoopVerdict{Close: []store.LoopCloseDecision{{
			ID: closedLoopID, Reason: "turn_flip", At: now, MessageID: &duplicate,
		}}},
	}))

	result, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)
	assert.Equal(int64(1), result.DuplicatesHidden)

	gotOpen, err := st.GetOpenLoopContext(t.Context(), openRows[0].ID)
	require.NoError(err)
	require.NotNil(gotOpen.AnchorMessageID)
	assert.Equal(survivor, *gotOpen.AnchorMessageID)
	require.NotNil(gotOpen.LastActivityMessageID)
	assert.Equal(survivor, *gotOpen.LastActivityMessageID)
	assert.Equal(destinationConversation, gotOpen.ConversationID)
	assert.Equal(fmt.Sprintf("turn:%d:reply_owed:%d", destinationConversation, survivor), gotOpen.TurnKey)
	assert.Equal("owner@example.test", gotOpen.OwnerAddress)
	assert.JSONEq(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"quote":"history question","owner_note":"keep this note"}]`, survivor, destinationConversation), string(gotOpen.EvidenceJSON))

	gotClosed, err := st.GetOpenLoopContext(t.Context(), closedLoopID)
	require.NoError(err)
	require.NotNil(gotClosed.AnchorMessageID)
	assert.Equal(survivor, *gotClosed.AnchorMessageID)
	require.NotNil(gotClosed.LastActivityMessageID)
	assert.Equal(survivor, *gotClosed.LastActivityMessageID)
	require.NotNil(gotClosed.ClosedMessageID)
	assert.Equal(survivor, *gotClosed.ClosedMessageID)
	assert.Equal(destinationConversation, gotClosed.ConversationID)
	assert.Equal(fmt.Sprintf("turn:%d:awaiting_reply:%d", destinationConversation, survivor), gotClosed.TurnKey)
	assert.JSONEq(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"quote":"closed history"}]`, survivor, destinationConversation), string(gotClosed.EvidenceJSON))
}

func TestMergeSourcesTransfersRetiredLoopOwnerStateToWinner(t *testing.T) {
	for _, test := range []struct {
		name          string
		retiredSource bool
	}{
		{name: "retired source loop", retiredSource: true},
		{name: "retired destination loop"},
	} {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := testutil.NewTestStore(t)
			from, err := st.GetOrCreateSource("beeper", "history-loop-owner-state-example")
			require.NoError(err)
			into, err := st.GetOrCreateSource("beeper", "live-loop-owner-state-example")
			require.NoError(err)

			historyMessage, historyConversation := mergeFixtureMessage(t, st, from.ID, "shared-owner-state", "history-message", "history payload", 1)
			liveMessage, liveConversation := mergeFixtureMessage(t, st, into.ID, "shared-owner-state", "live-message", "live payload", 2)
			now := time.Date(2035, 2, 1, 12, 0, 0, 0, time.UTC)
			retiredConversation := liveConversation
			winnerConversation := historyConversation
			retiredSourceID, winnerSourceID := into.ID, from.ID
			retiredMessage, winnerMessage := liveMessage, historyMessage
			retiredOwner, winnerOwner := "live-owner@example.test", "archive-owner@example.test"
			if test.retiredSource {
				retiredConversation, winnerConversation = historyConversation, liveConversation
				retiredSourceID, winnerSourceID = from.ID, into.ID
				retiredMessage, winnerMessage = historyMessage, liveMessage
				retiredOwner, winnerOwner = "archive-owner@example.test", "live-owner@example.test"
			}

			retiredOpen := sourceMergeLoopOpen(retiredConversation, retiredSourceID, retiredMessage, "awaiting_reply", now)
			retiredOpen.OwnerAddress = retiredOwner
			retiredOpen.EvidenceJSON = []byte(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"owner_note":"retired note"}]`, retiredMessage, retiredConversation))
			require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
				ConversationID: retiredConversation,
				Now:            now,
				Verdict:        store.LoopVerdict{Open: &retiredOpen},
			}))
			retiredRows, err := st.ListLiveConversationLoopsContext(t.Context(), retiredConversation)
			require.NoError(err)
			require.Len(retiredRows, 1)
			retiredLoopID := retiredRows[0].ID
			snoozedUntil := now.Add(24 * time.Hour)
			_, err = st.SnoozeLoopContext(t.Context(), retiredLoopID, snoozedUntil, now)
			require.NoError(err)
			fulfilmentSuggestedAt := now.Add(-30 * time.Minute)
			_, err = st.DB().Exec(st.Rebind(`UPDATE open_loops SET agenda_state = 'created', fulfilment_suggested_at = ? WHERE id = ?`), fulfilmentSuggestedAt, retiredLoopID)
			require.NoError(err)

			winnerOpen := sourceMergeLoopOpen(winnerConversation, winnerSourceID, winnerMessage, "awaiting_reply", now)
			winnerOpen.OwnerAddress = winnerOwner
			winnerOpen.EvidenceJSON = []byte(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"owner_note":"winner note"}]`, winnerMessage, winnerConversation))
			winnerOpen.LastActivityAt = now.Add(time.Hour)
			if test.retiredSource {
				winnerOpen.LastActivityAt = now
			}
			require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
				ConversationID: winnerConversation,
				Now:            now,
				Verdict:        store.LoopVerdict{Open: &winnerOpen},
			}))
			winnerRows, err := st.ListLiveConversationLoopsContext(t.Context(), winnerConversation)
			require.NoError(err)
			require.Len(winnerRows, 1)
			winnerLoopID := winnerRows[0].ID
			_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
			require.NoError(err)
			winner, err := st.GetOpenLoopContext(t.Context(), winnerLoopID)
			require.NoError(err)
			assert.Equal("open", winner.Status)
			assert.Equal(winnerOwner, winner.OwnerAddress)
			assert.Equal(liveConversation, winner.ConversationID)
			require.NotNil(winner.SnoozedUntil)
			assert.True(snoozedUntil.Equal(*winner.SnoozedUntil))
			assert.Equal("created", winner.AgendaState)
			require.NotNil(winner.FulfilmentSuggestedAt)
			assert.True(fulfilmentSuggestedAt.Equal(*winner.FulfilmentSuggestedAt))
			assert.JSONEq(fmt.Sprintf(`[{"message_id":%d,"conversation_id":%d,"owner_note":"winner note\nretired note"}]`, winnerMessage, liveConversation), string(winner.EvidenceJSON))

			retired, err := st.GetOpenLoopContext(t.Context(), retiredLoopID)
			require.NoError(err)
			assert.Equal("closed", retired.Status)
		})
	}
}

func TestMergeSourcesTransfersLoopStateWithConsolidatedConversations(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	st := testutil.NewTestStore(t)
	from, err := st.GetOrCreateSource("beeper", "history-loops-example")
	require.NoError(err)
	into, err := st.GetOrCreateSource("beeper", "live-loops-example")
	require.NoError(err)

	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	doneMessage, doneConversation := mergeFixtureMessage(t, st, from.ID, "done-room", "history-done", "history body", 1)
	intoDoneMessage, intoDoneConversation := mergeFixtureMessage(t, st, into.ID, "done-room", "live-done", "live body", 2)
	doneOpen := sourceMergeLoopOpen(doneConversation, from.ID, doneMessage, "reply_owed", now)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: doneConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &doneOpen},
	}))
	doneRows, err := st.ListLiveConversationLoopsContext(t.Context(), doneConversation)
	require.NoError(err)
	require.Len(doneRows, 1)
	doneLoop, err := st.CloseLoopContext(t.Context(), doneRows[0].ID, "done", now)
	require.NoError(err)

	snoozedMessage, snoozedConversation := mergeFixtureMessage(t, st, from.ID, "snoozed-room", "history-snoozed", "history snoozed", 3)
	intoSnoozedMessage, intoSnoozedConversation := mergeFixtureMessage(t, st, into.ID, "snoozed-room", "live-snoozed", "live snoozed", 4)
	intoSnoozedOpen := sourceMergeLoopOpen(intoSnoozedConversation, into.ID, intoSnoozedMessage, "awaiting_reply", now)
	intoSnoozedOpen.LastActivityAt = now.Add(-2 * time.Hour)
	intoSnoozedOpen.OwedSinceAt = now.Add(-72 * time.Hour)
	intoSnoozedOpen.OpensAt = now.Add(-72 * time.Hour)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: intoSnoozedConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &intoSnoozedOpen},
	}))
	intoSnoozedRows, err := st.ListLiveConversationLoopsContext(t.Context(), intoSnoozedConversation)
	require.NoError(err)
	require.Len(intoSnoozedRows, 1)
	snoozedOpen := sourceMergeLoopOpen(snoozedConversation, from.ID, snoozedMessage, "awaiting_reply", now)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: snoozedConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &snoozedOpen},
	}))
	snoozedRows, err := st.ListLiveConversationLoopsContext(t.Context(), snoozedConversation)
	require.NoError(err)
	require.Len(snoozedRows, 1)
	snoozedUntil := now.Add(24 * time.Hour)
	snoozedLoop, err := st.SnoozeLoopContext(t.Context(), snoozedRows[0].ID, snoozedUntil, now)
	require.NoError(err)

	mutedMessage, mutedConversation := mergeFixtureMessage(t, st, from.ID, "muted-room", "history-muted", "history muted", 5)
	intoMutedMessage, intoMutedConversation := mergeFixtureMessage(t, st, into.ID, "muted-room", "live-muted", "live muted", 6)
	mutedOpen := sourceMergeLoopOpen(mutedConversation, from.ID, mutedMessage, "reply_owed", now)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: mutedConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &mutedOpen},
	}))
	mutedRows, err := st.ListLiveConversationLoopsContext(t.Context(), mutedConversation)
	require.NoError(err)
	require.Len(mutedRows, 1)
	createMute := func(conversationID int64, note string) store.LoopSuppressionResult {
		result, err := st.CreateLoopSuppressionContext(t.Context(), store.LoopSuppressionRequest{
			TargetKind:     "conversation",
			ConversationID: &conversationID,
			Note:           note,
		}, now)
		require.NoError(err)
		require.NotNil(result.Suppression)
		return result
	}
	fromMute := createMute(mutedConversation, "history mute")
	intoMute := createMute(intoMutedConversation, "live mute")

	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: from.ID, IntoSourceID: into.ID})
	require.NoError(err)

	gotDone, err := st.GetOpenLoopContext(t.Context(), doneLoop.ID)
	require.NoError(err)
	assert.Equal(into.ID, gotDone.SourceID)
	assert.Equal(intoDoneConversation, gotDone.ConversationID, "closed occurrence follows the consolidated done conversation")
	assert.Equal("closed", gotDone.Status)
	require.NotNil(gotDone.ClosedReason)
	assert.Equal("done", *gotDone.ClosedReason)
	blockedDoneOpen := sourceMergeLoopOpen(intoDoneConversation, into.ID, intoDoneMessage, "reply_owed", now)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: intoDoneConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &blockedDoneOpen},
	}))
	doneLive, err := st.ListLiveConversationLoopsContext(t.Context(), intoDoneConversation)
	require.NoError(err)
	assert.Empty(doneLive, "a completed occurrence prevents the same obligation from reopening")

	gotSnoozed, err := st.GetOpenLoopContext(t.Context(), snoozedLoop.ID)
	require.NoError(err)
	assert.Equal(into.ID, gotSnoozed.SourceID)
	assert.Equal(intoSnoozedConversation, gotSnoozed.ConversationID, "snoozed occurrence follows the consolidated snoozed conversation")
	assert.Equal(snoozedLoop.Status, gotSnoozed.Status)
	require.NotNil(gotSnoozed.SnoozedUntil)
	assert.True(snoozedUntil.Equal(*gotSnoozed.SnoozedUntil))
	gotSuperseded, err := st.GetOpenLoopContext(t.Context(), intoSnoozedRows[0].ID)
	require.NoError(err)
	assert.Equal("closed", gotSuperseded.Status)
	require.NotNil(gotSuperseded.ClosedReason)
	assert.Equal("withdrawn", *gotSuperseded.ClosedReason)

	var suppressionCount int
	var suppressionNote string
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*), MAX(note) FROM loop_suppressions WHERE conversation_id = ?`), intoMutedConversation).Scan(&suppressionCount, &suppressionNote))
	assert.Equal(1, suppressionCount, "conversation mutes merge onto one canonical suppression")
	assert.Equal("live mute\nhistory mute", suppressionNote)
	var absorbedSuppressionCount int
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM loop_suppressions WHERE id = ?`), fromMute.Suppression.ID).Scan(&absorbedSuppressionCount))
	assert.Zero(absorbedSuppressionCount, "the absorbed suppression id is retired after deduplication")
	assert.NotZero(intoMute.Suppression.ID)

	var mutedLoopStatus string
	var mutedLoopConversation int64
	require.NoError(st.DB().QueryRow(st.Rebind(`SELECT status, conversation_id FROM open_loops WHERE id = ?`), mutedRows[0].ID).Scan(&mutedLoopStatus, &mutedLoopConversation))
	assert.Equal("closed", mutedLoopStatus)
	assert.Equal(intoMutedConversation, mutedLoopConversation)
	blockedMutedOpen := sourceMergeLoopOpen(intoMutedConversation, into.ID, intoMutedMessage, "reply_owed", now)
	require.NoError(st.ApplyLoopVerdictContext(t.Context(), store.LoopApply{
		ConversationID: intoMutedConversation,
		Now:            now,
		Verdict:        store.LoopVerdict{Open: &blockedMutedOpen},
	}))
	live, err := st.ListLiveConversationLoopsContext(t.Context(), intoMutedConversation)
	require.NoError(err)
	assert.Empty(live, "the transferred conversation mute prevents a muted obligation from reopening")
}
