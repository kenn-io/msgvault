package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdentityScopeGCChecksPlanAfterWriterFence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st, err := OpenForTest(filepath.Join(t.TempDir(), "gc-fence.db"))
	requirements.NoError(err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	requirements.NoError(st.InitSchema())
	st.db.SetMaxOpenConns(2)
	st.db.SetMaxIdleConns(2)
	source, err := st.GetOrCreateSource("gmail", "gc-fence@example.test")
	requirements.NoError(err)
	conversation, err := st.EnsureConversation(source.ID, "gc-fence-thread", "Synthetic GC Thread")
	requirements.NoError(err)
	_, err = st.UpsertMessage(&Message{SourceID: source.ID, ConversationID: conversation,
		SourceMessageID: "gc-fence-message", MessageType: "email"})
	requirements.NoError(err)
	requirements.NoError(st.MarkMessageDeleted(source.ID, "gc-fence-message"))
	plan, err := st.PlanGCContext(t.Context())
	requirements.NoError(err)
	requirements.Equal(int64(1), plan.SourceDeleted)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	fence, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = fence.Rollback() }()
	requirements.NoError(st.lockIdentityMutationTxContext(ctx, fence))
	observer, err := st.db.Conn(ctx)
	requirements.NoError(err)
	first := make(chan string, 1)
	var once sync.Once
	// Observe the real SQLite parser on the only connection available to GC.
	// No SQL is replaced: the authorizer permits every operation unchanged.
	requirements.NoError(observer.Raw(func(raw any) error {
		connection, ok := raw.(*sqlite3.SQLiteConn)
		require.True(t, ok)
		connection.RegisterAuthorizer(func(operation int, table, _, _ string) int {
			if table == "archive_metadata" && (operation == sqlite3.SQLITE_INSERT || operation == sqlite3.SQLITE_UPDATE) {
				once.Do(func() { first <- "identity fence" })
			} else if table == "messages" && operation == sqlite3.SQLITE_READ {
				once.Do(func() { first <- "message plan" })
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}))
	requirements.NoError(observer.Close())
	type result struct {
		deleted int64
		err     error
	}
	done := make(chan result, 1)
	go func() {
		deleted, err := st.ExecuteGCContext(ctx, plan)
		done <- result{deleted: deleted, err: err}
	}()
	var reached string
	select {
	case reached = <-first:
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}
	requirements.NoError(fence.Commit())
	assertions.Equal("identity fence", reached, "GC must reserve the writer fence before rechecking its message population")
	select {
	case got := <-done:
		requirements.NoError(got.err)
		assertions.Equal(int64(1), got.deleted)
	case <-ctx.Done():
		requirements.NoError(ctx.Err())
	}
	var remaining int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE source_id = ?`, source.ID).Scan(&remaining))
	assertions.Zero(remaining)
}
