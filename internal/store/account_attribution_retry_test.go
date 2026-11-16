package store

import (
	"context"
	"database/sql/driver"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmIdentityRetriesAccountAttribution(t *testing.T) {
	base := newRFC822IDBackfillBackendStore(t)
	source, err := base.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	conv, err := base.EnsureConversation(t.Context(), source.ID, "thread", "Thread")
	require.NoError(t, err)
	_, err = base.db.ExecContext(t.Context(), `INSERT INTO messages(source_id,source_message_id,conversation_id,message_type,account_address,account_path) VALUES(?,?,?,'email','work@example.org','inbound')`, source.ID, "one", conv)
	require.NoError(t, err)
	busy := error(sqlite3.Error{Code: sqlite3.ErrBusy})
	if base.IsPostgreSQL() {
		busy = &pgconn.PgError{Code: "55P03"}
	}
	var committed, injected atomic.Bool
	st := gatedMeetingProjectionStore(t, base, &meetingProjectionGate{
		committed: func(bool) { committed.Store(true) },
		before: func(context.Context, string, []driver.NamedValue) error {
			if committed.Load() && injected.CompareAndSwap(false, true) {
				return busy
			}
			return nil
		},
	})
	require.NoError(t, st.AddAccountIdentityContext(t.Context(), source.ID, "work@example.org", "manual"))
	assert.True(t, injected.Load())
	ids, err := st.PendingAccountAttributionSourcesContext(t.Context())
	require.NoError(t, err)
	assert.NotContains(t, ids, source.ID)
}

func TestRepairAccountAttributionRetriesBusyPage(t *testing.T) {
	base := newRFC822IDBackfillBackendStore(t)
	source, err := base.GetOrCreateSource("gmail", "inbox@example.com")
	require.NoError(t, err)
	conv, err := base.EnsureConversation(t.Context(), source.ID, "thread", "Thread")
	require.NoError(t, err)
	for _, id := range []string{"one", "two"} {
		_, err = base.db.ExecContext(t.Context(), `INSERT INTO messages(source_id,source_message_id,conversation_id,message_type) VALUES(?,?,?,'email')`, source.ID, id, conv)
		require.NoError(t, err)
	}
	// After the first page commits, the next statement meets a busy database once.
	busy := error(sqlite3.Error{Code: sqlite3.ErrBusy})
	if base.IsPostgreSQL() {
		busy = &pgconn.PgError{Code: "55P03"}
	}
	var committed, injected atomic.Bool
	st := gatedMeetingProjectionStore(t, base, &meetingProjectionGate{
		committed: func(bool) { committed.Store(true) },
		before: func(context.Context, string, []driver.NamedValue) error {
			if committed.Load() && injected.CompareAndSwap(false, true) {
				return busy
			}
			return nil
		},
	})
	st.accountRepairPageSizeOverride = 1
	var progress []int64
	summary, err := st.RepairAccountAttributionContext(t.Context(), source.ID, func(result AccountAttributionRepairSummary) {
		progress = append(progress, result.Scanned)
	})
	require.NoError(t, err)
	assert.True(t, injected.Load(), "the second page did not meet the busy database")
	assert.Equal(t, int64(2), summary.Scanned)
	assert.Equal(t, []int64{1, 2}, progress)
}
