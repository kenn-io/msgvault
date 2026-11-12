package telegram

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// An import that stops at --limit saves its checkpoint and then pauses its
// sync run. A cancel that lands after the checkpoint commits must not stop
// the pause, or the run stays recorded as running.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportLimitCancelAfterCheckpointPausesSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	root := t.TempDir()
	testutil.WriteFile(t, root, "result.json", []byte(`{"id":2,"type":"personal_chat","messages":[{"id":1,"type":"message","date_unixtime":"1610909872","text":"first"},{"id":2,"type":"message","date_unixtime":"1610909872","text":"second"}]}`))

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var saved int
		require.NoError(st.DB().QueryRowContext(queryCtx,
			`SELECT COUNT(*) FROM sync_runs WHERE cursor_before IS NOT NULL`).Scan(&saved))
		return saved > 0
	})

	summary, err := Import(ctx, st, ImportOptions{RootDir: root, Me: "1", Limit: 1})
	require.NoError(err)
	require.True(summary.LimitReached)
	require.True(canceled(), "the cancel must land after the checkpoint commits")

	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.Equal("paused", status)
}
