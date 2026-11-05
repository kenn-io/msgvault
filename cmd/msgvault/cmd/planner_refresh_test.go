package cmd

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/query"
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

// plannerQueryDialect records the SQL executed by the real filter endpoint.
type plannerQueryDialect struct {
	query.SQLiteQueryDialect

	listSQL string
}

func (d *plannerQueryDialect) Rebind(statement string) string {
	if strings.Contains(statement, "FROM messages m") {
		d.listSQL = statement
	}
	return d.SQLiteQueryDialect.Rebind(statement)
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
			otherSource := fixture.AddSource(dbtest.SourceOpts{Identifier: "other@example.com"})
			otherConversation := fixture.AddConversation(dbtest.ConversationOpts{SourceID: otherSource})
			fixture.AddMessage(dbtest.MessageOpts{SourceID: otherSource, ConversationID: otherConversation, SentAt: "2010-01-02 00:00:00"})
			dialect := &plannerQueryDialect{}
			adapter := &storeAPIAdapter{store: s}
			server := api.NewServerWithOptions(api.ServerOptions{
				Config: &config.Config{}, Store: adapter,
				Engine: query.NewEngineWithDialect(s.DB(), dialect), Logger: slog.New(slog.DiscardHandler),
			})
			request := func() {
				w := httptest.NewRecorder()
				server.Router().ServeHTTP(w, httptest.NewRequest(http.MethodGet,
					fmt.Sprintf("/api/v1/messages/filter?source_id=%d&after=2010-01-01&before=2010-01-08&limit=500", source), nil))
				require.Equal(http.StatusOK, w.Code, w.Body.String())
				var response api.FilteredMessagesResponse
				require.NoError(json.Unmarshal(w.Body.Bytes(), &response))
				require.Len(response.Messages, 150)
				assert.False(response.HasMore)
				for _, message := range response.Messages {
					assert.Equal(source, message.SourceID)
					assert.Equal("2010-01-01T00:00:00Z", message.SentAt)
				}
			}
			request()
			require.NotEmpty(dialect.listSQL)
			args := []any{source, "2010-01-01 00:00:00", "2010-01-08 00:00:00", 501, 0}
			var held []*sql.Conn
			for range 2 {
				assert.Contains(explainQueryPlan(t, s, dialect.listSQL, args...), "idx_messages_source")
				conn, err := s.DB().Conn(t.Context())
				require.NoError(err)
				held = append(held, conn)
				t.Cleanup(func() { _ = conn.Close() })
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
			assert.Equal("3004 1502", stat, "reload preserves the child's full statistics")
			request()
			// Reserve each checked connection until every serving slot is checked.
			for range 2 {
				assert.Contains(explainQueryPlan(t, s, dialect.listSQL, args...), "idx_messages_sent_at")
				conn, err := s.DB().Conn(t.Context())
				require.NoError(err)
				defer func() { _ = conn.Close() }()
			}
		})
	}
}
