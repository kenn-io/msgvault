package testutil

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"go.kenn.io/msgvault/internal/store"
)

// cancelAfterCommitHandler cancels synchronously on the first committed
// transaction after which committed reports true. The writes that follow it
// then run with an already-canceled context.
type cancelAfterCommitHandler struct {
	slog.Handler

	mu        sync.Mutex
	cancel    context.CancelFunc
	committed func() bool
	canceled  bool
}

func (*cancelAfterCommitHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *cancelAfterCommitHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message != "sql tx commit" {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.canceled && h.committed() {
		h.cancel()
		h.canceled = true
	}
	return nil
}

func (h *cancelAfterCommitHandler) landed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.canceled
}

// CancelAfterCommit calls cancel synchronously, inside the store's commit log
// line, on the first committed transaction after which committed reports true.
// The writes that follow that commit then run with an already-canceled
// context. The returned function reports whether the cancel has landed.
//
// committed runs while the commit is logged, so it must query through a
// context that the cancel does not reach, such as
// context.WithoutCancel(t.Context()).
//
// It replaces the process-wide slog default and SQL log options until the
// test ends, so tests that call it must not run in parallel.
func CancelAfterCommit(tb testing.TB, cancel context.CancelFunc, committed func() bool) (canceled func() bool) {
	tb.Helper()
	handler := &cancelAfterCommitHandler{Handler: slog.DiscardHandler, cancel: cancel, committed: committed}
	// A commit over the slow threshold logs "sql tx slow" in place of the
	// line the handler waits for.
	store.ConfigureSQLLogging(store.SQLLogOptions{SlowMs: -1})
	tb.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}) })
	previous := slog.Default()
	slog.SetDefault(slog.New(handler))
	tb.Cleanup(func() { slog.SetDefault(previous) })
	return handler.landed
}
