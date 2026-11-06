package store_test

import (
	"slices"
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
	var completedAt *time.Time
	if r.status != "running" && r.status != "paused" {
		completedAt = &at
	}
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (source_id,sync_type,started_at,completed_at,status,cursor_after) VALUES (?,?,?,?,?,?)`), source, r.syncType, at.Add(-time.Minute), completedAt, r.status, r.cursor)
	require.NoError(t, err)
}

func occurrenceTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return at
}

func occurrenceSource(t *testing.T, st *store.Store, name string) int64 {
	t.Helper()
	src, err := st.GetOrCreateSource("gcal", name)
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"primary","time_zone":"UTC"}`))
	return src.ID
}

func occurrenceWindow(t *testing.T) store.CalendarOccurrenceQuery {
	t.Helper()
	return store.CalendarOccurrenceQuery{
		From: occurrenceTime(t, "2027-03-01T00:00:00Z"),
		To:   occurrenceTime(t, "2027-04-12T00:00:00Z"),
	}
}

func TestCalendarCoverageStates(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, want, displayName string
		runs                            []coverageRun
		asof                            bool
		resetCursor, incremental        bool
	}{
		{name: "never synced", want: "no_baseline"},
		{name: "bounded only", cursor: "token", want: "no_baseline", runs: []coverageRun{{"completed", "", "full", time.Hour}}},
		{name: "cursor without history", cursor: "token", want: "no_baseline"},
		{name: "running", cursor: "token", want: "complete", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"running", "", "full", time.Minute}}, asof: true},
		{name: "paused", cursor: "token", want: "complete", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"paused", "", "full", time.Minute}}, asof: true},
		{name: "bounded does not refresh", cursor: "token", want: "stale", runs: []coverageRun{{"completed", "token", "full", 25 * time.Hour}, {"completed", "", "full", time.Minute}}, asof: true},
		{name: "failed", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"failed", "", "full", 30 * time.Minute}}, asof: true},
		{name: "same timestamp failure with higher id", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"failed", "", "full", time.Hour}}, asof: true},
		{name: "cancelled", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"cancelled", "", "full", 30 * time.Minute}}, asof: true},
		{name: "failure survives later runs", cursor: "token", want: "sync_failed", runs: []coverageRun{{"completed", "token", "full", time.Hour}, {"failed", "", "full", 30 * time.Minute}, {"running", "", "full", 3 * time.Minute}, {"paused", "", "full", 2 * time.Minute}, {"completed", "", "full", time.Minute}}, asof: true},
		{name: "recovered", cursor: "token", want: "complete", runs: []coverageRun{{"failed", "", "full", time.Hour}, {"completed", "token", "full", time.Minute}}, asof: true},
		{name: "cursor cleared", want: "no_baseline", asof: true, resetCursor: true},
		{name: "incremental without full baseline", cursor: "next-token", want: "no_baseline", runs: []coverageRun{{"completed", "next-token", "incremental", time.Minute}}},
		{name: "incremental refreshes stale baseline", cursor: "copied-token", want: "complete", runs: []coverageRun{{"completed", "full-token", "full", 25 * time.Hour}}, asof: true, incremental: true},
		{name: "display name", want: "no_baseline", displayName: "Work calendar"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			src := occurrenceSource(t, st, "coverage@example.com")
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor=?,display_name=? WHERE id=?`), tc.cursor, tc.displayName, src)
			requirements.NoError(err)
			var wantAsOf *time.Time
			if tc.resetCursor {
				baseline, err := st.StartSync(src, "full")
				requirements.NoError(err)
				requirements.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), baseline, src, "baseline-token"))
				before, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
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
				if tc.asof {
					before, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
					requirements.NoError(err)
					requirements.Len(before.Coverage.Sources, 1)
					assertions.Empty(before.Occurrences)
					assertions.Equal("no_baseline", before.Coverage.Sources[0].Status)
					assertions.Nil(before.Coverage.Sources[0].AsOf)
					assertions.False(before.Coverage.Complete)
					var completedAt time.Time
					requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`SELECT completed_at FROM sync_runs WHERE id=?`), incremental).Scan(&completedAt))
					wantAsOf = new(completedAt.UTC())
				}
			}
			now := time.Now().UTC()
			for _, r := range tc.runs {
				coverageRunFixture(t, st, src, now, r)
			}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
			requirements.NoError(err)
			requirements.Len(page.Coverage.Sources, 1)
			c := page.Coverage.Sources[0]
			wantName := "coverage@example.com"
			if tc.displayName != "" {
				wantName = tc.displayName
			}
			assertions.Equal(wantName, c.Name)
			assertions.Equal(tc.want, c.Status)
			assertions.Equal(tc.asof, c.AsOf != nil)
			if wantAsOf != nil {
				assertions.Equal(wantAsOf, c.AsOf)
			}
			assertions.Empty(page.Occurrences)
			assertions.Equal(tc.want == "complete", page.Coverage.Complete)
			assertions.Contains(page.Coverage.Notice, "does not prove free time")
		})
	}
	t.Run("empty archive", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st := testutil.NewTestStore(t)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
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
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor='token',last_sync_at=? WHERE id=?`), time.Now().UTC(), source)
		requirements.NoError(err)
		age := time.Hour
		if source == retired {
			age = 25 * time.Hour
		}
		coverageRunFixture(t, st, source, time.Now().UTC(), coverageRun{"completed", "token", "full", age})
	}
	_, err := st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: retired, IntoSourceID: active})
	requirements.NoError(err)

	page, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
	requirements.NoError(err)
	requirements.Len(page.Coverage.Sources, 1)
	assertions.Equal(active, page.Coverage.Sources[0].SourceID)
	assertions.Equal("complete", page.Coverage.Sources[0].Status)
	assertions.True(page.Coverage.Complete)
}

func TestCalendarCoverageEvidenceAcrossPages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	sources, _ := testutil.CalendarArchive(t, st)
	for _, src := range sources {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor='token' WHERE id=?`), src)
		requirements.NoError(err)
		coverageRunFixture(t, st, src, time.Now().UTC(), coverageRun{"completed", "token", "full", time.Hour})
	}
	f := occurrenceWindow(t)
	f.Limit = new(1)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), f)
	requirements.NoError(err)
	assertions.True(page.Coverage.Complete)
	requirements.NotEmpty(page.NextCursor)
	f.Cursor = page.NextCursor
	next, err := st.ListCalendarOccurrencesContext(t.Context(), f)
	requirements.NoError(err)
	assertions.Equal(page.Coverage, next.Coverage)
	f.Cursor = ""
	calendarMergeEvent(t, st, sources[0], "unsupported", `{"id":"unsupported","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=HOURLY;COUNT=3"]}`)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), f)
	requirements.NoError(err)
	requirements.NotEmpty(page.Unexpandable)
	assertions.False(page.Coverage.Complete, "unexpandable series invalidate an otherwise fresh feed")
	requirements.NotEmpty(page.NextCursor)
	f.Cursor = page.NextCursor
	next, err = st.ListCalendarOccurrencesContext(t.Context(), f)
	requirements.NoError(err)
	requirements.Len(next.Occurrences, 1)
	assertions.Empty(next.Unexpandable)
	assertions.False(next.Coverage.Complete)
}

// FuzzCalendarCoverageScope checks the public read against a source-scope oracle.
// Inputs retain invalid and unknown IDs; only store materialization is bounded.
func FuzzCalendarCoverageScope(f *testing.F) {
	f.Add(false, []byte{1, 2, 2})
	f.Add(true, []byte{})
	f.Add(false, []byte{})
	f.Add(false, []byte{255, 1, 0})
	f.Fuzz(func(t *testing.T, unrestricted bool, grants []byte) {
		st := testutil.NewTestStore(t)
		a := occurrenceSource(t, st, "scope-a@example.com")
		b, err := st.GetOrCreateSource("gcal", "scope-b@example.com")
		require.NoError(t, err)
		c, err := st.GetOrCreateSource("mbox", "scope-c@example.com")
		require.NoError(t, err)
		ids := []int64{a, b.ID, c.ID}
		materialize := func(data []byte) []int64 {
			var out []int64
			for _, v := range data {
				id := int64(int8(v))
				if v >= 1 && v <= 3 {
					id = ids[v-1]
				}
				out = append(out, id)
			}
			return out
		}
		granted := materialize(grants)
		ctx := t.Context()
		if !unrestricted {
			ctx = sourcescope.WithSourceIDs(ctx, granted)
		}
		thePage, err := st.ListCalendarOccurrencesContext(ctx, occurrenceWindow(t))
		require.NoError(t, err)
		want := []int64{}
		for _, id := range ids[:2] {
			if unrestricted || slices.Contains(granted, id) {
				want = append(want, id)
			}
		}
		got := []int64{}
		for _, s := range thePage.Coverage.Sources {
			got = append(got, s.SourceID)
		}
		assert.Equal(t, want, got)
	})
}
