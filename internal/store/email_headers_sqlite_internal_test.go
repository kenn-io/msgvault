package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sqliteutil"
)

// Public repair calls do not expose the interval between reserving the writer
// slot and releasing it. Exercise that exact production helper boundary with
// a real second SQLite connection, never a fake transaction or timed race.
func TestEmailHeaderSQLiteWriterReservation(t *testing.T) {
	for _, mode := range []string{"fenced", "fence only", "unscoped", "raw scoped", "unscoped fence no-op"} {
		for _, rollback := range []bool{false, true} {
			name := mode + "/commit"
			if rollback {
				name = mode + "/rollback"
			}
			t.Run(name, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				path := filepath.Join(t.TempDir(), "header-reservation.db")
				st, err := OpenForTest(path)
				requirements.NoError(err)
				t.Cleanup(func() { _ = st.Close() })
				requirements.NoError(st.InitSchema())
				st.db.SetMaxOpenConns(1)
				st.db.SetMaxIdleConns(1)
				source, err := st.GetOrCreateSource("apple-mail", "reservation@example.test")
				requirements.NoError(err)
				conversation, err := st.EnsureConversation(source.ID, "thread", "Synthetic thread")
				requirements.NoError(err)
				id, err := st.UpsertMessage(&Message{
					SourceID: source.ID, ConversationID: conversation,
					SourceMessageID: "reservation", MessageType: MessageTypeEmail,
				})
				requirements.NoError(err)
				runID, err := st.StartSync(source.ID, "import-emlx")
				requirements.NoError(err)
				scoped := st.ScopedToSync(source.ID, runID)

				// A separate database handle cannot accidentally wait for the
				// Store's single-connection pool rather than SQLite's writer slot.
				other, err := sql.Open(sqliteutil.DriverName(), path+"?_busy_timeout=0")
				requirements.NoError(err)
				t.Cleanup(func() { _ = other.Close() })
				contender, err := other.Conn(t.Context())
				requirements.NoError(err)
				t.Cleanup(func() { _ = contender.Close() })
				assertAvailable := func() {
					t.Helper()
					_, beginErr := contender.ExecContext(t.Context(), "BEGIN IMMEDIATE")
					requirements.NoError(beginErr, "independent contender can reserve the writer slot")
					_, endErr := contender.ExecContext(t.Context(), "ROLLBACK")
					requirements.NoError(endErr)
				}
				assertAvailable()
				aborted := errors.New("synthetic rollback after lock proof")
				checkHeld := func(tx *loggedTx) error {
					t.Cleanup(func() { _ = tx.Rollback() })
					if mode == "unscoped fence no-op" {
						if fenceErr := st.fenceSyncGenerationTx(t.Context(), tx); fenceErr != nil {
							return fenceErr
						}
					}
					if mode != "fence only" {
						owner := st
						if mode == "fenced" || mode == "raw scoped" {
							owner = scoped
						}
						if lockErr := owner.lockEmailHeaderRow(t.Context(), tx, id); lockErr != nil {
							return lockErr
						}
					}
					_, beginErr := contender.ExecContext(t.Context(), "BEGIN IMMEDIATE")
					// Undo an unexpectedly acquired reservation before failing,
					// so even a broken lock implementation leaves no open tx.
					if beginErr == nil {
						_, _ = contender.ExecContext(t.Context(), "ROLLBACK")
					}
					var busy sqlite3.Error
					requirements.ErrorAs(beginErr, &busy, "tested transaction must exclude another writer")
					assertions.Equal(sqlite3.ErrBusy, busy.Code)
					if rollback {
						return aborted
					}
					return nil
				}
				switch mode {
				case "raw scoped":
					// First complete an actual fenced transaction. The later
					// raw transaction must not inherit its protection even
					// though it comes from the same scoped Store/database pool.
					requirements.NoError(scoped.withTxContext(t.Context(), func(*loggedTx) error { return nil }))
					tx, beginErr := scoped.db.BeginTx(t.Context(), nil)
					requirements.NoError(beginErr)
					defer func() { _ = tx.Rollback() }()
					err = checkHeld(tx)
					if err == nil {
						err = tx.Commit()
					} else {
						requirements.NoError(tx.Rollback())
					}
				case "fenced", "fence only":
					err = scoped.withTxContext(t.Context(), checkHeld)
				default:
					err = st.withTxContext(t.Context(), checkHeld)
				}
				if rollback {
					requirements.ErrorIs(err, aborted)
				} else {
					requirements.NoError(err)
				}
				assertAvailable()
			})
		}
	}
}

func TestEmailHeaderSQLiteFenceRejectsBeforeCallback(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "header-fence.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	requirements.NoError(st.InitSchema())
	source, err := st.GetOrCreateSource("apple-mail", "fence@example.test")
	requirements.NoError(err)
	runID, err := st.StartSync(source.ID, "import-emlx")
	requirements.NoError(err)
	scoped := st.ScopedToSync(source.ID, runID)
	requirements.NoError(st.FailSync(runID, "synthetic interruption"))
	_, err = st.StartSync(source.ID, "import-emlx")
	requirements.NoError(err)
	entered := false
	err = scoped.withTxContext(t.Context(), func(*loggedTx) error {
		entered = true
		return nil
	})
	requirements.ErrorIs(err, ErrSyncRunSuperseded)
	assertions.False(entered, "superseded generation must reject before any header read/write")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = scoped.withTxContext(ctx, func(*loggedTx) error {
		entered = true
		return nil
	})
	requirements.ErrorIs(err, context.Canceled)
	assertions.False(entered)
}
