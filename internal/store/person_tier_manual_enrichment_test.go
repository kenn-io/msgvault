package store_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func tierManualSetAppliedTierOne(t *testing.T, f tierEnrichmentFixture) {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`INSERT INTO person_tier_state(person_id,tier,tier_since,lanes_json,enrichment_eligible,rule,rule_version,config_fingerprint,as_of_local_date,run_id,run_started_at,applied_at) VALUES(?,1,?,'["tracking","enrichment"]',TRUE,'tier1_intensity',1,'config','2026-08-22','run',?,?)`), f.person.ID, f.now, f.now, f.now)
	require.NoError(t, err)
}

func TestPersonTierManualTrackingPublishesOnNewScheduledAuthority(t *testing.T) {
	for _, alreadyEligible := range []bool{false, true} {
		name := "tier2"
		if alreadyEligible {
			name = "tier1"
		}
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newTierEnrichmentFixture(t)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto',actor='signal-tiers' WHERE person_id=?`), f.person.ID)
			require.NoError(err)
			if alreadyEligible {
				tierManualSetAppliedTierOne(t, f)
			}
			tracked, err := f.store.SetPersonTrackingActorContext(t.Context(), f.person.ID, true, "owner")
			require.NoError(err)
			assert.Equal("manual", tracked.Provenance)
			assert.Equal("owner", tracked.Actor)
			work := f.work(t)
			if alreadyEligible {
				assert.Empty(work)
			} else {
				require.Len(work, 1)
				assert.True(strings.HasPrefix(work[0].TriggerGeneration, "provenance:"))
			}
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_enrichment_work WHERE person_id=?`), f.person.ID)
			require.NoError(err)
			_, err = f.store.SetPersonTrackingActorContext(t.Context(), f.person.ID, true, "other-owner")
			require.NoError(err)
			assert.Empty(f.work(t), "repeating a manual grant must not publish duplicate work")
		})
	}
}

func TestPersonTierEnrichmentOptInPublishesOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	f.enqueue(t)
	require.NoError(f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", true, "owner", "pause"))
	assert.Empty(f.work(t))
	require.NoError(f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", false, "owner", ""))
	work := f.work(t)
	require.Len(work, 1)
	assert.True(strings.HasPrefix(work[0].TriggerGeneration, "opt-in:"))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_enrichment_work WHERE person_id=?`), f.person.ID)
	require.NoError(err)
	require.NoError(f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", false, "owner", ""))
	assert.Empty(f.work(t))
}

func TestPersonTierTrackingOptOutRefusesExactManualTarget(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(err)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "tier-opt-out", f.now)
	require.NoError(err)
	err = f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "tracking", true, "owner", "")
	assert.ErrorIs(err, store.ErrPersonTierManualRunOpen)
	require.Error(err)
	assert.Contains(err.Error(), "run ")
	opts, err := f.store.ListPersonTierOptOutsContext(t.Context(), f.person.ID)
	require.NoError(err)
	assert.Empty(opts)
	require.NoError(f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", true, "owner", ""))
	work := f.work(t)
	require.Len(work, 1)
	require.NotNil(work[0].RunID)
	assert.Equal(run.ID, *work[0].RunID)
}
