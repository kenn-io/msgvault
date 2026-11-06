package cmd

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/scheduler"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

// A DELETE-journal exclusive transaction gives a real, immediate SQLite read
// failure while the source and incarnation remain unchanged on disk.
func emailPushLockedStore(t *testing.T) (*store.Store, emailPushBinding, func()) {
	t.Helper()
	requirements := require.New(t)
	path := filepath.Join(t.TempDir(), "retry.db")
	st := testutil.NewSQLiteTestStoreAt(t, path)
	src, err := st.GetOrCreateSource(sourceTypeIMAP, "imap://retry@example.test")
	requirements.NoError(err)
	_, token, err := st.BindEmailPushSource(t.Context(), src.ID)
	requirements.NoError(err)
	st.DB().SetMaxOpenConns(1)
	var mode string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), "PRAGMA journal_mode=DELETE").Scan(&mode))
	requirements.Equal("delete", mode)
	_, err = st.DB().ExecContext(t.Context(), "PRAGMA busy_timeout=0")
	requirements.NoError(err)
	dialect := &store.SQLiteDialect{}
	lockingDB, err := sql.Open(dialect.DriverName(), path)
	requirements.NoError(err)
	lockingConn, err := lockingDB.Conn(t.Context())
	requirements.NoError(err)
	_, err = lockingConn.ExecContext(t.Context(), "BEGIN EXCLUSIVE")
	requirements.NoError(err)
	release := sync.OnceFunc(func() {
		_, err := lockingConn.ExecContext(context.Background(), "ROLLBACK")
		requirements.NoError(err)
		requirements.NoError(lockingConn.Close())
		requirements.NoError(lockingDB.Close())
	})
	t.Cleanup(release)
	_, err = st.EmailPushBoundSource(t.Context(), src.ID, token)
	requirements.True(dialect.IsBusyError(err), "fixture must produce real read contention")
	return st, emailPushBinding{sourceID: src.ID, sourceType: src.SourceType, identifier: src.Identifier, token: token}, release
}

func TestEmailPushBindingPreservesTransientStoreFailureProperty(t *testing.T) {
	st, binding, release := emailPushLockedStore(t)
	dialect := &store.SQLiteDialect{}
	hegel.Test(t, func(ht *hegel.T) {
		assertions := assert.New(ht)
		reads := 1 + min(hegel.Draw(ht, hegel.Integers(0, math.MaxInt)), 7)
		for range reads {
			_, err := binding.current(t.Context(), st)
			assertions.True(dialect.IsBusyError(err), "preserve the underlying retryable error")
			assertions.NotErrorIs(err, errEmailPushBinding, "contention does not retire a source")
		}
	})
	release()
	src, err := binding.current(t.Context(), st)
	require.NoError(t, err)
	assert.Equal(t, binding.sourceID, src.ID, "unlocking restores the same incarnation")
}

type emailPushRetryHandler struct {
	retry chan struct{}
	once  sync.Once
}

func (h *emailPushRetryHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *emailPushRetryHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "email push IMAP reconnecting; polling fallback retained" {
		h.once.Do(func() { close(h.retry) })
	}
	return nil
}
func (h *emailPushRetryHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *emailPushRetryHandler) WithGroup(string) slog.Handler      { return h }

func TestEmailPushListenerRetriesTransientStoreRead(t *testing.T) {
	requirements := require.New(t)
	st, binding, release := emailPushLockedStore(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.Data.DataDir = cfg.HomeDir
	state := testInvocationWithConfig(cfg)
	retry := make(chan struct{})
	state.logger = slog.New(&emailPushRetryHandler{retry: retry})
	s := scheduler.New(func(context.Context, string) error { return errors.New("unexpected sync") }).WithLogger(slog.New(slog.DiscardHandler))
	requirements.NoError(s.AddAccount(binding.identifier, "0 0 * * *"))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); runIMAPPushListener(ctx, binding, st, s, state) }()
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			requirements.FailNow("listener did not join after cancellation")
		}
		<-s.Stop().Done()
	})
	select {
	case <-retry:
	case <-done:
		requirements.FailNow("transient store failure permanently stopped the listener")
	case <-time.After(10 * time.Second):
		requirements.FailNow("listener did not admit a retry")
	}
}

func TestEmailPushStrictDurableDebt(t *testing.T) {
	for _, checkpoint := range []bool{false, true} {
		t.Run(map[bool]string{false: "item-error", true: "checkpoint"}[checkpoint], func(t *testing.T) {
			requirements := require.New(t)
			st, cfg, src := emailPushFixture(t)
			s := scheduler.New(func(context.Context, string) error { return errors.New("unexpected ordinary sync") }).WithLogger(slog.New(slog.DiscardHandler))
			t.Cleanup(func() { <-s.Stop().Done() })
			_, failures := s.AddAccountsFromConfig(cfg)
			requirements.Empty(failures)
			bindings, err := emailPushBindings(t.Context(), cfg, st, s)
			requirements.NoError(err)
			s.WithAccountWakeSyncFunc(emailPushWakeSync(bindings, st, func(context.Context, *store.Source) (*gmail.SyncSummary, error) {
				return &gmail.SyncSummary{}, nil
			}))
			run, err := st.StartSync(src.ID, "incremental")
			requirements.NoError(err)
			if checkpoint {
				requirements.NoError(st.PauseSyncWithCheckpoint(run, &store.Checkpoint{PageToken: "resume-cursor"}))
			} else {
				requirements.NoError(st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: run, SourceMessageID: "retry-message", Phase: "fetch", Status: store.SyncRunItemStatusError, ErrorKind: "fetch_error"}))
				requirements.NoError(st.CompleteSync(run, "1001"))
			}
			requirements.ErrorIs(s.WakeAccountAndWait(t.Context(), src.Identifier), errEmailPushIncomplete, "a clean callback cannot erase durable debt")
			recovered, err := st.StartSync(src.ID, "incremental")
			requirements.NoError(err)
			if !checkpoint {
				requirements.NoError(st.RecordSyncRunItem(store.SyncRunItem{SyncRunID: recovered, SourceMessageID: "retry-message", Phase: "fetch", Status: store.SyncRunItemStatusResolved, ErrorKind: "acquired"}))
			}
			requirements.NoError(st.CompleteSync(recovered, "1002"))
			requirements.NoError(s.WakeAccountAndWait(t.Context(), src.Identifier), "completed recovery permits the same source to acknowledge")
		})
	}
}
