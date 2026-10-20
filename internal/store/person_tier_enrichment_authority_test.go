package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
)

func TestTierEnrichmentScheduledPublicationAuthority(t *testing.T) {
	for _, site := range []string{"enqueue", "publish", "identity", "catch-up-missing", "catch-up-expired", "consent"} {
		for _, p := range tierEnrichmentPersonas {
			t.Run(site+"/"+p.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newTierEnrichmentFixture(t)
				setTierEnrichmentPersona(t, f, p)
				var err error
				switch site {
				case "enqueue":
					err = f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Kind: personenrichment.TriggerTracked, Generation: "scheduled", DueAt: f.now})
				case "publish":
					err = store.PublishTierEnrichmentForTest(t.Context(), f.store, f.person.ID)
				case "identity":
					name := "Synthetic Updated Person"
					before, getErr := f.store.GetPersonContext(t.Context(), f.person.ID)
					require.NoError(getErr)
					updated, updateErr := f.store.UpdatePersonDisplayNameContext(t.Context(), f.person.ID, before.Revision, &name)
					err = updateErr
					if updateErr == nil {
						assert.Greater(updated.Revision, before.Revision)
					}
				case "catch-up-missing", "catch-up-expired":
					if site == "catch-up-expired" {
						insertTierEnrichmentClaim(t, f.store, f.person.ID, f.profile.Fingerprint, "expired-tier", f.now.Add(-time.Minute))
					}
					_, err = f.store.EnqueueDuePersonEnrichmentContext(t.Context(), f.now, 200, []string{f.profile.Fingerprint})
				case "consent":
					_, err = f.store.RevokePersonEnrichmentConsent(t.Context(), f.profile.Fingerprint, "test")
					require.NoError(err)
					_, _, err = f.store.GrantPersonEnrichmentConsent(t.Context(), f.profile.Fingerprint, "test")
				}
				require.NoError(err)
				if p.want {
					assert.Len(f.work(t), 1)
				} else {
					assert.Empty(f.work(t))
				}
			})
		}
	}
}

func TestTierEnrichmentClaimRequiresScheduledAuthorityForUnboundWork(t *testing.T) {
	for _, kind := range []string{"scheduled", "manual"} {
		for _, p := range tierEnrichmentPersonas {
			t.Run(kind+"/"+p.name, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newTierEnrichmentFixture(t)
				setTierEnrichmentPersona(t, f, p)
				f.enqueue(t)
				run, _, err := f.store.StartRun(t.Context(), personenrichment.RunStart{Kind: kind, RequestedBy: "claim-tier", RequestedAt: f.now})
				require.NoError(err)
				lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{RunID: run.ID, Owner: "worker", ProviderName: f.profile.Name, ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: 5 * time.Minute})
				require.NoError(err)
				assert.Equal(p.want, lease != nil)
			})
		}
	}
}

func TestTierEnrichmentDispatchAndPollAuthority(t *testing.T) {
	for _, operation := range []string{"dispatch", "poll"} {
		for _, manual := range []bool{false, true} {
			for _, p := range tierEnrichmentPersonas {
				label := operation + "/scheduled/" + p.name
				if manual {
					label = operation + "/manual-target/" + p.name
				}
				t.Run(label, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					f := newTierEnrichmentFixture(t)
					var runID int64
					if manual {
						run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "exact-tier", f.now)
						require.NoError(err)
						runID = run.ID
					} else {
						runID = f.startRun(t, "scheduled-tier").ID
						f.enqueue(t)
					}
					lease := f.claim(t, runID, "worker")
					start := tierEnrichmentAttemptStart(&f, runID, "a")
					start.Trigger = lease.Trigger
					attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
					require.NoError(err)
					token := attempt.Token
					if operation == "poll" {
						require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), token))
						require.NoError(f.store.RecordProviderStarted(t.Context(), token, personenrichment.Attempt{State: personenrichment.AttemptPending, JobID: "tier-job", StartedAt: f.now, AdapterVersion: "adapter", SchemaVersion: "schema", ProgramFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}))
						require.NoError(f.store.SchedulePoll(t.Context(), token, personenrichment.Result{State: personenrichment.ResultPending, JobID: "tier-job", PollAfter: time.Minute, AdapterVersion: "adapter", SchemaVersion: "schema"}))
						f.setNow(f.now.Add(time.Minute))
						token = f.claim(t, runID, "worker").Token
					}
					setTierEnrichmentPersona(t, f, p)
					if operation == "dispatch" {
						err = f.store.AuthorizeAttemptDispatch(t.Context(), token)
					} else {
						err = f.store.AuthorizeAttemptPoll(t.Context(), token)
					}
					if manual || p.want {
						assert.NoError(err)
					} else {
						assert.ErrorIs(err, personenrichment.ErrConsentRequired)
					}
				})
			}
		}
	}
}

func TestTierEnrichmentManualRunKindDoesNotAuthorizeAnotherTarget(t *testing.T) {
	for _, operation := range []string{"dispatch", "poll"} {
		for _, mismatch := range []string{"person", "profile", "run"} {
			t.Run(operation+"/"+mismatch, func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newTierEnrichmentFixture(t)
				run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "target-only", f.now)
				require.NoError(err)
				lease := f.claim(t, run.ID, "worker")
				start := tierEnrichmentAttemptStart(&f, run.ID, "a")
				start.Trigger = lease.Trigger
				attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
				require.NoError(err)
				token := attempt.Token
				if operation == "poll" {
					require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), token))
					require.NoError(f.store.RecordProviderStarted(t.Context(), token, personenrichment.Attempt{State: personenrichment.AttemptPending, JobID: "exact-job", StartedAt: f.now, AdapterVersion: "adapter", SchemaVersion: "schema", ProgramFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}))
					require.NoError(f.store.SchedulePoll(t.Context(), token, personenrichment.Result{State: personenrichment.ResultPending, JobID: "exact-job", PollAfter: time.Minute, AdapterVersion: "adapter", SchemaVersion: "schema"}))
					f.setNow(f.now.Add(time.Minute))
					token = f.claim(t, run.ID, "worker").Token
				}
				setTierEnrichmentPersona(t, f, tierEnrichmentPersonas[2])
				switch mismatch {
				case "person":
					_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_manual_run_targets SET person_id=person_id+1000 WHERE run_id=?`), run.ID)
				case "profile":
					profile := tierEnrichmentProfile(t, "mismatch", "https://mismatch.example.test/search")
					_, err = f.store.EnsurePersonEnrichmentProfile(t.Context(), profile)
					require.NoError(err)
					_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_manual_run_targets SET profile_fingerprint=? WHERE run_id=?`), profile.Fingerprint, run.ID)
				case "run":
					_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`DELETE FROM person_enrichment_manual_run_targets WHERE run_id=?`), run.ID)
				}
				require.NoError(err)
				if operation == "dispatch" {
					err = f.store.AuthorizeAttemptDispatch(t.Context(), token)
				} else {
					err = f.store.AuthorizeAttemptPoll(t.Context(), token)
				}
				assert.ErrorIs(err, personenrichment.ErrConsentRequired)
			})
		}
	}
}

func TestTierEnrichmentOptOutRestoresManualTriggerForResumedAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "manual-coalesced-opt-out", f.now)
	require.NoError(err)

	lease := f.claim(t, run.ID, "manual-worker")
	start := tierEnrichmentAttemptStart(&f, run.ID, "a")
	start.Trigger = lease.Trigger
	attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.NoError(err)
	require.NoError(f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
	require.NoError(f.store.RecordProviderStarted(t.Context(), attempt.Token, personenrichment.Attempt{
		State: personenrichment.AttemptPending, JobID: "manual-job", StartedAt: f.now,
		AdapterVersion: "adapter", SchemaVersion: "schema",
		ProgramFingerprint: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}))

	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerTracked, Generation: "scheduled-after-manual", DueAt: f.now,
	}))
	coalesced := f.work(t)
	require.Len(coalesced, 1)
	assert.True(coalesced[0].HasFreshTrigger)
	assert.Equal("scheduled-after-manual", coalesced[0].TriggerGeneration)

	require.NoError(f.store.SchedulePoll(t.Context(), attempt.Token, personenrichment.Result{
		State: personenrichment.ResultPending, JobID: "manual-job", PollAfter: time.Minute,
		AdapterVersion: "adapter", SchemaVersion: "schema",
	}))
	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, "enrichment", true, "owner", "pause scheduled enrichment"))

	work := f.work(t)
	require.Len(work, 1)
	assert.Equal(int64(16), work[0].TriggerMask)
	assert.Equal(lease.Trigger.Generation, work[0].TriggerGeneration)
	assert.False(work[0].HasFreshTrigger)
	require.NotNil(work[0].ActiveAttemptID)
	assert.Equal(attempt.ID, *work[0].ActiveAttemptID)

	f.setNow(f.now.Add(time.Minute))
	resumed := f.claim(t, run.ID, "manual-worker-after-restart")
	require.NotNil(resumed.ActiveAttempt)
	assert.Equal(lease.Trigger, resumed.Trigger)
	assert.Equal(resumed.Trigger, resumed.ActiveAttempt.Trigger)
	require.NoError(f.store.AuthorizeAttemptPoll(t.Context(), resumed.Token))
}

func TestTierEnrichmentOptOutRestoresManualTriggerBeforeAttemptStarts(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	run, _, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "manual-unstarted-opt-out", f.now)
	require.NoError(err)

	lease := f.claim(t, run.ID, "manual-worker")
	require.Nil(lease.ActiveAttempt)
	assert.Equal("manual:manual-unstarted-opt-out", lease.Trigger.Generation)

	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerTracked, Generation: "scheduled-before-attempt", DueAt: f.now,
	}))
	coalesced := f.work(t)
	require.Len(coalesced, 1)
	assert.True(coalesced[0].HasFreshTrigger)
	assert.Equal("scheduled-before-attempt", coalesced[0].TriggerGeneration)

	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, "enrichment", true, "owner", "pause scheduled enrichment"))
	work := f.work(t)
	require.Len(work, 1)
	assert.Equal(int64(16), work[0].TriggerMask)
	assert.Equal("manual:manual-unstarted-opt-out", work[0].TriggerGeneration)
	assert.False(work[0].HasFreshTrigger)
	assert.Nil(work[0].ActiveAttemptID)

	f.setNow(lease.LeaseUntil)
	resumed := f.claim(t, run.ID, "manual-worker-after-restart")
	assert.Equal(personenrichment.TriggerManual, resumed.Trigger.Kind)
	assert.Equal("manual:manual-unstarted-opt-out", resumed.Trigger.Generation)

	start := tierEnrichmentAttemptStart(&f, run.ID, "c")
	start.Trigger = resumed.Trigger
	attempt, _, err := f.store.BeginAttempt(t.Context(), resumed.Token, start)
	require.NoError(err)
	assert.Equal("manual:manual-unstarted-opt-out", attempt.Trigger.Generation)
}

func TestTierEnrichmentOptOutRestoresManualTriggerAfterStalePublication(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	run, created, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "manual-stale-publication-opt-out", f.now)
	require.NoError(err)
	require.True(created)

	lease := f.claim(t, run.ID, "manual-worker")
	require.Nil(lease.ActiveAttempt)
	assert.Equal("manual:manual-stale-publication-opt-out", lease.Trigger.Generation)
	require.NoError(f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint,
		Kind: personenrichment.TriggerTracked, Generation: "scheduled-before-stale-publication", DueAt: f.now,
	}))

	start := tierEnrichmentAttemptStart(&f, run.ID, "c")
	start.Trigger = lease.Trigger
	attempt, attemptCreated, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
	require.ErrorIs(err, store.ErrStaleLease)
	require.Nil(attempt)
	require.False(attemptCreated)

	work := f.work(t)
	require.Len(work, 1)
	require.NotNil(work[0].RunID)
	assert.Equal(run.ID, *work[0].RunID)
	assert.Equal(int64(16), work[0].TriggerMask)
	assert.Equal("manual:manual-stale-publication-opt-out", work[0].TriggerGeneration)
	assert.False(work[0].HasFreshTrigger)

	require.NoError(f.store.SetPersonTierOptOutContext(
		t.Context(), f.person.ID, "enrichment", true, "owner", "pause scheduled enrichment"))
	work = f.work(t)
	require.Len(work, 1)
	require.NotNil(work[0].RunID)
	assert.Equal(run.ID, *work[0].RunID)
	assert.Equal(int64(16), work[0].TriggerMask)
	assert.Equal("manual:manual-stale-publication-opt-out", work[0].TriggerGeneration)
	assert.False(work[0].HasFreshTrigger)

	resumed := f.claim(t, run.ID, "manual-worker-after-stale-publication")
	assert.Equal(personenrichment.TriggerManual, resumed.Trigger.Kind)
	assert.Equal("manual:manual-stale-publication-opt-out", resumed.Trigger.Generation)
	start = tierEnrichmentAttemptStart(&f, run.ID, "d")
	start.Trigger = resumed.Trigger
	attempt, attemptCreated, err = f.store.BeginAttempt(t.Context(), resumed.Token, start)
	require.NoError(err)
	require.True(attemptCreated)
	assert.Equal("manual:manual-stale-publication-opt-out", attempt.Trigger.Generation)
}

func TestTierEnrichmentManualWorkRemainsClaimableAfterBudgetDeferral(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newTierEnrichmentFixture(t)
	setTierEnrichmentPersona(t, f, tierEnrichmentPersonas[2])
	run, created, err := f.store.StartManualPersonEnrichmentRunContext(
		t.Context(), f.person.ID, f.profile.Fingerprint, "manual-budget-deferral", f.now)
	require.NoError(err)
	require.True(created)

	lease := f.claim(t, run.ID, "manual-worker")
	assert.Equal(personenrichment.TriggerManual, lease.Trigger.Kind)
	nextDay := time.Date(f.now.Year(), f.now.Month(), f.now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 1)
	failure := personenrichment.SafeFailure{
		Class: personenrichment.FailurePolicy, Message: "enrichment budget exhausted",
	}
	require.NoError(f.store.ReleaseWork(t.Context(), lease.Token, personenrichment.WorkRelease{
		Outcome: "defer", Failure: &failure, NextActionAt: &nextDay,
	}))

	work := f.work(t)
	require.Len(work, 1)
	require.NotNil(work[0].RunID)
	assert.Equal(run.ID, *work[0].RunID)
	assert.ErrorIs(f.store.CompleteRun(t.Context(), run.ID, personenrichment.RunCompletion{
		CompletedAt: f.now,
	}), store.ErrRunNotTerminal)

	f.setNow(nextDay)
	reclaimed, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{
		RunID: run.ID, Owner: "manual-worker-next-day", ProviderName: f.profile.Name,
		ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: 5 * time.Minute,
	})
	require.NoError(err)
	require.NotNil(reclaimed)
	assert.Equal(personenrichment.TriggerManual, reclaimed.Trigger.Kind)
}

func TestTierEnrichmentManualStartAllowsEveryTrackedPersona(t *testing.T) {
	for _, p := range tierEnrichmentPersonas {
		t.Run(p.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newTierEnrichmentFixture(t)
			setTierEnrichmentPersona(t, f, p)
			run, created, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "manual-any-tier", f.now)
			require.NoError(err)
			require.NotNil(run)
			assert.True(created)
			lease, err := f.store.ClaimWork(t.Context(), personenrichment.ClaimOptions{RunID: run.ID, Owner: "manual-worker", ProviderName: f.profile.Name, ProfileFingerprint: f.profile.Fingerprint, Now: f.now, LeaseDuration: 5 * time.Minute})
			require.NoError(err)
			require.NotNil(lease)
			assert.Equal(personenrichment.TriggerManual, lease.Trigger.Kind)
		})
	}
}
