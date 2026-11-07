package calsync

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/gcal"
	"go.kenn.io/msgvault/internal/testutil"
)

// cancelOnTxBeginHandler cancels when the store is about to begin a
// transaction and ready reports true, so that transaction's write runs with a
// cancelled context.
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

// A cancellation that fails the search-index write for the last event of a
// page must not checkpoint the next page token, so the resumed full sync
// replays the page and indexes that event.
//
// Not parallel: it replaces the process-wide slog default.
func TestFull_CancelledIndexWriteDoesNotCheckpointPage(t *testing.T) {
	assert, require := assert.New(t), require.New(t)
	testutil.SkipIfPostgres(t, "reads the SQLite FTS5 table directly")

	m := gcal.NewMockAPI()
	m.Calendars = []gcal.Calendar{{ID: "primary", AccessRole: "owner"}}
	m.FullEvents["primary"] = [][]gcal.Event{
		{timedEvent("e1", "One", gcal.Attendee{Email: "bob@example.com", DisplayName: "Bob"})},
		{timedEvent("e2", "Two")},
	}
	m.FullSyncToken["primary"] = "T1"
	s, st := newSyncer(t, m, Options{})
	require.True(st.FTS5Available())

	indexed := func(ctx context.Context) bool {
		var n int
		require.NoError(st.DB().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM messages m
			WHERE m.source_message_id = 'e1'
			  AND EXISTS (SELECT 1 FROM messages_fts f WHERE f.rowid = m.id)`).Scan(&n))
		return n > 0
	}

	queryCtx := context.WithoutCancel(t.Context())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// e1's attendee recipients are its last write before the index write.
	handler := &cancelOnTxBeginHandler{
		Handler: slog.DiscardHandler,
		cancel:  cancel,
		ready: func() bool {
			var n int
			require.NoError(st.DB().QueryRowContext(queryCtx, `
				SELECT COUNT(*) FROM messages m
				WHERE m.source_message_id = 'e1'
				  AND EXISTS (
					SELECT 1 FROM message_recipients mr
					WHERE mr.message_id = m.id AND mr.recipient_type = 'to'
				)`).Scan(&n))
			return n > 0
		},
	}
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previous) })

	_, err := s.Full(ctx)
	require.ErrorIs(err, context.Canceled)
	require.True(handler.canceled)
	_, ok := getMsg(t, st, primarySource(t, st).ID, "e1")
	require.True(ok, "e1 is stored before cancellation")
	require.False(indexed(t.Context()), "cancellation must fail e1's index write")

	slog.SetDefault(previous)
	_, err = s.Full(t.Context())
	require.NoError(err)
	assert.True(indexed(t.Context()), "the resumed sync must replay the page and index e1")
}
