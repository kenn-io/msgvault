package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A REPEATABLE READ writer takes its snapshot before it waits for the
// identity fence. Another writer's committed clock update then makes this
// writer's own clock update fail serialization. That failure happens before
// the caller's work runs, so the Store retries it in a fresh transaction
// instead of handing an unrelated write's conflict to the caller.
func TestMCPEventsFenceRetriesRepeatableReadSerializationFailure(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := st.ConfigureMCPEvents(ctx, MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)

	writer, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = writer.Rollback() }()
	var writerPID int
	require.NoError(writer.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM archive_metadata WHERE key=$1 FOR UPDATE`, identityRevisionKey).Scan(&writerPID))

	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- st.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(*loggedTx) error {
			calls++
			return nil
		})
	}()
	waitForPGBlockedQuery(ctx, t, st, writerPID, "%FOR UPDATE%")
	_, err = writer.ExecContext(ctx, `UPDATE mcp_event_clock SET head_seq=head_seq WHERE singleton=1`)
	require.NoError(err)
	require.NoError(writer.Commit())

	select {
	case err = <-done:
	case <-ctx.Done():
		require.FailNow("repeatable-read writer did not finish")
	}
	require.NoError(err)
	assert.Equal(1, calls, "the caller's work runs once, after the retried fence")
}

// A process with capture off still checks the persisted switch before each
// write. A REPEATABLE READ writer that waited behind a daemon turning capture
// on must not trust its older snapshot: it must see capture on and record a
// gap by starting a new epoch.
func TestMCPEventsDisabledRepeatableReadWriterSeesCaptureEnabled(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	st := newPGStoreInternal(t, skipUnlessPostgresInternal(t))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, err := st.ConfigureMCPEvents(ctx, MCPEventsConfig{})
	require.NoError(err)

	enabler, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = enabler.Rollback() }()
	var enablerPID int
	require.NoError(enabler.QueryRowContext(ctx, `SELECT pg_backend_pid() FROM archive_metadata WHERE key=$1 FOR UPDATE`, identityRevisionKey).Scan(&enablerPID))

	done := make(chan error, 1)
	go func() {
		done <- st.withTxOptionsContext(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead}, func(*loggedTx) error { return nil })
	}()
	waitForPGBlockedQuery(ctx, t, st, enablerPID, "%FOR KEY SHARE%")
	var enabledEpoch int64
	require.NoError(enabler.QueryRowContext(ctx, `UPDATE mcp_event_clock SET enabled=TRUE, capture_epoch=capture_epoch+1 WHERE singleton=1 RETURNING capture_epoch`).Scan(&enabledEpoch))
	require.NoError(enabler.Commit())

	select {
	case err = <-done:
	case <-ctx.Done():
		require.FailNow("repeatable-read writer did not finish")
	}
	require.NoError(err)
	var enabled bool
	var epoch int64
	require.NoError(st.DB().QueryRowContext(ctx, `SELECT enabled, capture_epoch FROM mcp_event_clock WHERE singleton=1`).Scan(&enabled, &epoch))
	assert.False(enabled, "the uncaptured write must turn capture off")
	assert.Equal(enabledEpoch+1, epoch, "the uncaptured write must start a new epoch")
}
