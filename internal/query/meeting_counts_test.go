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
	t.Run("one transcript two occurrences", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		b := NewTestDataBuilder(t)
		source := b.AddSourceWithType("owner@example.org", "granola")
		owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
		person := b.AddParticipant("person@example.org", "example.org", "Person")
		b.AddOwnerParticipant(source, owner)
		id := b.AddMessage(MessageOpt{SourceID: source, ConversationID: 100, MessageType: "meeting_transcript", SentAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)})
		b.AddTo(id, owner, "Owner")
		b.AddTo(id, person, "Person")
		b.AddAttachment(id, 100, "notes.txt")
		b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:series/first'),(%d, 'uid:series/second')", id, id))
		engine := b.BuildEngine()
		people, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Query: "person@example.org", Explore: ExploreRequest{Context: Context{SourceIDs: []int64{source}, ParticipantIDs: []int64{person}}}})
		requirements.NoError(err)
		requirements.Len(people.Rows, 1)
		assertions.Equal(int64(2), people.Rows[0].MeetingCount)
		assertions.Equal(int64(1), people.Rows[0].ActivityCount)
		assertions.Equal(int64(1), people.Rows[0].FileCount)
	})
}

func TestRelationshipMeetingCounts(t *testing.T) {
	for _, tc := range []struct {
		name      string
		kinds     []string
		keys      [][]string
		at        time.Time
		want      int64
		crossYear bool
	}{
		{name: "transcripts only", kinds: []string{"meeting_transcript", "meeting_note", "meeting_minutes"}, at: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), want: 1},
		{name: "one transcript two occurrences", kinds: []string{"meeting_transcript"}, keys: [][]string{{"uid:series/first", "uid:series/second"}}, at: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC), want: 2},
		{name: "copies across years", kinds: []string{"meeting_transcript", "meeting_transcript"}, keys: [][]string{{"uid:shared@example.org/"}, {"uid:shared@example.org/"}}, at: time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC), want: 1, crossYear: true},
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
			for i, kind := range tc.kinds {
				at := tc.at.AddDate(0, 0, i)
				if tc.crossYear {
					at = tc.at.Add(time.Duration(i) * time.Hour)
				}
				id := b.AddMessage(MessageOpt{SourceID: sources[i%len(sources)], ConversationID: int64(100 + i), MessageType: kind, SentAt: at})
				b.AddTo(id, owner, "Owner")
				b.AddTo(id, person, "Person")
				if i < len(tc.keys) {
					for _, key := range tc.keys[i] {
						b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, '%s')", id, key))
					}
				}
			}
			engine := b.BuildEngine()
			for _, scope := range []Context{{}, {SourceIDs: sources, ParticipantIDs: []int64{person}}} {
				result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: scope, Now: tc.at.AddDate(0, 0, 3)})
				requirements.NoError(err)
				requirements.Len(result.Rows, 1)
				assertions.Equal(tc.want, result.Rows[0].Signals.MeetingCount)
				if !tc.crossYear {
					assertions.Equal(tc.at, result.Rows[0].LastAt)
				}
			}
			years := []int{tc.at.Year()}
			if tc.crossYear {
				years = append(years, 2026)
			}
			for _, year := range years {
				calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: year, Timezone: "UTC"})
				requirements.NoError(err)
				var meetings int64
				var transcriptDay RelationshipCalendarDay
				for _, day := range calendar.Days {
					meetings += day.Meetings
					if day.Date == tc.at.Format(time.DateOnly) {
						transcriptDay = day
					}
				}
				if year == tc.at.Year() {
					requirements.NotEmpty(transcriptDay.Date)
					assertions.Equal(tc.want, transcriptDay.Meetings)
					assertions.Equal(identityindex.ModalityMeeting, transcriptDay.ModalityMask)
					assertions.Equal(identityindex.HeatFirstQuartile, transcriptDay.Level)
				}
				want := tc.want
				if tc.crossYear && year == 2026 {
					want = 0
				}
				assertions.Equal(want, meetings, "year %d", year)
				if tc.name == "transcripts only" {
					requirements.Len(calendar.Annual, 1)
					assertions.InDelta(1.0, calendar.Annual[0].Signals.MeetingSignal, 0.000001)
				}
			}
		})
	}
}
