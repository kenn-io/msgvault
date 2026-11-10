package store

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pause after maintenance has read membership, before its first UPDATE. Both
// transactions use the real PostgreSQL pool; only scheduling is controlled.
type savedViewMaintenanceGate struct {
	*sql.Tx
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (g *savedViewMaintenanceGate) ExecContext(ctx context.Context, statement string, args ...any) (sql.Result, error) {
	g.once.Do(func() { close(g.reached) })
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return g.Tx.ExecContext(ctx, statement, args...)
}

func TestPostgreSQLSavedViewItemsMaintenancePreservesConcurrentPatch(t *testing.T) {
	skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t)
	from, err := st.GetOrCreateSource("gmail", "removed@example.test")
	require.NoError(t, err)
	into, err := st.GetOrCreateSource("gmail", "retained@example.test")
	require.NoError(t, err)
	refs := make([]SavedViewItemRef, 2)
	for i, source := range []int64{from.ID, into.ID} {
		conv, err := st.EnsureConversation(source, "thread", "Thread")
		require.NoError(t, err)
		id, err := st.UpsertMessage(&Message{SourceID: source, ConversationID: conv, SourceMessageID: "message", MessageType: "email"})
		require.NoError(t, err)
		refs[i] = SavedViewItemRef{MessageID: id, SourceID: source}
	}
	view, err := st.CreateSavedView(t.Context(), SavedViewInput{Name: "Concurrent items", CanonicalState: []byte(`{}`), SchemaVersion: 2})
	require.NoError(t, err)
	view, err = st.ChangeSavedViewItems(t.Context(), view.ID, 1, SavedViewItemChange{IncludeAdd: refs[:1]})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	tx, err := st.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var maintenancePID int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&maintenancePID))
	gate := &savedViewMaintenanceGate{Tx: tx.Tx, reached: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(gate.release) }) }
	defer unblock()
	maintenanceDone := make(chan error, 1)
	go func() {
		err := st.pruneSavedViewItemsForSource(ctx, gate, from.ID)
		if err == nil {
			err = tx.Commit()
		}
		maintenanceDone <- err
	}()
	select {
	case <-gate.reached:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	patchDone := make(chan error, 1)
	go func() {
		_, err := st.ChangeSavedViewItems(ctx, view.ID, view.Revision, SavedViewItemChange{IncludeAdd: refs[1:]})
		patchDone <- err
	}()
	var early bool
	var patchErr error
	require.Eventually(t, func() bool {
		select {
		case patchErr = <-patchDone:
			early = true
			return true
		default:
		}
		var blocked bool
		err := st.DB().QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%UPDATE saved_views%')`, maintenancePID).Scan(&blocked)
		return err == nil && blocked
	}, 15*time.Second, 10*time.Millisecond, "PATCH either commits before maintenance or waits on its row lock")
	unblock()
	require.NoError(t, <-maintenanceDone)
	if !early {
		patchErr = <-patchDone
	}
	if patchErr != nil {
		require.ErrorIs(t, patchErr, ErrSavedViewRevisionConflict)
		current, err := st.GetSavedView(ctx, view.ID)
		require.NoError(t, err)
		_, err = st.ChangeSavedViewItems(ctx, view.ID, current.Revision, SavedViewItemChange{IncludeAdd: refs[1:]})
		require.NoError(t, err)
	}
	items, err := st.GetSavedViewItems(ctx, view.ID)
	require.NoError(t, err)
	require.Len(t, items.Include, 1, "maintenance must not erase a successfully committed PATCH")
	assert.Equal(t, refs[1], items.Include[0].SavedViewItemRef)
}
