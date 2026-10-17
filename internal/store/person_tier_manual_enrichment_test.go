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
			f := newTierEnrichmentFixture(t)
			_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto',actor='signal-tiers' WHERE person_id=?`), f.person.ID)
			require.NoError(t, err)
			if alreadyEligible {
				tierManualSetAppliedTierOne(t, f)
			}
			tracked, err := f.store.SetPersonTrackingActorContext(t.Context(), f.person.ID, true, "owner")
			require.NoError(t, err)
			assert.Equal(t, "manual", tracked.Provenance)
			assert.Equal(t, "owner", tracked.Actor)
			work := f.work(t)
			if alreadyEligible {
				assert.Empty(t, work)
			} else {
				require.Len(t, work, 1)
				assert.True(t, strings.HasPrefix(work[0].TriggerGeneration, "provenance:"))
			}
			_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_enrichment_work WHERE person_id=?`), f.person.ID)
			require.NoError(t, err)
			_, err = f.store.SetPersonTrackingActorContext(t.Context(), f.person.ID, true, "other-owner")
			require.NoError(t, err)
			assert.Empty(t, f.work(t), "repeating a manual grant must not publish duplicate work")
		})
	}
}

func TestPersonTierEnrichmentOptInPublishesOnce(t *testing.T) {
	f := newTierEnrichmentFixture(t)
	f.enqueue(t)
	require.NoError(t, f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", true, "owner", "pause"))
	assert.Empty(t, f.work(t))
	require.NoError(t, f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", false, "owner", ""))
	work := f.work(t)
	require.Len(t, work, 1)
	assert.True(t, strings.HasPrefix(work[0].TriggerGeneration, "opt-in:"))
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_enrichment_work WHERE person_id=?`), f.person.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", false, "owner", ""))
	assert.Empty(t, f.work(t))
}

func TestPersonTierTrackingOptOutRefusesExactManualTarget(t *testing.T) {
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(t, err)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "tier-opt-out", f.now)
	require.NoError(t, err)
	err = f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "tracking", true, "owner", "")
	assert.ErrorIs(t, err, store.ErrPersonTierManualRunOpen)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "run ")
	opts, err := f.store.ListPersonTierOptOutsContext(t.Context(), f.person.ID)
	require.NoError(t, err)
	assert.Empty(t, opts)
	require.NoError(t, f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, "enrichment", true, "owner", ""))
	work := f.work(t)
	require.Len(t, work, 1)
	require.NotNil(t, work[0].RunID)
	assert.Equal(t, run.ID, *work[0].RunID)
}
