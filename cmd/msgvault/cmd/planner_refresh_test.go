package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/dbtest"
)

// runPlannerRefreshChild uses the test executable to commit statistics in a
// separate process. The production runner owns its pipes, cancellation, and exit.
func runPlannerRefreshChild() {
	db, err := sql.Open("sqlite3", os.Getenv("MSGVAULT_TEST_PLANNER_DB"))
	if err == nil {
		_, err = db.Exec("PRAGMA analysis_limit=0; ANALYZE messages")
		_ = db.Close()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(10)
	}
	if os.Getenv("MSGVAULT_TEST_PLANNER_MODE") == "cancel" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		_, _ = fmt.Fprintln(os.Stdout, "ready")
		<-ctx.Done()
		stop()
	}
	if os.Getenv("MSGVAULT_TEST_PLANNER_MODE") == "fail" {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestRunChildReloadsPlannerStatistics(t *testing.T) {
	for _, mode := range []string{"success", "fail", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			path := filepath.Join(t.TempDir(), "archive.db")
			s := testutil.NewSQLiteTestStoreAt(t, path)
			s.DB().SetMaxOpenConns(2)
			s.DB().SetMaxIdleConns(2)
			fixture := &dbtest.TestDB{DB: s.DB(), T: t}
			source := fixture.AddSource(dbtest.SourceOpts{Identifier: "account@example.com"})
			conversation := fixture.AddConversation(dbtest.ConversationOpts{SourceID: source})
			for i := range 3003 {
				fixture.AddMessage(dbtest.MessageOpts{SourceID: source, ConversationID: conversation,
					SentAt: fmt.Sprintf("%04d-01-01 00:00:00", 2000+i%20)})
			}
			const query = `SELECT id FROM messages WHERE source_id=? AND sent_at>='2010-01-01' AND sent_at<'2010-01-08'`
			var held []*sql.Conn
			for range 2 {
				conn, err := s.DB().Conn(t.Context())
				require.NoError(err)
				held = append(held, conn)
				t.Cleanup(func() { _ = conn.Close() })
				var id, parent, unused int
				var detail string
				require.NoError(conn.QueryRowContext(t.Context(), "EXPLAIN QUERY PLAN "+query, source).Scan(&id, &parent, &unused, &detail))
				assert.Contains(detail, "idx_messages_source")
			}
			for _, conn := range held {
				require.NoError(conn.Close())
			}
			executable, err := os.Executable()
			require.NoError(err)
			previous := daemonCLIExecutableResolver
			daemonCLIExecutableResolver = func() (string, error) { return executable, nil }
			t.Cleanup(func() { daemonCLIExecutableResolver = previous })
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			adapter := &storeAPIAdapter{store: s}
			err = adapter.runChild(ctx, nil, map[string]string{
				"MSGVAULT_TEST_PLANNER_DB": path, "MSGVAULT_TEST_PLANNER_MODE": mode,
			}, "", func(_, _ string) error {
				if mode == "cancel" {
					cancel()
				}
				return nil
			})
			if mode == "success" {
				require.NoError(err)
			} else {
				require.Error(err)
			}
			var stat string
			require.NoError(s.DB().QueryRow(`SELECT stat FROM sqlite_stat1 WHERE idx='idx_messages_source'`).Scan(&stat))
			assert.Equal("3003 3003", stat, "reload preserves the child's full statistics")
			// Reserve each checked connection until every serving slot is checked.
			for range 2 {
				assert.Contains(explainQueryPlan(t, s, query, source), "idx_messages_sent_at")
				conn, err := s.DB().Conn(t.Context())
				require.NoError(err)
				defer func() { _ = conn.Close() }()
			}
		})
	}
}
