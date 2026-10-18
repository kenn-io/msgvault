package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierProjectionRunFixture(t *testing.T) (*store.Store, int64, store.PersonTierPageRequest) {
	t.Helper()
	f := storetest.New(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("projection-tier@example.org", "Synthetic Projection", "example.org"))
	require.NoError(t, err)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	return f.Store, person.ID, store.PersonTierPageRequest{
		Config: cfg, Timezone: "UTC", MaxDirect: 25,
		AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500,
	}
}

func completeTierProjectionSettings(t *testing.T, s *store.Store, zone string, limit int) {
	t.Helper()
	tz, err := s.ClaimActivityTimezoneTransitionContext(t.Context(), zone)
	require.NoError(t, err)
	if tz.Active {
		require.NoError(t, s.CompleteActivityTimezoneTransitionContext(t.Context(), tz))
	}
	direct, err := s.ClaimActivityDirectLimitTransitionContext(t.Context(), limit)
	require.NoError(t, err)
	if direct.Active {
		require.NoError(t, s.CompleteActivityDirectLimitTransitionContext(t.Context(), direct))
	}
}

func TestPersonTierCompletedProjectionChangeSupersedesRun(t *testing.T) {
	for _, kind := range []string{"manual", "scheduled"} {
		for _, mode := range []string{"apply", "record_only"} {
			for _, setting := range []string{"timezone", "direct_limit", "unchanged"} {
				t.Run(kind+"/"+mode+"/"+setting, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					s, _, req := tierProjectionRunFixture(t)
					participant, err := s.EnsureParticipant("next-projection@example.org", "Next Projection", "example.org")
					require.NoError(err)
					_, _, err = s.CreatePersonFromParticipant(participant)
					require.NoError(err)
					completeTierProjectionSettings(t, s, "UTC", 25)
					run, err := s.StartPersonTierRunContext(t.Context(), req, kind, mode)
					require.NoError(err)
					first, err := s.ProcessPersonTierRunPageContext(t.Context(), *run, 1, "operator")
					require.NoError(err)
					require.NotEmpty(first.NextCursor)
					run, err = s.PersonTierRunContext(t.Context(), kind)
					require.NoError(err)
					require.NotNil(run)
					before := tierProjectionRowCounts(t, s)
					switch setting {
					case "timezone":
						completeTierProjectionSettings(t, s, "America/Los_Angeles", 25)
					case "direct_limit":
						completeTierProjectionSettings(t, s, "UTC", 1)
					default:
						completeTierProjectionSettings(t, s, "UTC", 25)
					}
					page, err := s.ProcessPersonTierRunPageContext(t.Context(), *run, 1, "operator")
					if setting == "unchanged" {
						require.NoError(err)
						require.Len(page.People, 1)
						assert.Empty(page.NextCursor)
						counts := tierProjectionRowCounts(t, s)
						if mode == "apply" {
							assert.Equal(2, counts["person_tier_state"])
						} else {
							assert.Equal(2, counts["person_tier_observations"])
						}
						return
					}
					require.ErrorIs(err, store.ErrPersonTierRunSuperseded)
					assert.Equal(before, tierProjectionRowCounts(t, s))
					remaining, err := s.PersonTierRunContext(t.Context(), kind)
					require.NoError(err)
					if kind == "scheduled" {
						assert.Nil(remaining)
					} else {
						assert.NotNil(remaining)
					}
				})
			}
		}
	}
}

func TestPersonTierProjectionChangeBetweenPlanAndWrite(t *testing.T) {
	for _, setting := range []string{"timezone", "direct_limit"} {
		t.Run(setting, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, _, req := tierProjectionRunFixture(t)
			completeTierProjectionSettings(t, s, "UTC", 25)
			run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
			require.NoError(err)
			page, err := s.PlanPersonTiersContext(t.Context(), req)
			require.NoError(err)
			require.Len(page.People, 1)
			before := tierProjectionRowCounts(t, s)
			if setting == "timezone" {
				completeTierProjectionSettings(t, s, "America/Los_Angeles", 25)
			} else {
				completeTierProjectionSettings(t, s, "UTC", 1)
			}
			require.ErrorIs(s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]), store.ErrPersonTierRunSuperseded)
			assert.Equal(before, tierProjectionRowCounts(t, s))
		})
	}
}

func TestPersonTierApplyRefreshesSignalsAfterProjectionAdvances(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	projectTierSignals(t, f, "UTC", 25)
	cfg := persontiers.DefaultConfig()
	cfg.DemotionGraceDays = 0
	request := store.PersonTierPageRequest{
		Config: cfg, Timezone: "UTC", MaxDirect: 25,
		AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500,
	}
	applySyntheticTierOne(t, f.Store, personID, request)

	request.AsOf = time.Date(2027, 1, 17, 12, 0, 0, 0, time.UTC)
	run, err := f.Store.StartPersonTierRunContext(t.Context(), request, "manual", "apply")
	require.NoError(err)
	page, err := f.Store.PlanPersonTiersContext(t.Context(), request)
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal("demotion", page.People[0].Decision.Reason)

	var counterpart int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id=?`), personID).Scan(&counterpart))
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	for _, event := range []struct {
		id   string
		days int
		from int64
		to   int64
	}{
		{id: "tier-apply-new-outbound-1", days: 0, from: owner, to: counterpart},
		{id: "tier-apply-new-outbound-2", days: 1, from: owner, to: counterpart},
		{id: "tier-apply-new-outbound-3", days: 2, from: owner, to: counterpart},
		{id: "tier-apply-new-inbound-1", days: 3, from: counterpart, to: owner},
		{id: "tier-apply-new-inbound-2", days: 4, from: counterpart, to: owner},
		{id: "tier-apply-new-inbound-3", days: 5, from: counterpart, to: owner},
	} {
		message := f.NewMessage().WithSourceMessageID(event.id).WithSentAt(request.AsOf.AddDate(0, 0, -event.days)).Build()
		message.SenderID = sql.NullInt64{Int64: event.from, Valid: true}
		messageID, upsertErr := f.Store.UpsertMessage(message)
		require.NoError(upsertErr)
		require.NoError(f.Store.ReplaceMessageRecipients(messageID, "from", []int64{event.from}, nil))
		require.NoError(f.Store.ReplaceMessageRecipients(messageID, "to", []int64{event.to}, nil))
	}
	projectTierSignals(t, f, "UTC", 25)
	current, err := f.Store.PersonTierSignalsContext(t.Context(), []int64{personID}, request.AsOf, cfg, 25)
	require.NoError(err)
	assert.Equal(3, current[personID].OutboundShort)
	assert.Equal(3, current[personID].InboundShort)
	assert.Equal(6, current[personID].ActiveDaysShort)

	require.NoError(f.Store.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	var tier int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT tier FROM person_tier_state WHERE person_id=?`), personID).Scan(&tier))
	assert.Equal(1, tier)
}

func TestPersonTierApplyRechecksReadinessBeforeDemotion(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	projectTierSignals(t, f, "UTC", 25)
	cfg := persontiers.DefaultConfig()
	cfg.DemotionGraceDays = 0
	request := store.PersonTierPageRequest{
		Config: cfg, Timezone: "UTC", MaxDirect: 25,
		AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500,
	}
	applySyntheticTierOne(t, f.Store, personID, request)

	request.AsOf = time.Date(2027, 1, 17, 12, 0, 0, 0, time.UTC)
	run, err := f.Store.StartPersonTierRunContext(t.Context(), request, "manual", "apply")
	require.NoError(err)
	page, err := f.Store.PlanPersonTiersContext(t.Context(), request)
	require.NoError(err)
	require.Len(page.People, 1)
	require.True(page.Readiness.Ready)

	f.CreateMessage("tier-apply-queued-after-plan")
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	require.False(ready.Ready)
	assert.Contains(ready.Reasons, "pending_activity")

	require.NoError(f.Store.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	var tier int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT tier FROM person_tier_state WHERE person_id=?`), personID).Scan(&tier))
	assert.Equal(1, tier)
	tracking, err := f.Store.GetPersonTrackingContext(t.Context(), personID)
	require.NoError(err)
	assert.True(tracking.Tracked)
	briefs, err := f.Store.GetPersonBriefEnrollmentContext(t.Context(), personID)
	require.NoError(err)
	assert.True(briefs.Enrolled)
	events, err := f.Store.PersonTierHistoryContext(t.Context(), personID, 5, 0)
	require.NoError(err)
	require.NotEmpty(events)
	assert.Equal("deferred", events[0].Reason)
}

func TestPersonTierApplyRechecksReadinessAfterIdentityRevision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	projectTierSignals(t, f, "UTC", 25)
	cfg := persontiers.DefaultConfig()
	cfg.DemotionGraceDays = 0
	request := store.PersonTierPageRequest{
		Config: cfg, Timezone: "UTC", MaxDirect: 25,
		AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500,
	}
	applySyntheticTierOne(t, f.Store, personID, request)

	request.AsOf = time.Date(2027, 1, 17, 12, 0, 0, 0, time.UTC)
	run, err := f.Store.StartPersonTierRunContext(t.Context(), request, "manual", "apply")
	require.NoError(err)
	page, err := f.Store.PlanPersonTiersContext(t.Context(), request)
	require.NoError(err)
	require.Len(page.People, 1)
	require.True(page.Readiness.Ready)

	require.NoError(f.Store.AddAccountIdentityContext(t.Context(), f.Source.ID, "owner@example.org", "tier-test"))
	ready, err := f.Store.PersonTierReadinessContext(t.Context())
	require.NoError(err)
	require.False(ready.Ready)
	assert.Contains(ready.Reasons, "identity_revision_changed")
	assert.NotContains(ready.Reasons, "pending_activity")

	require.NoError(f.Store.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	var tier int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT tier FROM person_tier_state WHERE person_id=?`), personID).Scan(&tier))
	assert.Equal(1, tier)
	events, err := f.Store.PersonTierHistoryContext(t.Context(), personID, 5, 0)
	require.NoError(err)
	require.NotEmpty(events)
	assert.Equal("deferred", events[0].Reason)
}

func TestPersonTierPreviewShowsEnrichmentLanePolicyTransitions(t *testing.T) {
	for _, tc := range []struct {
		name      string
		before    []string
		after     []string
		eligible  bool
		laneState string
	}{
		{name: "became eligible", before: []string{"tracking"}, after: []string{"tracking", "enrichment"}, eligible: true, laneState: "eligible"},
		{name: "became ineligible", before: []string{"tracking", "enrichment"}, after: []string{"tracking"}, eligible: false, laneState: "removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, personID := newTierSignalsFixture(t)
			asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
			seedTierSignalWindow(t, f, personID, asOf, "enrichment-lane-policy")
			cfg := persontiers.DefaultConfig()
			cfg.Tier1Lanes = tc.before
			request := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25,
				AsOf: asOf, Limit: 500}
			initial, err := f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: request, Actor: "operator"})
			require.NoError(err)
			require.Len(initial.People, 1)

			request.Config.Tier1Lanes = tc.after
			page, err := f.Store.PlanPersonTiersContext(t.Context(), request)
			require.NoError(err)
			require.Len(page.People, 1)
			assert.Equal(tc.eligible, page.People[0].Decision.EnrichmentEligible)
			assert.Equal(tc.laneState, page.People[0].Decision.Lanes["enrichment"])
		})
	}
}

func TestPersonTierChangesOnlyFindsPersistedEligibilityCorrection(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedTierSignalWindow(t, f, personID, asOf, "enrichment-eligibility-correction")
	req := store.PersonTierPageRequest{Config: persontiers.DefaultConfig(), Timezone: "UTC", MaxDirect: 25, AsOf: asOf, Limit: 500}
	initial, err := f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.Len(initial.People, 1)
	assert.True(initial.People[0].Decision.EnrichmentEligible)

	// Simulate the stale eligibility value left by an earlier apply under an
	// enrichment opt-out. The policy, archive signals, and applied lanes stay fixed.
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`UPDATE person_tier_state SET enrichment_eligible=FALSE WHERE person_id=?`), personID)
	require.NoError(err)
	req.ChangesOnly = true
	page, err := f.Store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	assert.True(page.People[0].Decision.EnrichmentEligible)
	assert.Equal("eligible", page.People[0].Decision.Lanes["enrichment"])
}

func TestPersonTierChangesOnlyUsesEffectiveManualAuthority(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	asOf := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	seedTierSignalWindow(t, f, personID, asOf, "manual-enrichment-authority")
	_, err := f.Store.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(err)
	cfg := persontiers.DefaultConfig()
	cfg.Tier1Lanes = []string{"tracking"}
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: asOf, Limit: 500}
	initial, err := f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.Len(initial.People, 1)
	assert.True(initial.People[0].Decision.EnrichmentEligible)

	var appliedLaneEligible bool
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT enrichment_eligible FROM person_tier_state WHERE person_id=?`), personID).Scan(&appliedLaneEligible))
	scheduledEligible, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.Store, personID)
	require.NoError(err)
	assert.False(appliedLaneEligible, "the enrichment lane was not applied")
	assert.True(scheduledEligible, "manual tracking grants current scheduled authority")

	req.ChangesOnly = true
	page, err := f.Store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	assert.Empty(page.People, "current manual authority already matches the proposed preview")
}

func applySyntheticTierOne(t *testing.T, s *store.Store, personID int64, request store.PersonTierPageRequest) {
	t.Helper()
	_, err := s.SetPersonTrackingContext(t.Context(), personID, true)
	require.NoError(t, err)
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(
		`UPDATE person_tracking SET provenance='auto' WHERE person_id=?`), personID)
	require.NoError(t, err)
	run, err := s.StartPersonTierRunContext(t.Context(), request, "manual", "apply")
	require.NoError(t, err)
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *run, store.PersonTierInput{
		PersonID: personID,
		Signals:  persontiers.Signals{OutboundShort: 2, InboundShort: 2, ActiveDaysShort: 6},
	}))
}

func TestPersonTierReadsRejectMismatchedProjection(t *testing.T) {
	for _, setting := range []string{"timezone", "direct_limit"} {
		t.Run(setting, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, personID, req := tierProjectionRunFixture(t)
			completeTierProjectionSettings(t, s, "UTC", 25)
			if setting == "timezone" {
				req.Timezone = "America/Los_Angeles"
			} else {
				req.MaxDirect = 1
			}
			before := tierProjectionRowCounts(t, s)
			_, err := s.PlanPersonTiersContext(t.Context(), req)
			require.ErrorIs(err, store.ErrInvalidPersonTierRequest)
			_, err = s.ExplainPersonTierContext(t.Context(), personID, req)
			require.ErrorIs(err, store.ErrInvalidPersonTierRequest)
			assert.Equal(before, tierProjectionRowCounts(t, s))
		})
	}
}

func tierProjectionRowCounts(t *testing.T, s *store.Store) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range []string{"person_tier_state", "person_tier_observations", "person_tier_events", "person_tracking", "person_brief_enrollments"} {
		var count int
		require.NoError(t, s.DB().QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&count))
		counts[table] = count
	}
	return counts
}
