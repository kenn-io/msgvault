package store_test

import (
	"database/sql"
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

func seedTierSignalWindow(t *testing.T, f *storetest.Fixture, personID int64, asOf time.Time, prefix string) {
	t.Helper()
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	var counterpart int64
	require.NoError(t, f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT participant_id FROM person_participants WHERE person_id=?`), personID).Scan(&counterpart))
	for i := range 6 {
		from, to := owner, counterpart
		if i%2 == 1 {
			from, to = counterpart, owner
		}
		message := f.NewMessage().WithSourceMessageID(fmt.Sprintf("%s-%d", prefix, i)).WithSentAt(asOf.AddDate(0, 0, -i)).Build()
		message.SenderID = sql.NullInt64{Int64: from, Valid: true}
		messageID, err := f.Store.UpsertMessage(message)
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "from", []int64{from}, nil))
		require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "to", []int64{to}, nil))
	}
	projectTierSignals(t, f, "UTC", 25)
}

func TestPersonTierPreviewWritesNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, id, req := tierRunFixture(t)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal(id, page.People[0].PersonID)
	assert.Equal(2, page.People[0].Decision.ComputedTier)
	for _, table := range []string{"person_tier_state", "person_tier_observations", "person_tier_events", "person_tracking", "person_brief_enrollments"} {
		var n int
		require.NoError(s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		assert.Zero(n, table)
	}
}

func TestPersonTierApplyPreservesManualIntent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, id, req := tierRunFixture(t)
	page, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.Len(page.People, 1)
	var tier int
	var provenance, grantActor string
	var eligible bool
	require.NoError(s.DB().QueryRow(s.Rebind("SELECT tier,enrichment_eligible FROM person_tier_state WHERE person_id=?"), id).Scan(&tier, &eligible))
	assert.Equal(2, tier)
	assert.False(eligible)
	require.NoError(s.DB().QueryRow(s.Rebind("SELECT provenance,actor FROM person_tracking WHERE person_id=?"), id).Scan(&provenance, &grantActor))
	assert.Equal("auto", provenance)
	assert.Equal("people-tiers", grantActor)
	_, err = s.SetPersonTrackingContext(t.Context(), id, false)
	require.NoError(err)
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	tracked, err := s.GetPersonTrackingContext(t.Context(), id)
	require.NoError(err)
	assert.False(tracked.Tracked)
}

func TestPersonTierRunFencesAndRetainedTierThree(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, id := newTierSignalsFixture(t)
	projectTierSignals(t, f, "UTC", 25)
	s := f.Store
	cfg := persontiers.DefaultConfig()
	cfg.MinimumTier = 2
	req := store.PersonTierPageRequest{Config: cfg, Timezone: "UTC", MaxDirect: 25, AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500}
	old, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
	require.NoError(err)
	newer, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	require.ErrorIs(s.ApplyPersonTierDecisionContext(t.Context(), *old, page.People[0]), store.ErrPersonTierRunSuperseded)
	require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *newer, page.People[0]))
	req.Config.MinimumTier = 3
	req.Config.DemotionGraceDays = 0
	next, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	input := page.People[0]
	input.Signals = persontiers.Signals{}
	require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *next, input))
	var tier int
	require.NoError(s.DB().QueryRow(s.Rebind("SELECT tier FROM person_tier_state WHERE person_id=?"), id).Scan(&tier))
	assert.Equal(3, tier)
	req.Config.MinimumTier = 2
	promoted, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *promoted, input))
	events, err := s.PersonTierHistoryContext(t.Context(), id, 5, 0)
	require.NoError(err)
	require.NotEmpty(events)
	assert.Equal("config_change", events[0].Reason)
	require.ErrorIs(s.ApplyPersonTierDecisionContext(t.Context(), *newer, page.People[0]), store.ErrPersonTierRunSuperseded)
	reset, err := s.ResetPersonTiersContext(t.Context(), "operator")
	require.NoError(err)
	assert.Equal(1, reset.Cleared)
	require.ErrorIs(s.ApplyPersonTierDecisionContext(t.Context(), *next, input), store.ErrPersonTierRunSuperseded)
	var n int
	require.NoError(s.DB().QueryRow("SELECT COUNT(*) FROM person_tier_state").Scan(&n))
	assert.Zero(n)
}

func TestPersonTierRecordOnlyDoesNotSeedAppliedHysteresis(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _, req := tierRunFixture(t)
	run, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "record_only")
	require.NoError(err)
	_, err = s.ProcessPersonTierRunPageContext(t.Context(), *run, 500, "people-tiers")
	require.NoError(err)
	var n int
	require.NoError(s.DB().QueryRow("SELECT COUNT(*) FROM person_tier_observations").Scan(&n))
	assert.Equal(1, n)
	require.NoError(s.DB().QueryRow("SELECT COUNT(*) FROM person_tier_state").Scan(&n))
	assert.Zero(n)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	assert.Equal("promotion", page.People[0].Decision.Reason)
}

func TestPersonTierFrozenRunContinuesAfterConfigChange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _, req := tierRunFixture(t)
	p, _, err := s.CreatePersonFromParticipant(func() int64 {
		f := s
		participant, err := f.EnsureParticipant("second-tier@example.org", "Second Synthetic Contact", "example.org")
		require.NoError(err)
		return participant
	}())
	require.NoError(err)
	require.Positive(p.ID)
	req.Limit = 1
	first, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.NotEmpty(first.NextCursor)
	req.Config.MinimumTier = 3
	req.Cursor = first.NextCursor
	second, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, RunID: first.RunID, Actor: "operator"})
	require.NoError(err)
	require.Len(second.People, 1)
	assert.Equal(2, second.People[0].Decision.ComputedTier)
	assert.Empty(second.NextCursor)
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, RunID: first.RunID, Actor: "operator"})
	require.ErrorIs(err, store.ErrPersonTierRunSuperseded)
}

func TestPersonTierApplyRunPreservesPopulationFilters(t *testing.T) {
	t.Run("tier", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		s, id, req := tierRunFixture(t)
		req.Tier = 1
		preview, err := s.PlanPersonTiersContext(t.Context(), req)
		require.NoError(err)
		require.Empty(preview.People)
		run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
		require.NoError(err)
		frozen, err := s.PersonTierRunContext(t.Context(), "manual")
		require.NoError(err)
		require.NotNil(frozen)
		require.Equal(run.Tier, frozen.Tier)
		require.Equal(req.Tier, frozen.Tier)
		req.Tier = 2

		applied, err := s.ProcessPersonTierRunPageContext(t.Context(), *frozen, req.Limit, "operator")
		require.NoError(err)
		assert.Empty(applied.People)
		var count int
		require.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM person_tier_state WHERE person_id=?`), id).Scan(&count))
		assert.Zero(count)
	})

	t.Run("changes_only", func(t *testing.T) {
		assert := assert.New(t)
		require := require.New(t)
		s, id, req := tierRunFixture(t)
		first, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
		require.NoError(err)
		require.Len(first.People, 1)
		var priorRunID string
		require.NoError(s.DB().QueryRow(s.Rebind(`SELECT run_id FROM person_tier_state WHERE person_id=?`), id).Scan(&priorRunID))

		req.ChangesOnly = true
		preview, err := s.PlanPersonTiersContext(t.Context(), req)
		require.NoError(err)
		require.Empty(preview.People)
		run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
		require.NoError(err)
		frozen, err := s.PersonTierRunContext(t.Context(), "manual")
		require.NoError(err)
		require.NotNil(frozen)
		require.Equal(run.ChangesOnly, frozen.ChangesOnly)
		require.True(frozen.ChangesOnly)
		req.ChangesOnly = false
		applied, err := s.ProcessPersonTierRunPageContext(t.Context(), *frozen, req.Limit, "operator")
		require.NoError(err)
		assert.Empty(applied.People)
		var afterRunID string
		require.NoError(s.DB().QueryRow(s.Rebind(`SELECT run_id FROM person_tier_state WHERE person_id=?`), id).Scan(&afterRunID))
		assert.Equal(priorRunID, afterRunID)
	})
}

func TestPersonTierChangesOnlyPersistsHysteresisRecovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, personID := newTierSignalsFixture(t)
	cfg := persontiers.DefaultConfig()
	cfg.DemotionGraceDays = 30
	req := store.PersonTierPageRequest{
		Config: cfg, Timezone: "UTC", MaxDirect: 25,
		AsOf: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), Limit: 500,
	}
	seedTierSignalWindow(t, f, personID, req.AsOf, "changes-only-initial")
	initial, err := f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.Len(initial.People, 1)
	assert.Equal(1, initial.People[0].Decision.Tier)

	req.AsOf = time.Date(2027, 1, 20, 12, 0, 0, 0, time.UTC)
	decline, err := f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.Len(decline.People, 1)
	assert.Equal(2, decline.People[0].Decision.CandidateTier)
	assert.Equal("2027-01-20", decline.People[0].Decision.CandidateSince)

	recoveryAt := time.Date(2027, 1, 25, 12, 0, 0, 0, time.UTC)
	seedTierSignalWindow(t, f, personID, recoveryAt, "changes-only-recovery")
	req.AsOf = recoveryAt
	req.ChangesOnly = true
	recovery, err := f.Store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(recovery.People, 1)
	assert.Zero(recovery.People[0].Decision.CandidateTier)
	assert.Empty(recovery.People[0].Decision.CandidateSince)
	_, err = f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)

	req.AsOf = time.Date(2027, 5, 1, 12, 0, 0, 0, time.UTC)
	continuedDecline, err := f.Store.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(continuedDecline.People, 1)
	assert.Equal(1, continuedDecline.People[0].Decision.Tier)
	assert.Empty(continuedDecline.People[0].Decision.Reason)
	assert.Equal(2, continuedDecline.People[0].Decision.CandidateTier)
	assert.Equal("2027-05-01", continuedDecline.People[0].Decision.CandidateSince)

	_, err = f.Store.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	var tier, candidateTier int
	var candidateSince sql.NullString
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(
		`SELECT tier,candidate_tier,candidate_since FROM person_tier_state WHERE person_id=?`), personID).Scan(&tier, &candidateTier, &candidateSince))
	assert.Equal(1, tier)
	assert.Equal(2, candidateTier)
	require.True(candidateSince.Valid)
	assert.Equal("2027-05-01", candidateSince.String)
}

func TestPersonTierChangesOnlyPersistsPolicyState(t *testing.T) {
	for _, change := range []string{"config", "rule_version"} {
		t.Run(change, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, personID, req := tierRunFixture(t)
			_, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
			require.NoError(err)
			if change == "config" {
				req.Config.DemotionGraceDays++
			} else {
				_, err = s.DB().ExecContext(t.Context(), s.Rebind(
					`UPDATE person_tier_state SET rule_version=0 WHERE person_id=?`), personID)
				require.NoError(err)
			}
			req.ChangesOnly = true
			page, err := s.PlanPersonTiersContext(t.Context(), req)
			require.NoError(err)
			require.Len(page.People, 1)
			_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
			require.NoError(err)
			var fingerprint string
			var ruleVersion int
			require.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(
				`SELECT config_fingerprint,rule_version FROM person_tier_state WHERE person_id=?`), personID).Scan(&fingerprint, &ruleVersion))
			assert.Equal(req.Config.Fingerprint(), fingerprint)
			assert.Equal(1, ruleVersion)
		})
	}
}

func TestPersonTierApplyRechecksManualStateAndReplaysSafely(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, id, req := tierRunFixture(t)
	run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	_, err = s.SetPersonTrackingActorContext(t.Context(), id, true, "operator")
	require.NoError(err)
	require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
	tracking, err := s.GetPersonTrackingContext(t.Context(), id)
	require.NoError(err)
	assert.Equal("manual", tracking.Provenance)
	assert.Equal("operator", tracking.Actor)
	events, err := s.PersonTierHistoryContext(t.Context(), id, 20, 0)
	require.NoError(err)
	require.Len(events, 1)
	assert.Equal(2, events[0].ToTier)
	assert.Equal(3, *events[0].FromTier)
}

func TestPersonTierInvalidApplyDoesNotSupersedeRun(t *testing.T) {
	require := require.New(t)
	s, _, req := tierRunFixture(t)
	run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
	require.NoError(err)
	req.Limit = 501
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.ErrorIs(err, store.ErrInvalidPersonTierRequest)
	req.Limit = 500
	page, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(page.People, 1)
	require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
}

func TestPersonTierDeletionClearsFailureMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, id, _ := tierRunFixture(t)
	person, err := s.GetPersonContext(t.Context(), id)
	require.NoError(err)
	key := fmt.Sprintf("people_tiers_failure:%d", id)
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(`INSERT INTO archive_metadata(key,value) VALUES(?,?)`), key, `{"run_id":"failed-run","count":2}`)
	require.NoError(err)
	require.NoError(s.DeletePersonContext(t.Context(), id, person.Revision))
	var count int
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT COUNT(*) FROM archive_metadata WHERE key=?`), key).Scan(&count))
	assert.Zero(count)
}

func TestPersonTierFailedApplyCountsOnlyCommittedChanges(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _, req := tierRunFixture(t)
	if s.IsPostgreSQL() {
		t.Skip("SQLite abort trigger exercises the same transactional apply path")
	}
	_, err := s.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_tier_state BEFORE INSERT ON person_tier_state BEGIN SELECT RAISE(ABORT,'synthetic tier apply failure'); END`)
	require.NoError(err)
	page, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	assert.Equal(1, page.Counts["failed"])
	assert.Zero(page.Counts["created"])
	var count int
	require.NoError(s.DB().QueryRow(`SELECT COUNT(*) FROM person_tracking`).Scan(&count))
	assert.Zero(count)
}

func TestPersonTierSinceUsesFrozenLocalDate(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, id, req := tierRunFixture(t)
	req.Timezone = "Asia/Tokyo"
	first, err := s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	require.Len(first.People, 1)
	second, err := s.PlanPersonTiersContext(t.Context(), req)
	require.NoError(err)
	require.Len(second.People, 1)
	assert.Equal("2026-10-17", second.People[0].Prior.TierSince)
	_, err = s.ApplyPersonTiersPageContext(t.Context(), store.PersonTierApplyRequest{PersonTierPageRequest: req, Actor: "operator"})
	require.NoError(err)
	var since time.Time
	require.NoError(s.DB().QueryRow(s.Rebind(`SELECT tier_since FROM person_tier_state WHERE person_id=?`), id).Scan(&since))
	loc, err := time.LoadLocation("Asia/Tokyo")
	require.NoError(err)
	assert.Equal("2026-10-17", since.In(loc).Format(time.DateOnly))
}

func TestPersonTierFrozenSnapshotCannotBeChangedByContinuation(t *testing.T) {
	for _, field := range []string{"config", "as_of", "mode", "started_at", "tier", "changes_only"} {
		t.Run(field, func(t *testing.T) {
			require := require.New(t)
			s, _, req := tierRunFixture(t)
			run, err := s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
			require.NoError(err)
			page, err := s.PlanPersonTiersContext(t.Context(), req)
			require.NoError(err)
			require.Len(page.People, 1)
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
			case "tier":
				altered.Tier = 1
			case "changes_only":
				altered.ChangesOnly = true
			}
			require.ErrorIs(s.ApplyPersonTierDecisionContext(t.Context(), altered, page.People[0]), store.ErrPersonTierRunSuperseded)
			require.NoError(s.ApplyPersonTierDecisionContext(t.Context(), *run, page.People[0]))
		})
	}
}

func TestPersonTierResetStopsPendingScheduledContinuation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	s, _, req := tierRunFixture(t)
	scheduled, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
	require.NoError(err)
	_, err = s.ResetPersonTiersContext(t.Context(), "operator")
	require.NoError(err)
	stale, err := s.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	require.NotNil(stale, "retain the stale run until its pending continuation rejects it")
	assert.Equal(scheduled.RunID, stale.RunID)
	_, err = s.ProcessPersonTierRunPageContext(t.Context(), *stale, 500, "people-tiers")
	require.ErrorIs(err, store.ErrPersonTierRunSuperseded)
	cleared, err := s.PersonTierRunContext(t.Context(), "scheduled")
	require.NoError(err)
	assert.Nil(cleared)
	for _, table := range []string{"person_tier_state", "person_tracking", "person_tier_observations"} {
		var n int
		require.NoError(s.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
		assert.Zero(n, table)
	}
}

func TestPersonTierSupersededRunReportsCleanupFailure(t *testing.T) {
	for _, phase := range []string{"page_entry", "page_commit"} {
		t.Run(phase, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			s, _, req := tierRunFixture(t)
			if s.IsPostgreSQL() {
				t.Skip("SQLite abort triggers exercise the shared cleanup error path")
			}
			scheduled, err := s.StartPersonTierRunContext(t.Context(), req, "scheduled", "apply")
			require.NoError(err)
			if phase == "page_entry" {
				_, err = s.StartPersonTierRunContext(t.Context(), req, "manual", "apply")
				require.NoError(err)
			} else {
				// Force the same committed state as a newer manual start between the
				// person transaction and the page's progress transaction.
				_, err = s.DB().ExecContext(t.Context(), `CREATE TRIGGER supersede_completed_tier_person AFTER INSERT ON person_tier_observations BEGIN
     INSERT INTO archive_metadata(key,value) VALUES('people_tiers_apply_fence','2099-01-01T00:00:00Z')
     ON CONFLICT(key) DO UPDATE SET value=excluded.value;
    END`)
				require.NoError(err)
			}
			_, err = s.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_scheduled_tier_cleanup BEFORE DELETE ON archive_metadata
    WHEN OLD.key='people_tiers_run' BEGIN SELECT RAISE(ABORT,'synthetic scheduled cleanup failure'); END`)
			require.NoError(err)
			_, err = s.ProcessPersonTierRunPageContext(t.Context(), *scheduled, 500, "people-tiers")
			require.ErrorContains(err, "synthetic scheduled cleanup failure")
			assert.NotErrorIs(err, store.ErrPersonTierRunSuperseded, "cleanup failure must not become a successful scheduler deferral")
			stale, err := s.PersonTierRunContext(t.Context(), "scheduled")
			require.NoError(err)
			require.NotNil(stale)
			_, err = s.DB().ExecContext(t.Context(), `DROP TRIGGER reject_scheduled_tier_cleanup`)
			require.NoError(err)
			_, err = s.ProcessPersonTierRunPageContext(t.Context(), *scheduled, 500, "people-tiers")
			require.ErrorIs(err, store.ErrPersonTierRunSuperseded)
			cleared, err := s.PersonTierRunContext(t.Context(), "scheduled")
			require.NoError(err)
			assert.Nil(cleared)
		})
	}
}
