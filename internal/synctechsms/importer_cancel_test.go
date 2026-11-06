package synctechsms

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// Ctrl-C after a message commits leaves that message in its conversation.
// The failed import must still refresh the conversation's statistics, or the
// conversation keeps a message_count of zero.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImportPathCancelAfterMessageCommitRecomputesStats(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	f := storetest.New(t)
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "messages.xml"), `<smses count="1">
  <sms address="+15551234567" date="1717214400000" type="1" body="row saved before cancel" read="1" status="-1" contact_name="Alice" />
</smses>`)

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	canceled := testutil.CancelAfterCommit(t, cancel, func() bool {
		var count int
		require.NoError(f.Store.DB().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
		return count > 0
	})

	imp := NewImporter(f.Store, ImportOptions{OwnerPhone: "+15550000001", IncludeSMS: true})
	_, err := imp.ImportPath(ctx, dir)
	require.ErrorIs(err, context.Canceled)
	require.True(canceled(), "the cancel must land after the message commits")

	var messageCount int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(MAX(message_count), 0) FROM conversations`).Scan(&messageCount))
	assert.Equal(1, messageCount, "conversation message_count")
	var status string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs`).Scan(&status))
	assert.Equal(store.SyncStatusFailed, status)
}
