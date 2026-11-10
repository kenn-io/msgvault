package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRecordSyncRunItemGenerationFence(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	first := f.StartSync()
	failure := store.SyncRunItem{SyncRunID: first, SourceMessageID: "retry-message",
		Phase: "fetch", Status: "error", ErrorKind: "fetch_error", ErrorMessage: "request failed"}
	require.NoError(f.Store.RecordSyncRunItem(failure))
	stale := f.Store.ScopedToSync(f.Source.ID, first)
	require.NoError(f.Store.FailSync(first, "worker stopped"))
	current := f.StartSync()
	resolution := store.SyncRunItem{SyncRunID: first, SourceMessageID: failure.SourceMessageID,
		Phase: "fetch", Status: "resolved", ErrorKind: "acquired"}
	require.ErrorIs(stale.RecordSyncRunItemContext(t.Context(), resolution), store.ErrSyncRunSuperseded)
	resolution.SyncRunID = current
	require.ErrorIs(stale.RecordSyncRunItemContext(t.Context(), resolution), store.ErrSyncRunSuperseded,
		"a scoped worker cannot write to a different run")
	items, err := f.Store.ListSyncRunItems(first, "", 10)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("error", items[0].Status)
	live := f.Store.ScopedToSync(f.Source.ID, current)
	require.NoError(live.RecordSyncRunItemContext(t.Context(), resolution))
	items, err = f.Store.ListSyncRunItems(current, "", 10)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("resolved", items[0].Status)
}

func TestRecordSyncRunItemWaitsForGenerationTransitionPG(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	if !f.Store.IsPostgreSQL() {
		t.Skip("PostgreSQL row-lock regression")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	run := f.StartSync()
	require.NoError(f.Store.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run,
		SourceMessageID: "retry-message", Phase: "fetch", Status: "error", ErrorKind: "fetch_error"}))
	scoped := f.Store.ScopedToSync(f.Source.ID, run)
	transition, err := f.Store.DB().BeginTx(ctx, nil)
	require.NoError(err)
	t.Cleanup(func() { _ = transition.Rollback() })
	var locked int64
	require.NoError(transition.QueryRowContext(ctx,
		`SELECT id FROM sync_runs WHERE id = $1 FOR UPDATE`, run).Scan(&locked))
	done := make(chan error, 1)
	go func() {
		done <- scoped.RecordSyncRunItemContext(ctx, store.SyncRunItem{SyncRunID: run,
			SourceMessageID: "retry-message", Phase: "fetch", Status: "resolved", ErrorKind: "acquired"})
	}()
	waitForLockWait(t, f.Store, "sync_run", "the acquisition acknowledgement must wait for the run transition")
	// Commit a competing terminal transition while the old worker waits. Its
	// run must be checked again after taking the lock, before inserting an ack.
	_, err = transition.ExecContext(ctx, `UPDATE sync_runs SET status = 'failed' WHERE id = $1`, run)
	require.NoError(err)
	require.NoError(transition.Commit())
	select {
	case err := <-done:
		require.ErrorIs(err, store.ErrSyncRunSuperseded)
	case <-ctx.Done():
		stacktest.Dump(t)
		require.FailNow("acquisition writer did not finish", "%v", ctx.Err())
	}
	items, err := f.Store.ListSyncRunItems(run, "", 10)
	require.NoError(err)
	require.Len(items, 1)
	assert.Equal("fetch_error", items[0].ErrorKind)
}
