package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Each group of four messages has one active, resolved, historical and unknown
// outcome. Setup uses the real archive schema.
func acquisitionScaleStore(t *testing.T, count int) (*Store, int64) {
	t.Helper()
	var st *Store
	if IsPostgresURL(os.Getenv("MSGVAULT_TEST_DB")) {
		st = newPGStoreInternal(t)
	} else {
		var err error
		st, err = OpenForTest(t.Context(), filepath.Join(t.TempDir(), "scale.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = st.Close() })
		require.NoError(t, st.InitSchema())
	}
	source, err := st.GetOrCreateSource("gmail", "scale@example.com")
	require.NoError(t, err)
	run, err := st.StartSync(source.ID, "full")
	require.NoError(t, err)
	tx, err := st.DB().BeginTx(t.Context(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	stmt, err := tx.PrepareContext(t.Context(), st.Rebind(`INSERT INTO sync_run_items
		(sync_run_id, source_message_id, phase, status, error_kind, error_message)
		VALUES (?, ?, ?, ?, ?, ?)`))
	require.NoError(t, err)
	defer func() { _ = stmt.Close() }()
	for i := range count {
		id := fmt.Sprintf("scale-%05d", i)
		entry := acquisitionOperations[(i/4)%len(acquisitionOperations)]
		phase, kind, status, message := entry.phase, entry.kind, "error", "synthetic request outcome"
		if i%4 == 3 {
			kind = "future_kind"
		}
		if i%4 == 2 {
			status = "skipped"
		}
		_, err = stmt.ExecContext(t.Context(), run, id, phase, status, kind, message)
		require.NoError(t, err)
		switch i % 4 {
		case 1:
			_, err = stmt.ExecContext(t.Context(), run, id, "fetch", "resolved", "acquired", "acquired")
		}
		require.NoError(t, err)
	}
	require.NoError(t, stmt.Close())
	require.NoError(t, tx.Commit())
	return st, source.ID
}

func TestAcquisitionDiagnosticScale(t *testing.T) {
	for _, count := range []int{1000, 10000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			st, sourceID := acquisitionScaleStore(t, count)
			want := AcquisitionDiagnosticSummary{
				ActiveObligations: int64(count / 4), ResolvedObligations: int64(count / 4),
				ActiveErrors: int64(count / 4), HistoricalErrors: int64(count / 2),
				Historical: int64(count / 4), Indeterminate: int64(count / 4),
			}
			for stateIndex, state := range []string{"active", "resolved", "historical", "indeterminate"} {
				for _, limit := range []int{1, 50, 1000} {
					opts := AcquisitionDiagnosticOptions{SourceID: sourceID, State: state, Limit: limit, RemoteImagesEnabled: true}
					start := time.Now()
					seen := 0
					// Two pages exercise the cursor without turning the limit=1
					// measurement into thousands of full-ledger diagnostics.
					for range 2 {
						remaining := count/4 - seen
						report, err := st.DiagnoseAcquisitionContext(t.Context(), opts)
						require.NoError(err)
						require.Len(report.Items, min(limit, remaining))
						assert.Equal(want, report.Summary)
						for _, item := range report.Items {
							assert.Equal(fmt.Sprintf("scale-%05d", seen*4+stateIndex), item.SourceMessageID)
							if state == "resolved" {
								assert.Greater(item.ResolvedByID, item.OutcomeID)
							} else {
								assert.Zero(item.ResolvedByID)
							}
							seen++
						}
						assert.Equal(seen < count/4, report.HasMore)
						if !report.HasMore {
							break
						}
						opts.AfterID = report.NextAfterID
					}
					// The same source-wide totals survive an exhausted cursor.
					opts.AfterID = 1 << 60
					report, err := st.DiagnoseAcquisitionContext(t.Context(), opts)
					require.NoError(err)
					assert.Equal(want, report.Summary)
					assert.Empty(report.Items)
					t.Logf("messages=%d state=%s limit=%d duration=%v", count, state, limit, time.Since(start))
				}
			}
		})
	}
}
