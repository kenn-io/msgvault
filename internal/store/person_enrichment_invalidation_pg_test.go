package store

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestPersonEnrichmentResultAndInvalidationSharePersonFirstLockOrder(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newEnrichmentResultFixture(t)
	prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
	require.NoError(err)
	participantID := enrichmentInvalidationParticipant(t, f.store, f.person.ID)
	survivorID, err := f.store.EnsureParticipant(
		"pg-race-survivor@example.test", "PG Race Survivor", "example.test")
	require.NoError(err)
	resultBeforeLock := make(chan struct{}, 1)
	resultLocked := make(chan struct{}, 1)
	releaseResult := make(chan struct{})
	resumeResult := sync.OnceFunc(func() { close(releaseResult) })
	defer resumeResult()
	invalidationBeforeLock := make(chan struct{}, 1)
	invalidationLocked := make(chan struct{}, 1)
	SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
		switch phase {
		case "result_before_person_lock":
			resultBeforeLock <- struct{}{}
		case "result_person_locked":
			resultLocked <- struct{}{}
			select {
			case <-releaseResult:
			case <-t.Context().Done():
			}
		case "invalidation_before_person_lock":
			invalidationBeforeLock <- struct{}{}
		case "invalidation_person_locked":
			invalidationLocked <- struct{}{}
		}
	})
	type commitResult struct {
		outcome *personenrichment.ClaimOutcome
		err     error
	}
	resultDone := make(chan commitResult, 1)
	go func() {
		outcome, commitErr := f.store.commitPreparedPersonEnrichmentResult(t.Context(), prepared)
		resultDone <- commitResult{outcome: outcome, err: commitErr}
	}()
	requireReceiveEnrichmentBarrier(t, resultBeforeLock, "result did not reach person gate")
	select {
	case <-resultLocked:
	case result := <-resultDone:
		require.Fail("result bypassed person gate", "result: %+v", result)
	case <-time.After(5 * time.Second):
		require.Fail("result did not acquire person gate")
	}
	var resultPID int
	if f.store.IsPostgreSQL() {
		require.NoError(f.store.db.QueryRowContext(t.Context(), `SELECT activity.pid FROM pg_stat_activity activity
 WHERE activity.datname=current_database() AND activity.state='idle in transaction'
 AND EXISTS (SELECT 1 FROM pg_locks held WHERE held.pid=activity.pid
 AND held.relation='persons'::regclass AND held.mode='RowShareLock' AND held.granted)`).Scan(&resultPID))
	}
	mergeDone := make(chan error, 1)
	go func() {
		mergeDone <- f.store.MergeParticipants(participantID, survivorID)
	}()
	if f.store.IsPostgreSQL() {
		// The merge must reach its person revision update after inserting the
		// survivor binding, whose foreign key holds a KEY SHARE lock on person.
		require.Eventually(func() bool {
			var blocked bool
			err := f.store.db.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity activity
 WHERE activity.datname=current_database() AND ?=ANY(pg_blocking_pids(activity.pid)))`, resultPID).Scan(&blocked)
			require.NoError(err)
			return blocked
		}, 5*time.Second, 10*time.Millisecond, "merge did not wait for enrichment's person lock")
		select {
		case <-invalidationLocked:
			require.Fail("invalidation acquired person while result still owned it")
		default:
		}
	}
	resumeResult()
	result := <-resultDone
	require.NoError(result.err)
	require.NotNil(result.outcome)
	assert.Equal(personenrichment.ClaimApplied, result.outcome.Status)
	require.NoError(<-mergeDone)
	if f.store.IsPostgreSQL() {
		requireReceiveEnrichmentBarrier(t, invalidationBeforeLock, "invalidation did not reach person gate")
	}
	requireReceiveEnrichmentBarrier(t, invalidationLocked, "invalidation never acquired released person gate")
	rows, err := f.store.ListPersonEnrichmentWorkContext(t.Context(), PersonEnrichmentWorkFilter{
		PersonID: f.person.ID, ProfileFingerprint: f.profile.Fingerprint, Limit: 10,
	})
	require.NoError(err)
	require.Len(rows, 1)
	assert.Equal(personEnrichmentTriggerBit(personenrichment.TriggerIdentity), rows[0].TriggerMask)
}

func TestPostgreSQLPersonEnrichmentResultSerializesWithDefinitionExposure(t *testing.T) {
	if !IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		t.Skip("PostgreSQL enrichment/catalog lock barrier")
	}
	for _, exposure := range []string{"activation", "seed mapping"} {
		t.Run(exposure, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newEnrichmentResultFixture(t)
			prepared, err := f.store.preparePersonEnrichmentCommit(t.Context(), f.commit)
			require.NoError(err)
			definition, err := f.store.GetAttributeDefinitionBySlugContext(t.Context(), AttributeObjectPerson, AttributeSlugNotes)
			require.NoError(err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			personLocked := make(chan struct{}, 1)
			release := make(chan struct{})
			var releaseOnce sync.Once
			resume := func() { releaseOnce.Do(func() { close(release) }) }
			defer resume()
			SetPersonEnrichmentTxBarrierForTest(f.store, func(phase string) {
				if phase == "result_person_locked" {
					personLocked <- struct{}{}
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
			})
			resultDone := make(chan enrichmentClaimResult, 1)
			go func() {
				outcome, err := f.store.commitPreparedPersonEnrichmentResult(ctx, prepared)
				resultDone <- enrichmentClaimResult{outcome: outcome, err: err}
			}()
			requireReceiveEnrichmentBarrier(t, personLocked, "enrichment did not acquire its person")
			var resultPID int
			require.NoError(f.store.db.QueryRowContext(ctx, `SELECT activity.pid FROM pg_stat_activity activity
 WHERE activity.datname=current_database() AND activity.state='idle in transaction'
 AND EXISTS (SELECT 1 FROM pg_locks held WHERE held.pid=activity.pid
 AND held.relation='persons'::regclass AND held.mode='RowShareLock' AND held.granted)`).Scan(&resultPID))
			exposureDone := make(chan error, 1)
			go func() {
				var err error
				if exposure == "activation" {
					_, err = f.store.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision, AttributeDefinitionUpdate{IsActive: new(false)})
				} else {
					var seed AttributeDefinitionInput
					for _, candidate := range SeededAttributeDefinitions() {
						if candidate.Slug == AttributeSlugNotes {
							seed = candidate
						}
					}
					seed.VCardProperty = nil
					err = f.store.reconcileSeededDefinition(ctx, definition, seed)
				}
				exposureDone <- err
			}()
			require.Eventually(func() bool {
				var blocked bool
				err := f.store.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity activity
 WHERE activity.datname=current_database() AND ?=ANY(pg_blocking_pids(activity.pid)))`, resultPID).Scan(&blocked)
				require.NoError(err)
				return blocked
			}, 5*time.Second, 10*time.Millisecond, "catalog exposure did not wait for enrichment")
			resume()
			select {
			case result := <-resultDone:
				require.NoError(result.err)
				require.NotNil(result.outcome)
				assert.Equal(personenrichment.ClaimApplied, result.outcome.Status)
			case <-ctx.Done():
				require.FailNow("enrichment result did not finish", ctx.Err())
			}
			select {
			case err := <-exposureDone:
				require.NoError(err)
			case <-ctx.Done():
				require.FailNow("catalog exposure did not finish", ctx.Err())
			}
			got, err := f.store.GetAttributeDefinitionBySlugContext(ctx, AttributeObjectPerson, AttributeSlugNotes)
			require.NoError(err)
			if exposure == "activation" {
				assert.False(got.IsActive)
			} else {
				assert.Nil(got.VCardProperty)
			}
		})
	}
}
