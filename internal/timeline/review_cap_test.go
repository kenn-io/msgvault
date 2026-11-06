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
	require := require.New(t)
	require.NoError(err)
	require.NotNil(got.Person)
	assert := assert.New(t)
	assert.Equal(1, got.Person.InteractionCount)
	require.Len(got.History, 1)
	assert.WithinDuration(past, got.History[0].Time, time.Microsecond)
	assert.Equal(1, got.History[0].Burst.Inbound)
	assert.Equal([]int64{id}, got.History[0].Burst.LatestMessageIDs)
}

func TestBuildDayIncludesOverlappingMeeting(t *testing.T) {
	for _, date := range []string{"2026-10-21", "2026-10-22"} {
		t.Run(date, func(t *testing.T) {
			f := newTimelineFixture(t)
			f.calendar("overnight", strings.NewReplacer("2026-10-22T10:00:00Z", "2026-10-21T23:30:00Z", "2026-10-22T11:00:00Z", "2026-10-22T00:30:00Z").Replace(singleDayEvent))
			req := dayRequest()
			req.Date = date
			got, err := Build(t.Context(), f.src, req)
			require := require.New(t)
			require.NoError(err)
			entries := allDayEntries(got)
			require.Len(entries, 1)
			assert := assert.New(t)
			assert.Equal(date, got.Days[0].Date)
			assert.Equal("2026-10-21T23:30:00Z", entries[0].Time.Format(time.RFC3339))
			assert.Equal(entries[0].Time, entries[0].Meeting.Start)
			assert.Equal("2026-10-22T00:30:00Z", entries[0].Meeting.End.Format(time.RFC3339))
		})
	}
}
