package omi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/jobctx"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportMalformedSummaryPreservesArchivedEvidence(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%t", full), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			st := testutil.NewTestStore(t)
			src, err := st.GetOrCreateSource(SourceType, "work")
			require.NoError(err)
			rows := []Conversation{decodeFixture(t, summaryOnlyFixture)}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				serveOmiPage(w, r, rows)
			}))
			t.Cleanup(server.Close)
			imp := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic"))
			opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: full}
			_, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			ids, err := st.MessageExistsBatch(src.ID, []string{"summary-only"})
			require.NoError(err)
			id := ids["summary-only"]
			require.NotZero(id)
			originalBody, err := st.GetMessageBodyText(id)
			require.NoError(err)
			completed, err := st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)

			rows[0] = decodeFixture(t, strings.Replace(summaryOnlyFixture, `"Initialsummary"`, `42`, 1))
			sum, err := imp.Import(t.Context(), opts)
			require.ErrorContains(err, "unavailable summary evidence")
			assert.Equal(int64(1), sum.Errors)
			assert.Zero(sum.MeetingsUpdated)
			assert.True(sum.CheckpointSaved)
			raw, err := st.GetMessageRaw(id)
			require.NoError(err)
			assert.JSONEq(summaryOnlyFixture, string(raw))
			body, err := st.GetMessageBodyText(id)
			require.NoError(err)
			assert.Equal(originalBody, body)
			last, err := st.GetLastSuccessfulSync(src.ID)
			require.NoError(err)
			assert.Equal(completed.ID, last.ID)
			failed, err := st.GetLatestCheckpointedSyncByType(src.ID, SourceType)
			require.NoError(err)
			assert.Equal(store.SyncStatusFailed, failed.Status)

			repaired := strings.Replace(summaryOnlyFixture, "Initialsummary", "Correctedsummary", 1)
			rows[0] = decodeFixture(t, repaired)
			sum, err = imp.Import(t.Context(), opts)
			require.NoError(err)
			assert.Equal(int64(1), sum.MeetingsUpdated)
			raw, err = st.GetMessageRaw(id)
			require.NoError(err)
			assert.JSONEq(repaired, string(raw))
		})
	}
}

func TestFullImportProgressAcrossMidPagePauses(t *testing.T) {
	testutil.SkipIfPostgres(t, "uses a SQLite trigger to interrupt real archive maintenance")
	for _, tc := range []struct {
		name    string
		tied    bool
		limit   int
		discard bool
	}{
		{name: "descending"},
		{name: "tied", tied: true},
		{name: "limited", tied: true, limit: 3},
		{name: "limited after discard", tied: true, limit: 3, discard: true},
	} {
		for _, cause := range []error{jobctx.ErrRunBudgetExceeded, jobctx.ErrYieldedToWaiter} {
			t.Run(fmt.Sprintf("%s/%s", tc.name, cause), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				st := testutil.NewSQLiteTestStore(t)
				st.DB().SetMaxOpenConns(1)
				src, err := st.GetOrCreateSource(SourceType, "work")
				require.NoError(err)
				provider := newMutableOmiProvider(t, 4, tc.tied)
				server := httptest.NewServer(http.HandlerFunc(provider.serve))
				t.Cleanup(server.Close)
				imp := NewImporter(st, testOmiClient(t, server.URL, "omi_dev_synthetic"))
				opts := ImportOptions{Identifier: "work", AccountEmail: "owner@example.com", Full: true}
				_, err = imp.Import(t.Context(), opts)
				require.NoError(err)
				completed, err := st.GetLastSuccessfulSync(src.ID)
				require.NoError(err)
				opts.Limit = tc.limit
				passes := 4
				if tc.limit != 0 {
					passes = tc.limit
				}
				_, err = st.DB().Exec(`CREATE TABLE synthetic_rewrites (source_message_id TEXT); CREATE TRIGGER synthetic_rewrite AFTER UPDATE OF snippet ON messages BEGIN INSERT INTO synthetic_rewrites VALUES (NEW.source_message_id); END`)
				require.NoError(err)
				var cancel context.CancelCauseFunc
				calls := 0

				// Interrupt the second row after its message commit, leaving its
				// failed conversation maintenance retriable on the next pass.
				_, err = st.DB().Exec(`CREATE TRIGGER synthetic_full_scan_pause BEFORE UPDATE OF message_count ON conversations BEGIN SELECT CASE WHEN synthetic_full_scan_pause() = 1 THEN (WITH RECURSIVE steps(x) AS (SELECT 0 UNION ALL SELECT x+1 FROM steps WHERE x < 1000000000) SELECT SUM(x) FROM steps) ELSE 0 END; END`)
				require.NoError(err)
				for pass := range passes {
					calls = 0
					ctx, stop := context.WithCancelCause(t.Context())
					cancel = stop
					conn, err := st.DB().Conn(t.Context())
					require.NoError(err)
					require.NoError(conn.Raw(func(driverConn any) error {
						sqlite, ok := driverConn.(*sqlite3.SQLiteConn)
						require.True(ok)
						return sqlite.RegisterFunc("synthetic_full_scan_pause", func() int {
							calls++
							if calls == 2 {
								cancel(cause)
								return 1
							}
							return 0
						}, false)
					}))
					require.NoError(conn.Close())
					sum, err := imp.Import(ctx, opts)
					stop(nil)
					require.NoError(err)
					assert.Zero(sum.Errors)
					if pass < passes-1 {
						require.ErrorIs(sum.PauseReason, context.Canceled)
						assert.True(sum.CheckpointSaved)
						last, err := st.GetLastSuccessfulSync(src.ID)
						require.NoError(err)
						assert.Equal(completed.ID, last.ID)
					} else {
						require.NoError(sum.PauseReason, "each pass must advance beyond its completed prefix")
					}
					if pass == 0 && tc.discard {
						provider.discarded[provider.rows[0].ID] = true
					}
				}
				last, err := st.GetLastSuccessfulSync(src.ID)
				require.NoError(err)
				assert.Greater(last.ID, completed.ID)
				for i, row := range provider.rows {
					var rewrites int
					require.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM synthetic_rewrites WHERE source_message_id = ?`, row.ID).Scan(&rewrites))
					want := 2
					if i == 0 {
						want = 1
					} else if i >= passes {
						want = 0
					}
					assert.Equal(want, rewrites, "only the row whose maintenance failed is rewritten on resume")
				}
			})
		}
	}
}
