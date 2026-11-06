package store

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmailCalendarGmailAuditExcludesDerivedEvents(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	id := persistInviteEmailForSource(t, st, source.ID, "audit-invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
	requirements.Len(emailEvents(t, st, source.ID), 1)
	for _, sourceID := range []int64{source.ID, 0} {
		var ids []int64
		count, err := st.StreamGmailAuditEvidencePageContext(t.Context(), sourceID, 0, 10, func(item GmailAuditEvidence) error {
			ids = append(ids, item.ID)
			return nil
		})
		requirements.NoError(err)
		assertions.Equal(1, count)
		assertions.Equal([]int64{id}, ids)
	}
}

func TestEmailCalendarSingleParticipantLockOrder(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL participant directory locks")
	}
	source, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	_, err = st.EnsureParticipant("existing@example.com", "Existing", "example.com")
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	blocker, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	requirements.NoError(st.lockParticipantDirectoryMutationTxContext(ctx, blocker))
	var id int64
	done := make(chan error, 1)
	go func() {
		var persistErr error
		id, persistErr = st.PersistMessageWithParticipantsContext(ctx, []ParticipantPersistData{
			{EmailAddress: "organizer@example.com", DisplayName: "Organizer", Domain: "example.com"},
		}, func(ids []int64) *MessagePersistData {
			return &MessagePersistData{
				Message:      &Message{SourceID: source.ID, SourceMessageID: "single-invite", MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}},
				Conversation: &ConversationPersistData{SourceConversationID: "single-invite"},
				RawMIME:      inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")),
			}
		})
		done <- persistErr
	}()
	// Release the blocker and join the writer even when a lock assertion fails.
	joined := false
	defer func() {
		_ = blocker.Rollback()
		cancel()
		if !joined {
			<-done
		}
	}()
	var pid int
	requirements.NoError(blocker.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	requirements.Eventually(func() bool {
		var waiting bool
		return st.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND ? = ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting) == nil && waiting
	}, 5*time.Second, 10*time.Millisecond, "email writer must reach the directory lock")
	// An ICS import holding the directory must still be able to update the revision.
	_, err = blocker.ExecContext(ctx, `SELECT value FROM archive_metadata WHERE key='participant_display_name_revision' FOR UPDATE NOWAIT`)
	assertions.NoError(err, "email writer must acquire the directory before the revision row")
	requirements.NoError(blocker.Rollback())
	err = <-done
	joined = true
	requirements.NoError(err)
	assertions.Positive(id)
	assertions.Len(emailEvents(t, st, source.ID), 1)
}

// emailFrom wraps a calendar part in an email with a plain-text alternative.
func emailFrom(from string, calendar []byte) []byte {
	return []byte("From: " + from + "\r\nTo: guest@example.org\r\nSubject: Original email\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=invite\r\n\r\n--invite\r\nContent-Type: text/plain\r\n\r\nEmail body stays intact\r\n--invite\r\nContent-Type: text/calendar\r\n\r\n" + string(calendar) + "\r\n--invite--\r\n")
}

func inviteEmail(calendar []byte) []byte {
	return emailFrom("organizer@example.com", calendar)
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

func TestEmailCalendarSourceCountsIncludeDerivedEvents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "count-invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
	requirements.Len(emailEvents(t, st, source), 1)
	stats, err := st.GetStats()
	requirements.NoError(err)
	assertions.Equal(int64(2), stats.MessageCount)
	counts, err := st.CountMessagesBySourceContext(t.Context())
	requirements.NoError(err)
	assertions.Equal(stats.MessageCount, counts[source].Live)

	gcalData, _ := calendarFixture(t, st, "count-gcal", calendarRaw)
	_, err = st.PersistMessageContext(t.Context(), gcalData)
	requirements.NoError(err)
	icsSourceID := icsSource(t, st)
	_, err = st.PersistICalendarContext(t.Context(), icsSourceID, calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	requirements.NoError(err)
	counts, err = st.CountMessagesBySourceContext(t.Context())
	requirements.NoError(err)
	for _, sourceID := range []int64{source, gcalData.Message.SourceID, icsSourceID} {
		live, err := st.CountMessagesForSourceContext(t.Context(), sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), live)
		raw, err := st.CountMessagesWithRaw(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), raw)
		wantListingCount := int64(1)
		if sourceID == source {
			wantListingCount = 2
		}
		assertions.Equal(wantListingCount, counts[sourceID].Live)
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
		{name: "confirmed Microsoft", sourceType: "msmail", identifier: "owner@example.com", confirmed: true, want: "owner@example.com"},
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
	t.Run("revisions", func(t *testing.T) {
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
			raw := emailFrom("organizer@example.com", calendarDocument(revision.method, revision.props+"\r\nORGANIZER:mailto:organizer@example.com"))
			id, sid := persistInviteEmail(t, st, fmt.Sprintf("email-%d", i), raw)
			emailIDs = append(emailIDs, id)
			source = sid
			stored, err := st.GetMessageRaw(id)
			requirements.NoError(err)
			assertions.Equal(raw, stored)
		}
		ids := emailEvents(t, st, source)
		requirements.Len(ids, 1)
		rows := calendarRows(t, st, ids...)
		requirements.Len(rows, 1)
		for _, id := range emailIDs {
			email, err := st.GetMessage(id)
			requirements.NoError(err)
			assertions.Equal("Original email", email.Subject)
			assertions.Equal("Email body stays intact", email.BodyText)
		}
		var emails int
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE source_id=? AND message_type='email'`, source).Scan(&emails))
		assertions.Equal(3, emails)
	})

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

func TestEmailCalendarNULFields(t *testing.T) {
	for _, tc := range []struct{ name, property, wantName, wantEmail, wantSubject string }{
		{"summary", "SUMMARY:Before\x00after", "", "", "Before\uFFFDafter"},
		{"attendee name", "ATTENDEE;CN=Before\x00after:mailto:guest@example.org", "Before\uFFFDafter", "guest@example.org", ""},
		{"organizer name", "ORGANIZER;CN=Before\x00after:mailto:organizer@example.com", "Before\uFFFDafter", "organizer@example.com", ""},
		{"address", "ATTENDEE:mailto:before\x00after@example.org", "", "before\uFFFDafter@example.org", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\n"+tc.property))
			id, source := persistInviteEmail(t, st, "nul-attendee", raw)
			stored, err := st.GetMessageRaw(id)
			requirements.NoError(err)
			assertions.Equal(raw, stored)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			rows := calendarRows(t, st, ids...)
			requirements.Len(rows, 1)
			if tc.wantSubject != "" {
				assertions.Equal(tc.wantSubject, rows[0].Subject)
			} else {
				requirements.Len(rows[0].Event.Attendees, 1)
				assertions.Equal(tc.wantName, rows[0].Event.Attendees[0].DisplayName)
				assertions.Equal(tc.wantEmail, rows[0].Event.Attendees[0].Email)
			}
		})
	}
}

func TestEmailCalendarCancellationPreemptsOlderInvite(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	cancel := inviteEmail(calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Cancelled meeting"))
	_, source := persistInviteEmail(t, st, "cancel-first", cancel)
	assertions.Len(emailEvents(t, st, source), 1)
	invite := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:0\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Older title"))
	persistInviteEmail(t, st, "older-invite", invite)
	update := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated"))
	persistInviteEmail(t, st, "older-update", update)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	row := calendarRows(t, st, ids...)[0]
	assertions.Equal("cancelled", row.Event.Status)
	requirements.NotNil(row.Event.Start)
	assertions.Equal("2027-03-02T14:00:00Z", row.Event.Start.Format(time.RFC3339))
	assertions.Equal("Cancelled meeting", row.Subject)
	raw, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	assertions.Equal(calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Cancelled meeting"), raw)
}

func TestEmailCalendarExistingRevisionRequiresOriginalOrganizer(t *testing.T) {
	for _, tc := range []struct {
		name, organizer, cancel, status, diagnostic string
	}{
		{"matching claimed organizer", "ORGANIZER;CN=Owner;SENT-BY=\"mailto:service@example.org\":mailto:OWNER@EXAMPLE.COM", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER;CN=Different;SENT-BY=\"mailto:other@example.net\":mailto:owner@example.com", "cancelled", ""},
		{"missing incoming organizer", "ORGANIZER:mailto:owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1", "confirmed", "update ignored: ORGANIZER does not match invite_organizer"},
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
			_, source := persistInviteEmail(t, st, "original", request)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			before := calendarRows(t, st, ids...)
			rawCancel := inviteEmail(calendarDocument("CANCEL", tc.cancel))
			cancel, _ := persistInviteEmail(t, st, "cancel", rawCancel)
			assertions.Equal(ids, emailEvents(t, st, source))
			for _, row := range calendarRows(t, st, ids...) {
				assertions.Equal(tc.status, row.Event.Status)
			}
			if tc.diagnostic != "" {
				metadata, err := st.GetMessageMetadata(cancel)
				requirements.NoError(err)
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
	reply := emailFrom("guest@example.org", calendarDocument("REPLY", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTAMP:20270301T130000Z\r\nATTENDEE;PARTSTAT=DECLINED:mailto:guest@example.org"))
	persistInviteEmail(t, st, "reply", reply)
	assertions.Equal(before, calendarRows(t, st, ids...))
	persistInviteEmail(t, st, "cancel", inviteEmail(calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER:mailto:organizer@example.com")))
	event := calendarRows(t, st, ids...)[0].Event
	assertions.Equal("cancelled", event.Status)
	assertions.Equal(before[0].Event.Attendees, event.Attendees)
	assertions.Equal("accepted", event.Attendees[0].RSVP)
	stored, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	assertions.Equal(raw, stored)
}

func TestEmailCalendarStaleRevisionDoesNotPinOrganizer(t *testing.T) {
	t.Run("first applied revision pins organizer within one email", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		first := calendarDocument("REQUEST", "DTSTAMP:20270301T120000Z\r\nSEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Accepted\r\nORGANIZER:mailto:owner-a@example.com")
		later := "BEGIN:VEVENT\r\nUID:family@example.com\r\nDTSTAMP:20270301T120000Z\r\nSEQUENCE:2\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Rejected\r\nORGANIZER:mailto:owner-b@example.com\r\nEND:VEVENT\r\n"
		calendar := []byte(strings.Replace(string(first), "END:VCALENDAR\r\n", later+"END:VCALENDAR\r\n", 1))
		_, source := persistInviteEmail(t, st, "revisions", inviteEmail(calendar))
		ids := emailEvents(t, st, source)
		requirements.Len(ids, 1)
		rows := calendarRows(t, st, ids...)
		requirements.Len(rows, 1)
		assertions.Equal("Accepted", rows[0].Subject)
		var organizer string
		requirements.NoError(st.db.QueryRow(`SELECT metadata ->> 'invite_organizer' FROM messages WHERE id=?`, ids[0]).Scan(&organizer))
		assertions.Equal("owner-a@example.com", organizer)
	})
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "original", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Original")))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)

	stale := calendarDocument("REQUEST", "DTSTAMP:20270301T120000Z\r\nSEQUENCE:1\r\nDTSTART:20270302T110000Z\r\nSUMMARY:Stale\r\nORGANIZER:mailto:owner-a@example.com")
	newer := "BEGIN:VEVENT\r\nUID:family@example.com\r\nDTSTAMP:20270301T120000Z\r\nSEQUENCE:3\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated\r\nORGANIZER:mailto:owner-b@example.com\r\nEND:VEVENT\r\n"
	calendar := []byte(strings.Replace(string(stale), "END:VCALENDAR\r\n", newer+"END:VCALENDAR\r\n", 1))
	persistInviteEmail(t, st, "update", inviteEmail(calendar))

	assertions.Equal(ids, emailEvents(t, st, source))
	rows := calendarRows(t, st, ids...)
	requirements.Len(rows, 1)
	assertions.Equal("Updated", rows[0].Subject)
	requirements.NotNil(rows[0].Event.Start)
	assertions.Equal("2027-03-02T13:00:00Z", rows[0].Event.Start.Format(time.RFC3339))
	var organizer string
	requirements.NoError(st.db.QueryRow(`SELECT metadata ->> 'invite_organizer' FROM messages WHERE id=?`, ids[0]).Scan(&organizer))
	assertions.Equal("owner-b@example.com", organizer)
}

func TestEmailCalendarFamilyOrganizerAdoption(t *testing.T) {
	for _, tc := range []struct {
		name              string
		tombstone, single bool
	}{
		{"series", false, false},
		{"tombstone", true, false},
		{"missing original organizer", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			first := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nRRULE:FREQ=DAILY;COUNT=2")
			if tc.tombstone {
				first = calendarDocument("CANCEL", "SEQUENCE:0\r\nSTATUS:CANCELLED\r\nDTSTART:20270302T120000Z")
			}
			if tc.single {
				first = calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original")
			}
			_, source := persistInviteEmail(t, st, "original", inviteEmail(first))
			originalIDs := emailEvents(t, st, source)
			props := "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com"
			if !tc.tombstone && !tc.single {
				props += "\r\nRECURRENCE-ID:20270303T120000Z"
			}
			if tc.single {
				props = "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated\r\nORGANIZER:mailto:owner@example.com"
			}
			persistInviteEmail(t, st, "adoption", inviteEmail(calendarDocument("REQUEST", props)))
			ids := emailEvents(t, st, source)
			before := calendarRows(t, st, ids...)
			requirements.NotEmpty(before)
			persistInviteEmail(t, st, "mismatch", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:3\r\nDTSTART:20270302T150000Z\r\nSUMMARY:Rejected\r\nORGANIZER:mailto:other@example.org")))
			assertions.Equal(before, calendarRows(t, st, ids...))
			assertions.Equal(ids, emailEvents(t, st, source))

			if tc.single {
				requirements.Len(originalIDs, 1)
				assertions.Equal(originalIDs, ids)
				assertions.Equal("Updated", before[0].Subject)
			}
		})
	}
}

func TestEmailCalendarRevisionAfterSourceMerge(t *testing.T) {
	t.Run("master and exception retain renamed conversation", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, from := persistInviteEmail(t, st, "family", inviteEmail(calendarFamilyDocument()))
		into, err := st.GetOrCreateSource("mbox", "destination@example.com")
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
		requirements.NoError(err)
		ids := emailEvents(t, st, into.ID)
		requirements.Len(ids, 2)
		var conversationID int64
		requirements.NoError(st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, ids[0]).Scan(&conversationID))
		update := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Updated series")
		persistInviteEmailForSource(t, st, into.ID, "update", inviteEmail(update))
		for _, id := range ids {
			var got int64
			requirements.NoError(st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, id).Scan(&got))
			assertions.Equal(conversationID, got)
		}
		var count int
		requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE source_id=? AND conversation_type='calendar'`, into.ID).Scan(&count))
		assertions.Equal(1, count)
		requirements.NoError(st.db.QueryRow(`SELECT message_count FROM conversations WHERE id=?`, conversationID).Scan(&count))
		assertions.Equal(2, count)
	})
	for _, tc := range []struct {
		name, initialSequence, sequence, organizer, wantSubject string
		cancel, reject                                          bool
	}{
		{"update", "", "1", "organizer@example.com", "Moved", false, false},
		{"cancel", "", "1", "organizer@example.com", "Moved", true, false},
		{"different organizer", "SEQUENCE:2\r\n", "3", "other@example.org", "Original", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, from := persistInviteEmail(t, st, "original", inviteEmail(calendarDocument("REQUEST", tc.initialSequence+"DTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com")))
			into, err := st.GetOrCreateSource("mbox", "destination@example.com")
			requirements.NoError(err)
			_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
			requirements.NoError(err)
			ids := emailEvents(t, st, into.ID)
			requirements.Len(ids, 1)
			subject := "Moved"
			if tc.reject {
				subject = "Rejected"
			}
			update := calendarDocument("REQUEST", "SEQUENCE:"+tc.sequence+"\r\nDTSTART:20270303T120000Z\r\nSUMMARY:"+subject+"\r\nORGANIZER:mailto:"+tc.organizer)
			raw := inviteEmail(update)
			id := persistInviteEmailForSource(t, st, into.ID, "update", raw)
			assertions.Equal(ids, emailEvents(t, st, into.ID))
			if tc.reject {
				stored, err := st.GetMessageRaw(id)
				requirements.NoError(err)
				assertions.Equal(raw, stored)
			} else {
				stored, err := st.GetMessageRaw(ids[0])
				requirements.NoError(err)
				assertions.Equal(update, stored)
			}
			if tc.cancel {
				persistInviteEmailForSource(t, st, into.ID, "cancel", inviteEmail(calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:organizer@example.com")))
			}
			fromDate := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), CalendarOccurrenceQuery{From: fromDate, To: fromDate.AddDate(0, 0, 2)})
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 1)
			assertions.Equal(tc.wantSubject, page.Occurrences[0].Subject)
			status := "confirmed"
			if tc.cancel {
				status = "cancelled"
			}
			assertions.Equal(status, page.Occurrences[0].Status)
		})
	}
}

func TestEmailCalendarArchiveOnlyMarker(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("gmail", "marker@example.org")
	requirements.NoError(err)
	raw := []byte("Content-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:marker@example.org\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Invite\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, err = st.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "parent", MessageType: "email"},
		Conversation: &ConversationPersistData{SourceConversationID: "parent"}, RawMIME: raw,
	})
	requirements.NoError(err)
	var eventID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND message_type='calendar_event'`), source.ID).Scan(&eventID))
	var marker int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_merge_archive_only_messages WHERE message_id=?`), eventID).Scan(&marker))
	assertions.Equal(1, marker)
}
