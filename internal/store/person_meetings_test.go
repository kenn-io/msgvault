package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func personMeetingWindow() PersonMeetingsQuery {
	return PersonMeetingsQuery{From: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2027, 4, 1, 0, 0, 0, 0, time.UTC)}
}

func TestPersonMeetingsUnionTranscriptsAndInvites(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	person, err := st.EnsureParticipant("guest@example.com", "Synthetic Guest", "example.com")
	requirements.NoError(err)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", `{"id":"invitation","iCalUID":"invite@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`)
	note, _ := recordNote(t, st, "attended", `{"transcript":"Evidence"}`, "2027-03-15T10:00:00Z")
	_, err = st.db.Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, person, note)
	requirements.NoError(err)
	q := personMeetingWindow()
	q.ParticipantIDs = []int64{person}
	q.Emails = []string{"GUEST@example.com"}
	items, cursor, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(items, 2)
	assertions.Empty(cursor)
	assertions.Equal(fmt.Sprintf("evt:%d", event), items[0].Ref)
	assertions.Zero(items[0].ItemCount)
	assertions.False(items[0].HasTranscript)
	assertions.Equal(fmt.Sprintf("msg:%d", note), items[1].Ref)
	assertions.Equal(1, items[1].ItemCount)
	assertions.True(items[1].HasTranscript)
}

func TestPersonMeetingsPhoneOnlyIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	participant, err := st.EnsureParticipant("phone-only@example.com", "Synthetic Phone Contact", "example.com")
	requirements.NoError(err)
	_, err = st.db.Exec(`UPDATE participants SET email_address = NULL, phone_number = ? WHERE id = ?`, "+12025550123", participant)
	requirements.NoError(err)
	note, _ := recordNote(t, st, "phone-note", `{"transcript":"Phone evidence"}`, "2027-03-14T10:00:00Z")
	_, err = st.db.Exec(`UPDATE messages SET sender_id = ? WHERE id = ?`, participant, note)
	requirements.NoError(err)
	q := personMeetingWindow()
	q.ParticipantIDs = []int64{participant}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(fmt.Sprintf("msg:%d", note), items[0].Ref)
}

func TestPersonMeetingsGrantedNoteUngrantedCalendar(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", singleLinkRaw("event", "linked@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note, source := recordNote(t, st, "scoped-note", `{"transcript":"Note evidence","attendees":[{"email":"guest@example.com"}]}`, "2027-03-14T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", event), note))
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), SourceScope{IDs: []int64{source}}, q)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(fmt.Sprintf("msg:%d", note), items[0].Ref)
	assertions.Nil(items[0].Occurrence)
}

func TestPersonMeetingsDedupeVisibleCalendarCopies(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := `{"id":"event","iCalUID":"same@example.org","start":{"dateTime":"2027-03-14T10:00:00Z"},"end":{"dateTime":"2027-03-14T11:00:00Z"},"attendees":[{"email":"guest@example.com"}]}`
	a, _ := calendarLinkFixture(t, st, "calendar-a@example.com", raw)
	b, _ := calendarLinkFixture(t, st, "calendar-b@example.org", raw)
	note, _ := recordNote(t, st, "copy-note", `{"transcript":"Evidence"}`, "2027-03-14T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", b), note))
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", a), items[0].Ref)
	assertions.Equal(1, items[0].ItemCount)
}

func TestPersonMeetingsUsesLinkedTimeForDelayedNote(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", singleLinkRaw("event", "delayed@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	note, _ := recordNote(t, st, "late-import", `{"attendees":[{"email":"guest@example.com"}],"transcript":"Evidence"}`, "2027-05-01T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", event), note))
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(fmt.Sprintf("evt:%d", event), items[0].Ref)
	assertions.Equal(time.Date(2027, 3, 14, 10, 0, 0, 0, time.UTC), items[0].Start)
}

func TestPersonMeetingsDefaultCursorFreezesWindow(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	now := time.Now().UTC()
	for i := range 3 {
		_, note := projectionFixture(t, st, fmt.Sprintf("recent-%d", i), "meeting_json", `{"attendees":[{"email":"guest@example.com"}],"transcript":"Evidence"}`)
		_, err := st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, now.Add(time.Duration(i-5)*time.Hour), note)
		requirements.NoError(err)
	}
	q := PersonMeetingsQuery{Emails: []string{"guest@example.com"}, Limit: 1}
	first, cursor, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(first, 1)
	requirements.NotEmpty(cursor)
	q.Cursor = cursor
	second, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(second, 1)
	assertions.NotEqual(first[0].Ref, second[0].Ref)
	q.Emails = []string{"different@example.org"}
	_, _, err = st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	assertions.ErrorIs(err, ErrInvalidArgument)
}

func TestPersonMeetingsUnsupportedSeriesIsExplicit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "calendar@example.com", `{"id":"unsupported","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=HOURLY"],"attendees":[{"email":"guest@example.com"}]}`)
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.Error(err)
	assertions.Empty(items)
}

func TestPersonMeetingsEmptyAndInvalidQuery(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	q := personMeetingWindow()
	items, cursor, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	assertions.Empty(items)
	assertions.Empty(cursor)
	q.Emails = []string{"guest@example.com"}
	q.Limit = 501
	_, _, err = st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.ErrorIs(err, ErrInvalidArgument)
	q.Limit = 1
	q.To = q.From.Add(401 * 24 * time.Hour)
	_, _, err = st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.ErrorIs(err, ErrInvalidArgument)
}

func TestPersonMeetingsCursorOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for _, day := range []string{"20", "05", "10"} {
		_, note := projectionFixture(t, st, "date-"+day, "meeting_json", `{"attendees":[{"email":"guest@example.com"}]}`)
		at, err := time.Parse(time.RFC3339, "2027-03-"+day+"T10:00:00Z")
		requirements.NoError(err)
		_, err = st.db.Exec(`UPDATE messages SET sent_at = ? WHERE id = ?`, sql.NullTime{Time: at, Valid: true}, note)
		requirements.NoError(err)
	}
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	q.Limit = 1
	var dates []int
	for range 3 {
		items, cursor, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
		requirements.NoError(err)
		requirements.Len(items, 1)
		dates = append(dates, items[0].Start.Day())
		q.Cursor = cursor
	}
	assertions.Equal([]int{5, 10, 20}, dates)
	assertions.Empty(q.Cursor)
}

func TestPersonMeetingsCursorRejectsWindowAndScopeChanges(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	for _, day := range []string{"05", "10"} {
		recordNote(t, st, "cursor-"+day, `{"attendees":[{"email":"guest@example.com"}]}`, "2027-03-"+day+"T10:00:00Z")
	}
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	q.Limit = 1
	_, cursor, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.NotEmpty(cursor)
	q.Cursor = cursor
	q.To = q.To.Add(time.Hour)
	_, _, err = st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.ErrorIs(err, ErrInvalidArgument)
	q.To = q.To.Add(-time.Hour)
	_, _, err = st.ListPersonMeetingsContext(t.Context(), SourceScope{}, q)
	requirements.ErrorIs(err, ErrInvalidArgument)
}

func TestPersonMeetingsHiddenInvitesCannotChangeVisibleNote(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	note, source := recordNote(t, st, "visible-note", `{"attendees":[{"email":"guest@example.com"}],"transcript":"Evidence"}`, "2027-03-14T10:00:00Z")
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	scope := SourceScope{IDs: []int64{source}}
	before, _, err := st.ListPersonMeetingsContext(t.Context(), scope, q)
	requirements.NoError(err)
	event, _ := calendarLinkFixture(t, st, "hidden@example.com", singleLinkRaw("hidden", "hidden@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", event), note))
	after, _, err := st.ListPersonMeetingsContext(t.Context(), scope, q)
	requirements.NoError(err)
	assertions.Equal(before, after)
}

func TestPersonMeetingsRecurrenceBudgetIsExplicit(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "calendar@example.com", `{"id":"old-series","start":{"dateTime":"1800-03-01T10:00:00Z"},"end":{"dateTime":"1800-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=DAILY"],"attendees":[{"email":"guest@example.com"}]}`)
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.ErrorIs(err, ErrQueryTooBroad)
	assertions.Empty(items)
}

func TestPersonMeetingsIgnoresProvenUnrelatedUnsupportedSeries(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendarLinkFixture(t, st, "calendar@example.com", `{"id":"unsupported","start":{"dateTime":"2027-03-01T10:00:00Z"},"end":{"dateTime":"2027-03-01T11:00:00Z"},"recurrence":["RRULE:FREQ=HOURLY"],"attendees":[{"email":"unrelated@example.org"}]}`)
	recordNote(t, st, "related-note", `{"attendees":[{"email":"guest@example.com"}],"transcript":"Evidence"}`, "2027-03-14T10:00:00Z")
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	assertions.Len(items, 1)
}

func TestPersonMeetingsConflictingTargetsKeepMessageIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	a, _ := calendarLinkFixture(t, st, "calendar@example.com", singleLinkRaw("a", "a@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	b, _ := calendarLinkFixture(t, st, "other@example.org", singleLinkRaw("b", "b@example.org", "2027-03-15T10:00:00Z", "2027-03-15T11:00:00Z"))
	note, _ := recordNote(t, st, "conflicted-note", `{"attendees":[{"email":"guest@example.com"}],"transcript":"Evidence"}`, "2027-03-16T10:00:00Z")
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", a), note))
	requirements.NoError(st.LinkMeetingContext(t.Context(), fmt.Sprintf("evt:%d", b), note))
	q := personMeetingWindow()
	q.Emails = []string{"guest@example.com"}
	items, _, err := st.ListPersonMeetingsContext(t.Context(), OwnerScope(), q)
	requirements.NoError(err)
	requirements.Len(items, 1)
	assertions.Equal(fmt.Sprintf("msg:%d", note), items[0].Ref)
	assertions.Equal(time.Date(2027, 3, 16, 10, 0, 0, 0, time.UTC), items[0].Start)
	assertions.Nil(items[0].Occurrence)
}

func TestPersonMeetingsOccurrenceReadUsesSharedBudget(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	event, _ := calendarLinkFixture(t, st, "calendar@example.com", singleLinkRaw("event", "budget@example.org", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z"))
	ctx, release, err := st.BeginReadSnapshotContext(t.Context())
	requirements.NoError(err)
	defer release()
	budget := 0
	_, err = calendarOccurrenceWithReader(ctx, st.snapshotReader(ctx), OwnerScope(), fmt.Sprintf("evt:%d", event), &budget)
	assertions.ErrorIs(err, ErrQueryTooBroad)
}
