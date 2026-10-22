package query

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
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
			result, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Explore: ExploreRequest{Context: tc.context}})
			requirements.NoError(err)
			requirements.Len(result.Rows, 1)
			assertions.Equal(tc.want, result.Rows[0].MeetingCount)
			detail, err := engine.GetPerson(t.Context(), person, tc.context, nil)
			requirements.NoError(err)
			requirements.NotNil(detail)
			assertions.Equal(tc.want, detail.MeetingCount)
		})
	}
}

func TestRelationshipMeetingCountsCollapseLinkedOccurrence(t *testing.T) {
	for _, unlinkedCalendar := range []bool{false, true} {
		t.Run(fmt.Sprintf("unlinkedCalendar=%t", unlinkedCalendar), func(t *testing.T) {
			require := require.New(t)
			b := NewTestDataBuilder(t)
			first := b.AddSourceWithType("owner@example.org", "granola")
			second := b.AddSourceWithType("owner@example.org", "twenty")
			owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
			person := b.AddParticipant("person@example.org", "example.org", "Person")
			b.AddOwnerParticipant(first, owner)
			b.AddOwnerParticipant(second, owner)
			at := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
			for i, kind := range []string{"calendar_event", "meeting_transcript", "meeting_transcript", "calendar_event", "calendar_event"} {
				if i == 4 && !unlinkedCalendar {
					break
				}
				source := first
				if i == 2 || i == 3 {
					source = second
				}
				id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: int64(100 + i), MessageType: kind, SentAt: at})
				b.AddTo(id, owner, "Owner")
				b.AddTo(id, person, "Person")
				if i < 4 {
					b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:shared@example.org/')", id))
				}
			}
			engine := b.BuildEngine()
			want := int64(1)
			if unlinkedCalendar {
				want = 2
			}
			for _, scope := range []Context{{}, {SourceIDs: []int64{first, second}, ParticipantIDs: []int64{person}}} {
				result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: scope, Now: at.Add(time.Hour)})
				require.NoError(err)
				require.Len(result.Rows, 1)
				assert.Equal(t, want, result.Rows[0].Signals.MeetingCount)
			}
			calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: 2026, Timezone: "UTC"})
			require.NoError(err)
			var meetings int64
			for _, day := range calendar.Days {
				meetings += day.Meetings
			}
			assert.Equal(t, want, meetings)
			var scored int64
			require.NoError(engine.db.QueryRow(`SELECT sum(meeting_count) FROM read_parquet(?) WHERE canonical_id = ?`, engine.parquetPath(identityindex.DatasetTemperatureContributions), person).Scan(&scored))
			assert.Equal(t, want, scored)
		})
	}
}

func TestRelationshipCalendarLinkedMeetingAcrossYears(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.org", "gcal")
	owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
	person := b.AddParticipant("person@example.org", "example.org", "Person")
	b.AddOwnerParticipant(source, owner)
	for i, kind := range []string{"calendar_event", "meeting_transcript"} {
		at := time.Date(2024, 12, 31, 23, 59, 0, 0, time.UTC).Add(time.Duration(i) * 2 * time.Minute)
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: int64(100 + i), MessageType: kind, SentAt: at})
		b.AddTo(id, owner, "Owner")
		b.AddTo(id, person, "Person")
		b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:shared@example.org/')", id))
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		year int
		want int64
	}{{2024, 1}, {2025, 0}} {
		calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: tc.year, Timezone: "UTC"})
		requirements.NoError(err)
		var meetings int64
		for _, day := range calendar.Days {
			meetings += day.Meetings
		}
		assertions.Equal(tc.want, meetings, "year %d", tc.year)
	}
	var temperatureMeetings int64
	requirements.NoError(engine.db.QueryRow(`SELECT sum(meeting_count) FROM read_parquet(?) WHERE canonical_id = ?`, engine.parquetPath(identityindex.DatasetTemperatureContributions), person).Scan(&temperatureMeetings))
	assertions.Equal(int64(1), temperatureMeetings)
}
