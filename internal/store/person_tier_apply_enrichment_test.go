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
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), f.person.ID)
	require.NoError(t, err)
	cfg := persontiers.DefaultConfig()
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	run, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	page, err := f.store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	input := page.People[0]
	input.Signals = persontiers.Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6}
	require.NoError(t, f.store.ApplyPersonTierDecisionContext(t.Context(), *run, input))
	work := f.work(t)
	require.Len(t, work, 1)
	assert.Equal(t, "tier:"+run.RunID, work[0].TriggerGeneration)
	var eligible bool
	require.NoError(t, f.store.DB().QueryRow(f.store.Rebind(`SELECT enrichment_eligible FROM person_tier_state WHERE person_id=?`), f.person.ID).Scan(&eligible))
	assert.True(t, eligible)
	// Editing live config alone cannot change the persisted authority predicate.
	req.Config.Tier1Lanes = []string{"tracking", "briefs"}
	allowed, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(t, err)
	assert.True(t, allowed)
	next, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	require.NoError(t, f.store.ApplyPersonTierDecisionContext(t.Context(), *next, input))
	assert.Empty(t, f.work(t))
	allowed, err = store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
	require.NoError(t, err)
	assert.False(t, allowed)
	events, err := f.store.PersonTierHistoryContext(t.Context(), f.person.ID, 20, 0)
	require.NoError(t, err)
	require.Len(t, events, 2)
	assert.Equal(t, "config_change", events[0].Reason)
}

func TestPersonTierManualApplySupersedesScheduledRunWithFrozenClock(t *testing.T) {
	f := newTierEnrichmentFixture(t)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	_, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	scheduled, err := f.store.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
	require.NoError(t, err)
	newest, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	assert.True(t, newest.StartedAt.After(scheduled.StartedAt))
	page, err := f.store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	require.ErrorIs(t, f.store.ApplyPersonTierDecisionContext(t.Context(), *scheduled, page.People[0]), store.ErrPersonTierRunSuperseded)
}

func TestPersonTierLaneChangesTakePrecedenceOverManualBlockInAudit(t *testing.T) {
	f := newTierEnrichmentFixture(t)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: f.now, Limit: 500}
	_, err := f.store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	req.Config.Tier2Lanes = []string{"tracking", "briefs"}
	_, err = f.store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	events, err := f.store.PersonTierHistoryContext(t.Context(), f.person.ID, 5, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, "config_change", events[0].Reason)
}

func TestPersonTierAsOfUsesInjectedClockAndLocalCalendarDate(t *testing.T) {
	f := newTierEnrichmentFixture(t)
	cfg := persontiers.DefaultConfig()
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "America/New_York", MaxDirect: 25, Limit: 500}
	page, err := f.store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	loc, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	expected := time.Date(2026, 8, 22, 0, 0, 0, 0, loc)
	assert.True(t, expected.Equal(page.AsOf))
	run, err := f.store.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	assert.True(t, expected.Equal(run.AsOf))
	explanation, err := f.store.ExplainPersonTierContext(t.Context(), f.person.ID, req)
	require.NoError(t, err)
	assert.True(t, expected.Equal(explanation.AsOf))
}
