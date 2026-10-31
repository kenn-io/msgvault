package timeline

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	assertpkg "github.com/stretchr/testify/assert"
	requirepkg "github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreSourcesSatisfyInterfaces(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	require.NoError(err)
	assert.NotEmpty(src.ArchiveUID)
	ctx := t.Context()
	now := time.Date(2026, 10, 22, 12, 0, 0, 123, time.UTC)
	_, err = src.Activity.ListActivityBurstsContext(ctx, store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10})
	require.NoError(err)
	_, err = src.Cold.CountColdActivityMessagesContext(ctx, store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10})
	require.NoError(err)
	_, err = src.Calendar.ListCalendarOccurrencesContext(ctx, store.CalendarOccurrenceQuery{From: now, To: now.Add(time.Hour)})
	require.NoError(err)
	_, err = src.Meetings.ResolveMeetingIdentitiesContext(ctx, store.MeetingIdentityRequest{})
	require.NoError(err)
	_, _, err = src.Calls.ListLogicalCallsContext(ctx, store.LogicalCallQuery{After: now, Before: now.Add(time.Hour), Limit: 10})
	require.NoError(err)
	_, err = src.Loops.List(ctx, loops.Options{Limit: 10}, now)
	require.NoError(err)
	_, err = src.People.PersonTiersContext(ctx, nil, store.PersonTierOptions{})
	require.NoError(err)
	_, err = src.Notes.ListDailyNoteEntriesContext(ctx, "2026-10-22", 10, 0)
	require.NoError(err)
	_, err = src.Briefs.GetPersonBriefContext(ctx, 999, 0)
	require.ErrorIs(err, store.ErrPersonBriefNotFound)
	report, err := src.Inventory.Build(ctx, now)
	require.NoError(err)
	assert.Equal(now, report.AsOf)
}

func TestFreshnessCalDAVNeverSyncedHasRecoveryCommand(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	cfg := config.NewDefaultConfig()
	cfg.CalDAV.Sources = []config.CalDAVSource{{Name: "personal", URL: "https://calendar.example.com/dav/", Username: "owner@example.com"}}
	_, err := st.GetOrCreateSource("caldav", `["personal","https://calendar.example.com/dav/work/"]`)
	require.NoError(err)
	src, err := NewStoreSources(st, cfg)
	require.NoError(err)
	report, err := src.Inventory.Build(t.Context(), time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(report.Sources, 1)
	assert.Equal("never_synced", report.Sources[0].Status)
	assert.Equal("msgvault sync-caldav personal", report.Sources[0].SyncCommand)
}

func TestFreshnessICSImportIsStatic(t *testing.T) {
	assert := assertpkg.New(t)
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	_, err := st.GetOrCreateSource("ics", "example.ics")
	require.NoError(err)
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	require.NoError(err)
	report, err := src.Inventory.Build(t.Context(), time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC))
	require.NoError(err)
	require.Len(report.Sources, 1)
	assert.Equal("static", report.Sources[0].Scheduling)
	assert.Empty(report.Sources[0].SyncCommand)
	assert.Equal("current", report.Sources[0].Status)
}

func TestFreshnessHiddenSourceChangeLeavesScopedReportUnchanged(t *testing.T) {
	require := requirepkg.New(t)
	st := testutil.NewTestStore(t)
	visible, err := st.GetOrCreateSource("gmail", "visible@example.com")
	require.NoError(err)
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	require.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{visible.ID})
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	before, err := src.Inventory.Build(ctx, now)
	require.NoError(err)
	_, err = st.GetOrCreateSource("gmail", "hidden@example.com")
	require.NoError(err)
	after, err := src.Inventory.Build(ctx, now)
	require.NoError(err)
	assertpkg.Equal(t, before, after)
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
			assert := assertpkg.New(t)
			require := requirepkg.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(os.WriteFile(path, []byte(tc.body), 0o600))
			cfg, err := config.Load(path, "")
			require.NoError(err)
			src, err := NewStoreSources(st, cfg)
			require.NoError(err)
			assert.Equal(tc.zone, src.Location.String())
			assert.Equal(tc.hint, src.ZoneDefault)
		})
	}
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	requirepkg.NoError(t, err)
	assertpkg.True(t, src.ZoneDefault)
}
