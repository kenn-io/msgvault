package store_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

type coverageRun struct {
	status, cursor, syncType string
	age                      time.Duration
}

func coverageRunFixture(t *testing.T, st *store.Store, source int64, now time.Time, r coverageRun) {
	t.Helper()
	at := now.Add(-r.age)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (source_id,sync_type,started_at,completed_at,status,cursor_after) VALUES (?,?,?,?,?,?)`), source, r.syncType, at.Add(-time.Minute), at, r.status, r.cursor)
	require.NoError(t, err)
}

var coverageFrom = time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)

func occurrenceSource(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gcal", name)
	require.NoError(t, err)
	return src.ID
}

func TestCalendarCoverageStates(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, want       string
		runs                     []coverageRun
		asof                     bool
		resetCursor, incremental bool
	}{
		{name: "never synced", want: "no_baseline"},
		{name: "bounded only", cursor: "token", want: "no_baseline", runs: []coverageRun{{"completed", "", "full", time.Hour}}},
		{name: "bounded does not refresh", cursor: "token", want: "stale", runs: []coverageRun{{"completed", "token", "full", 25 * time.Hour}, {"completed", "", "full", time.Minute}}, asof: true},
		{name: "failed", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"failed", "", "full", 30 * time.Minute}}, asof: true},
		{name: "same timestamp failure with higher id", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"failed", "", "full", time.Hour}}, asof: true},
		{name: "cancelled", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"cancelled", "", "full", 30 * time.Minute}}, asof: true},
		{name: "recovered", cursor: "token", want: "complete", runs: []coverageRun{{"failed", "", "full", time.Hour}, {"completed", "token", "full", time.Minute}}, asof: true},
		{name: "cursor cleared", want: "no_baseline", asof: true, resetCursor: true},
		{name: "incremental refreshes stale baseline", cursor: "copied-token", want: "complete", runs: []coverageRun{{"completed", "full-token", "full", 25 * time.Hour}}, asof: true, incremental: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			src := occurrenceSource(t, st, "coverage@example.com")
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor=? WHERE id=?`), tc.cursor, src)
			requirements.NoError(err)
			now := time.Now().UTC()
			for _, r := range tc.runs {
				coverageRunFixture(t, st, src, now, r)
			}
			var wantAsOf *time.Time
			if tc.resetCursor {
				baseline, err := st.StartSync(src, "full")
				requirements.NoError(err)
				requirements.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), baseline, src, "baseline-token"))
				before, err := st.ListCalendarOccurrencesContext(t.Context(), coverageFrom, coverageFrom.AddDate(0, 1, 0))
				requirements.NoError(err)
				requirements.Len(before.Coverage.Sources, 1)
				requirements.True(before.Coverage.Complete)
				wantAsOf = before.Coverage.Sources[0].AsOf
				failed, err := st.StartSync(src, "incremental")
				requirements.NoError(err)
				requirements.NoError(st.FailSyncAndClearSourceCursorContext(t.Context(), failed, src, "expired sync token"))
			}
			if tc.incremental {
				incremental, err := st.StartSync(src, "incremental")
				requirements.NoError(err)
				requirements.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), incremental, src, "next-token"))
				var completedAt time.Time
				requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT completed_at FROM sync_runs WHERE id=?`), incremental).Scan(&completedAt))
				wantAsOf = new(completedAt.UTC())
			}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), coverageFrom, coverageFrom.AddDate(0, 1, 0))
			requirements.NoError(err)
			requirements.Len(page.Coverage.Sources, 1)
			c := page.Coverage.Sources[0]
			assertions.Equal(tc.want, c.Status)
			assertions.Equal(tc.asof, c.AsOf != nil)
			if wantAsOf != nil {
				assertions.Equal(wantAsOf, c.AsOf)
			}
			assertions.Empty(page.Occurrences)
			assertions.Equal(tc.want == "complete", page.Coverage.Complete)
		})
	}
	t.Run("empty archive", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st := testutil.NewTestStore(t)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), coverageFrom, coverageFrom.AddDate(0, 1, 0))
		requirements.NoError(err)
		assertions.False(page.Coverage.Complete)
		assertions.NotNil(page.Coverage.Sources)
		assertions.Empty(page.Coverage.Sources)
		assertions.Contains(page.Coverage.Notice, "does not prove free time")
	})
}

func TestCalendarCoverageExcludesRetiredSources(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	retired := occurrenceSource(t, st, "merged-calendar@example.com")
	active := occurrenceSource(t, st, "active-calendar@example.com")
	for _, source := range []int64{retired, active} {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor='token' WHERE id=?`), source)
		requirements.NoError(err)
		age := time.Hour
		if source == retired {
			age = 25 * time.Hour
		}
		coverageRunFixture(t, st, source, time.Now().UTC(), coverageRun{"completed", "token", "full", age})
	}
	_, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired, IntoSourceID: active})
	requirements.NoError(err)

	page, err := st.ListCalendarOccurrencesContext(t.Context(), coverageFrom, coverageFrom.AddDate(0, 1, 0))
	requirements.NoError(err)
	requirements.Len(page.Coverage.Sources, 1)
	assertions.Equal(active, page.Coverage.Sources[0].SourceID)
	assertions.Equal("complete", page.Coverage.Sources[0].Status)
	assertions.True(page.Coverage.Complete)
}

func TestCalendarCoveragePermittedSources(t *testing.T) {
	st := testutil.NewTestStore(t)
	allowed := occurrenceSource(t, st, "allowed@example.com")
	occurrenceSource(t, st, "other@example.com")
	page, err := st.ListCalendarOccurrencesContext(sourcescope.WithSourceIDs(t.Context(), []int64{allowed}), coverageFrom, coverageFrom.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, page.Coverage.Sources, 1)
	assert.Equal(t, allowed, page.Coverage.Sources[0].SourceID)
}
