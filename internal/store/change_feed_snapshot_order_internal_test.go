package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Wrapping the existing dialect seam places a committed write between the
// bound read and snapshot start without adding a production hook.
type changeFeedOrderingDialect struct {
	Dialect
	t     *testing.T
	write func()
}

func (d *changeFeedOrderingDialect) ReadWatermarkBounds(ctx context.Context, db *sql.DB) (WatermarkBounds, error) {
	// A begun read snapshot owns a connection even before its first statement.
	// This catches a reordering that opens it before the pooled bound read.
	assert.Zero(d.t, db.Stats().InUse, "bound must be read before opening a snapshot")
	bounds, err := d.Dialect.ReadWatermarkBounds(ctx, db)
	if err == nil && d.write != nil {
		write := d.write
		d.write = nil
		write()
	}
	return bounds, err
}

func TestChangedMessagesSnapshotDoesNotSkipLaterCommit(t *testing.T) {
	st, err := OpenForTest(t.Context(), filepath.Join(t.TempDir(), "feed.db"))
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, st.Close()) })
	require.NoError(t, st.InitSchema())
	source, err := st.GetOrCreateSource("test", "ordering@example.test")
	require.NoError(t, err)
	conv, err := st.EnsureConversation(t.Context(), source.ID, "ordering", "Ordering")
	require.NoError(t, err)
	var id int64
	st.dialect = &changeFeedOrderingDialect{Dialect: st.dialect, t: t, write: func() {
		var err error
		id, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "between-bound-and-snapshot", MessageType: "email"})
		require.NoError(t, err)
	}}
	ctx, release, err := st.BeginChangedMessagesReadSnapshotContext(t.Context())
	require.NoError(t, err)
	defer release()
	require.NotZero(t, id)
	page, err := st.ListChangedMessages(ctx, ChangedMessagesFrom(time.Time{}), 10)
	require.NoError(t, err)
	require.False(t, page.CompleteThrough.IsZero())
	release()
	cursor := ChangedMessagesFrom(page.CompleteThrough)
	require.Eventually(t, func() bool {
		later, err := st.ListChangedMessages(t.Context(), cursor, 10)
		if err != nil {
			return false
		}
		for _, message := range later.Messages {
			if message.ID == id {
				return true
			}
		}
		return false
	}, 10*time.Second, 20*time.Millisecond)
}
