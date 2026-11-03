package fbmessenger

import (
	"context"
	"database/sql"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelOnTxBeginHandler cancels when the store is about to begin a
// transaction and ready reports true, so that transaction's write runs with a
// cancelled context. It drives the real importer and store without timing
// waits or production hooks.
type cancelOnTxBeginHandler struct {
	slog.Handler

	mu       sync.Mutex
	cancel   context.CancelFunc
	ready    func() bool
	canceled bool
}

func (*cancelOnTxBeginHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelOnTxBeginHandler) Handle(_ context.Context, record slog.Record) error {
	// ready queries the store, which logs too; only the begin line locks.
	if record.Message != "sql tx begin" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.canceled && h.ready() {
		h.cancel()
		h.canceled = true
	}
	return nil
}

// A cancellation that fails a write for the last message of a thread must not
// let the thread checkpoint advance past it, so the resumed import retries the
// thread and stores what the cancelled run skipped.
//
// Not parallel: it replaces the process-wide slog default.
func TestImportDYI_CancelledWriteDoesNotCheckpointPastThread(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	root := writeMultiThreadFixture(t, 3)

	bodyOf := func(ctx context.Context, snippet string) sql.NullString {
		var body sql.NullString
		err := st.DB().QueryRowContext(ctx, st.Rebind(`
			SELECT mb.body_text FROM messages m
			LEFT JOIN message_bodies mb ON mb.message_id = m.id
			WHERE m.snippet = ?`), snippet).Scan(&body)
		require.NoError(err)
		return body
	}

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// The second thread's only message is written first; the transaction
	// that follows is its body write.
	handler := &cancelOnTxBeginHandler{
		Handler: slog.DiscardHandler,
		cancel:  cancel,
		ready: func() bool {
			var n int
			require.NoError(st.DB().QueryRowContext(queryCtx, st.Rebind(
				`SELECT COUNT(*) FROM messages WHERE snippet = ?`), "hello from 1").Scan(&n))
			return n > 0
		},
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, err := ImportDYI(ctx, st, ImportOptions{
		Me:             "test.user@facebook.messenger",
		RootDir:        root,
		Format:         formatAuto,
		AttachmentsDir: t.TempDir(),
	})
	require.ErrorIs(err, context.Canceled)
	require.True(handler.canceled)
	require.False(bodyOf(t.Context(), "hello from 1").Valid,
		"cancellation must land before the second thread's body write")

	slog.SetDefault(previous)
	summary := importFixture(t, st, root)
	assert.True(summary.WasResumed)
	assert.Equal(sql.NullString{String: "hello from 1", Valid: true}, bodyOf(t.Context(), "hello from 1"),
		"the resumed import must retry the thread whose write was cancelled")
}
