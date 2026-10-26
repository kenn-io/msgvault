package store_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/store"
)

func TestMergePersons_DefersMemberOnlyActivity(t *testing.T) {
	for _, scenario := range []struct {
		name  string
		limit int
		owner bool
	}{{"threshold", 2, false}, {"direct_members", 25, false}, {"owner_merge", 2, true}} {
		limit := scenario.limit
		for _, splitBeforeProjection := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/split_%t", scenario.name, splitBeforeProjection), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f, survivor, absorbed, directID := mergeDerivedStateFixture(t)
				ctx := t.Context()
				if scenario.owner {
					require.NoError(f.Store.AddAccountIdentity(f.Source.ID, "merge-derived-survivor@example.com", "test"))
				}
				owner := f.EnsureParticipant("merge-derived-owner@example.com", "Owner", "example.com")
				thirdParticipant := f.EnsureParticipant("room-third@example.com", "Third", "example.com")
				third, _, err := f.Store.CreatePersonFromParticipant(thirdParticipant)
				require.NoError(err)
				room, err := f.Store.EnsureConversationWithType(f.Source.ID, "scope-room", "group_chat", "Synthetic room")
				require.NoError(err)
				for _, participantID := range []int64{owner, survivor.ParticipantIDs[0], absorbed.ParticipantIDs[0], thirdParticipant} {
					require.NoError(f.Store.EnsureConversationParticipant(room, participantID, "member"))
				}
				var roomIDs []int64
				for i := range 6 {
					message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("room-%d", i)).
						WithSentAt(time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)).WithIsFromMe(true).Build()
					message.ConversationID = room
					message.SenderID = sql.NullInt64{Int64: owner, Valid: true}
					id, insertErr := f.Store.UpsertMessage(message)
					require.NoError(insertErr)
					require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{owner}, []string{"Owner"}))
					roomIDs = append(roomIDs, id)
				}
				initial, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 2, MaxDirectCounterparts: limit})
				require.NoError(err)
				_, err = initial.RunOnce(ctx)
				require.NoError(err)
				var oldEpoch int64
				require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
					`SELECT projected_identity_revision FROM activity_events WHERE message_id = ?`), roomIDs[0]).Scan(&oldEpoch))
				var evidence string
				require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
					`SELECT evidence FROM activity_event_persons WHERE message_id = ? AND person_id = ? AND role = 'member'`),
					roomIDs[0], absorbed.ID).Scan(&evidence))
				wantEvidence := "co_presence"
				if limit == 25 || scenario.owner {
					wantEvidence = "direct"
				}
				assert.Equal(wantEvidence, evidence)
				merged, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
					SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
					ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
					IdempotencyKey: "scope-merge", Actor: "test",
				})
				require.NoError(err)
				var epoch, absorbedRefs int64
				require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
					`SELECT projected_identity_revision FROM activity_events WHERE message_id = ?`), roomIDs[0]).Scan(&epoch))
				assert.Equal(oldEpoch, epoch, "membership-only messages must stay outside synchronous reclassification")
				require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
					`SELECT COUNT(*) FROM activity_event_persons WHERE person_id = ?`), absorbed.ID).Scan(&absorbedRefs))
				assert.Zero(absorbedRefs)
				var directPeople int
				require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
					`SELECT COUNT(*) FROM activity_event_persons WHERE message_id = ? AND person_id = ? AND role = 'addressed' AND evidence = 'direct'`),
					directID, survivor.ID).Scan(&directPeople))
				wantDirect := 1
				if scenario.owner {
					wantDirect = 0
				}
				assert.Equal(wantDirect, directPeople, "native addressed activity is repaired immediately")
				state, err := f.Store.ContactStateContext(ctx, survivor.ID, time.Now())
				require.NoError(err)
				assert.True(state.Stale)

				splitID := int64(0)
				if splitBeforeProjection {
					split, splitErr := f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
						SourcePersonID: survivor.ID, MergeID: merged.Merge.ID,
						ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: merged.Person.Revision,
						IdempotencyKey: "scope-split", Actor: "test",
					})
					require.NoError(splitErr)
					splitID = split.NewPerson.ID
				}
				bounded, err := activity.NewProjector(f.Store, activity.Options{
					Timezone: "UTC", BatchSize: 2, MaxBatches: 1, MaxDirectCounterparts: limit,
				})
				require.NoError(err)
				drain := func() int {
					passes := 0
					for {
						result, projectErr := bounded.RunOnce(ctx)
						assert.LessOrEqual(result.Batches, 1)
						passes++
						if projectErr == nil {
							break
						}
						require.ErrorIs(projectErr, activity.ErrWorkRemaining)
						require.Less(passes, 30, "bounded passes must converge")
					}
					return passes
				}
				assert.Greater(drain(), 1)
				state, err = f.Store.ContactStateContext(ctx, third.ID, time.Now())
				require.NoError(err)
				assert.False(state.Stale)
				wantCount := int64(len(roomIDs))
				if splitBeforeProjection && limit == 2 && !scenario.owner {
					wantCount = 0
				}
				assert.Equal(wantCount, state.InteractionCount, "backstop repairs threshold changes for other room members")
				if wantCount > 0 {
					assert.Equal(fmt.Sprintf("message:%d", roomIDs[0]), state.FirstContactRef)
					assert.Equal(fmt.Sprintf("message:%d", roomIDs[len(roomIDs)-1]), state.LastContactRef)
				}
				var finalEpoch int64
				require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
					`SELECT projected_identity_revision FROM activity_events WHERE message_id = ?`), roomIDs[0]).Scan(&finalEpoch))
				revisions, err := f.Store.ContactRevisionsContext(ctx)
				require.NoError(err)
				assert.Equal(revisions.IdentityRevision, finalEpoch)
				if scenario.owner && !splitBeforeProjection {
					var ownerLinks int
					require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
						`SELECT COUNT(*) FROM activity_event_persons WHERE person_id = ? AND role = 'member'`), survivor.ID).Scan(&ownerLinks))
					assert.Zero(ownerLinks, "owner-linked members are excluded after background repair")
				}
				if splitBeforeProjection {
					var splitLinks int
					require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
						`SELECT COUNT(*) FROM activity_event_persons WHERE person_id = ? AND role = 'member'`), splitID).Scan(&splitLinks))
					assert.Equal(len(roomIDs), splitLinks)
				} else {
					// A split after the merged epoch has already converged must
					// also restore the original room audience and evidence.
					_, err = f.Store.SplitPersonMergeContext(ctx, store.PersonSplitRequest{
						SourcePersonID: survivor.ID, MergeID: merged.Merge.ID,
						ParticipantIDs: absorbed.ParticipantIDs, ExpectedSourceRevision: merged.Person.Revision,
						IdempotencyKey: "scope-split-after-projection", Actor: "test",
					})
					require.NoError(err)
					drain()
					restored, readErr := f.Store.ContactStateContext(ctx, third.ID, time.Now())
					require.NoError(readErr)
					assert.False(restored.Stale)
					restoredCount := int64(len(roomIDs))
					if limit == 2 && !scenario.owner {
						restoredCount = 0
					}
					assert.Equal(restoredCount, restored.InteractionCount)
				}
			})
		}
	}
}

func TestMergePersons_RepairsNonMemberLinksAfterNativeEdgesRemoved(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, survivor, absorbed, messageID := mergeDerivedStateFixture(t)
	ctx := t.Context()
	// The old materialized links must be retracted even when the native edge
	// which originally selected this event is gone and its queue is pending.
	require.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", nil, nil))
	_, err := f.Store.MergePersonsContext(ctx, store.PersonMergeRequest{
		SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
		ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "removed-native-edge", Actor: "test",
	})
	require.NoError(err)
	var links int
	require.NoError(f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(
		`SELECT COUNT(*) FROM activity_event_persons WHERE message_id = ?`), messageID).Scan(&links))
	assert.Zero(links)
}
