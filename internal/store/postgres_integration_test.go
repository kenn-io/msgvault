package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPostgresPersonMergeParity(t *testing.T) {
	st := testutil.NewTestStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL integration database is not configured")
	}
	ctx := context.Background()

	t.Run("merge replay and stale revision rollback", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		survivor := mustPromotedPerson(t, st, "pg-parity-survivor@example.com", "Survivor")
		absorbed := mustPromotedPerson(t, st, "pg-parity-absorbed@example.com", "Absorbed")
		request := store.PersonMergeRequest{
			SurvivorID: survivor.ID, AbsorbedID: absorbed.ID,
			ExpectedSurvivorRevision: survivor.Revision,
			ExpectedAbsorbedRevision: absorbed.Revision,
			IdempotencyKey:           "pg-parity-merge", Actor: "test",
		}
		merged, err := st.MergePersonsContext(ctx, request)
		require.NoError(err)
		replayed, err := st.MergePersonsContext(ctx, request)
		require.NoError(err)
		assertJSONEquivalent(t, merged, replayed)

		staleSurvivor := mustPromotedPerson(t, st, "pg-stale-survivor@example.com", "Stale Survivor")
		staleAbsorbed := mustPromotedPerson(t, st, "pg-stale-absorbed@example.com", "Stale Absorbed")
		var mergeCountBefore int
		require.NoError(st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM person_merges`).Scan(&mergeCountBefore))
		_, err = st.MergePersonsContext(ctx, store.PersonMergeRequest{
			SurvivorID: staleSurvivor.ID, AbsorbedID: staleAbsorbed.ID,
			ExpectedSurvivorRevision: staleSurvivor.Revision + 1,
			ExpectedAbsorbedRevision: staleAbsorbed.Revision,
			IdempotencyKey:           "pg-stale-merge", Actor: "test",
		})
		require.ErrorIs(err, store.ErrPersonRevisionConflict)
		var mergeCountAfter int
		require.NoError(st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM person_merges`).Scan(&mergeCountAfter))
		assert.Equal(mergeCountBefore, mergeCountAfter)
		_, err = st.GetPersonContext(ctx, staleAbsorbed.ID)
		require.NoError(err, "failed merge must retain the absorbed profile")
	})

	t.Run("candidate decision", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newPersonMergeInspectionFixture(t, "pg-parity-candidate")
		require.True(f.store.IsPostgreSQL())
		require.Len(f.merge.ReviewCandidates, 1)
		candidate, err := f.store.DecidePersonMergeCandidateContext(ctx,
			store.PersonMergeCandidateDecisionRequest{
				CandidateID: f.merge.ReviewCandidates[0].ID, PersonID: f.person.ID,
				ExpectedPersonRevision: f.person.Revision,
				Decision:               store.PersonMergeCandidateReject, Actor: "reviewer",
			})
		require.NoError(err)
		assert.Equal("rejected", candidate.State)
	})

	t.Run("identity link conflict payload", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		left := mustPromotedPerson(t, st, "pg-conflict-left@example.com", "Left")
		right := mustPromotedPerson(t, st, "pg-conflict-right@example.com", "Right")
		_, err := st.LinkParticipants(left.ParticipantIDs[0], right.ParticipantIDs[0])
		require.Error(err)
		require.ErrorIs(err, store.ErrPersonBindingConflict)
		var conflict *store.PersonBindingConflictError
		require.ErrorAs(err, &conflict)
		assert.ElementsMatch([]int64{left.ID, right.ID}, conflict.PersonIDs)
	})
}

func TestPostgresPersonSplitParity(t *testing.T) {
	t.Run("exact reversal and replay", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newPersonSplitFixture(t)
		if !f.store.IsPostgreSQL() {
			t.Skip("PostgreSQL integration database is not configured")
		}
		request := store.PersonSplitRequest{
			SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
			ParticipantIDs:         f.absorbedParticipants,
			ExpectedSourceRevision: f.survivor.Revision,
			IdempotencyKey:         "pg-parity-exact-split", Actor: "test",
		}
		result, err := f.store.SplitPersonMergeContext(context.Background(), request)
		require.NoError(err)
		assert.True(result.ExactReversal)
		replayed, err := f.store.SplitPersonMergeContext(context.Background(), request)
		require.NoError(err)
		assertJSONEquivalent(t, result, replayed)
	})

	t.Run("partial split", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newPersonSplitFixture(t)
		if !f.store.IsPostgreSQL() {
			t.Skip("PostgreSQL integration database is not configured")
		}
		result, err := f.store.SplitPersonMergeContext(context.Background(), store.PersonSplitRequest{
			SourcePersonID: f.survivor.ID, MergeID: f.merge.Merge.ID,
			ParticipantIDs:         []int64{f.absorbedParticipants[0]},
			ExpectedSourceRevision: f.survivor.Revision,
			IdempotencyKey:         "pg-parity-partial-split", Actor: "test",
		})
		require.NoError(err)
		assert.False(result.ExactReversal)
		assert.Equal([]int64{f.absorbedParticipants[0]}, result.NewPerson.ParticipantIDs)
		assert.ElementsMatch([]int64{f.survivorParticipant, f.absorbedParticipants[1]},
			result.SourcePerson.ParticipantIDs)
	})
}
