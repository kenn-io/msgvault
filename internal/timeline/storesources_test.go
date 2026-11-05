package timeline

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreSourcesSatisfyInterfaces(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	requirements.NoError(err)
	assertions.NotEmpty(src.ArchiveUID)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 12, 0, 0, 123, time.UTC)
	_, err = src.Activity.ListActivityBurstsContext(ctx, store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10})
	requirements.NoError(err)
	_, err = src.Cold.CountColdActivityMessagesContext(ctx, store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10})
	requirements.NoError(err)
	_, err = src.Calendar.ListCalendarOccurrencesContext(ctx, store.CalendarOccurrenceQuery{From: now, To: now.Add(time.Hour)})
	requirements.NoError(err)
	_, err = src.Meetings.ResolveMeetingIdentitiesContext(ctx, store.MeetingIdentityRequest{})
	requirements.NoError(err)
	_, _, err = src.Calls.ListLogicalCallsContext(ctx, store.LogicalCallQuery{After: now, Before: now.Add(time.Hour), Limit: 10})
	requirements.NoError(err)
	_, err = src.Loops.List(ctx, loops.Options{Limit: 10}, now)
	requirements.NoError(err)
	_, err = src.People.PersonTiersContext(ctx, nil, store.PersonTierOptions{})
	requirements.NoError(err)
	_, err = src.Notes.ListDailyNoteEntriesContext(ctx, "2026-10-22", 10, 0)
	requirements.NoError(err)
	_, err = src.Briefs.GetPersonBriefContext(ctx, 999, 0)
	requirements.ErrorIs(err, store.ErrPersonBriefNotFound)
	report, err := src.Inventory.Build(ctx, now)
	requirements.NoError(err)
	assertions.Equal(now, report.AsOf)
}

func TestFreshnessCalDAVNeverSyncedHasRecoveryCommand(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal", URL: "https://calendar.example.com/dav/", Username: "owner@example.com"}}
	_, err := st.GetOrCreateSource("caldav", `["personal","https://calendar.example.com/dav/work/"]`)
	requirements.NoError(err)
	src, err := NewStoreSources(st, cfg)
	requirements.NoError(err)
	report, err := src.Inventory.Build(t.Context(), time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	requirements.Len(report.Sources, 1)
	assertions.Equal("never_synced", report.Sources[0].Status)
	assertions.Equal("msgvault sync-caldav personal", report.Sources[0].SyncCommand)
}

func TestFreshnessICSImportIsStatic(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	for i := range 32 {
		_, err := st.GetOrCreateSource("ics", fmt.Sprintf("example-%d.ics", i))
		requirements.NoError(err)
	}
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	requirements.NoError(err)
	report, err := src.Inventory.Build(t.Context(), time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	requirements.NoError(err)
	assertions.Empty(report.Sources)
	v := budgetTimeline(0)
	v.Freshness = report
	out, err := Fit(v, "json", 2048)
	requirements.NoError(err)
	assertions.LessOrEqual(encodedSize(t, out, "json"), 2048)
}

func TestFreshnessSourcesOnlyIncludeIssues(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, status string
		errors                int
		disabled, stale, keep bool
	}{
		{name: "current"},
		{name: "stale", stale: true, status: "stale", keep: true},
		{name: "failed", outcome: "failed", status: "current", keep: true},
		{name: "cancelled", outcome: "cancelled", status: "current", keep: true},
		{name: "paused", outcome: "paused", status: "current", keep: true},
		{name: "partial", errors: 2, status: "current", keep: true},
		{name: "disabled", disabled: true, status: "current", keep: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := testutil.NewTestStore(t)
			source, err := st.GetOrCreateSource("gmail", "owner@example.test")
			requirements.NoError(err)
			run, err := st.StartSync(source.ID, "incremental")
			requirements.NoError(err)
			requirements.NoError(st.CompleteSync(run, "cursor"))
			_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET errors_count = ? WHERE id = ?"), tc.errors, run)
			requirements.NoError(err)
			if tc.outcome != "" {
				run, err = st.StartSync(source.ID, "incremental")
				requirements.NoError(err)
				_, err = st.DB().Exec(st.Rebind("UPDATE sync_runs SET status = ? WHERE id = ?"), tc.outcome, run)
				requirements.NoError(err)
			}
			cfg := config.NewDefaultConfig()
			cfg.Accounts = []config.AccountSchedule{{Email: "owner@example.test", Enabled: !tc.disabled}}
			now := time.Now().UTC()
			if tc.stale {
				now = now.Add(48 * time.Hour)
			}
			src, err := NewStoreSources(st, cfg)
			requirements.NoError(err)
			report, err := src.Inventory.Build(t.Context(), now)
			requirements.NoError(err)
			if tc.keep {
				requirements.Len(report.Sources, 1)
				assertions.Equal(tc.status, report.Sources[0].Status)
				assertions.Contains(Markdown(&Timeline{Freshness: report}), "gmail:owner@example.test")
			} else {
				assertions.Empty(report.Sources)
			}
		})
	}
}

func TestFreshnessHiddenSourceChangeLeavesScopedReportUnchanged(t *testing.T) {
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	visible, err := st.GetOrCreateSource("gmail", "visible@example.com")
	requirements.NoError(err)
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	requirements.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.ID})
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	before, err := src.Inventory.Build(ctx, now)
	requirements.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "hidden@example.com")
	requirements.NoError(err)
	after, err := src.Inventory.Build(ctx, now)
	requirements.NoError(err)
	assert.Equal(t, before, after)
}

func TestStoreSourcesDefaultLoadedTimezoneHint(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, tc := range []struct {
		name, body, zone string
		hint             bool
	}{
		{"omitted", "", "UTC", true},
		{"explicit UTC", "[activity]\ntimezone = \"UTC\"\n", "UTC", true},
		{"configured IANA", "[activity]\ntimezone = \"America/Vancouver\"\n", "America/Vancouver", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tc.body), 0o600))
			cfg, err := config.Load(path, "")
			requirements.NoError(err)
			src, err := NewStoreSources(st, cfg)
			requirements.NoError(err)
			assertions.Equal(tc.zone, src.Location.String())
			assertions.Equal(tc.hint, src.ZoneDefault)
		})
	}
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	require.NoError(t, err)
	assert.True(t, src.ZoneDefault)
}
