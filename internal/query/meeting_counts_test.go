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
			b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:shared@example.org/')", id))
		}
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		name    string
		context Context
		want    int64
	}{
		{"all", Context{}, 2},
		{"one source", Context{SourceIDs: []int64{firstSource}}, 2},
		{"both sources", Context{SourceIDs: []int64{firstSource, secondSource}}, 2},
		{"explore both sources", Context{SourceIDs: []int64{firstSource, secondSource}, ParticipantIDs: []int64{person}}, 2},
		{"explore one source", Context{SourceIDs: []int64{secondSource}, ParticipantIDs: []int64{person}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			if len(tc.context.SourceIDs) > 0 {
				result, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: ExploreRequest{Context: tc.context}})
				requirements.NoError(err)
				requirements.Len(result.Rows, 1)
				assertions.Equal(tc.want, result.Rows[0].MeetingCount)
			}
			detail, err := engine.GetPerson(t.Context(), person, tc.context, nil)
			requirements.NoError(err)
			requirements.NotNil(detail)
			assertions.Equal(tc.want, detail.MeetingCount)
		})
	}
}

func TestMeetingCountsOneTranscriptTwoOccurrences(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.org", "granola")
	owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
	person := b.AddParticipant("person@example.org", "example.org", "Person")
	b.AddOwnerParticipant(source, owner)
	at := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 100, MessageType: "meeting_transcript", SentAt: at})
	b.AddTo(id, owner, "Owner")
	b.AddTo(id, person, "Person")
	b.AddAttachment(id, 100, "notes.txt")
	b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:series/first'),(%d, 'uid:series/second')", id, id))
	engine := b.BuildEngine()
	for _, scope := range []Context{{}, {SourceIDs: []int64{source}, ParticipantIDs: []int64{person}}} {
		people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Query: "person@example.org", Explore: ExploreRequest{Context: scope}})
		requirements.NoError(err)
		requirements.Len(people.Rows, 1)
		assertions.Equal(int64(2), people.Rows[0].MeetingCount)
		assertions.Equal(int64(1), people.Rows[0].ActivityCount)
		assertions.Equal(int64(1), people.Rows[0].FileCount)
		result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: scope, Now: at.Add(time.Hour)})
		requirements.NoError(err)
		requirements.Len(result.Rows, 1)
		assertions.Equal(int64(2), result.Rows[0].Signals.MeetingCount)
		assertions.Equal(at, result.Rows[0].LastAt)
	}
	calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: 2026, Timezone: "UTC"})
	requirements.NoError(err)
	var meetings int64
	for _, day := range calendar.Days {
		meetings += day.Meetings
	}
	assertions.Equal(int64(2), meetings)
}

func TestRelationshipMeetingCountsCollapseLinkedOccurrence(t *testing.T) {
	require := require.New(t)
	b := NewTestDataBuilder(t)
	first := b.AddSourceWithType("owner@example.org", "granola")
	second := b.AddSourceWithType("owner@example.org", "twenty")
	owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
	person := b.AddParticipant("person@example.org", "example.org", "Person")
	b.AddOwnerParticipant(first, owner)
	b.AddOwnerParticipant(second, owner)
	at := time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC)
	for i, source := range []int64{first, second} {
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: int64(100 + i), MessageType: "meeting_transcript", SentAt: at.Add(time.Duration(i) * time.Hour)})
		b.AddTo(id, owner, "Owner")
		b.AddTo(id, person, "Person")
		b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:shared@example.org/')", id))
	}
	engine := b.BuildEngine()
	want := int64(1)
	for _, scope := range []Context{{}, {SourceIDs: []int64{first, second}, ParticipantIDs: []int64{person}}} {
		result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: scope, Now: at.Add(time.Hour)})
		require.NoError(err)
		require.Len(result.Rows, 1)
		assert.Equal(t, want, result.Rows[0].Signals.MeetingCount)
	}
	for _, tc := range []struct {
		year int
		want int64
	}{{2025, 1}, {2026, 0}} {
		calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: tc.year, Timezone: "UTC"})
		require.NoError(err)
		var meetings int64
		for _, day := range calendar.Days {
			meetings += day.Meetings
		}
		assert.Equal(t, tc.want, meetings, "year %d", tc.year)
	}
}
