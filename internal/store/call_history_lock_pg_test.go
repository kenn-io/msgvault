package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/callhistory"
)

func TestCallLabelLocksCatalogBeforeSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	skipUnlessPostgresInternal(t)
	st := newPGStoreInternal(t)
	source, err := st.GetOrCreateSourceContext(t.Context(), callhistory.SourceType, "synthetic-call-source")
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	blocker, err := st.DB().BeginTx(ctx, nil)
	require.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	_, err = blocker.ExecContext(ctx, `UPDATE source_maintenance_lock SET singleton = singleton WHERE singleton = 1`)
	require.NoError(err)
	var blockerPID int
	require.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID))
	o := callhistory.Observation{SourceLabel: "Example calls", RecordID: "example", Raw: []byte(`{}`)}
	o.Evidence = callhistory.Evidence{
		Version: 1, Provider: "example", Account: "account",
		Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined",
		OccurredAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
	done := make(chan error, 1)
	go func() { _, err := st.PutCallObservationContext(ctx, source.ID, o); done <- err }()
	waitForPGBlockedQuery(ctx, t, st, blockerPID, "%UPDATE source_maintenance_lock%")
	// The importer is waiting for the catalog, so it cannot hold the source row.
	_, err = blocker.ExecContext(ctx, `UPDATE sources SET display_name = 'User name' WHERE id = $1`, source.ID)
	require.NoError(err)
	require.NoError(blocker.Commit())
	require.NoError(<-done)
	updated, err := st.GetSourceByIDContext(ctx, source.ID)
	require.NoError(err)
	assert.Equal("User name", updated.DisplayName.String)
}
