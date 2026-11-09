package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/personscope"
)

func TestMeetingMetricsDurationCoverageAndUTCMonths(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	ids := append([]int64(nil), fixture.meetingIDs...)

	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	requirements.NoError(err)
	assertions.Equal(int64(4), metrics.Totals.MeetingCount)
	assertions.Equal(int64(3), metrics.Totals.KnownDurationCount)
	assertions.Equal(int64(1), metrics.Totals.UnknownDurationCount)
	assertions.InDelta(6000.0, metrics.Totals.TotalKnownSeconds, 0)
	requirements.NotNil(metrics.Totals.AverageKnownSeconds)
	assertions.InDelta(2000.0, *metrics.Totals.AverageKnownSeconds, 0)
	assertions.Zero(metrics.UndatedCount)
	requirements.NotNil(metrics.FirstMeetingAt)
	requirements.NotNil(metrics.LastMeetingAt)
	assertions.Equal("2026-01-05T09:00:00Z", metrics.FirstMeetingAt.Format("2006-01-02T15:04:05Z07:00"))
	assertions.Equal("2026-02-02T08:00:00Z", metrics.LastMeetingAt.Format("2006-01-02T15:04:05Z07:00"))
	assertions.Equal([]meetingcontent.BasisTotals{
		{Basis: meetingcontent.DurationProvider, Count: 1, TotalSeconds: 1800},
		{Basis: meetingcontent.DurationScheduled, Count: 1, TotalSeconds: 3600},
		{Basis: meetingcontent.DurationTranscriptSpan, Count: 1, TotalSeconds: 600},
	}, metrics.DurationByBasis)
	requirements.Len(metrics.Months, 2)
	assertions.Equal("2026-01", metrics.Months[0].Month)
	assertions.Equal(meetingcontent.DurationTotals{
		MeetingCount: 3, KnownDurationCount: 3, TotalKnownSeconds: 6000,
		AverageKnownSeconds: new(float64(2000)),
	}, metrics.Months[0].Totals)
	assertions.Equal("2026-02", metrics.Months[1].Month)
	assertions.Equal(meetingcontent.DurationTotals{
		MeetingCount: 1, UnknownDurationCount: 1,
	}, metrics.Months[1].Totals)
	assertions.Equal("direct", metrics.Scope.Kind)
}

func TestMeetingMetricsOrdersSQLiteBoundaryOffsetsByInstant(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	earlier := time.Date(2026, time.January, 1, 0, 30, 0, 0, time.FixedZone("UTC+2", 2*60*60))
	later := time.Date(2025, time.December, 31, 23, 0, 0, 0, time.UTC)
	_, err := fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`,
		earlier, fixture.meetingIDs[0])
	requirements.NoError(err)
	_, err = fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`,
		later, fixture.meetingIDs[1])
	requirements.NoError(err)

	ids := []int64{fixture.meetingIDs[0], fixture.meetingIDs[1]}
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	requirements.NoError(err)
	requirements.NotNil(metrics.FirstMeetingAt)
	requirements.NotNil(metrics.LastMeetingAt)
	assertions.Equal(earlier.UTC(), *metrics.FirstMeetingAt)
	assertions.Equal(later.UTC(), *metrics.LastMeetingAt)
	requirements.Len(metrics.Months, 1)
	assertions.Equal("2025-12", metrics.Months[0].Month)
	assertions.Equal(int64(2), metrics.Months[0].Totals.MeetingCount)
}

func TestMeetingMetricsPreservesSubmillisecondExtrema(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	earlier := time.Date(2026, time.January, 1, 0, 0, 0, 100*1000, time.UTC)
	later := time.Date(2026, time.January, 1, 0, 0, 0, 200*1000, time.UTC)
	_, err := fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`,
		earlier, fixture.meetingIDs[1])
	requirements.NoError(err)
	_, err = fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`,
		later, fixture.meetingIDs[0])
	requirements.NoError(err)

	ids := []int64{fixture.meetingIDs[0], fixture.meetingIDs[1]}
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	requirements.NoError(err)
	requirements.NotNil(metrics.FirstMeetingAt)
	requirements.NotNil(metrics.LastMeetingAt)
	assertions.Equal(earlier, *metrics.FirstMeetingAt)
	assertions.Equal(later, *metrics.LastMeetingAt)
}

func TestMeetingMetricsDeletionUndatedAndMissingProjection(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)

	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{})
	requirements.NoError(err)
	assertions.Equal(int64(6), metrics.Totals.MeetingCount)
	assertions.Equal(int64(3), metrics.Totals.KnownDurationCount)
	assertions.Equal(int64(3), metrics.Totals.UnknownDurationCount)
	assertions.Equal(int64(2), metrics.UndatedCount)

	active, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{Deletion: "active"})
	requirements.NoError(err)
	assertions.Equal(int64(5), active.Totals.MeetingCount)
	deleted, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{Deletion: "deleted"})
	requirements.NoError(err)
	assertions.Equal(int64(1), deleted.Totals.MeetingCount)

	empty := []int64{}
	none, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &empty})
	requirements.NoError(err)
	assertions.Equal(meetingcontent.DurationTotals{}, none.Totals)
	assertions.Nil(none.FirstMeetingAt)
	assertions.Nil(none.LastMeetingAt)
	assertions.Empty(none.DurationByBasis)
	assertions.Empty(none.Months)
	encoded, err := json.Marshal(none)
	requirements.NoError(err)
	assertions.Contains(string(encoded), `"duration_by_basis":[]`)
	assertions.Contains(string(encoded), `"months":[]`)

	missingProjection := []int64{fixture.missingProj}
	unknown, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &missingProjection})
	requirements.NoError(err)
	assertions.Equal(int64(1), unknown.Totals.MeetingCount)
	assertions.Equal(int64(1), unknown.Totals.UnknownDurationCount)
	assertions.Nil(unknown.Totals.AverageKnownSeconds)
}

func TestMeetingMetricsScopeIntersectionsAndPersonSemantics(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newMeetingQueryFixture(t)
	after := time.Date(2026, time.January, 6, 0, 0, 0, 0, time.UTC)
	before := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{
		SourceIDs:      []int64{fixture.sourceOne},
		ParticipantIDs: []int64{fixture.attendee},
		Domains:        []string{"EXAMPLE.TEST"},
		After:          &after,
		Before:         &before,
	})
	requirements.NoError(err)
	assertions.Equal(meetingcontent.DurationTotals{
		MeetingCount: 2, KnownDurationCount: 2, TotalKnownSeconds: 2400,
		AverageKnownSeconds: new(float64(1200)),
	}, metrics.Totals)

	exactParticipant, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{
		ParticipantIDs: []int64{fixture.attendee},
	})
	requirements.NoError(err)
	assertions.Equal(int64(6), exactParticipant.Totals.MeetingCount)
	organizerAppearsOnEveryEdge, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{
		ParticipantIDs: []int64{fixture.organizer},
	})
	requirements.NoError(err)
	assertions.Equal(int64(6), organizerAppearsOnEveryEdge.Totals.MeetingCount)
	fromPerson, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{
		Person: &personscope.Scope{
			ParticipantIDs: []int64{fixture.attendee},
			Directions:     []personscope.Direction{personscope.FromPerson},
		},
	})
	requirements.NoError(err)
	assertions.Zero(fromPerson.Totals.MeetingCount)
	emptyPerson, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{
		Person: &personscope.Scope{Directions: []personscope.Direction{personscope.ToPerson}},
	})
	requirements.NoError(err)
	assertions.Zero(emptyPerson.Totals.MeetingCount)

	suffix, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{Domains: []string{"ample.test"}})
	requirements.NoError(err)
	assertions.Zero(suffix.Totals.MeetingCount)
	missingSource, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{SourceIDs: []int64{9223372036854770000}})
	requirements.NoError(err)
	assertions.Zero(missingSource.Totals.MeetingCount)
}

func TestMeetingScopeChangedRejectsDriftedExplorePopulation(t *testing.T) {
	assertions := assert.New(t)
	fixture := newMeetingQueryFixture(t)
	ids := []int64{fixture.meetingIDs[0], fixture.localDelete, fixture.nonMeetingID, -1}
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{
		MessageIDs: &ids,
		Authority:  "synthetic-explore-authority",
	})
	assertions.Nil(metrics)
	require.ErrorIs(t, err, ErrMeetingScopeChanged)
	var selectionErr *MeetingSelectionError
	require.ErrorAs(t, err, &selectionErr)
	assertions.Equal([]int64{-1, fixture.localDelete, fixture.nonMeetingID}, selectionErr.MessageIDs)
}

func TestMeetingMetricsLargeInternalMessagePopulationUsesBoundedBinding(t *testing.T) {
	fixture := newMeetingQueryFixture(t)
	ids := make([]int64, 0, 600)
	ids = append(ids, fixture.meetingIDs[0])
	for id := int64(-1000); len(ids) < 600; id++ {
		ids = append(ids, id)
	}
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	require.NoError(t, err)
	assert.Equal(t, int64(1), metrics.Totals.MeetingCount)
}
