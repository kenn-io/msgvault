package whatsapp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// A cancel after the last message commits fails that message's remaining
// writes and ends the last chat's loops. The import must still report the
// cancellation and mark its sync run failed, since the cancelled writes left
// that message without its body and search entry. The committed messages
// still need current conversation statistics.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportCancelAfterLastMessageCommitFailsSync(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	waDBPath := createGroupParticipantsImportFixture(t, nil)
	st := testutil.NewSQLiteTestStore(t)
	opts := DefaultOptions()
	opts.Phone = "+12025550123"
	opts.DisplayName = "Example User"

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(st.DB().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
		return count == 2
	})

	_, err := NewImporter(st, nil).Import(ctx, waDBPath, opts)
	require.ErrorIs(err, context.Canceled)
	require.True(canceled(), "the cancel must land after the last message commits")

	var status string
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	assert.NotEqual("completed", status, "an interrupted import must not be recorded as completed")
	var counted, stored int
	require.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(SUM(message_count), 0) FROM conversations`).Scan(&counted))
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM messages`).Scan(&stored))
	assert.Equal(stored, counted, "conversation message counts must cover every committed message")
}
