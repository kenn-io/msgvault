package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// With Events capture on, every archive write first takes the Events fences.
// A busy archive must still produce an error that writer retry loops
// recognise, instead of a fixed Events reason that hides the cause.
func TestMCPEventsCaptureKeepsBusyWritesRetryable(t *testing.T) {
	if !sqliteutil.Available {
		t.Skip("the writer-slot holder needs SQLite")
	}
	assert := assert.New(t)
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "events-contention.db")
	st, err := openSQLite(path, "?_journal_mode=WAL&_busy_timeout=0&_foreign_keys=ON")
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.ConfigureMCPEvents(t.Context(), MCPEventsConfig{
		Enabled: true, Principal: "owner:synthetic",
		Capabilities: []MCPEventCapability{{Family: "msgvault.message_archived", SourceType: "gmail", Kinds: []string{"message"}}},
	})
	require.NoError(err)
	require.True(st.captureMCPEnabled())

	holder, err := sql.Open(sqliteutil.DriverName(), path+"?_busy_timeout=0")
	require.NoError(err)
	t.Cleanup(func() { _ = holder.Close() })
	held, err := holder.Conn(t.Context())
	require.NoError(err)
	t.Cleanup(func() { _ = held.Close() })
	_, err = held.ExecContext(t.Context(), "BEGIN IMMEDIATE")
	require.NoError(err)

	input := DailyNoteEntryInput{LocalDate: "2026-10-08", Body: "synthetic contended note", Author: "user"}
	_, err = st.CreateDailyNoteEntryContext(t.Context(), input)
	require.Error(err)
	assert.True(st.IsBusyError(err), "busy cause must stay classifiable: %v", err)
	assert.True(dailyNoteRetryable(t.Context(), st, err))

	_, err = held.ExecContext(context.Background(), "ROLLBACK")
	require.NoError(err)
	entry, err := st.CreateDailyNoteEntryContext(t.Context(), input)
	require.NoError(err)
	assert.Equal(input.Body, entry.Body)
}

// The commit after an Events write keeps its driver text out of messages, but
// retry loops still see the cause.
func TestMCPSafeErrorRedactsMessageButKeepsCause(t *testing.T) {
	assert := assert.New(t)
	driverErr := errors.New("synthetic-private-driver-text")
	safe := mcpSafeError(fmt.Errorf("commit: %w", driverErr))
	assert.Equal("events_storage_unavailable", safe.Error())
	assert.ErrorIs(safe, driverErr)
	assert.Equal(safe, mcpSafeError(safe))
	assert.Equal(mcpStoreError("unknown_scope"), mcpSafeError(mcpStoreError("unknown_scope")))
}
