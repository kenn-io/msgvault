package store

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotReaderRebindsOnPostgres(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL snapshot binding")
	}
	ctx, release, err := st.BeginReadSnapshotContext(context.Background())
	requirements.NoError(err)
	defer release()
	var got int
	requirements.NoError(st.snapshotReader(ctx).QueryRowContext(ctx, `SELECT CAST(? AS INTEGER)`, 42).Scan(&got))
	assertions.Equal(42, got)
	rows, err := st.snapshotReader(ctx).QueryContext(ctx, `SELECT CAST(? AS INTEGER)`, 43)
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	requirements.True(rows.Next())
	requirements.NoError(rows.Scan(&got))
	assertions.Equal(43, got)
	requirements.NoError(rows.Err())
	requirements.NoError(rows.Close())
	// A repeatable-read snapshot stays on its original source set.
	var before, after int
	requirements.NoError(st.snapshotReader(ctx).QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&before))
	_, err = st.GetOrCreateSource("gcal", "snapshot@example.com")
	requirements.NoError(err)
	requirements.NoError(st.snapshotReader(ctx).QueryRowContext(ctx, `SELECT COUNT(*) FROM sources`).Scan(&after))
	assertions.Equal(before, after)
}
