//go:build cgo

package discord

import (
	"context"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestPackageCancellationAfterCommittedMessage(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	if st.IsPostgreSQL() {
		t.Skip("SQLite driver transition hook")
	}
	st.DB().SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	conn, err := st.DB().Conn(t.Context())
	requirements.NoError(err)
	requirements.NoError(conn.Raw(func(raw any) error {
		driver, ok := raw.(*sqlite3.SQLiteConn)
		requirements.True(ok)
		inserts := 0
		driver.RegisterUpdateHook(func(operation int, _ string, table string, _ int64) {
			if operation == sqlite3.SQLITE_INSERT && table == "messages" {
				inserts++
				// Entering the second message's transaction proves the first committed.
				if inserts == 2 {
					cancel()
				}
			}
		})
		return nil
	}))
	requirements.NoError(conn.Close())
	t.Cleanup(func() {
		cleanup := context.WithoutCancel(t.Context())
		conn, err := st.DB().Conn(cleanup)
		requirements.NoError(err)
		defer func() { requirements.NoError(conn.Close()) }()
		requirements.NoError(conn.Raw(func(raw any) error {
			driver, ok := raw.(*sqlite3.SQLiteConn)
			requirements.True(ok)
			driver.RegisterUpdateHook(nil)
			return nil
		}))
	})
	root := packageFixture(t, `[{"ID":"1","Contents":"committed"},{"ID":"2","Contents":"cancel here"},{"ID":"3","Contents":"not reached"}]`)
	summary, err := ImportPackage(ctx, st, root)
	requirements.Error(err)
	checks.ErrorIs(ctx.Err(), context.Canceled)
	var committed int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT count(*) FROM messages`).Scan(&committed))
	checks.GreaterOrEqual(committed, int64(1))
	checks.Equal(committed, summary.MessagesAdded)
	checkPackageRunCounts(t, st, summary)
	var status string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), `SELECT status FROM sync_runs ORDER BY id DESC LIMIT 1`).Scan(&status))
	checks.Equal("failed", status)
}
