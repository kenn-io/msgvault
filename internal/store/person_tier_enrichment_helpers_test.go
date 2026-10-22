package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTierEnrichmentPersistedEligibility(t *testing.T) {
	for _, p := range tierEnrichmentPersonas {
		t.Run(p.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTierEnrichmentFixture(t)
			setTierEnrichmentPersona(t, f, p)
			got, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
			require.NoError(err)
			assert.Equal(p.want, got)
		})
	}
}

func TestTierEnrichmentOpenManualRunReadsTargetsForThePerson(t *testing.T) {
	for _, mismatch := range []string{"none", "person", "profile", "run"} {
		t.Run(mismatch, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTierEnrichmentFixture(t)
			run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "open-exact", f.now)
			require.NoError(err)
			switch mismatch {
			case "person":
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_manual_run_targets SET person_id=person_id+1000 WHERE run_id=?`), run.ID)
			case "profile":
				profile := tierEnrichmentProfile(t, "other", "https://other.example.test/search")
				_, err = f.store.EnsurePersonEnrichmentProfile(t.Context(), profile)
				require.NoError(err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_manual_run_targets SET profile_fingerprint=? WHERE run_id=?`), profile.Fingerprint, run.ID)
			case "run":
				other, _, otherErr := f.store.StartRun(t.Context(), personenrichment.RunStart{Kind: "manual", RequestedBy: "unbound", RequestedAt: f.now})
				require.NoError(otherErr)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_work SET run_id=? WHERE person_id=?`), other.ID, f.person.ID)
			}
			require.NoError(err)
			id, err := store.OpenManualTierEnrichmentForTest(t.Context(), f.store, f.person.ID)
			require.NoError(err)
			if mismatch == "person" {
				assert.Empty(id)
			} else {
				assert.Equal(strconv.FormatInt(run.ID, 10), id)
			}
		})
	}
}

func TestTierEnrichmentCancellationPreservesOnlyExactManualWork(t *testing.T) {
	for _, manual := range []bool{false, true} {
		for _, state := range []string{"queued", "starting", "pending", "retry_wait", "uncertain_start"} {
			label := state + "/scheduled"
			if manual {
				label = state + "/exact-manual"
			}
			t.Run(label, func(t *testing.T) {
				require := require.New(t)
				assert := assert.New(t)
				f := newTierEnrichmentCappedFixture(t)
				var runID int64
				if manual {
					run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "cancel-exact", f.now)
					require.NoError(err)
					runID = run.ID
				} else {
					runID = f.startRun(t, "cancel-scheduled").ID
					f.enqueue(t)
				}
				lease := f.claim(t, runID, "worker")
				start := tierEnrichmentAttemptStart(&f, runID, "a")
				start.Trigger = lease.Trigger
				start.HardCostCap = true
				start.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 75}
				attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
				require.NoError(err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_attempts SET state=? WHERE id=?`), state, attempt.ID)
				require.NoError(err)
				if manual {
					require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Kind: personenrichment.TriggerIdentity, Generation: "revision:2", DueAt: f.now}))
				}
				require.NoError(store.CancelScheduledTierEnrichmentForTest(t.Context(), f.store, f.person.ID))
				var gotState string
				var reserved, charged int64
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT state,reserved_cost_usd_micros FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&gotState, &reserved))
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT cost_charged_usd_micros FROM person_enrichment_run_counters WHERE run_id=?`), runID).Scan(&charged))
				if manual {
					work := f.work(t)
					require.Len(work, 1)
					assert.Equal(int64(16), work[0].TriggerMask)
					assert.Equal("manual:cancel-exact", work[0].TriggerGeneration)
					assert.False(work[0].HasFreshTrigger)
					assert.Equal(state, gotState)
					assert.Equal(int64(75), reserved)
					if state == "starting" {
						f.setNow(f.now.Add(6 * time.Minute))
						reclaimed := f.claim(t, runID, "reclaim-worker")
						assert.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
						assert.Equal("manual:cancel-exact", reclaimed.Trigger.Generation)
						require.NotNil(reclaimed.ActiveAttempt)
						retryStart := tierEnrichmentAttemptStart(&f, runID, "a")
						retryStart.Trigger = reclaimed.Trigger
						continued, created, err := f.store.BeginAttempt(t.Context(), reclaimed.Token, retryStart)
						require.NoError(err)
						assert.False(created)
						assert.Equal(attempt.ID, continued.ID)
					}
				} else {
					assert.Empty(f.work(t))
					assert.Equal("terminal", gotState)
					assert.Zero(reserved)
					var failure string
					require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT failure_class FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&failure))
					assert.Equal("policy", failure)
				}
				wantCharged := int64(75)
				if manual {
					wantCharged = 0
				}
				assert.Equal(wantCharged, charged)
				var actual sql.NullInt64
				require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT actual_cost_usd_micros FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&actual))
				assert.False(actual.Valid, "an unconfirmed provider charge must remain unknown")
				for _, table := range []string{"person_enrichment_person_day_counters", "person_enrichment_day_counters"} {
					var counterReserved, counterCharged int64
					require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT cost_reserved_usd_micros,cost_charged_usd_micros FROM `+table+` WHERE profile_fingerprint=?`), f.profile.Fingerprint).Scan(&counterReserved, &counterCharged))
					assert.Equal(reserved, counterReserved, table)
					assert.Equal(wantCharged, counterCharged, table)
				}
			})
		}
	}
}

func TestTierEnrichmentCancellationDispatchBarrierPreservesManualTargets(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "scheduled"
		if manual {
			name = "exact-manual"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTierEnrichmentFixture(t)
			var runID int64
			if manual {
				run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "dispatch-cancel", f.now)
				require.NoError(err)
				runID = run.ID
			} else {
				runID = f.startRun(t, "dispatch-cancel").ID
				f.enqueue(t)
			}
			lease := f.claim(t, runID, "worker")
			start := tierEnrichmentAttemptStart(&f, runID, "a")
			start.Trigger = lease.Trigger
			attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
			require.NoError(err)
			require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
			err = store.CancelScheduledTierEnrichmentForTest(t.Context(), f.store, f.person.ID)
			if manual {
				require.NoError(err)
			} else {
				require.ErrorIs(err, store.ErrPersonEnrichmentDispatchInProgress)
			}
			assert.Len(f.work(t), 1)
			var state string
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT state FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&state))
			assert.Equal("starting", state)
		})
	}
}

func TestTierEnrichmentOptOutBlocksScheduledPublicationAndCatchUp(t *testing.T) {
	for _, path := range []string{"identity", "catch-up", "trigger"} {
		t.Run(path, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTierEnrichmentFixture(t)
			setTierEnrichmentPersona(t, f, tierEnrichmentPersona{
				name: "opted-out", provenance: store.PersonTierProvenanceAuto,
				eligible: true, optedOut: true,
			})

			var err error
			switch path {
			case "identity":
				err = store.PublishTierEnrichmentForTest(t.Context(), f.store, f.person.ID)
			case "catch-up":
				var count int
				count, err = f.store.EnqueueDuePersonEnrichmentContext(
					t.Context(), f.now, 10, []string{f.profile.Fingerprint},
				)
				assert.Zero(count)
			case "trigger":
				err = f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
					PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
					Kind: personenrichment.TriggerIdentity, Generation: "revision:2", DueAt: f.now,
				})
			}
			require.NoError(err)
			assert.Empty(f.work(t))
		})
	}
}

func TestTierEnrichmentOptOutBlocksScheduledDispatchAndPoll(t *testing.T) {
	t.Run("dispatch", func(t *testing.T) {
		require := require.New(t)
		f := newTierEnrichmentFixture(t)
		run := f.startRun(t, "opt-out-dispatch")
		f.enqueue(t)
		lease := f.claim(t, run.ID, "opt-out-dispatch-worker")
		start := tierEnrichmentAttemptStart(&f, run.ID, "a")
		start.Trigger = lease.Trigger
		attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
		require.NoError(err)
		setTierEnrichmentPersona(t, f, tierEnrichmentPersona{
			name: "opted-out", provenance: store.PersonTierProvenanceAuto,
			eligible: true, optedOut: true,
		})
		require.ErrorIs(
			f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token),
			personenrichment.ErrConsentRequired,
		)
	})

	t.Run("poll", func(t *testing.T) {
		require := require.New(t)
		f := newTierEnrichmentFixture(t)
		run := f.startRun(t, "opt-out-poll")
		f.enqueue(t)
		lease := f.claim(t, run.ID, "opt-out-poll-worker")
		start := tierEnrichmentAttemptStart(&f, run.ID, "b")
		start.Trigger = lease.Trigger
		attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
		require.NoError(err)
		require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
		require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
			State: personenrichment.AttemptPending, RequestID: "opaque-opt-out-request",
			JobID: "opaque-opt-out-job", StartedAt: f.now, AdapterVersion: "adapter-v1",
			SchemaVersion: "schema-v1", ProgramFingerprint: strings.Repeat("c", 64),
		}))
		setTierEnrichmentPersona(t, f, tierEnrichmentPersona{
			name: "opted-out", provenance: store.PersonTierProvenanceAuto,
			eligible: true, optedOut: true,
		})
		require.ErrorIs(
			f.store.AuthorizeAttemptPoll(t.Context(), attempt.Token),
			personenrichment.ErrConsentRequired,
		)
	})

	t.Run("exact-manual-target", func(t *testing.T) {
		require := require.New(t)
		f := newTierEnrichmentFixture(t)
		run, _, err := f.store.StartManualPersonEnrichmentRunContext(
			t.Context(), f.person.ID, f.profile.Fingerprint, "opt-out-manual", f.now,
		)
		require.NoError(err)
		require.NoError(f.store.SetPersonTierOptOutContext(
			t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
		))
		lease := f.claim(t, run.ID, "opt-out-manual-worker")
		start := tierEnrichmentAttemptStart(&f, run.ID, "d")
		start.Trigger = lease.Trigger
		attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
		require.NoError(err)
		require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	})

	t.Run("manual-target-reclaims-after-stale-publication", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		f := newTierEnrichmentFixture(t)
		run, _, err := f.store.StartManualPersonEnrichmentRunContext(
			t.Context(), f.person.ID, f.profile.Fingerprint, "stale-reclaim", f.now,
		)
		require.NoError(err)
		require.NoError(f.store.SetPersonTierOptOutContext(
			t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
		))
		lease := f.claim(t, run.ID, "stale-manual-worker")
		start := tierEnrichmentAttemptStart(&f, run.ID, "e")
		start.Trigger = lease.Trigger
		start.PersonRevision = f.person.Revision + 1
		_, _, err = f.store.BeginAttempt(t.Context(), lease.Token, start)
		require.ErrorIs(err, store.ErrStaleLease)

		var detachedRunID sql.NullInt64
		require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
			`SELECT run_id FROM person_enrichment_work WHERE person_id=? AND profile_fingerprint=?`,
		), f.person.ID, f.profile.Fingerprint).Scan(&detachedRunID))
		require.False(detachedRunID.Valid)

		reclaimed, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
			RunID: run.ID, Owner: "stale-manual-reclaim-worker", ProviderName: f.profile.Name,
			ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: 5 * time.Minute,
		})
		require.NoError(err)
		require.NotNil(reclaimed, "the open exact manual target remains claimable after a stale publication")
		assert.Equal(run.ID, reclaimed.RunID)
		assert.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
		assert.Equal("manual:stale-reclaim", reclaimed.Trigger.Generation)
	})
}

func TestTierEnrichmentManualTargetReclaimsAfterDeferredRelease(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "deferred-manual", f.now,
	)
	require.NoError(err)
	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
	))

	lease := f.claim(t, run.ID, "budget-deferred-worker")
	nextAction := f.now.Add(time.Minute)
	require.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "defer", NextActionAt: &nextAction,
	}))
	var detachedRunID sql.NullInt64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT run_id FROM person_enrichment_work WHERE person_id=? AND profile_fingerprint=?`,
	), f.person.ID, f.profile.Fingerprint).Scan(&detachedRunID))
	require.False(detachedRunID.Valid)
	require.ErrorIs(
		f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{}),
		store.ErrRunNotTerminal,
	)
	stillRunning, err := f.store.GetPersonEnrichmentRunContext(t.Context(), run.ID)
	require.NoError(err)
	assert.Equal("running", stillRunning.State)

	reclaimed, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "budget-reclaim-worker", ProviderName: f.profile.Name,
		ProfileFingerprint: f.profile.Fingerprint, Now: nextAction, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(err)
	require.NotNil(reclaimed, "an open manual target remains claimable after a deferred release clears run_id")
	assert.Equal(run.ID, reclaimed.RunID)
	assert.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
	assert.Equal("manual:deferred-manual", reclaimed.Trigger.Generation)
}

func TestTierEnrichmentScheduledRunCannotClaimDetachedManualTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	manualRun, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "scheduled-cannot-steal", f.now,
	)
	require.NoError(err)
	manualLease := f.claim(t, manualRun.ID, "stale-manual-worker")
	start := tierEnrichmentAttemptStart(&f, manualRun.ID, "f")
	start.Trigger = manualLease.Trigger
	start.PersonRevision = f.person.Revision + 1
	_, _, err = f.store.BeginAttempt(t.Context(), manualLease.Token, start)
	require.ErrorIs(err, store.ErrStaleLease)

	scheduledRun := f.startRun(t, "cannot-steal-detached-manual")
	scheduledLease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: scheduledRun.ID, Owner: "scheduled-worker", ProviderName: f.profile.Name,
		ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(err)
	require.Nil(scheduledLease, "a scheduled run cannot rebind an open exact manual target")

	manualReclaim := f.claim(t, manualRun.ID, "manual-reclaim-worker")
	assert.Equal(manualRun.ID, manualReclaim.RunID)
	assert.Equal(personenrichment.TriggerManual, manualReclaim.Trigger.Kind)
}

func TestTierEnrichmentAutomaticTrackingOptOutRejectsDetachedManualTarget(t *testing.T) {
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_tracking SET provenance='auto', actor='signal-tiers' WHERE person_id=?`,
	), f.person.ID)
	require.NoError(err)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "cannot-untrack-detached-manual", f.now,
	)
	require.NoError(err)
	lease := f.claim(t, run.ID, "detached-manual-worker")
	nextAction := f.now.Add(time.Minute)
	require.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "defer", NextActionAt: &nextAction,
	}))
	err = f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, store.PersonTierLaneTracking, true, "owner", "explicit preference",
	)
	require.ErrorIs(err, store.ErrPersonTierManualRunOpen)
}

func TestTierEnrichmentScheduledRefreshPreservesDeferredManualWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "deferred-refresh", f.now,
	)
	require.NoError(err)
	lease := f.claim(t, run.ID, "deferred-refresh-worker")
	nextAction := f.now.Add(time.Minute)
	require.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "defer", NextActionAt: &nextAction,
	}))
	var manualState string
	var manualTarget bool
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT r.state, EXISTS (
			SELECT 1 FROM person_enrichment_manual_run_targets target
			WHERE target.run_id = r.id AND target.person_id = ? AND target.profile_fingerprint = ?)
		FROM person_enrichment_runs r WHERE r.id = ?`),
		f.person.ID, f.profile.Fingerprint, run.ID).Scan(&manualState, &manualTarget))
	assert.Equal("running", manualState)
	assert.True(manualTarget)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerIdentity, Generation: "revision:2", DueAt: f.now,
	}))

	work := f.work(t)
	require.Len(work, 1)
	assert.Equal(nextAction, work[0].DueAt)

	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
	))
	work = f.work(t)
	require.Len(work, 1)
	assert.Equal(nextAction, work[0].DueAt)
	assert.Equal(int64(16), work[0].TriggerMask)
	assert.Equal("manual:deferred-refresh", work[0].TriggerGeneration)
	assert.False(work[0].HasFreshTrigger)

	tooEarly, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "early-deferred-refresh-worker", ProviderName: f.profile.Name,
		ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(err)
	assert.Nil(tooEarly)

	f.setNow(nextAction)
	reclaimed := f.claim(t, run.ID, "deferred-refresh-reclaim-worker")
	assert.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
	assert.Equal("manual:deferred-refresh", reclaimed.Trigger.Generation)
}

func TestTierEnrichmentOptOutAllowsTriggerForExactManualTarget(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "manual-trigger-opt-out", f.now,
	)
	require.NoError(err)
	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
	))
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerIdentity, Generation: "revision:2", DueAt: f.now,
	}))

	work := f.work(t)
	require.Len(work, 1)
	assert.Equal(int64(18), work[0].TriggerMask)
	assert.Equal("manual:manual-trigger-opt-out", work[0].TriggerGeneration)
	assert.False(work[0].HasFreshTrigger, "the queued work has no active attempt to refresh")
	lease := f.claim(t, run.ID, "manual-trigger-reclaim-worker")
	assert.Equal(personenrichment.TriggerManual, lease.Trigger.Kind)
	assert.Equal("manual:manual-trigger-opt-out", lease.Trigger.Generation)
}

func TestTierEnrichmentOptOutRejectsStaleScheduledClaim(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	run := f.startRun(t, "opt-out-stale-claim")
	f.enqueue(t)
	insertTierEnrichmentOptOut(t, f)

	lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "worker", ProviderName: f.profile.Name,
		ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: time.Minute,
	})
	require.NoError(err)
	assert.Nil(lease)
}

func TestTierEnrichmentOptOutPreservesExactManualDispatch(t *testing.T) {
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "opt-out-manual-dispatch", f.now)
	require.NoError(err)
	lease := f.claim(t, run.ID, "worker")
	start := tierEnrichmentAttemptStart(&f, run.ID, "c")
	start.Trigger = lease.Trigger
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.NoError(err)
	insertTierEnrichmentOptOut(t, f)

	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
}

func TestTierEnrichmentOptOutPreservesManualTargetAfterStalePublication(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "stale-opt-out", f.now,
	)
	require.NoError(err)
	lease := f.claim(t, run.ID, "stale-opt-out-worker")
	start := tierEnrichmentAttemptStart(&f, run.ID, "a")
	start.Trigger = lease.Trigger
	start.PersonRevision = f.person.Revision + 1
	_, _, err = f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.ErrorIs(err, store.ErrStaleLease)
	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
	))

	work := f.work(t)
	require.Len(work, 1, "an open manual target survives scheduled cancellation after its work is detached")
	assert.Equal(int64(16), work[0].TriggerMask)
	assert.Equal("manual:stale-opt-out", work[0].TriggerGeneration)

	reclaimed := f.claim(t, run.ID, "manual-opt-out-reclaim-worker")
	assert.Equal(run.ID, reclaimed.RunID)
	assert.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
	assert.Equal("manual:stale-opt-out", reclaimed.Trigger.Generation)
}

func TestTierEnrichmentOptOutGatesPollAndPreservesExactManualTarget(t *testing.T) {
	for _, manual := range []bool{false, true} {
		name := "scheduled"
		if manual {
			name = "exact-manual"
		}
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			f := newTierEnrichmentFixture(t)
			var runID int64
			if manual {
				run, _, err := f.store.StartManualPersonEnrichmentRunContext(
					t.Context(), f.person.ID, f.profile.Fingerprint, "opt-out-manual-poll", f.now)
				require.NoError(err)
				runID = run.ID
			} else {
				runID = f.startRun(t, "opt-out-scheduled-poll").ID
				f.enqueue(t)
			}
			lease := f.claim(t, runID, "worker")
			start := tierEnrichmentAttemptStart(&f, runID, "d")
			start.Trigger = lease.Trigger
			attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
			require.NoError(err)
			require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
			require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
				State: personenrichment.AttemptPending, RequestID: "opaque-request", JobID: "opaque-job",
				StartedAt: f.now, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
				ProgramFingerprint: strings.Repeat("d", 64),
			}))
			require.NoError(f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
				State: personenrichment.ResultPending, RequestID: "opaque-request", JobID: "opaque-job",
				PollAfter: time.Second, AdapterVersion: "adapter-v1", SchemaVersion: "schema-v1",
			}))
			f.setNow(f.now.Add(time.Second))
			polling := f.claim(t, runID, "worker")
			insertTierEnrichmentOptOut(t, f)

			err = f.store.AuthorizeAttemptPoll(t.Context(), polling.Token)
			if manual {
				require.NoError(err)
			} else {
				require.ErrorIs(err, personenrichment.ErrConsentRequired)
			}
		})
	}
}

func TestTierEnrichmentCancellationRemovesQueuedScheduledWork(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentFixture(t)
	f.enqueue(t)
	require.Len(f.work(t), 1)
	require.NoError(store.CancelScheduledTierEnrichmentForTest(t.Context(), f.store, f.person.ID))
	assert.Empty(f.work(t))
	var attemptCount int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT COUNT(*) FROM person_enrichment_attempts WHERE person_id = ?`), f.person.ID).Scan(&attemptCount))
	assert.Zero(attemptCount)
}

// A completed uncertain-start attempt is immutable audit history. A later tier
// opt-out must not rewrite its state as a canceled scheduled attempt.
func TestTierEnrichmentOptOutPreservesCompletedUncertainStartAudit(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTierEnrichmentCappedFixture(t)
	run := f.startRun(t, "completed-uncertain-start-opt-out")
	f.enqueue(t)
	lease := f.claim(t, run.ID, "uncertain-start-worker")
	start := tierEnrichmentAttemptStart(&f, run.ID, "e")
	start.Trigger = lease.Trigger
	start.HardCostCap = true
	start.GuaranteedMaxCost = personenrichment.Cost{Currency: "USD", AmountMicros: 75}
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.NoError(err)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))

	f.setNow(lease.LeaseUntil.Add(time.Nanosecond))
	reclaimed := f.claim(t, run.ID, "uncertain-start-reclaim-worker")
	require.NotNil(reclaimed.ActiveAttempt)
	require.NoError(f.store.MarkUncertainStart(t.Context(), reclaimed.Token, personenrichment.SafeFailure{
		Class: personenrichment.FailureUncertainStart, Message: "start outcome is uncertain",
	}))

	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, store.PersonTierLaneEnrichment, true, "owner", "explicit preference",
	))

	var state, failure string
	var completed sql.NullTime
	var reserved, charged int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT state, failure_class, completed_at, reserved_cost_usd_micros
		 FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(
		&state, &failure, &completed, &reserved))
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT cost_charged_usd_micros FROM person_enrichment_run_counters WHERE run_id=?`),
		run.ID).Scan(&charged))

	assert.Equal("uncertain_start", state)
	assert.Equal(string(personenrichment.FailureUncertainStart), failure)
	assert.True(completed.Valid)
	assert.Zero(reserved)
	assert.Equal(int64(75), charged)
}

func insertTierEnrichmentOptOut(t *testing.T, f tierEnrichmentFixture) {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		INSERT INTO person_tier_opt_outs(person_id,lane,actor,reason,created_at)
		VALUES (?,'enrichment','owner','do not enrich',?)`), f.person.ID, f.now)
	require.NoError(t, err)
}
