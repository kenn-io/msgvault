package imazingcsv

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelAfterMessageCommitHandler cancels on the first committed transaction
// that leaves a message row behind, so the rest of the import runs with a
// cancelled context.
type cancelAfterMessageCommitHandler struct {
	slog.Handler

	mu               sync.Mutex
	cancel           context.CancelFunc
	messageCommitted func() bool
	canceled         bool
}

func (*cancelAfterMessageCommitHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelAfterMessageCommitHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "sql tx commit" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.canceled && h.messageCommitted() {
		h.cancel()
		h.canceled = true
	}
	return nil
}

// A cancel that stops persistence after the first message commits must still
// recompute conversation statistics. If the recompute ran on the cancelled
// context, it would fail and leave message_count and the last-message fields
// describing an empty conversation that holds a committed message.
// Not parallel: it replaces the process-wide slog default and SQL log options.
func TestImporterCancelAfterFirstMessageRecomputesConversationStats(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewSQLiteTestStore(t)
	exportDir := newTestExport(t, [][]string{
		{"Alice", "2024-06-01 12:00:00", "", "", "iMessage", "Outgoing", "", "", "", "", "", "first message", "", ""},
		{"Alice", "2024-06-01 12:01:00", "", "", "iMessage", "Incoming", "+15550000002", "Alice", "", "", "", "second message", "", ""},
	})

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	handler := &cancelAfterMessageCommitHandler{
		Handler: slog.DiscardHandler,
		cancel:  cancel,
		messageCommitted: func() bool {
			var count int
			require.NoError(st.DB().QueryRowContext(queryCtx, `SELECT COUNT(*) FROM messages`).Scan(&count))
			return count > 0
		},
	}
	// A commit over the slow threshold logs "sql tx slow" in place of the
	// line the handler waits for.
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, err := NewImporter(st, Options{Owner: "+15550000001", Timezone: "UTC"}).ImportPath(ctx, exportDir)
	require.ErrorIs(err, context.Canceled)
	require.True(handler.canceled, "the cancel must land after the first message commits")

	source, err := st.GetSourceByTypeAndIdentifier(t.Context(), SourceType, "+15550000001")
	require.NoError(err)
	var messages, messageCount int
	var lastMessagePreview sql.NullString
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM messages WHERE source_id = ?`), source.ID).Scan(&messages))
	require.Equal(1, messages, "only the first message commits before the cancel")
	require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT message_count, last_message_preview
		FROM conversations WHERE source_id = ?`), source.ID).Scan(&messageCount, &lastMessagePreview))
	assert.Equal(1, messageCount, "conversation message_count must count the committed message")
	assert.Equal(sql.NullString{String: "first message", Valid: true}, lastMessagePreview,
		"last_message_preview must describe the committed message")
}
