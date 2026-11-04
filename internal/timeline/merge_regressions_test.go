package timeline

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func composeCalendarEntries(t *testing.T, loc *time.Location, now time.Time, occurrences []store.CalendarOccurrence) ([]Entry, []Entry) {
	t.Helper()
	timeline := &Timeline{Mode: ModeDay, Timezone: loc.String(), Now: now, Window: Window{From: "2026-10-22", To: "2026-10-22"}}
	c := composer{ctx: t.Context(), req: Request{Mode: ModeDay, Scope: Scope{Owner: true}}, t: timeline, loc: loc, entryDates: map[string]string{}}
	entries, upcoming := c.mergeEntries(nil, nil, occurrences, nil, nil)
	return entries, upcoming
}

func TestMergeEntriesAllDayUsesActivityZoneForBothBounds(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	loc := time.FixedZone("activity-west", -12*60*60)
	occurrence := store.CalendarOccurrence{
		Ref: "evt:21", MessageID: 21, AllDay: true, StartDate: "2026-10-22", EndDate: "2026-10-23",
		Start: time.Date(2026, 10, 21, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
	}
	entries, _ := composeCalendarEntries(t, loc, time.Date(2026, 10, 22, 18, 0, 0, 0, time.UTC), []store.CalendarOccurrence{occurrence})
	requirements.Len(entries, 1)
	assertions.Equal(time.Date(2026, 10, 22, 0, 0, 0, 0, loc), entries[0].Meeting.Start)
	assertions.Equal(time.Date(2026, 10, 23, 0, 0, 0, 0, loc), entries[0].Meeting.End)
	assertions.True(entries[0].Meeting.End.After(entries[0].Meeting.Start))
}

func TestMergeEntriesOmitsCancelledMeetingFromUpcomingAndRendersStatus(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	occurrence := store.CalendarOccurrence{Ref: "evt:22", MessageID: 22, Subject: "Planning", Status: "cancelled", Start: now.Add(time.Hour), End: now.Add(2 * time.Hour)}
	entries, upcoming := composeCalendarEntries(t, time.UTC, now, []store.CalendarOccurrence{occurrence})
	requirements.Len(entries, 1)
	assertions.Empty(upcoming)
	assertions.Equal("cancelled", entries[0].Meeting.Status)
	got := Markdown(&Timeline{Mode: ModeDay, Timezone: "UTC", Window: Window{From: "2026-10-22", To: "2026-10-22"}, Days: []Day{{Date: "2026-10-22", Meetings: entries}}})
	assertions.Contains(got, "[cancelled]")
}
