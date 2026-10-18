package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func suggestionNoteFixture(t *testing.T, st *Store) int64 {
	t.Helper()
	data, id := projectionFixture(t, st, "suggestion", "plaud_json", `{"summary":"Project planning"}`)
	at, err := time.Parse(time.RFC3339, "2027-03-14T10:00:00Z")
	require.NoError(t, err)
	data.Message.SentAt = sql.NullTime{Time: at, Valid: true}
	data.Message.Subject = sql.NullString{String: "Project planning", Valid: true}
	_, err = st.UpsertMessage(data.Message)
	require.NoError(t, err)
	return id
}

func TestLinkSeriesMasterIndexCoversLargeExceptionSets(t *testing.T) {
	assertions := assert.New(t)
	rows := make([]CalendarEventRow, 20001)
	rows[0] = CalendarEventRow{SourceID: 4, Event: calevent.Event{Provider: "google", Namespace: "account", ProviderEventID: "series", Kind: meetingLinkSeriesKind}}
	for i := 1; i < len(rows); i++ {
		rows[i] = CalendarEventRow{SourceID: 4, Event: calevent.Event{Provider: "google", Namespace: "account", ProviderEventID: fmt.Sprintf("exception-%d", i), SeriesProviderID: "series", Kind: calendarExceptionKind}}
	}
	index := linkSeriesMasterIndex(rows)
	assertions.Len(index, 1)
	assertions.Contains(index, linkSeriesKey{SourceID: 4, Provider: "google", Namespace: "account", ProviderEventID: "series"})
}

func TestSuggestRequiresOverlap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("before", "before@example.org", "2027-03-14T09:00:00Z", "2027-03-14T10:00:00Z"))
	matching, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("match", "match@example.org", "2027-03-14T10:15:00Z", "2027-03-14T11:00:00Z"))
	calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("after", "after@example.org", "2027-03-14T10:30:00Z", "2027-03-14T11:00:00Z"))
	note := suggestionNoteFixture(t, st)
	suggestions, err := st.SuggestMeetingLinksContext(context.Background(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", matching), suggestions[0].Ref)
	assertions.Equal(15, suggestions[0].OverlapMinutes)
}
func TestSuggestPlaud(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("plausible", "plausible@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := suggestionNoteFixture(t, st)
	assertions.Empty(memberLinks(t, st, OwnerScope(), note))
	suggestions, err := st.SuggestMeetingLinksContext(context.Background(), OwnerScope(), fmt.Sprintf("evt:%d", event), 10)
	requirements.NoError(err)
	requirements.Len(suggestions, 1)
	assertions.Equal(note, suggestions[0].MemberMessageID)
}

func TestSuggestionsUseDurationForReverseOverlap(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("long-recording-target", "target@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := suggestionNoteFixture(t, st)
	start := time.Date(2027, 3, 13, 9, 0, 0, 0, time.UTC)
	metadata := sql.NullString{String: `{"platform":"granola","calendar_event_id":"unmatched-event","scheduled_start":"2027-03-13T09:00:00Z"}`, Valid: true}
	requirements.NoError(st.SetMessageMetadata(note, metadata))
	_, err := st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, start, note)
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE meeting_details SET duration_seconds = ? WHERE message_id = ?`, float64((26*time.Hour)/time.Second), note)
	requirements.NoError(err)

	forward, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	requirements.Len(forward, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), forward[0].Ref)
	assertions.Equal(60, forward[0].OverlapMinutes)

	reverse, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), fmt.Sprintf("evt:%d", event), 10)
	requirements.NoError(err)
	requirements.Len(reverse, 1)
	assertions.Equal(note, reverse[0].MemberMessageID)
	assertions.Equal(forward[0].OverlapMinutes, reverse[0].OverlapMinutes)
}

func TestSuggestionsExcludeMovedExceptionAtOriginalSlot(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"moved","recurringEventId":"series","iCalUID":"weekly@example.org","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-18T10:00:00Z"},"end":{"dateTime":"2027-03-18T11:00:00Z"}}`)
	note := suggestionNoteFixture(t, st)

	suggestions, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	assertions.Empty(suggestions, "a moved instance must suppress its original nominal occurrence")
}

func TestSuggestScopeHidesCandidates(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "hidden@example.org", singleLinkRaw("hidden", "hidden@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note := suggestionNoteFixture(t, st)
	var source int64
	require.NoError(t, st.db.QueryRow(`SELECT source_id FROM messages WHERE id = ?`, note).Scan(&source))
	suggestions, err := st.SuggestMeetingLinksContext(context.Background(), SourceScope{IDs: []int64{source}}, fmt.Sprintf("msg:%d", note), 10)
	require.NoError(t, err)
	assert.Empty(t, suggestions)
}

func TestSuggestionsRankVisibleEvidenceAndRooms(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	note := suggestionNoteFixture(t, st)
	participant, err := st.EnsureParticipant("guest@example.org", "Guest Example", "example.org")
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(note, "to", []int64{participant}, []string{"Guest Example"}))
	a, _ := calendarLinkFixture(t, st, "primary@example.com", singleLinkRaw("no-attendee", "a@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	raw := strings.Replace(singleLinkRaw("attendee", "b@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"), `"start":`, `"attendees":[{"email":"guest@example.org"},{"email":"guest@example.org","resource":true}],"start":`, 1)
	b, _ := calendarLinkFixture(t, st, "primary@example.com", raw)
	got, err := st.SuggestMeetingLinksContext(context.Background(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	requirements.Len(got, 2)
	assertions.Equal(fmt.Sprintf("evt:%d", b), got[0].Ref)
	assertions.Equal(1, got[0].SharedAttendees)
	assertions.Equal(fmt.Sprintf("evt:%d", a), got[1].Ref)
	reverse, err := st.SuggestMeetingLinksContext(context.Background(), OwnerScope(), fmt.Sprintf("evt:%d", b), 10)
	requirements.NoError(err)
	requirements.Len(reverse, 1)
	assertions.Equal(note, reverse[0].MemberMessageID)
}

func TestSuggestionsExcludeCancelledEventsInBothDirections(t *testing.T) {
	for _, kind := range []string{"single", "series", "series-unsupported", "orphan", "override"} {
		t.Run(kind, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := strings.Replace(singleLinkRaw("cancelled", "cancelled@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"), `"start":`, `"status":"cancelled","start":`, 1)
			key := ""
			if kind == "series" || kind == "series-unsupported" {
				raw = strings.Replace(linkSeriesRaw, `"start":`, `"status":"cancelled","start":`, 1)
				key = "20270314T100000Z"
				if kind == "series-unsupported" {
					raw = strings.Replace(raw, "FREQ=WEEKLY", "FREQ=SECONDLY", 1)
				}
			}
			if kind == "orphan" || kind == "override" {
				raw = `{"id":"cancelled-instance","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"},"start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"}}`
			}
			var master int64
			if kind == "override" {
				master, _ = calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
			}
			event, _ := calendarLinkFixture(t, st, "primary@example.com", raw)
			note := suggestionNoteFixture(t, st)
			got, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
			requirements.NoError(err)
			assertions.Empty(got)
			if kind == "override" {
				event, key = master, "20270314T100000Z"
			}
			ref := FormatOccurrenceRef(OccurrenceRef{MessageID: event, Key: key})
			got, err = st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), ref, 10)
			requirements.NoError(err)
			assertions.Empty(got)
		})
	}
}

func TestSuggestionsKeepLiveSiblingOfCancelledOverride(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	master, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
	calendarLinkFixture(t, st, "primary@example.com", `{"id":"cancelled","recurringEventId":"series","status":"cancelled","originalStartTime":{"dateTime":"2027-03-14T10:00:00Z"}}`)
	note := suggestionNoteFixture(t, st)
	at := time.Date(2027, 3, 21, 10, 0, 0, 0, time.UTC)
	_, err := st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, at, note)
	requirements.NoError(err)
	got, err := st.SuggestMeetingLinksContext(t.Context(), OwnerScope(), fmt.Sprintf("msg:%d", note), 10)
	requirements.NoError(err)
	requirements.Len(got, 1)
	assertions.Equal(FormatOccurrenceRef(OccurrenceRef{MessageID: master, Key: "20270321T100000Z"}), got[0].Ref)
}
