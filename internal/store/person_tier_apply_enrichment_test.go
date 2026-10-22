package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

func TestPersonTierApplyPublishesFreshEligibilityAndCancelsLaneEdit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(err)
	cfg := persontiers.DefaultConfig()
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	run, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	page, err := f.store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	input := page.People[0]
	input.Signals = persontiers.Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6}
	require.NoError(f.store.ApplyPersonTierDecisionContext(t.Context(), *run, input))
	work := f.work(t)
	require.Len(work, 1)
	assert.Equal("tier:"+run.RunID, work[0].TriggerGeneration)
	var eligible bool
	require.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT enrichment_eligible FROM person_tier_state WHERE person_id=?`), f.person.ID).Scan(&eligible))
	assert.True(eligible)
	// Editing live config alone cannot change the persisted authority predicate.
	req.Config.Tier1Lanes = []string{"tracking", "briefs"}
	allowed, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(err)
	assert.True(allowed)
	next, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	require.NoError(f.store.ApplyPersonTierDecisionContext(t.Context(), *next, input))
	assert.Empty(f.work(t))
	allowed, err = store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(err)
	assert.False(allowed)
	events, err := f.store.PersonTierHistoryContext(t.Context(), f.person.ID, 20, 0)
	require.NoError(err)
	require.Len(events, 2)
	assert.Equal("config_change", events[0].Reason)
}

func TestPersonTierApplyRequiresTrackingAndEnrichmentLanesForScheduledEligibility(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(err)
	manualRun, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "tier-lanes-only-enrichment", f.now)
	require.NoError(err)

	cfg := persontiers.DefaultConfig()
	cfg.Tier1Lanes = []string{"enrichment"}
	cfg.Tier2Lanes = []string{}
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	tierRun, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	require.NoError(f.store.ApplyPersonTierDecisionContext(t.Context(), *tierRun, store.PersonTierInput{
		PersonID: f.person.ID,
		Signals:  persontiers.Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6},
	}))

	var eligible bool
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT enrichment_eligible FROM person_tier_state WHERE person_id=?`), f.person.ID).Scan(&eligible))
	assert.False(eligible)
	authorized, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(err)
	assert.False(authorized)
	work := f.work(t)
	require.Len(work, 1)
	assert.Equal("manual:tier-lanes-only-enrichment", work[0].TriggerGeneration)
	lease := f.claim(t, manualRun.ID, "manual-tier-worker")
	assert.NotNil(lease, "the exact manual target remains authorized")
}

func TestPersonTierApplyPersistsEnrichmentLaneAcrossOptOut(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(err)
	require.NoError(f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "operator", "pause scheduled enrichment"))

	cfg := persontiers.DefaultConfig()
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	run, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	require.NoError(f.store.ApplyPersonTierDecisionContext(t.Context(), *run, store.PersonTierInput{
		PersonID: f.person.ID,
		Signals:  persontiers.Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6},
	}))

	var persistedEligible bool
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT enrichment_eligible FROM person_tier_state WHERE person_id=?`), f.person.ID).Scan(&persistedEligible))
	assert.True(persistedEligible, "the persisted authority reflects applied lanes, independently of an opt-out")
	eligible, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(err)
	assert.False(eligible, "an enrichment opt-out still blocks scheduled authority")
	assert.Empty(f.work(t))

	require.NoError(f.store.SetPersonTierOptOutContext(t.Context(), f.person.ID, store.PersonTierLaneEnrichment, false, "operator", "resume scheduled enrichment"))
	eligible, err = store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(err)
	assert.True(eligible, "clearing the opt-out restores scheduled authority from the applied lanes")
	work := f.work(t)
	require.Len(work, 1)
	assert.Contains(work[0].TriggerGeneration, "opt-in:")
}

func TestPersonTierManualApplySupersedesScheduledRunWithFrozenClock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	_, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	scheduled, err := f.store.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
	require.NoError(err)
	newest, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	assert.True(newest.StartedAt.After(scheduled.StartedAt))
	page, err := f.store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	require.ErrorIs(f.store.ApplyPersonTierDecisionContext(t.Context(), *scheduled, page.People[0]), store.ErrPersonTierRunSuperseded)
}

func TestPersonTierLaneChangesTakePrecedenceOverManualBlockInAudit(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	_, err := f.store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	req.Config.Tier2Lanes = []string{"tracking", "briefs"}
	_, err = f.store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	events, err := f.store.PersonTierHistoryContext(t.Context(), f.person.ID, 5, 0)
	require.NoError(err)
	require.NotEmpty(events)
	assert.Equal("config_change", events[0].Reason)
}

func TestPersonTierAsOfUsesInjectedClockAndLocalCalendarDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	cfg := persontiers.DefaultConfig()
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "America/New_York", MaxDirect: 25, Limit: 500}
	page, err := f.store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(err)
	expected := time.Date(2026, 8, 22, 0, 0, 0, 0, loc)
	assert.True(expected.Equal(page.AsOf))
	run, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	assert.True(expected.Equal(run.AsOf))
	explanation, err := f.store.ExplainPersonTierContext(t.Context(), f.person.ID, req)
	require.NoError(err)
	assert.True(expected.Equal(explanation.AsOf))
}
