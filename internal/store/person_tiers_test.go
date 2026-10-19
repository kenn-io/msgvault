package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func tierRunFixture(t *testing.T) (*store.Store, int64, store.PersonTierPageRequest) {
	t.Helper()
	f := storetest.New(t)
	p, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("run-tier@example.org", "Synthetic Run", "example.org"))
	require.NoError(t, err)
	c := persontiers.DefaultConfig()
	c.MinimumTier = 2
	return f.Store, p.ID, store.PersonTierPageRequest{Config: c, Timezone: "UTC", MaxDirect: 25, AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500}
}

func TestPersonTierPreviewWritesNothing(t *testing.T) {
	s, id, req := tierRunFixture(t)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	assert.Equal(t, id, page.People[0].PersonID)
	assert.Equal(t, 2, page.People[0].Decision.ComputedTier)
	for _, table := range []string{"person_tier_state", "person_tier_observations", "person_tier_events", "person_tracking", "person_brief_enrollments"} {
		var n int
		require.NoError(t, s.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		assert.Zero(t, n, table)
	}
}

func TestPersonTierApplyPreservesManualIntent(t *testing.T) {
	s, id, req := tierRunFixture(t)
	page, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	var tier int
	var provenance, grantActor string
	var eligible bool
	require.NoError(t, s.DB().QueryRow(s.Rebind("SELECT tier,enrichment_eligible FROM person_tier_state WHERE person_id=?"), id).Scan(&tier, &eligible))
	assert.Equal(t, 2, tier)
	assert.False(t, eligible)
	require.NoError(t, s.DB().QueryRow(s.Rebind("SELECT provenance,actor FROM person_tracking WHERE person_id=?"), id).Scan(&provenance, &grantActor))
	assert.Equal(t, "auto", provenance)
	assert.Equal(t, "people-tiers", grantActor)
	_, err = s.SetPersonTrackingContext(t.Context(), id, false)
	require.NoError(t, err)
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	tracked, err := s.GetPersonTrackingContext(t.Context(), id)
	require.NoError(t, err)
	assert.False(t, tracked.Tracked)
}

func TestPersonTierRunFencesAndRetainedTierThree(t *testing.T) {
	s, id, req := tierRunFixture(t)
	old, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
	require.NoError(t, err)
	newer, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	require.ErrorIs(t, s.ApplyPersonTierDecisionContext(t.Context(), *old, page.People[0]), store.ErrPersonTierRunSuperseded)
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *newer, page.People[0]))
	req.Config.MinimumTier = 3
	req.Config.DemotionGraceDays = 0
	next, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	input := page.People[0]
	input.Readiness = store.PersonTierReadiness{Ready: true}
	input.Signals = persontiers.Signals{}
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *next, input))
	var tier int
	require.NoError(t, s.DB().QueryRow(s.Rebind("SELECT tier FROM person_tier_state WHERE person_id=?"), id).Scan(&tier))
	assert.Equal(t, 3, tier)
	req.Config.MinimumTier = 2
	promoted, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *promoted, input))
	events, err := s.PersonTierHistoryContext(t.Context(), id, 5, 0)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Equal(t, "config_change", events[0].Reason)
	require.ErrorIs(t, s.ApplyPersonTierDecisionContext(t.Context(), *newer, page.People[0]), store.ErrPersonTierRunSuperseded)
	reset, err := s.ResetPersonTiersContext(t.Context(), "operator")
	require.NoError(t, err)
	assert.Equal(t, 1, reset.Cleared)
	require.ErrorIs(t, s.ApplyPersonTierDecisionContext(t.Context(), *next, input), store.ErrPersonTierRunSuperseded)
	var n int
	require.NoError(t, s.DB().QueryRow("SELECT COUNT(*) FROM person_tier_state").Scan(&n))
	assert.Zero(t, n)
}

func TestScheduledPersonTierRunPersistsEachCommittedCheckpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := storetest.New(t)
	first, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("first-checkpoint@example.org", "First Checkpoint", "example.org"))
	require.NoError(err)
	second, _, err := f.Store.CreatePersonFromParticipant(f.EnsureParticipant("second-checkpoint@example.org", "Second Checkpoint", "example.org"))
	require.NoError(err)
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	request := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500}
	run, err := f.Store.StartPersonTierRunContext(t.Context(), request, "scheduled", "apply")
	require.NoError(err)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	checkpoints := 0
	_, err = f.Store.ProcessPersonTierRunPageWithCheckpointContext(ctx, *run, 500, "people-tiers", func() {
		checkpoints++
		cancel()
	})
	require.ErrorIs(err, context.Canceled)
	assert.Equal(1, checkpoints)

	resumed, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	require.NotNil(resumed)
	assert.NotEmpty(resumed.Cursor)
	request.Cursor = resumed.Cursor
	remaining, err := f.Store.PlanPersonTiersContext(t.Context(), request)
	require.NoError(err)
	require.Len(remaining.People, 1)
	assert.Equal(second.ID, remaining.People[0].PersonID)
	assert.NotEqual(first.ID, remaining.People[0].PersonID)

	firstTracking, err := f.Store.GetPersonTrackingContext(t.Context(), first.ID)
	require.NoError(err)
	assert.True(firstTracking.Tracked)
	secondTracking, err := f.Store.GetPersonTrackingContext(t.Context(), second.ID)
	require.NoError(err)
	assert.False(secondTracking.Tracked)

	_, err = f.Store.ProcessPersonTierRunPageContext(t.Context(), *resumed, 500, "people-tiers")
	require.NoError(err)
	secondTracking, err = f.Store.GetPersonTrackingContext(t.Context(), second.ID)
	require.NoError(err)
	assert.True(secondTracking.Tracked)
	completed, err := f.Store.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(completed)
}

func TestPersonTierRecordOnlyDoesNotSeedAppliedHysteresis(t *testing.T) {
	s, _, req := tierRunFixture(t)
	run, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "record_only")
	require.NoError(t, err)
	_, err = s.ProcessPersonTierRunPageContext(t.Context(), *run, 500, "people-tiers")
	require.NoError(t, err)
	var n int
	require.NoError(t, s.DB().QueryRow("SELECT COUNT(*) FROM person_tier_observations").Scan(&n))
	assert.Equal(t, 1, n)
	require.NoError(t, s.DB().QueryRow("SELECT COUNT(*) FROM person_tier_state").Scan(&n))
	assert.Zero(t, n)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	assert.Equal(t, "promotion", page.People[0].Decision.Reason)
}

func TestPersonTierFrozenRunContinuesAfterConfigChange(t *testing.T) {
	s, _, req := tierRunFixture(t)
	p, _, err := s.CreatePersonFromParticipant(func() int64 {
		f := s
		participant, err := f.EnsureParticipant("second-tier@example.org", "Second Synthetic Contact", "example.org")
		require.NoError(t, err)
		return participant
	}())
	require.NoError(t, err)
	require.Positive(t, p.ID)
	req.Limit = 1
	first, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	require.NotEmpty(t, first.NextCursor)
	req.Config.MinimumTier = 3
	req.Cursor = first.NextCursor
	second, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, RunID: first.RunID, Actor: "operator"})
	require.NoError(t, err)
	require.Len(t, second.People, 1)
	assert.Equal(t, 2, second.People[0].Decision.ComputedTier)
	assert.Empty(t, second.NextCursor)
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, RunID: first.RunID, Actor: "operator"})
	require.ErrorIs(t, err, store.ErrPersonTierRunSuperseded)
}

func TestPersonTierApplyRechecksManualStateAndReplaysSafely(t *testing.T) {
	s, id, req := tierRunFixture(t)
	run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	_, err = s.SetPersonTrackingActorContext(t.Context(), id, true, "operator")
	require.NoError(t, err)
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	tracking, err := s.GetPersonTrackingContext(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "manual", tracking.Provenance)
	assert.Equal(t, "operator", tracking.Actor)
	events, err := s.PersonTierHistoryContext(t.Context(), id, 20, 0)
	require.NoError(t, err)
	require.Len(t, events, 1)
	assert.Equal(t, 2, events[0].ToTier)
	assert.Equal(t, 3, *events[0].FromTier)
}

func TestPersonTierInvalidApplyDoesNotSupersedeRun(t *testing.T) {
	s, _, req := tierRunFixture(t)
	run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(t, err)
	req.Limit = 501
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.ErrorIs(t, err, store.ErrInvalidPersonTierRequest)
	req.Limit = 500
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, page.People, 1)
	require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
}

func TestPersonTierDeletionClearsFailureMetadata(t *testing.T) {
	s, id, _ := tierRunFixture(t)
	person, err := s.GetPersonContext(t.Context(), id)
	require.NoError(t, err)
	key := fmt.Sprintf("people_tiers_failure:%d", id)
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(`INSERT INTO archive_metadata(key,value) VALUES(?,?)`), key, `{"run_id":"failed-run","count":2}`)
	require.NoError(t, err)
	require.NoError(t, s.DeletePersonContext(t.Context(), id, person.Revision))
	var count int
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key=?`), key).Scan(&count))
	assert.Zero(t, count)
}

func TestPersonTierFailedApplyCountsOnlyCommittedChanges(t *testing.T) {
	s, _, req := tierRunFixture(t)
	if s.IsPostgreSQL() {
		t.Skip("SQLite abort trigger exercises the same transactional apply path")
	}
	_, err := s.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_tier_state BEFORE INSERT ON person_tier_state BEGIN SELECT RAISE(ABORT,'synthetic tier apply failure'); END`)
	require.NoError(t, err)
	page, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	assert.Equal(t, 1, page.Counts["failed"])
	assert.Zero(t, page.Counts["created"])
	var count int
	require.NoError(t, s.DB().QueryRow(`SELECT COUNT(*) FROM person_tracking`).Scan(&count))
	assert.Zero(t, count)
}

func TestPersonTierSinceUsesFrozenLocalDate(t *testing.T) {
	s, id, req := tierRunFixture(t)
	req.Timezone = "Asia/Tokyo"
	first, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	require.Len(t, first.People, 1)
	second, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(t, err)
	require.Len(t, second.People, 1)
	assert.Equal(t, "2026-10-17", second.People[0].Prior.TierSince)
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(t, err)
	var since time.Time
	require.NoError(t, s.DB().QueryRow(s.Rebind(`SELECT tier_since FROM person_tier_state WHERE person_id=?`), id).Scan(&since))
	loc, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(t, err)
	assert.Equal(t, "2026-10-17", since.In(loc).Format(time.DateOnly))
}

func TestPersonTierFrozenSnapshotCannotBeChangedByContinuation(t *testing.T) {
	for _, field := range []string{"config", "as_of", "mode", "started_at"} {
		t.Run(field, func(t *testing.T) {
			s, _, req := tierRunFixture(t)
			run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
			require.NoError(t, err)
			page, err := s.PlanPersonTiersContext(t.Context(), req)
			require.NoError(t, err)
			require.Len(t, page.People, 1)
			altered := *run
			switch field {
			case "config":
				altered.Config.MinimumTier = 3
				altered.ConfigFingerprint = altered.Config.Fingerprint()
			case "as_of":
				altered.AsOf = altered.AsOf.AddDate(0, 0, 1)
			case "mode":
				altered.Mode = "record_only"
			case "started_at":
				altered.StartedAt = altered.StartedAt.Add(time.Second)
			}
			require.ErrorIs(t, s.ApplyPersonTierDecisionContext(t.Context(), altered, page.People[0]), store.ErrPersonTierRunSuperseded)
			require.NoError(t, s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
		})
	}
}

func TestPersonTierResetStopsPendingScheduledContinuation(t *testing.T) {
	s, _, req := tierRunFixture(t)
	scheduled, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
	require.NoError(t, err)
	_, err = s.ResetPersonTiersContext(t.Context(), "operator")
	require.NoError(t, err)
	stale, err := s.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(t, err)
	require.NotNil(t, stale, "retain the stale run until its pending continuation rejects it")
	assert.Equal(t, scheduled.RunID, stale.RunID)
	_, err = s.ProcessPersonTierRunPageContext(t.Context(), *stale, 500, "people-tiers")
	require.ErrorIs(t, err, store.ErrPersonTierRunSuperseded)
	cleared, err := s.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(t, err)
	assert.Nil(t, cleared)
	for _, table := range []string{"person_tier_state", "person_tracking", "person_tier_observations"} {
		var n int
		require.NoError(t, s.DB().QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n))
		assert.Zero(t, n, table)
	}
}

func TestPersonTierSupersededRunReportsCleanupFailure(t *testing.T) {
	for _, phase := range []string{"page_entry", "page_commit"} {
		t.Run(phase, func(t *testing.T) {
			s, _, req := tierRunFixture(t)
			if s.IsPostgreSQL() {
				t.Skip("SQLite abort triggers exercise the shared cleanup error path")
			}
			scheduled, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
			require.NoError(t, err)
			if phase == "page_entry" {
				_, err = s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
				require.NoError(t, err)
			} else {
				// Force the same committed state as a newer manual start between the
				// person transaction and the page's progress transaction.
				_, err = s.DB().ExecContext(t.Context(), `CREATE TRIGGER supersede_completed_tier_person AFTER INSERT ON person_tier_observations BEGIN
     INSERT INTO archive_metadata(key,value) VALUES('people_tiers_apply_fence','2099-01-01T00:00:00Z')
     ON CONFLICT(key) DO UPDATE SET value=excluded.value;
    END`)
				require.NoError(t, err)
			}
			_, err = s.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_scheduled_tier_cleanup BEFORE DELETE ON archive_metadata
    WHEN OLD.key='people_tiers_run' BEGIN SELECT RAISE(ABORT,'synthetic scheduled cleanup failure'); END`)
			require.NoError(t, err)
			_, err = s.ProcessPersonTierRunPageContext(t.Context(), *scheduled, 500, "people-tiers")
			require.ErrorContains(t, err, "synthetic scheduled cleanup failure")
			assert.NotErrorIs(t, err, store.ErrPersonTierRunSuperseded, "cleanup failure must not become a successful scheduler deferral")
			stale, err := s.PersonTierRunContext(t.Context(), "scheduled")
			require.NoError(t, err)
			require.NotNil(t, stale)
			_, err = s.DB().ExecContext(t.Context(), `DROP TRIGGER reject_scheduled_tier_cleanup`)
			require.NoError(t, err)
			_, err = s.ProcessPersonTierRunPageContext(t.Context(), *scheduled, 500, "people-tiers")
			require.ErrorIs(t, err, store.ErrPersonTierRunSuperseded)
			cleared, err := s.PersonTierRunContext(t.Context(), "scheduled")
			require.NoError(t, err)
			assert.Nil(t, cleared)
		})
	}
}
