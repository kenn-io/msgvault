package store_test

import (
	"database/sql"
	"encoding/json"
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
	status, cursor string
	age            time.Duration
}

func coverageRunFixture(t *testing.T, st *store.Store, source int64, r coverageRun) {
	t.Helper()
	at := time.Now().UTC().Add(-r.age)
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs (source_id,sync_type,started_at,completed_at,status,cursor_after) VALUES (?,'full',?,?,?,?)`), source, at.Add(-time.Minute), at, r.status, r.cursor)
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

func putOccurrenceEvent(t *testing.T, st *store.Store, sourceID int64, event map[string]any) int64 {
	t.Helper()
	providerID, ok := event["id"].(string)
	require.True(t, ok, "event fixture requires a string id")
	conversationID, err := st.EnsureConversation(sourceID, providerID, "Calendar fixture")
	require.NoError(t, err)
	messageID, err := st.UpsertMessage(&store.Message{
		SourceID: sourceID, ConversationID: conversationID, SourceMessageID: providerID,
		MessageType: "calendar_event", Subject: sql.NullString{String: "Planning", Valid: true},
	})
	require.NoError(t, err)
	raw, err := json.Marshal(event)
	require.NoError(t, err)
	require.NoError(t, st.UpsertMessageRawWithFormat(messageID, raw, "gcal_json"))
	return messageID
}

func timedOccurrenceEvent(id, start string) map[string]any {
	at, _ := time.Parse(time.RFC3339, start)
	return map[string]any{
		"id": id, "summary": "Planning", "status": "confirmed", "iCalUID": "planning@example.com",
		"start": map[string]any{"dateTime": start}, "end": map[string]any{"dateTime": at.Add(time.Hour).Format(time.RFC3339)},
		"attendees": []map[string]any{{"email": "invited@example.com", "responseStatus": "accepted"}},
	}
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
		name, cursor, want, latest string
		runs                       []coverageRun
		asof                       bool
	}{
		{name: "never synced", want: "no_baseline"},
		{name: "bounded only", want: "no_baseline", latest: "completed", runs: []coverageRun{{"completed", "", time.Hour}}},
		{name: "cursor without history", cursor: "token", want: "no_baseline"},
		{name: "fresh", cursor: "token", want: "complete", latest: "completed", runs: []coverageRun{{"completed", "token", time.Hour}}, asof: true},
		{name: "stale", cursor: "token", want: "stale", latest: "completed", runs: []coverageRun{{"completed", "token", 25 * time.Hour}}, asof: true},
		{name: "reset", want: "no_baseline", latest: "failed", runs: []coverageRun{{"completed", "token", time.Hour}, {"failed", "", time.Minute}}, asof: true},
		{name: "running", cursor: "token", want: "complete", latest: "running", runs: []coverageRun{{"completed", "token", time.Hour}, {"running", "", time.Minute}}, asof: true},
		{name: "paused", cursor: "token", want: "complete", latest: "paused", runs: []coverageRun{{"completed", "token", time.Hour}, {"paused", "", time.Minute}}, asof: true},
		{name: "bounded does not refresh", cursor: "token", want: "stale", latest: "completed", runs: []coverageRun{{"completed", "token", 25 * time.Hour}, {"completed", "", time.Minute}}, asof: true},
		{name: "failed then running", cursor: "token", want: "sync_failed", latest: "running", runs: []coverageRun{{"completed", "token", time.Hour}, {"failed", "", 10 * time.Minute}, {"running", "", time.Minute}}, asof: true},
		{name: "cancelled then paused", cursor: "token", want: "sync_failed", latest: "paused", runs: []coverageRun{{"completed", "token", time.Hour}, {"cancelled", "", 10 * time.Minute}, {"paused", "", time.Minute}}, asof: true},
		{name: "failed then bounded", cursor: "token", want: "sync_failed", latest: "completed", runs: []coverageRun{{"completed", "token", time.Hour}, {"failed", "", 10 * time.Minute}, {"completed", "", time.Minute}}, asof: true},
		{name: "recovered", cursor: "token", want: "complete", latest: "completed", runs: []coverageRun{{"failed", "", time.Hour}, {"completed", "token", time.Minute}}, asof: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			st := testutil.NewTestStore(t)
			src := occurrenceSource(t, st, "coverage@example.com")
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor=?,last_sync_at=? WHERE id=?`), tc.cursor, time.Now().UTC(), src)
			requirements.NoError(err)
			for _, r := range tc.runs {
				coverageRunFixture(t, st, src, r)
			}
			page, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
			requirements.NoError(err)
			requirements.Len(page.Coverage.Sources, 1)
			c := page.Coverage.Sources[0]
			assertions.Equal(tc.want, c.Status)
			assertions.Equal(tc.latest, c.LatestRunStatus)
			assertions.Equal(tc.asof, c.AsOf != nil)
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
		coverageRunFixture(t, st, source, coverageRun{"completed", "token", age})
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

func TestCalendarCoverageEvidenceAndScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	feed := occurrenceSource(t, st, "empty-feed@example.com")
	other, err := st.GetOrCreateSource("caldav", "other-feed@example.com")
	requirements.NoError(err)
	imported, err := st.GetOrCreateSource("mbox", "imports@example.com")
	requirements.NoError(err)
	margin, err := st.GetOrCreateSource("mbox", "outside@example.com")
	requirements.NoError(err)
	absent, err := st.GetOrCreateSource("mbox", "series-outside@example.com")
	requirements.NoError(err)
	for _, src := range []int64{feed, other.ID} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor='token' WHERE id=?`), src)
		requirements.NoError(err)
		coverageRunFixture(t, st, src, coverageRun{"completed", "token", time.Hour})
	}
	putOccurrenceEvent(t, st, feed, timedOccurrenceEvent("early", "2027-03-01T10:00:00Z"))
	late := timedOccurrenceEvent("late", "2027-03-20T10:00:00Z")
	late["status"] = "cancelled"
	putOccurrenceEvent(t, st, imported.ID, late)
	putOccurrenceEvent(t, st, margin.ID, timedOccurrenceEvent("margin", "2027-02-28T22:00:00Z"))
	master := timedOccurrenceEvent("annual", "2027-01-01T10:00:00Z")
	master["recurrence"] = []string{"RRULE:FREQ=YEARLY"}
	putOccurrenceEvent(t, st, absent.ID, master)
	f := occurrenceWindow(t)
	f.Limit = new(1)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), f)
	requirements.NoError(err)
	requirements.Len(page.Coverage.Sources, 3)
	assertions.Equal([]int64{feed, other.ID, imported.ID}, []int64{page.Coverage.Sources[0].SourceID, page.Coverage.Sources[1].SourceID, page.Coverage.Sources[2].SourceID})
	assertions.Equal("partial_evidence", page.Coverage.Sources[2].Status)
	assertions.Nil(page.Coverage.Sources[2].AsOf)
	assertions.False(page.Coverage.Complete)
	requirements.NotEmpty(page.NextCursor)
	f.Cursor = page.NextCursor
	next, err := st.ListCalendarOccurrencesContext(t.Context(), f)
	requirements.NoError(err)
	assertions.Equal(page.Coverage, next.Coverage)
	f.Cursor = ""
	scopedContext := sourcescope.WithSourceIDs(t.Context(), []int64{feed})
	scoped, err := st.ListCalendarOccurrencesContext(scopedContext, f)
	requirements.NoError(err)
	requirements.Len(scoped.Coverage.Sources, 1)
	assertions.Equal(feed, scoped.Coverage.Sources[0].SourceID)
	assertions.True(scoped.Coverage.Complete)
	emptyScope, err := st.ListCalendarOccurrencesContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), f)
	requirements.NoError(err)
	assertions.Empty(emptyScope.Coverage.Sources)
	assertions.False(emptyScope.Coverage.Complete)
	bad := timedOccurrenceEvent("unsupported", "2027-03-01T10:00:00Z")
	bad["recurrence"] = []string{"RRULE:FREQ=DAILY;BYSECOND=10"}
	putOccurrenceEvent(t, st, feed, bad)
	scoped, err = st.ListCalendarOccurrencesContext(scopedContext, f)
	requirements.NoError(err)
	requirements.NotEmpty(scoped.Unexpandable)
	assertions.False(scoped.Coverage.Complete, "unexpandable series invalidate an otherwise fresh feed")
}

func TestCalendarCoverageKeepsSeriesEvidenceAcrossCursorPages(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	feed := occurrenceSource(t, st, "cursor-feed@example.com")
	imported, err := st.GetOrCreateSource("mbox", "cursor-series@example.com")
	requirements.NoError(err)
	putOccurrenceEvent(t, st, feed, timedOccurrenceEvent("cursor-first", "2027-03-01T10:00:00Z"))
	series := timedOccurrenceEvent("cursor-series", "2027-03-02T10:00:00Z")
	series["recurrence"] = []string{"RRULE:FREQ=DAILY;COUNT=1"}
	putOccurrenceEvent(t, st, imported.ID, series)
	putOccurrenceEvent(t, st, feed, timedOccurrenceEvent("cursor-middle", "2027-03-03T10:00:00Z"))
	putOccurrenceEvent(t, st, feed, timedOccurrenceEvent("cursor-last", "2027-03-04T10:00:00Z"))

	query := occurrenceWindow(t)
	query.Limit = new(1)
	first, err := st.ListCalendarOccurrencesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.NotEmpty(first.NextCursor)
	query.Cursor = first.NextCursor
	second, err := st.ListCalendarOccurrencesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(second.Occurrences, 1)
	assertions.Equal(imported.ID, second.Occurrences[0].SourceID)
	assertions.Equal(first.Coverage, second.Coverage)
	requirements.Len(second.Coverage.Sources, 2)
	assertions.Contains([]int64{second.Coverage.Sources[0].SourceID, second.Coverage.Sources[1].SourceID}, imported.ID)
	requirements.NotEmpty(second.NextCursor)
	query.Cursor = second.NextCursor
	third, err := st.ListCalendarOccurrencesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(third.Occurrences, 1)
	assertions.Equal(first.Coverage, third.Coverage)
	requirements.NotEmpty(third.NextCursor)
	query.Cursor = third.NextCursor
	fourth, err := st.ListCalendarOccurrencesContext(t.Context(), query)
	requirements.NoError(err)
	requirements.Len(fourth.Occurrences, 1)
	assertions.Equal(first.Coverage, fourth.Coverage)
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
		b, err := st.GetOrCreateSource("caldav", "scope-b@example.com")
		require.NoError(t, err)
		c, err := st.GetOrCreateSource("mbox", "scope-c@example.com")
		require.NoError(t, err)
		ids := []int64{a, b.ID, c.ID}
		putOccurrenceEvent(t, st, c.ID, timedOccurrenceEvent("import", "2027-03-03T10:00:00Z"))
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
		for _, id := range ids {
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

func FuzzCalendarCoverageFailurePersistence(f *testing.F) {
	f.Add([]byte{0, 1, 2}, true)
	f.Add([]byte{}, false)
	f.Fuzz(func(t *testing.T, sequence []byte, tied bool) {
		st := testutil.NewTestStore(t)
		src := occurrenceSource(t, st, "failure@example.com")
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE sources SET sync_cursor='token' WHERE id=?`), src)
		require.NoError(t, err)
		at := time.Now().UTC().Add(-time.Hour)
		insert := func(status, cursor string, started time.Time) {
			_, err := st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO sync_runs(source_id,sync_type,started_at,completed_at,status,cursor_after) VALUES (?,'full',?,?,?,?)`), src, started, at.Add(time.Minute), status, cursor)
			require.NoError(t, err)
		}
		insert("completed", "token", at)
		// The failed run has the same timestamp but a later ID than the baseline.
		insert("failed", "", at)
		for i, v := range sequence[:min(len(sequence), 12)] {
			started := at
			if !tied {
				started = at.Add(time.Duration(i+1) * time.Second)
			}
			insert([]string{"running", "paused", "completed"}[int(v)%3], "", started)
		}
		page, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
		require.NoError(t, err)
		require.Len(t, page.Coverage.Sources, 1)
		assert.Equal(t, "sync_failed", page.Coverage.Sources[0].Status)
	})
}

func TestCalendarCoverageIncludesUnexpandableSourceEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	feed := occurrenceSource(t, st, "feed@example.com")
	imported, err := st.GetOrCreateSource("mbox", "diagnostics@example.com")
	requirements.NoError(err)
	event := timedOccurrenceEvent("bad-imported-series", "2027-03-01T10:00:00Z")
	event["recurrence"] = []string{"RRULE:FREQ=DAILY;BYSECOND=10"}
	putOccurrenceEvent(t, st, imported.ID, event)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
	requirements.NoError(err)
	assertions.Empty(page.Occurrences)
	assertions.Len(page.Unexpandable, 1)
	assertions.False(page.Coverage.Complete)
	requirements.Len(page.Coverage.Sources, 2)
	assertions.Equal(feed, page.Coverage.Sources[0].SourceID)
	assertions.Equal(imported.ID, page.Coverage.Sources[1].SourceID)
	assertions.Equal("partial_evidence", page.Coverage.Sources[1].Status)
}

func TestCalendarCoverageExpiredTokenReset(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src := occurrenceSource(t, st, "reset@example.com")
	baseline, err := st.StartSync(src, "full")
	requirements.NoError(err)
	requirements.NoError(st.CompleteSyncAndUpdateSourceCursorContext(t.Context(), baseline, src, "baseline-token"))
	before, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
	requirements.NoError(err)
	requirements.Len(before.Coverage.Sources, 1)
	requirements.True(before.Coverage.Complete)
	failed, err := st.StartSync(src, "incremental")
	requirements.NoError(err)
	requirements.NoError(st.FailSyncAndClearSourceCursorContext(t.Context(), failed, src, "expired sync token"))
	after, err := st.ListCalendarOccurrencesContext(t.Context(), occurrenceWindow(t))
	requirements.NoError(err)
	requirements.Len(after.Coverage.Sources, 1)
	assertions.Equal("no_baseline", after.Coverage.Sources[0].Status)
	assertions.Equal("failed", after.Coverage.Sources[0].LatestRunStatus)
	assertions.Equal(before.Coverage.Sources[0].AsOf, after.Coverage.Sources[0].AsOf)
	assertions.False(after.Coverage.Complete)
}
