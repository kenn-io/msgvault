package query

import (
	"fmt"
	"strconv"
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
			b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:shared@example.org/', TIMESTAMP '2026-08-01 10:00:00', NULL::DATE)", id))
		}
		if i == 0 {
			b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:series/20260725T100000Z', TIMESTAMP '2026-07-25 10:00:00', NULL::DATE)", id))
		}
	}
	engine := b.BuildEngine()
	for _, tc := range []struct {
		name    string
		context Context
		want    int64
		detail  bool
	}{
		{"both sources", Context{SourceIDs: []int64{firstSource, secondSource}}, 3, true},
		{"explore both sources", Context{SourceIDs: []int64{firstSource, secondSource}, ParticipantIDs: []int64{person}}, 3, false},
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

func TestMeetingOccurrenceDateFilters(t *testing.T) {
	b := NewTestDataBuilder(t)
	source := b.AddSourceWithType("owner@example.org", "granola")
	owner := b.AddParticipant("owner@example.org", "example.org", "Owner")
	person := b.AddParticipant("person@example.org", "example.org", "Person")
	b.AddOwnerParticipant(source, owner)
	id := b.AddMessage(MessageOpt{SourceID: source, MessageType: "meeting_transcript", SentAt: time.Date(2026, 3, 7, 10, 0, 0, 0, time.UTC)})
	b.AddTo(id, owner, "Owner")
	b.AddTo(id, person, "Person")
	b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, 'uid:series/20260307T100000Z', TIMESTAMP '2026-03-07 10:00:00', NULL::DATE),(%d, 'uid:series/20260314T100000Z', TIMESTAMP '2026-03-14 10:00:00', NULL::DATE)", id, id))
	engine := b.BuildEngine()
	first := time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC)
	second := first.AddDate(0, 0, 7)
	for _, tc := range []struct {
		name          string
		after, before *time.Time
	}{
		{"first day", &first, new(first.AddDate(0, 0, 1))},
		{"second day", &second, new(second.AddDate(0, 0, 1))},
		{"after only", &second, nil},
		{"before only", nil, &second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: Context{After: tc.after, Before: tc.before}, Now: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), ShowAll: true})
			require.NoError(t, err)
			require.Len(t, result.Rows, 1)
			assert.Equal(t, int64(1), result.Rows[0].Signals.MeetingCount)
		})
	}
	for _, day := range []int{7, 14} {
		t.Run("people "+strconv.Itoa(day), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			after := time.Date(2026, 3, day, 0, 0, 0, 0, time.UTC)
			before := after.AddDate(0, 0, 1)
			context := Context{SourceIDs: []int64{source}, ParticipantIDs: []int64{person}, After: &after, Before: &before}
			result, err := engine.SearchPeople(t.Context(), PersonSearchRequest{Query: "person@example.org", Explore: ExploreRequest{Context: context}})
			requirements.NoError(err)
			requirements.Len(result.Rows, 1)
			assertions.Equal(int64(1), result.Rows[0].MeetingCount)
			wantActivity := int64(0)
			if day == 7 {
				wantActivity = 1
			}
			assertions.Equal(wantActivity, result.Rows[0].ActivityCount)
			detail, err := engine.GetPerson(t.Context(), person, context, nil)
			requirements.NoError(err)
			requirements.NotNil(detail)
			assertions.Equal(int64(1), detail.MeetingCount)
			assertions.Equal(wantActivity, detail.ActivityCount)
			summary, err := engine.GetPersonSummary(t.Context(), person, ExploreRequest{Context: context}, nil)
			requirements.NoError(err)
			requirements.Len(summary.Rows, 1)
			assertions.Equal(int64(1), summary.Rows[0].MeetingCount)
			assertions.Equal(wantActivity, summary.Rows[0].ActivityCount)
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
		calendarCounts map[string]int64
	}{
		{name: "one transcript two occurrences", keys: [][]string{{"uid:series/20260801T100000Z", "uid:series/20260808T100000Z"}}, at: time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC), want: 2, calendarCounts: map[string]int64{"2026-08-01": 1, "2026-08-08": 1}},
		{name: "copies across years", keys: [][]string{{"uid:shared@example.org/"}, {"uid:shared@example.org/"}}, at: time.Date(2025, 12, 31, 23, 30, 0, 0, time.UTC), want: 1, crossYear: true, calendarCounts: map[string]int64{"2026-01-01": 1}},
		{name: "all day", keys: [][]string{{"uid:series/20260308"}}, at: time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC), want: 1, calendarCounts: map[string]int64{"2026-03-08": 1}},
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
					at, date := "TIMESTAMP '2026-01-01 00:30:00'", "NULL::DATE"
					switch key {
					case "uid:series/20260801T100000Z":
						at = "TIMESTAMP '2026-08-01 10:00:00'"
					case "uid:series/20260808T100000Z":
						at = "TIMESTAMP '2026-08-08 10:00:00'"
					case "uid:series/20260308":
						at, date = "TIMESTAMP '2026-03-08 00:00:00'", "DATE '2026-03-08'"
					}
					b.meetingKeys = append(b.meetingKeys, fmt.Sprintf("(%d, '%s', %s, %s)", id, key, at, date))
				}
			}
			engine := b.BuildEngine()
			for _, scope := range []Context{{SourceIDs: sources, ParticipantIDs: []int64{person}}} {
				result, err := engine.Relationships(t.Context(), RelationshipsRequest{Context: scope, Now: tc.at.AddDate(0, 0, 3)})
				requirements.NoError(err)
				requirements.Len(result.Rows, 1)
				assertions.Equal(tc.want, result.Rows[0].Signals.MeetingCount)
			}
			for _, year := range []int{2025, 2026} {
				zone := "UTC"
				if tc.name == "all day" {
					zone = "America/Los_Angeles"
				}
				calendar, err := engine.RelationshipCalendar(t.Context(), RelationshipCalendarRequest{CanonicalID: person, Year: year, Timezone: zone})
				requirements.NoError(err)
				meetings := make(map[string]int64)
				for _, day := range calendar.Days {
					if day.Meetings != 0 {
						meetings[day.Date] = day.Meetings
					}
				}
				for date, count := range meetings {
					assertions.Equal(tc.calendarCounts[date], count)
				}
				for date, count := range tc.calendarCounts {
					if date[:4] == strconv.Itoa(year) {
						assertions.Equal(count, meetings[date])
					}
				}
			}
		})
	}
}
