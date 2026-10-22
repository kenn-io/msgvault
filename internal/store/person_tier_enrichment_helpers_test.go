package store_test

import (
	"database/sql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/store"
	"strconv"
	"testing"
)

func TestTierEnrichmentPersistedEligibility(t *testing.T) {
	for _, p := range tierEnrichmentPersonas {
		t.Run(p.name, func(t *testing.T) {
			f := newTierEnrichmentFixture(t)
			setTierEnrichmentPersona(t, f, p)
			got, err := store.TierEnrichmentAuthorityForTest(t.Context(), f.store, f.person.ID)
			require.NoError(t, err)
			assert.Equal(t, p.want, got)
		})
	}
}

func TestTierEnrichmentOpenManualRequiresExactTarget(t *testing.T) {
	for _, mismatch := range []string{"none", "person", "profile", "run", "terminal"} {
		t.Run(mismatch, func(t *testing.T) {
			f := newTierEnrichmentFixture(t)
			run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "open-exact", f.now)
			require.NoError(t, err)
			switch mismatch {
			case "person":
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_manual_run_targets SET person_id=person_id+1000 WHERE run_id=?`), run.ID)
			case "profile":
				profile := tierEnrichmentProfile(t, "other", "https://other.example.test/search")
				_, err = f.store.EnsurePersonEnrichmentProfile(t.Context(), profile)
				require.NoError(t, err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_manual_run_targets SET profile_fingerprint=? WHERE run_id=?`), profile.Fingerprint, run.ID)
			case "run":
				other, _, otherErr := f.store.StartRun(t.Context(), personenrichment.RunStart{Kind: "manual", RequestedBy: "unbound", RequestedAt: f.now})
				require.NoError(t, otherErr)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_work SET run_id=? WHERE person_id=?`), other.ID, f.person.ID)
			case "terminal":
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_runs SET state='succeeded' WHERE id=?`), run.ID)
			}
			require.NoError(t, err)
			id, err := store.OpenManualTierEnrichmentForTest(t.Context(), f.store, f.person.ID)
			require.NoError(t, err)
			if mismatch == "none" {
				assert.Equal(t, strconv.FormatInt(run.ID, 10), id)
			} else {
				assert.Empty(t, id)
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
				f := newTierEnrichmentFixture(t)
				var runID int64
				if manual {
					run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "cancel-exact", f.now)
					require.NoError(t, err)
					runID = run.ID
				} else {
					runID = f.startRun(t, "cancel-scheduled").ID
					f.enqueue(t)
				}
				lease := f.claim(t, runID, "worker")
				start := tierEnrichmentAttemptStart(&f, runID, "a")
				start.Trigger = lease.Trigger
				attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
				require.NoError(t, err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_attempts SET state=?,hard_cost_cap_enforced=TRUE,reserved_cost_usd_micros=75 WHERE id=?`), state, attempt.ID)
				require.NoError(t, err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_run_counters SET cost_reserved_usd_micros=75 WHERE run_id=?`), runID)
				require.NoError(t, err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_person_day_counters SET cost_reserved_usd_micros=75 WHERE person_id=?`), f.person.ID)
				require.NoError(t, err)
				_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`UPDATE person_enrichment_day_counters SET cost_reserved_usd_micros=75 WHERE profile_fingerprint=?`), f.profile.Fingerprint)
				require.NoError(t, err)
				if manual {
					require.NoError(t, f.store.EnqueuePersonEnrichmentContext(t.Context(), store.EnrichmentTriggerInput{PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Kind: personenrichment.TriggerIdentity, Generation: "revision:2", DueAt: f.now}))
				}
				require.NoError(t, store.CancelScheduledTierEnrichmentForTest(t.Context(), f.store, f.person.ID))
				var gotState string
				var reserved, charged int64
				require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT state,reserved_cost_usd_micros FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&gotState, &reserved))
				require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT cost_charged_usd_micros FROM person_enrichment_run_counters WHERE run_id=?`), runID).Scan(&charged))
				if manual {
					work := f.work(t)
					require.Len(t, work, 1)
					assert.Equal(t, int64(16), work[0].TriggerMask)
					assert.Equal(t, state, gotState)
					assert.Equal(t, int64(75), reserved)
				} else {
					assert.Empty(t, f.work(t))
					assert.Equal(t, "terminal", gotState)
					assert.Zero(t, reserved)
					var failure string
					require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT failure_class FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&failure))
					assert.Equal(t, "policy", failure)
				}
				wantCharged := int64(75)
				if manual {
					wantCharged = 0
				}
				assert.Equal(t, wantCharged, charged)
				var actual sql.NullInt64
				require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT actual_cost_usd_micros FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&actual))
				assert.False(t, actual.Valid, "an unconfirmed provider charge must remain unknown")
				for _, table := range []string{"person_enrichment_person_day_counters", "person_enrichment_day_counters"} {
					var counterReserved, counterCharged int64
					require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT cost_reserved_usd_micros,cost_charged_usd_micros FROM `+table+` WHERE profile_fingerprint=?`), f.profile.Fingerprint).Scan(&counterReserved, &counterCharged))
					assert.Equal(t, reserved, counterReserved, table)
					assert.Equal(t, wantCharged, counterCharged, table)
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
			f := newTierEnrichmentFixture(t)
			var runID int64
			if manual {
				run, _, err := f.store.StartManualPersonEnrichmentRunContext(t.Context(), f.person.ID, f.profile.Fingerprint, "dispatch-cancel", f.now)
				require.NoError(t, err)
				runID = run.ID
			} else {
				runID = f.startRun(t, "dispatch-cancel").ID
				f.enqueue(t)
			}
			lease := f.claim(t, runID, "worker")
			start := tierEnrichmentAttemptStart(&f, runID, "a")
			start.Trigger = lease.Trigger
			attempt, _, err := f.store.BeginAttempt(t.Context(), lease.Token, start)
			require.NoError(t, err)
			require.NoError(t, f.store.AuthorizeAttemptDispatch(t.Context(), attempt.Token))
			err = store.CancelScheduledTierEnrichmentForTest(t.Context(), f.store, f.person.ID)
			if manual {
				assert.NoError(t, err)
			} else {
				assert.ErrorIs(t, err, store.ErrPersonEnrichmentDispatchInProgress)
			}
			assert.Len(t, f.work(t), 1)
			var state string
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`SELECT state FROM person_enrichment_attempts WHERE id=?`), attempt.ID).Scan(&state))
			assert.Equal(t, "starting", state)
		})
	}
}
