package store

import (
	"bytes"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// emailFrom wraps a calendar part in an email with a plain-text alternative.
func emailFrom(from string, calendar []byte) []byte {
	return []byte("From: " + from + "\r\nTo: guest@example.org\r\nSubject: Original email\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=invite\r\n\r\n--invite\r\nContent-Type: text/plain\r\n\r\nEmail body stays intact\r\n--invite\r\nContent-Type: text/calendar\r\n\r\n" + string(calendar) + "\r\n--invite--\r\n")
}

func inviteEmail(calendar []byte) []byte {
	return emailFrom("organizer@example.com", calendar)
}

func TestEmailCalendarLearnsMissingOrganizer(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "no-organizer", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original")))
	persistInviteEmail(t, st, "update", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated\r\nORGANIZER:mailto:owner@example.com")))
	ids := emailEvents(t, st, source)
	require.Len(t, ids, 1)
	assert.Equal(t, "Updated", calendarRows(t, st, ids...)[0].Subject)
	persistInviteEmail(t, st, "mismatch", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Rejected\r\nORGANIZER:mailto:other@example.org")))
	assert.Equal(t, "Updated", calendarRows(t, st, ids...)[0].Subject)
}

func persistInviteEmail(t *testing.T, st *Store, key string, raw []byte) (int64, int64) {
	t.Helper()
	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	require.NoError(t, err)
	return persistInviteEmailForSource(t, st, source.ID, key, raw), source.ID
}

func persistInviteEmailForSource(t *testing.T, st *Store, sourceID int64, key string, raw []byte) int64 {
	t.Helper()
	id, err := st.PersistMessageContext(t.Context(), &MessagePersistData{Message: &Message{SourceID: sourceID, SourceMessageID: key, MessageType: "email", Subject: sql.NullString{String: "Original email", Valid: true}}, Conversation: &ConversationPersistData{SourceConversationID: key, Title: "Original email"}, BodyText: sql.NullString{String: "Email body stays intact", Valid: true}, RawMIME: raw})
	require.NoError(t, err)
	return id
}
func emailEvents(t *testing.T, st *Store, source int64) []int64 {
	t.Helper()
	rows, err := st.db.Query(`SELECT id FROM messages WHERE source_id=? AND message_type='calendar_event' ORDER BY id`, source)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		require.NoError(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	require.NoError(t, rows.Err())
	return ids
}

func TestEmailCalendarSourceCountsExcludeDerivedEvents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "count-invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
	requirements.Len(emailEvents(t, st, source), 1)
	gcalData, _ := calendarFixture(t, st, "count-gcal", calendarRaw)
	_, err := st.PersistMessageContext(t.Context(), gcalData)
	requirements.NoError(err)
	icsSourceID := icsSource(t, st)
	_, err = st.PersistICalendarContext(t.Context(), icsSourceID, calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	requirements.NoError(err)
	counts, err := st.CountMessagesBySourceContext(t.Context())
	requirements.NoError(err)
	for _, sourceID := range []int64{source, gcalData.Message.SourceID, icsSourceID} {
		live, err := st.CountMessagesForSourceContext(t.Context(), sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), live)
		raw, err := st.CountMessagesWithRaw(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), raw)
		assertions.Equal(int64(1), counts[sourceID].Live)
	}
}

func TestEmailCalendarEventsUseConfirmedMailAccountScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sourceType string
		identifier string
		confirmed  bool
		want       string
	}{
		{name: "confirmed Gmail", sourceType: "gmail", identifier: "owner@example.com", confirmed: true, want: "owner@example.com"},
		{name: "confirmed IMAP", sourceType: "imap", identifier: "imap://owner%40example.com@mail.example.com:143", confirmed: true, want: "owner@example.com"},
		{name: "unconfirmed Gmail", sourceType: "gmail", identifier: "owner@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source, err := st.GetOrCreateSource(tc.sourceType, tc.identifier)
			requirements.NoError(err)
			if tc.confirmed {
				requirements.NoError(st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
			}
			persistInviteEmailForSource(t, st, source.ID, "calendar-account-scope", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
			eventIDs := emailEvents(t, st, source.ID)
			requirements.Len(eventIDs, 1)

			var address, path sql.NullString
			requirements.NoError(st.db.QueryRow(`SELECT account_address, account_path FROM messages WHERE id=?`, eventIDs[0]).Scan(&address, &path))
			assertions.Equal(tc.want, address.String)
			assertions.Equal("calendar", path.String)
		})
	}
}

func TestEmailCalendarMaterialization(t *testing.T) {
	for _, sender := range []string{"organizer@example.com", "other@example.net"} {
		t.Run(sender, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var source int64
			var emailIDs []int64
			for i, revision := range []struct{ method, props string }{
				{"REQUEST", "SEQUENCE:0\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nSUMMARY:Original"},
				{"REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nDURATION:PT1H\r\nSUMMARY:Updated"},
				{"CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2"},
			} {
				raw := emailFrom(sender, calendarDocument(revision.method, revision.props+"\r\nORGANIZER:mailto:organizer@example.com"))
				id, sid := persistInviteEmail(t, st, fmt.Sprintf("email-%d", i), raw)
				emailIDs = append(emailIDs, id)
				source = sid
				stored, err := st.GetMessageRaw(id)
				requirements.NoError(err)
				assertions.Equal(raw, stored)
				ids := emailEvents(t, st, source)
				requirements.Len(ids, 1)
				row := calendarRows(t, st, ids...)[0]
				assertions.Equal([]string{"Original", "Updated", "Updated"}[i], row.Subject)
				assertions.Equal([]string{"confirmed", "confirmed", "cancelled"}[i], row.Event.Status)
				requirements.NotNil(row.Event.Start)
				assertions.Equal([]string{"2027-03-02T12:00:00Z", "2027-03-02T13:00:00Z", "2027-03-02T13:00:00Z"}[i], row.Event.Start.UTC().Format(time.RFC3339))
			}
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			rows := calendarRows(t, st, ids...)
			requirements.Len(rows, 1)
			assertions.Equal("cancelled", rows[0].Event.Status)
			assertions.Equal("Updated", rows[0].Subject)
			for _, id := range emailIDs[1:] {
				metadata, err := st.GetMessageMetadata(id)
				requirements.NoError(err)
				assertions.NotContains(metadata.String, "calendar_invite_errors")
			}
			var emails int
			requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE source_id=? AND message_type='email'`, source).Scan(&emails))
			assertions.Equal(3, emails)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC), time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal("cancelled", page.Occurrences[0].Status)
			assertions.Equal("Updated", page.Occurrences[0].Subject)
		})
	}

	t.Run("claim identity", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		request := inviteEmail([]byte(strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\n")))
		parent, source := persistInviteEmail(t, st, "organizer-message", request)
		originalIDs := emailEvents(t, st, source)
		requirements.Len(originalIDs, 2)
		for _, id := range originalIDs {
			assertions.NotEqual(parent, id)
		}
		_, err := st.PersistMessageContext(t.Context(), &MessagePersistData{
			Message:      &Message{SourceID: source, SourceMessageID: "organizer-message", MessageType: "email", Subject: sql.NullString{String: "Original email", Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "organizer-message", Title: "Original email"},
			RawMIME:      request,
		})
		requirements.NoError(err)
		assertions.Equal(originalIDs, emailEvents(t, st, source), "rebuilding the same email keeps its claim identity")
	})
}

func TestEmailCalendarMaterializesOnRawMIMEUpsert(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "raw-upsert", []byte("Subject: ordinary\r\n\r\nBody"))
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, raw, "mime"))
	assertions.Len(emailEvents(t, st, source), 1)
}

func TestEmailCalendarInvalidPartPreservesEmail(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	raw := inviteEmail([]byte("invalid calendar"))
	id, source := persistInviteEmail(t, st, "bad-calendar", raw)
	assertions.Empty(emailEvents(t, st, source))
	stored, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	metadata, err := st.GetMessageMetadata(id)
	requirements.NoError(err)
	assertions.False(metadata.Valid)
	assertions.Contains(logs.String(), "level=WARN")
	assertions.Contains(logs.String(), fmt.Sprintf("message_id=%d", id))
}

func TestEmailCalendarCancellationPreemptsOlderInvite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	cancel := inviteEmail(calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com"))
	cancelID, source := persistInviteEmail(t, st, "cancel-first", cancel)
	assertions.Len(emailEvents(t, st, source), 1)
	metadata, err := st.GetMessageMetadata(cancelID)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "calendar_invite_errors")
	invite := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:0\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Older title"))
	persistInviteEmail(t, st, "older-invite", invite)
	update := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated"))
	persistInviteEmail(t, st, "older-update", update)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	row := calendarRows(t, st, ids...)[0]
	assertions.Equal("cancelled", row.Event.Status)
	requirements.NotNil(row.Event.Start)
	assertions.Equal("2027-03-02T12:00:00Z", row.Event.Start.Format(time.RFC3339))
	assertions.Equal("Older title", row.Subject)
	raw, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	assertions.Equal(calendarDocument("REQUEST", "SEQUENCE:0\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Older title"), raw)
}

func TestEmailCalendarExistingRevisionRequiresOriginalOrganizer(t *testing.T) {
	for _, tc := range []struct {
		name, organizer, sender, cancel, status, diagnostic string
		events                                              int
		forged                                              bool
	}{
		{"matching claimed organizer", "ORGANIZER;CN=Owner;SENT-BY=\"mailto:service@example.org\":mailto:OWNER@EXAMPLE.COM", "owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER;CN=Different;SENT-BY=\"mailto:other@example.net\":mailto:owner@example.com", "cancelled", "", 1, false},
		{"missing original organizer", "", "owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER:mailto:owner@example.com", "cancelled", "", 1, false},
		{"missing incoming organizer", "ORGANIZER:mailto:owner@example.com", "owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1", "confirmed", "update ignored: ORGANIZER does not match invite_organizer", 1, false},
		{"forged", "", "attendee@example.net", "STATUS:CANCELLED\r\nSEQUENCE:999\r\nORGANIZER:mailto:attendee@example.net\r\nSUMMARY:Forged cancellation", "confirmed", "update ignored: ORGANIZER does not match invite_organizer", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			props := "DTSTART:20270302T120000Z\r\nSUMMARY:Original"
			if tc.organizer != "" {
				props += "\r\n" + tc.organizer
			}
			request := emailFrom("notifications@example.com", calendarDocument("REQUEST", props))
			if tc.forged {
				request = inviteEmail([]byte(strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\n")))
			}
			_, source := persistInviteEmail(t, st, "original", request)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, tc.events)
			before := calendarRows(t, st, ids...)
			rawCancel := emailFrom(tc.sender, calendarDocument("CANCEL", tc.cancel))
			cancel, _ := persistInviteEmail(t, st, "cancel", rawCancel)
			assertions.Equal(ids, emailEvents(t, st, source))
			for _, row := range calendarRows(t, st, ids...) {
				assertions.Equal(tc.status, row.Event.Status)
			}
			metadata, err := st.GetMessageMetadata(cancel)
			requirements.NoError(err)
			if tc.diagnostic == "" {
				assertions.NotContains(metadata.String, "calendar_invite_errors")
			} else {
				assertions.False(metadata.Valid)
				assertions.Contains(logs.String(), tc.diagnostic)
				assertions.Equal(before, calendarRows(t, st, ids...))
			}
			retained, err := st.GetMessageRaw(cancel)
			requirements.NoError(err)
			assertions.Equal(rawCancel, retained)
		})
	}
}

func TestEmailCalendarReplyAndCancellationPreserveInviteRSVP(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org")
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(raw))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	before := calendarRows(t, st, ids...)
	reply := emailFrom("guest@example.org", calendarDocument("REPLY", "ATTENDEE;PARTSTAT=DECLINED:mailto:guest@example.org"))
	parent, _ := persistInviteEmail(t, st, "reply", reply)
	assertions.Equal(before, calendarRows(t, st, ids...))
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "calendar_invite_errors")
	cancelID, _ := persistInviteEmail(t, st, "cancel", inviteEmail(calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER:mailto:organizer@example.com")))
	event := calendarRows(t, st, ids...)[0].Event
	assertions.Equal("cancelled", event.Status)
	assertions.Equal(before[0].Event.Attendees, event.Attendees)
	assertions.Equal("accepted", event.Attendees[0].RSVP)
	metadata, err = st.GetMessageMetadata(cancelID)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "calendar_invite_errors")
	stored, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	assertions.Equal(raw, stored)
}
