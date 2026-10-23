package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestObservationMergeSerializesFactGenerationBeforePersonLock(t *testing.T) {
	require := require.New(t)
	f, in, generation := observationBatchFixture(t, "I run")
	if !f.store.IsPostgreSQL() {
		t.Skip("PostgreSQL lock ordering")
	}
	id := applyObservationInput(t, f.store, in, generation).Items[0].ObservationID
	input := observationStatusInput(t, f.store, in.PersonID, id, false, 1)
	survivor, err := f.store.GetPersonContext(t.Context(), f.bobPersonID)
	require.NoError(err)
	absorbed, err := f.store.GetPersonContext(t.Context(), in.PersonID)
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	paused := make(chan int, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	results := make(chan error, 2)
	go func() { results <- f.store.ApplyObservationStatusPausedForTest(ctx, input, paused, release) }()
	var factPID int
	select {
	case factPID = <-paused:
	case <-ctx.Done():
		require.NoError(ctx.Err())
	}
	go func() {
		_, err := f.store.MergeObservationPersonOnceForTest(ctx, store.PersonMergeRequest{SurvivorID: survivor.ID, AbsorbedID: absorbed.ID, ExpectedSurvivorRevision: survivor.Revision, ExpectedAbsorbedRevision: absorbed.Revision, IdempotencyKey: "observation-merge-generation-lock", Actor: "owner"})
		results <- err
	}()
	// Wait for a real blocking edge, rather than making scheduling speed a test.
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := f.store.DB().QueryRowContext(ctx, f.store.Rebind(`SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE ?=ANY(pg_blocking_pids(pid)))`), factPID).Scan(&waiting)
		require.NoError(err)
		if waiting {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			require.NoError(ctx.Err())
		}
	}
	close(release)
	for range 2 {
		select {
		case err := <-results:
			require.NoError(err)
		case <-ctx.Done():
			require.NoError(ctx.Err())
		}
	}
}
