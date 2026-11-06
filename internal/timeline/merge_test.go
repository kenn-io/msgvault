package timeline

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
	"go.kenn.io/msgvault/internal/store"
)

func TestTimelineMeetingKeepsAttendeesWithoutEmail(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	source, err := f.Store.GetOrCreateSource("caldav", "calendar@example.com")
	requirements.NoError(err)
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:named-guests@example.com\r\nDTSTART:20261022T100000Z\r\nDTEND:20261022T110000Z\r\nATTENDEE;CN=Guest One:\r\nATTENDEE;CN=Guest Two:\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	_, err = f.Store.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "named-guests", MessageType: "calendar_event"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "named-guests", ConversationType: "calendar"},
		RawMIME:      []byte(raw), RawFormat: calevent.CalDAVRawFormat,
		Metadata: &sql.NullString{String: `{"event_id":"[\"named-guests@example.com\",\"\"]","account_email":"calendar@example.com"}`, Valid: true},
	})
	requirements.NoError(err)
	got, err := Build(t.Context(), f.src, dayRequest())
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	requirements.NotNil(entries[0].Meeting)
	assertions.Equal(2, entries[0].Meeting.AttendeeCount)
	assertions.Equal(2, entries[0].Meeting.PeopleCount)
	var names []string
	for _, person := range entries[0].Meeting.Attendees {
		names = append(names, person.Name)
	}
	assertions.ElementsMatch([]string{"Guest One", "Guest Two"}, names)
	req := dayRequest()
	req.Date, req.From, req.To = "", "2026-10-22", "2026-10-24"
	summarized, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(summarized.Days, 1)
	requirements.NotNil(summarized.Days[0].Summary)
	assertions.Equal(2, summarized.Days[0].Summary.PeopleCount)
}

func TestTimelinePeopleIdentityPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		people []Person
		want   int
	}{
		{"saved aliases", []Person{{PersonID: 1, ParticipantID: 2}, {PersonID: 1, ParticipantID: 3}}, 1},
		{"different people", []Person{{PersonID: 1, ParticipantID: 2}, {PersonID: 3, ParticipantID: 2}}, 2},
		{"participant", []Person{{ParticipantID: 2}, {ParticipantID: 2}}, 1},
		{"email", []Person{{email: " Guest@example.com "}, {email: "guest@example.com"}}, 1},
		{"unknown", []Person{{Name: "Guest"}, {Name: "Guest"}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Len(t, uniquePeople(tc.people), tc.want)
		})
	}
}

func TestTimelineLinkedMeetingPeopleOutsideNoteDay(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f := newTimelineFixture(t)
	event := f.calendar("overnight", strings.NewReplacer(
		`"dateTime":"2026-10-22T10:00:00Z"`, `"dateTime":"2026-10-21T23:00:00Z"`,
		`"dateTime":"2026-10-22T11:00:00Z"`, `"dateTime":"2026-10-22T01:00:00Z"`,
		`"attendees":[{"email":"alice@example.com","displayName":"Alice"}]`, `"attendees":[]`,
	).Replace(singleDayEvent))
	note := f.transcript("overnight", "", time.Date(2026, 10, 21, 23, 30, 0, 0, time.UTC))
	requirements.NoError(f.Store.LinkMeetingContext(t.Context(), note, store.OccurrenceRef{MessageID: event}))
	person := f.savePerson()
	f.tier(person, 1)
	f.project()
	req := dayRequest()
	req.Mode, req.With = ModeDay, "email:alice@example.com"
	got, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	entries := allDayEntries(got)
	requirements.Len(entries, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), entries[0].Ref)
	requirements.NotNil(entries[0].Meeting)
	assertions.True(entries[0].Meeting.Attended)
	assertions.Equal([]int64{note}, entries[0].Meeting.LinkedTranscriptIDs)
	requirements.Len(entries[0].Meeting.People, 1)
	assertions.Equal(f.peer, entries[0].Meeting.People[0].ParticipantID)
	assertions.Equal(1, entries[0].Meeting.People[0].Tier)
	req.Tiers = []int{1}
	tiered, err := Build(t.Context(), f.src, req)
	requirements.NoError(err)
	requirements.Len(allDayEntries(tiered), 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), allDayEntries(tiered)[0].Ref)
}

// The cap selects a prefix of the readers' shared (FirstAt, Key) ordering,
// before the day renderer applies its separate tier/exchange ranking.
func TestMergeBurstsSelectsCombinedReaderOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	var want []string
	for i, cold := range []bool{true, false, true, false} {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("ordered-%d", i), "Ordered")
		requirements.NoError(err)
		at := f.now.Add(time.Duration(i/2) * time.Minute)
		f.add(at, false, func(m *store.Message) { m.ConversationID = conv })
		if cold {
			requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, fmt.Sprintf("ordered-%d", i)))
		}
		want = append(want, fmt.Sprintf("burst:%d:%d:2026-10-22", f.Source.ID, conv))
	}
	f.project()
	q := store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Filters: f.src.Filters, Limit: 5000}
	warm, err := f.src.Activity.ListActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	cold, err := f.src.Cold.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(warm.Bursts, 2)
	requirements.Len(cold.Bursts, 2)
	// Equal-time ties use lexical keys, rather than source preference.
	slices.Sort(want[:2])
	slices.Sort(want[2:])
	merged := mergeBursts(warm.Bursts, cold.Bursts)
	got := []string{}
	for _, b := range merged[:3] {
		got = append(got, b.Key)
	}
	assertions.Equal(want[:3], got)
	// The deduplication path must retain the same selection order.
	merged = mergeBursts(warm.Bursts, append(cold.Bursts, warm.Bursts...))
	requirements.Len(merged, 4)
	assertions.Equal(want[0], merged[0].Key)
}
