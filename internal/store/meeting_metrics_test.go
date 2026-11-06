package store

import (
	"encoding/json"
	"fmt"
	"strings"
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

func TestReviewMeetingMetricsCountsLinkedCopiesOnce(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	fixture := newMeetingQueryFixture(t)
	_, event := calendarFixture(t, fixture.store, "series", linkSeriesRaw)
	ref := OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}
	ids := []int64{fixture.meetingIDs[0], fixture.meetingIDs[1], fixture.meetingIDs[3]}
	winnerDate := time.Date(2026, time.February, 10, 10, 0, 0, 0, time.UTC)
	_, err := fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, winnerDate, ids[1])
	requirements.NoError(err)
	for _, id := range ids {
		requirements.NoError(fixture.store.LinkMeetingContext(t.Context(), id, ref))
	}
	keys, err := fixture.store.MeetingCountKeysContext(t.Context())
	requirements.NoError(err)
	requirements.Equal(keys[ids[0]], keys[ids[1]])
	requirements.NotEmpty(keys[ids[0]])
	metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	requirements.NoError(err)
	assertions.Equal(int64(1), metrics.Totals.MeetingCount)
	requirements.Len(metrics.Months, 1)
	assertions.Equal(int64(1), metrics.Months[0].Totals.MeetingCount)
	assertions.Equal("2026-02", metrics.Months[0].Month)
	assertions.Equal(meetingcontent.DurationTotals{
		MeetingCount: 1, KnownDurationCount: 1, TotalKnownSeconds: 3600, AverageKnownSeconds: new(float64(3600)),
	}, metrics.Totals)
	assertions.Equal(metrics.Totals, metrics.Months[0].Totals)
	assertions.Equal([]meetingcontent.BasisTotals{{Basis: meetingcontent.DurationScheduled, Count: 1, TotalSeconds: 3600}}, metrics.DurationByBasis)
	requirements.NotNil(metrics.FirstMeetingAt)
	requirements.NotNil(metrics.LastMeetingAt)
	assertions.Equal(winnerDate, *metrics.FirstMeetingAt)
	assertions.Equal(winnerDate, *metrics.LastMeetingAt)

	// Archive navigation scopes one transcript, even when it covers multiple occurrences.
	requirements.NoError(fixture.store.LinkMeetingContext(t.Context(), ids[0], OccurrenceRef{MessageID: event, Key: "20270321T100000Z"}))
	for _, id := range ids {
		single := []int64{id}
		metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &single})
		requirements.NoError(err)
		assertions.Equal(int64(1), metrics.Totals.MeetingCount)
	}
}

func TestReviewMetricsDateWindowsCountOnce(t *testing.T) {
	requirements := require.New(t)
	f := newMeetingQueryFixture(t)
	_, event := calendarFixture(t, f.store, "series", linkSeriesRaw)
	ids := f.meetingIDs[:2]
	january := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	february := january.AddDate(0, 1, 0)
	march := february.AddDate(0, 1, 0)
	januaryCopy, februaryCopy := february.Add(-time.Hour), february.Add(time.Hour)
	for i, at := range []time.Time{januaryCopy, februaryCopy} {
		_, err := f.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, at, ids[i])
		requirements.NoError(err)
		requirements.NoError(f.store.LinkMeetingContext(t.Context(), ids[i], OccurrenceRef{MessageID: event, Key: "20270314T100000Z"}))
	}
	for _, tc := range []struct {
		name          string
		after, before *time.Time
		sources       []int64
		count         int64
		seconds       float64
		at            *time.Time
		month         string
	}{
		{"all history", nil, nil, nil, 1, 3600, &februaryCopy, "2026-02"},
		{"January", &january, &february, nil, 0, 0, nil, ""},
		{"February", &february, &march, nil, 1, 3600, &februaryCopy, "2026-02"},
		{"before February", nil, &february, nil, 0, 0, nil, ""},
		{"after January", &february, nil, nil, 1, 3600, &februaryCopy, "2026-02"},
		{"January source", &january, &february, []int64{f.sourceOne}, 1, 1800, &januaryCopy, "2026-01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			metrics, err := f.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids, SourceIDs: tc.sources, After: tc.after, Before: tc.before})
			requirements.NoError(err)
			assertions.Equal(tc.count, metrics.Totals.MeetingCount)
			assertions.Equal(tc.count, metrics.Totals.KnownDurationCount)
			assertions.InDelta(tc.seconds, metrics.Totals.TotalKnownSeconds, 0)
			assertions.Equal(tc.at, metrics.FirstMeetingAt)
			assertions.Equal(tc.at, metrics.LastMeetingAt)
			if tc.count == 0 {
				assertions.Empty(metrics.Months)
			} else {
				requirements.Len(metrics.Months, 1)
				assertions.Equal(tc.month, metrics.Months[0].Month)
				assertions.Equal(metrics.Totals, metrics.Months[0].Totals)
			}
		})
	}
}

func TestMeetingMetricsUnavailableOccurrenceKeepsLinkedCopiesTogether(t *testing.T) {
	for _, recurrence := range []string{"FREQ=WEEKLY;COUNT=1", "FREQ=HOURLY;COUNT=6"} {
		t.Run(recurrence, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f := newMeetingQueryFixture(t)
			_, event := calendarFixture(t, f.store, "series", linkSeriesRaw)
			ids := f.meetingIDs[:2]
			for _, id := range ids {
				requirements.NoError(f.store.LinkMeetingContext(t.Context(), id, OccurrenceRef{MessageID: event, Key: "20270321T100000Z"}))
			}
			requirements.NoError(f.store.UpsertMessageRawWithFormat(event, []byte(strings.Replace(linkSeriesRaw, "FREQ=WEEKLY;COUNT=6", recurrence, 1)), "gcal_json"))
			metrics, err := f.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
			requirements.NoError(err)
			assertions.Equal(int64(1), metrics.Totals.MeetingCount)
			for _, id := range ids {
				record, err := f.store.MeetingRecordContext(t.Context(), MeetingLinkRef{Kind: "msg", MessageID: id})
				requirements.NoError(err)
				assertions.Empty(record.Links, "an unavailable occurrence stays hidden")
				assertions.Equal([]int64{id}, record.MessageIDs)
			}
		})
	}
}

func TestIndependentMetricsPartialOverlap(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newMeetingQueryFixture(t)
	_, event := calendarFixture(t, f.store, "series", linkSeriesRaw)
	ids := f.meetingIDs[:2]
	for i, id := range ids {
		for _, key := range [][]string{{"20270307T100000Z", "20270314T100000Z"}, {"20270314T100000Z", "20270321T100000Z"}}[i] {
			requirements.NoError(f.store.LinkMeetingContext(t.Context(), id, OccurrenceRef{MessageID: event, Key: key}))
		}
	}
	metrics, err := f.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
	requirements.NoError(err)
	assertions.Equal(meetingcontent.DurationTotals{
		MeetingCount: 2, KnownDurationCount: 2, TotalKnownSeconds: 5400, AverageKnownSeconds: new(float64(2700)),
	}, metrics.Totals)
	requirements.Len(metrics.Months, 1)
	assertions.Equal(metrics.Totals, metrics.Months[0].Totals)
}

func TestMeetingMetricsDurationTiesKeepLowestID(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(fmt.Sprintf("known=%t", known), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			fixture := newMeetingQueryFixture(t)
			_, event := calendarFixture(t, fixture.store, "event", singleLinkRaw("event", "shared@example.org"))
			ids := []int64{fixture.meetingIDs[0], fixture.meetingIDs[1]}
			raw := `{}`
			want := meetingcontent.DurationTotals{MeetingCount: 1, UnknownDurationCount: 1}
			if known {
				raw = `{"started_at":"2026-01-01T10:00:00Z","ended_at":"2026-01-01T10:30:00Z"}`
				want = meetingcontent.DurationTotals{MeetingCount: 1, KnownDurationCount: 1, TotalKnownSeconds: 1800, AverageKnownSeconds: new(float64(1800))}
			}
			for i, id := range ids {
				requirements.NoError(fixture.store.UpsertMessageRawWithFormat(id, []byte(raw), "meeting_json"))
				_, err := fixture.store.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, time.Date(2026, time.Month(i+1), 1, 10, 0, 0, 0, time.UTC), id)
				requirements.NoError(err)
				requirements.NoError(fixture.store.LinkMeetingContext(t.Context(), id, OccurrenceRef{MessageID: event}))
			}
			metrics, err := fixture.store.GetMeetingMetricsContext(t.Context(), MeetingQueryScope{MessageIDs: &ids})
			requirements.NoError(err)
			assertions.Equal(want, metrics.Totals)
			requirements.Len(metrics.Months, 1)
			assertions.Equal("2026-01", metrics.Months[0].Month)
			assertions.Equal(want, metrics.Months[0].Totals)
		})
	}
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
