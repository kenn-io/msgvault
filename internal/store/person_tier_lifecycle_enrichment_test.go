package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierMergeRefusesOptOutRemovingOpenManualTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(err)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "tier-merge-refusal", f.now)
	require.NoError(err)
	participant, err := f.store.EnsureParticipant("opted-out-merge@example.test", "Opted Out", "example.test")
	require.NoError(err)
	absorbed, _, err := f.store.CreatePersonFromParticipant(participant)
	require.NoError(err)
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_tier_opt_outs(person_id,lane,actor) VALUES(?,'tracking','owner')`), absorbed.ID)
	require.NoError(err)
	_, err = f.store.MergePersonsContext(t.Context(), store.PersonMergeRequest{
		SurvivorID: f.person.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: f.person.Revision, ExpectedAbsorbedRevision: absorbed.Revision,
		IdempotencyKey: "tier-merge-refusal", Actor: "owner",
	})
	assert.ErrorIs(err, store.ErrPersonTierManualRunOpen)
	tracking, err := f.store.GetPersonTrackingContext(t.Context(), f.person.ID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	_, err = f.store.GetPersonContext(t.Context(), absorbed.ID)
	require.NoError(err)
	work := f.work(t)
	require.Len(work, 1)
	require.NotNil(work[0].RunID)
	assert.Equal(run.ID, *work[0].RunID)
}
