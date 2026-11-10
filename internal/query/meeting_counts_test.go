package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMeetingCountsAcrossFilters(t *testing.T) {
	b := NewTestDataBuilder(t)
	firstSource := b.AddSourceWithType("first@example.org", "granola")
	secondSource := b.AddSourceWithType("second@example.org", "twenty")
	person := b.AddParticipant("person@example.org", "example.org", "Example Person")
	for i, source := range []int64{firstSource, secondSource, firstSource} {
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: int64(100 + i), MessageType: "meeting_transcript", SentAt: time.Date(2026, 8, 1, 10+i, 0, 0, 0, time.UTC)})
		b.AddTo(id, person, "Example Person")
		if i < 2 {
			b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:shared@example.org/', NULL::TIMESTAMP, NULL::DATE)", id))
		}
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		name    string
		context Context
		want    int64
		detail  bool
	}{
		{"both sources", Context{SourceIDs: []int64{firstSource, secondSource}}, 2, true},
		{"explore both sources", Context{SourceIDs: []int64{firstSource, secondSource}, ParticipantIDs: []int64{person}}, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			result, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: ExploreRequest{Context: tc.context}})
			requirements.NoError(err)
			requirements.Len(result.Rows, 1)
			assertions.Equal(tc.want, result.Rows[0].MeetingCount)
			if tc.detail {
				detail, err := engine.GetPerson(t.Context(), person, tc.context, nil)
				requirements.NoError(err)
				requirements.NotNil(detail)
				assertions.Equal(tc.want, detail.MeetingCount)
			}
		})
	}
}

func TestRelationshipMeetingCounts(t *testing.T) {
	for _, tc := range []struct {
		name           string
		keys           [][]string
		at             time.Time
		want           int64
		crossYear      bool
		people         bool
		calendarCounts map[int]int64
	}{
		{name: "one transcript two occurrences", keys: [][]string{{"uid:series/20260801T100000Z", "uid:series/20260808T100000Z"}}, at: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), want: 2, people: true, calendarCounts: map[int]int64{2026: 2}},
		{name: "copies across years", keys: [][]string{{"uid:shared@example.org/"}, {"uid:shared@example.org/"}}, at: time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC), want: 1, crossYear: true, calendarCounts: map[int]int64{2025: 1, 2026: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			b := NewTestDataBuilder(t)
			source := b.AddSourceWithType("owner@example.org", "granola")
			sources := []int64{source}
			owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
			person := b.AddParticipant("person@example.org", "example.org", "Person")
			b.AddOwnerParticipant(source, owner)
			if tc.crossYear {
				second := b.AddSourceWithType("owner@example.org", "twenty")
				b.AddOwnerParticipant(second, owner)
				sources = append(sources, second)
			}
			for i, keys := range tc.keys {
				id := b.AddMessage(MessageOpt{SourceID: sources[i%len(sources)], ConversationID: int64(100 + i), MessageType: "meeting_transcript", SentAt: tc.at.Add(time.Duration(i) * time.Hour)})
				b.AddTo(id, owner, "Owner")
				b.AddTo(id, person, "Person")
				for _, key := range keys {
					b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, '%s', NULL::TIMESTAMP, NULL::DATE)", id, key))
				}
			}
			engine := b.BuildEngine()
			if tc.people {
				people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Query: "person@example.org", Explore: ExploreRequest{Context: Context{SourceIDs: sources, ParticipantIDs: []int64{person}}}})
				requirements.NoError(err)
				requirements.Len(people.Rows, 1)
				assertions.Equal(tc.want, people.Rows[0].MeetingCount)
				assertions.Equal(int64(1), people.Rows[0].ActivityCount)
			}
			for _, scope := range []Context{{}, {SourceIDs: sources, ParticipantIDs: []int64{person}}} {
				result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: scope, Now: tc.at.AddDate(0, 0, 3)})
				requirements.NoError(err)
				requirements.Len(result.Rows, 1)
				assertions.Equal(tc.want, result.Rows[0].Signals.MeetingCount)
			}
			for year, want := range tc.calendarCounts {
				calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: year, Timezone: "UTC"})
				requirements.NoError(err)
				var meetings int64
				for _, day := range calendar.Days {
					meetings += day.Meetings
				}
				assertions.Equal(want, meetings, "year %d", year)
			}
		})
	}
}
