package timeline

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelationshipHistoryKeepsPastPartOfBurst(t *testing.T) {
	f := newTimelineFixture(t)
	past := f.now.Add(-time.Hour)
	id := f.add(past, false)
	f.add(f.now.Add(time.Minute), false)
	f.project()
	got, err := Build(t.Context(), f.src, relationshipRequest("email:alice@example.com"))
	require.NoError(t, err)
	require.NotNil(t, got.Person)
	assert.Equal(t, 1, got.Person.InteractionCount)
	require.Len(t, got.History, 1)
	assert.WithinDuration(t, past, got.History[0].Time, time.Microsecond)
	assert.Equal(t, 1, got.History[0].Burst.Inbound)
	assert.Equal(t, []int64{id}, got.History[0].Burst.LatestMessageIDs)
}

func TestBuildDayIncludesOverlappingMeeting(t *testing.T) {
	for _, date := range []string{"2026-10-21", "2026-10-22"} {
		t.Run(date, func(t *testing.T) {
			f := newTimelineFixture(t)
			f.calendar("overnight", strings.NewReplacer("2026-10-22T10:00:00Z", "2026-10-21T23:30:00Z", "2026-10-22T11:00:00Z", "2026-10-22T00:30:00Z").Replace(singleDayEvent))
			req := dayRequest()
			req.Date = date
			got, err := Build(t.Context(), f.src, req)
			require.NoError(t, err)
			entries := allDayEntries(got)
			require.Len(t, entries, 1)
			assert.Equal(t, date, got.Days[0].Date)
			assert.Equal(t, "2026-10-21T23:30:00Z", entries[0].Time.Format(time.RFC3339))
			assert.Equal(t, entries[0].Time, entries[0].Meeting.Start)
			assert.Equal(t, "2026-10-22T00:30:00Z", entries[0].Meeting.End.Format(time.RFC3339))
		})
	}
}
