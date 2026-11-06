package store_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

func timelineContactQuery(f *timelineActivityFixture) store.ContactQuery {
	return store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), Location: time.UTC}
}
func TestMessageAggregateCountsOnlyCountedInScope(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	f.add(t, f.ConvID, f.Source.ID, q.Now.AddDate(-2, 0, 0), false, "")
	f.add(t, f.ConvID, f.Source.ID, q.Now, true, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	requirements.NoError(err)
	assertions.Equal(2, got.Count)
	requirements.NotNil(got.FirstAt)
	assertions.Equal(q.Now.AddDate(-2, 0, 0), *got.FirstAt)
	assertions.Equal([]store.ActivityChannel{store.ChannelEmail}, got.Channels)
	got, err = f.Store.MessageContactAggregateContext(sourcescope.WithSourceIDs(t.Context(), []int64{}), q)
	requirements.NoError(err)
	assertions.Zero(got.Count)
	assertions.Nil(got.FirstAt)
}
func TestMessageAggregateInitiationUsesSevenDayGap(t *testing.T) {
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for _, v := range []struct {
		days int
		out  bool
	}{{-30, false}, {-24, true}, {-17, true}, {-9, false}} {
		f.add(t, f.ConvID, f.Source.ID, q.Now.AddDate(0, 0, v.days), v.out, "")
	}
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(t, err)
	assert.Equal(t, 2, got.InitiatedByContact)
	assert.Equal(t, 1, got.InitiatedByOwner)
}
func TestMessageAggregateMonthlyRowEndsAtNow(t *testing.T) {
	for _, tc := range []struct {
		zone string
		year int
		want [12]int
	}{
		{"UTC", 2026, [12]int{1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{"America/New_York", 2026, [12]int{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{"Asia/Tokyo", 2026, [12]int{2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
		{"America/Asuncion", 2023, [12]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 1}},
	} {
		t.Run(tc.zone, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			f := newTimelineActivityFixture(t)
			loc, err := time.LoadLocation(tc.zone)
			require.NoError(err)
			q := timelineContactQuery(f)
			q.Location = loc
			q.Now = time.Date(tc.year, 10, 22, 12, 0, 0, 0, loc)
			start := time.Date(2025, 11, 1, 0, 0, 0, 0, loc)
			month := time.Date(2026, 10, 1, 0, 0, 0, 0, loc)
			ats := []time.Time{start.Add(-time.Minute), start, start.AddDate(0, 0, 8), month.Add(-time.Minute), month}
			switch tc.zone {
			case "UTC":
				ats = []time.Time{time.Date(2025, 10, 31, 23, 0, 0, 0, time.UTC), start, month.Add(-time.Hour), q.Now, q.Now.AddDate(0, 1, 0)}
			case "America/Asuncion":
				// October starts at 01:00 after a midnight clock jump.
				ats = []time.Time{time.Date(2023, 10, 1, 3, 30, 0, 0, time.UTC), time.Date(2023, 10, 1, 4, 30, 0, 0, time.UTC)}
			}
			for _, at := range ats {
				f.add(t, f.ConvID, f.Source.ID, at, false, "")
			}
			projectTierSignals(t, f.Fixture, "UTC", 0)
			got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(err)
			assert.Equal(tc.want, got.Monthly)
		})
	}
}

func TestAttendedTranscriptIDsByRole(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	for i := range 3 {
		at := q.Now.Add(time.Duration(i-3) * time.Minute)
		id, err := f.Store.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: f.Source.ID, SourceMessageID: fmt.Sprintf("role-%d", i), MessageType: "meeting_transcript", IsFromMe: true, SentAt: sql.NullTime{Time: at, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: fmt.Sprintf("role-%d", i), ConversationType: "meeting", Title: "Synthetic meeting"},
			BodyText:     sql.NullString{String: "Synthetic notes", Valid: true}, RawMIME: []byte(`{"schema_version":1,"recording":{},"participants":[{"displayName":"Alice","handle":"alice@example.com"}]}`), RawFormat: "twenty_json",
		})
		requirements.NoError(err)
		requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer}, nil))
	}
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, truncated, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.Len(got.IDs, 2)
	assertions.True(truncated)
	q.Subject.ParticipantIDs = []int64{f.owner}
	got, _, err = f.Store.AttendedTranscriptIDsContext(t.Context(), q, 2)
	requirements.NoError(err)
	assertions.Empty(got.IDs)
}
func TestCadenceForPersonUsesSuppliedLastContact(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f := newTimelineActivityFixture(t)
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	// The existing attribute writer validates and stores a real integer frequency.
	days := int64(21)
	_, err = f.Store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugContactFrequency, Value: store.AttributeValue{Type: store.AttributeValueInteger, Integer: &days}, Source: store.ProvenanceUser})
	requirements.NoError(err)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	storedLast := now.AddDate(0, 0, -1)
	f.add(t, f.ConvID, f.Source.ID, storedLast, false, "")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	stored, err := f.Store.ContactStateContext(t.Context(), person.ID, now)
	requirements.NoError(err)
	requirements.Equal(&storedLast, stored.LastContactAt)
	for _, tc := range []struct {
		days int
		want string
	}{{30, store.CadenceOverdue}, {10, store.CadenceOK}} {
		last := now.AddDate(0, 0, -tc.days)
		status, due, err := f.Store.CadenceForPersonContext(t.Context(), person.ID, &last, now)
		requirements.NoError(err)
		assertions.Equal(tc.want, status)
		requirements.NotNil(due)
		assertions.Equal(last.AddDate(0, 0, 21), *due)
	}
}

func TestContactReadersDiscardFutureCopyBeforeRanking(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := newTimelineActivityFixture(t)
	q := timelineContactQuery(f)
	past := q.Now.Add(-time.Hour)
	// The lower-ID future copy wins the normal tie between equal threads.
	f.add(t, f.ConvID, f.Source.ID, q.Now.Add(time.Hour), false, "<copy@example.com>")
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	require.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
	conv, err := f.Store.EnsureConversation(other.ID, "past-copy", "Past copy")
	require.NoError(err)
	f.add(t, conv, other.ID, past, false, "<copy@example.com>")
	projectTierSignals(t, f.Fixture, "UTC", 0)
	got, err := f.Store.MessageContactAggregateContext(t.Context(), q)
	require.NoError(err)
	assert.Equal(1, got.Count)
	assert.Equal(&past, got.FirstAt)
	assert.Equal(&past, got.LastAt)
	assert.Equal(&past, got.LastInboundAt)
	assert.Equal(1, got.Monthly[11])
}

func calendarAttendanceFixture(t *testing.T, address, envelope string) (*timelineActivityFixture, time.Time, int64, []int64) {
	t.Helper()
	requirements := require.New(t)
	f := newTimelineActivityFixture(t)
	now := timelineContactQuery(f).Now
	observation := f.add(t, f.ConvID, f.Source.ID, now, false, "")
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET is_sent=FALSE WHERE id=?`), observation)
	requirements.NoError(err)
	source, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	requirements.NoError(err)
	requirements.NoError(f.Store.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	raw := `{"id":"planning","iCalUID":"planning@example.com","summary":"Planning","start":{"dateTime":"2026-10-22T10:00:00Z"},"end":{"dateTime":"2026-10-22T11:00:00Z"},"attendees":[{"email":"` + address + `","displayName":"Alice"}]}`
	if envelope == "none" {
		raw = strings.NewReplacer(`"attendees":[{`, `"organizer":{`, `}]}`, `}}`).Replace(raw)
	}
	eventRaw := raw
	if envelope == "canonical subject" {
		eventRaw = strings.ReplaceAll(raw, "alice@example.com", "other@example.com")
	}
	event := calendarMergeEvent(t, f.Store, source.ID, "planning", eventRaw)
	copyID := calendarMergeEvent(t, f.Store, source.ID, "calendar-copy", strings.ReplaceAll(raw, `"id":"planning"`, `"id":"calendar-copy"`))
	for _, id := range []int64{event, copyID} {
		requirements.NoError(f.Store.SetMessageMetadata(id, sql.NullString{String: `{"ical_uid":"planning@example.com","calendar_id":"calendar@example.com"}`, Valid: true}))
	}
	noteSource, err := f.Store.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	var notes []int64
	for _, key := range []string{"one", "two"} {
		at := now.Add(-time.Hour)
		raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":"planning@example.com","startsAt":%q},"participants":[{"displayName":"Alice","handle":"alice@example.com"}]}`, at.Format(time.RFC3339))
		note, err := f.Store.PersistMessage(&store.MessagePersistData{
			Message:      &store.Message{SourceID: noteSource.ID, SourceMessageID: key, MessageType: "meeting_transcript", IsFromMe: true, Subject: sql.NullString{String: "Synthetic meeting", Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}},
			Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting", Title: "Synthetic meeting"},
			BodyText:     sql.NullString{String: "Synthetic notes", Valid: true}, RawMIME: []byte(raw), RawFormat: "twenty_json",
		})
		requirements.NoError(err)
		var recipients []int64
		if envelope == "other" {
			recipients = []int64{f.EnsureParticipant("other@example.com", "Other", "example.com")}
		}
		requirements.NoError(f.Store.ReplaceMessageRecipients(note, "to", recipients, nil))
		notes = append(notes, note)
	}
	projectColdTimeline(t, f, "UTC")
	return f, now, event, notes
}

func TestCalendarAttendanceTranscriptIDs(t *testing.T) {
	for _, saved := range []bool{false, true} {
		for _, envelope := range []string{"other", "none", "canonical subject", "source scope"} {
			t.Run(fmt.Sprintf("calendar/saved=%t/envelope=%s", saved, envelope), func(t *testing.T) {
				assertions, requirements := assert.New(t), require.New(t)
				f, now, event, notes := calendarAttendanceFixture(t, "alice@example.com", envelope)
				subject := store.ActivitySubject{ParticipantIDs: []int64{f.peer}}
				if saved {
					person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
					requirements.NoError(err)
					subject = store.ActivitySubject{PersonID: person.ID}
				}
				q := store.ContactQuery{Subject: subject, Now: now, Location: time.UTC}
				ctx := t.Context()
				if envelope == "source scope" {
					calendar, err := f.Store.GetMessageSourceContext(ctx, event)
					requirements.NoError(err)
					ctx = sourcescope.WithSourceIDs(ctx, []int64{f.Source.ID, calendar.ID})
				}
				ids, more, err := f.Store.AttendedTranscriptIDsContext(ctx, q, 10)
				requirements.NoError(err)
				assertions.False(more)
				if envelope == "canonical subject" || envelope == "source scope" {
					assertions.Empty(ids.IDs)
					return
				}
				assertions.ElementsMatch(notes, ids.IDs)
				bounded, more, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 1)
				requirements.NoError(err)
				assertions.Len(bounded.IDs, 1)
				assertions.True(more)
			})
		}
	}
}

func TestCalendarAttendanceIgnoresUnobservedPersonAddressInScope(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	f, now, event, notes := calendarAttendanceFixture(t, "unobserved@example.com", "empty")
	person, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirements.NoError(err)
	unobserved := f.EnsureParticipant("unobserved@example.com", "Unobserved", "example.com")
	detail, err := f.Store.GetCalendarOccurrenceContext(t.Context(), fmt.Sprintf("evt:%d", event))
	requirements.NoError(err)
	noteSource, err := f.Store.GetOrCreateSource("meeting_import", "notes@example.com")
	requirements.NoError(err)
	ctx := sourcescope.WithSourceIDs(t.Context(), []int64{f.Source.ID, detail.SourceID, noteSource.ID})
	q := store.ContactQuery{Subject: store.ActivitySubject{PersonID: person.ID}, Now: now, Location: time.UTC}
	before, _, err := f.Store.AttendedTranscriptIDsContext(ctx, q, 10)
	requirements.NoError(err)
	assertions.Empty(before.IDs)
	_, err = f.Store.LinkParticipants(f.peer, unobserved)
	requirements.NoError(err)
	after, _, err := f.Store.AttendedTranscriptIDsContext(ctx, q, 10)
	requirements.NoError(err)
	assertions.Equal(before.IDs, after.IDs)
	owner, _, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
	requirements.NoError(err)
	assertions.ElementsMatch(notes, owner.IDs)
}

func TestCalendarAttendanceExcludesUnflaggedAccountIdentity(t *testing.T) {
	f, now, _, _ := calendarAttendanceFixture(t, "owner@example.com", "none")
	q := store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.owner}}, Now: now, Location: time.UTC}
	ids, _, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
	require.NoError(t, err)
	assert.Empty(t, ids.IDs)
}

func TestCalendarAttendanceAgreesOnEmailIdentifierType(t *testing.T) {
	for _, identifierType := range []string{"email", "username"} {
		t.Run(identifierType, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			f, now, _, _ := calendarAttendanceFixture(t, "alias@example.com", "empty")
			_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`INSERT INTO participant_identifiers
		(participant_id, identifier_type, identifier_value) VALUES (?, ?, 'alias@example.com')`), f.peer, identifierType)
			requirements.NoError(err)
			q := store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{f.peer}}, Now: now, Location: time.UTC}
			attended, _, err := f.Store.AttendedTranscriptIDsContext(t.Context(), q, 10)
			requirements.NoError(err)
			byEmail, err := f.Store.ParticipantsByEmailContext(t.Context(), []string{"alias@example.com"})
			requirements.NoError(err)
			_, resolved := byEmail["alias@example.com"]
			// Day/history attendance and the relationship count must agree.
			assertions.Equal(identifierType == "email", resolved)
			assertions.Equal(resolved, len(attended.IDs) > 0)
		})
	}
}
