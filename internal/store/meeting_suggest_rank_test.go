package store

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/calevent"
)

func TestSuggestRankRequiresKnownOverlap(t *testing.T) {
	member := suggestMember{ID: 4, Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), End: time.Date(2027, 3, 14, 10, 30, 0, 0, time.UTC)}
	for _, tc := range []struct {
		name        string
		start, end  time.Time
		known, want bool
	}{
		{"ends-at-start", member.Start.Add(-time.Hour), member.Start, true, false},
		{"starts-at-end", member.End, member.End.Add(time.Hour), true, false},
		{"unknown-bounds", member.Start, member.End, false, false},
		{"overlap", member.Start.Add(15 * time.Minute), member.End.Add(time.Hour), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := rankMeetingSuggestion(member, linkOccurrence{Ref: OccurrenceRef{MessageID: 7}, Start: tc.start, End: tc.end, BoundsKnown: tc.known})
			assert.Equal(t, tc.want, ok)
		})
	}
}

func TestSuggestRankSignalsHumanAttendeesAndTitleTokens(t *testing.T) {
	member := suggestMember{ID: 4, Title: "Roadmap café follow-up", Start: time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), End: time.Date(2027, 3, 14, 10, 30, 0, 0, time.UTC), Emails: map[string]bool{"guest@example.org": true, "room@example.org": true}}
	event := linkOccurrence{Ref: OccurrenceRef{MessageID: 7}, Start: time.Date(2027, 3, 14, 10, 15, 0, 0, time.UTC), End: time.Date(2027, 3, 14, 11, 0, 0, 0, time.UTC), BoundsKnown: true, Row: CalendarEventRow{Subject: "Quarterly: ROADMAP / café", Event: calevent.Event{Attendees: []calevent.Attendee{{Email: " Guest@Example.org ", Role: "required"}, {Email: "guest@example.org", Role: "optional"}, {Email: "room@example.org", Role: "room"}}}}}
	got, ok := rankMeetingSuggestion(member, event)
	assert.True(t, ok)
	assert.Equal(t, MeetingSuggestion{Ref: "evt:7", MemberMessageID: 4, OverlapMinutes: 15, SharedAttendees: 1, TitleTokens: 2, Reasons: []string{"time_overlap", "shared_attendees", "title_tokens"}}, got)
}
