package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/calevent"
)

func inviteEmail(calendar []byte) []byte {
	return inviteEmailFrom("organizer@example.com", calendar)
}
func inviteEmailFrom(from string, calendar []byte) []byte {
	return []byte("From: " + from + "\r\nTo: guest@example.org\r\nSubject: Original email\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=invite\r\n\r\n--invite\r\nContent-Type: text/plain\r\n\r\nEmail body stays intact\r\n--invite\r\nContent-Type: text/calendar\r\n\r\n" + string(calendar) + "\r\n--invite--\r\n")
}
func persistInviteEmail(t *testing.T, st *Store, key string, raw []byte) (int64, int64) {
	t.Helper()
	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	require.NoError(t, err)
	id, err := st.PersistMessageContext(t.Context(), &MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: key, MessageType: "email", Subject: sql.NullString{String: "Original email", Valid: true}}, Conversation: &ConversationPersistData{SourceConversationID: key, Title: "Original email"}, BodyText: sql.NullString{String: "Email body stays intact", Valid: true}, RawMIME: raw})
	require.NoError(t, err)
	return id, source.ID
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
func TestEmailCalendarMaterialization(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	raw := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nDURATION:PT1H"))
	parent, source := persistInviteEmail(t, st, "email-one", raw)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	assertions.NotEqual(parent, ids[0])
	var kind, subject string
	requirements.NoError(st.db.QueryRow(`SELECT message_type,subject FROM messages WHERE id=?`, parent).Scan(&kind, &subject))
	assertions.Equal("email", kind)
	assertions.Equal("Original email", subject)
	body, err := st.GetMessageBodyText(parent)
	requirements.NoError(err)
	assertions.Equal("Email body stays intact", body)
	stored, err := st.GetMessageRaw(parent)
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	secondParent, _ := persistInviteEmail(t, st, "email-two", raw)
	claimIDs := emailEvents(t, st, source)
	requirements.Len(claimIDs, 2, "identical content from distinct source emails remains separately addressable")
	assertions.Equal(ids[0], claimIDs[0])
	secondID := claimIDs[1]
	revision := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated series"))
	requirements.NoError(st.UpsertMessageRawWithFormat(secondParent, revision, "mime"))
	rows := calendarRows(t, st, ids[0], secondID)
	requirements.Len(rows, 2)
	assertions.Equal(int64(1), rows[0].Event.Sequence)
	assertions.Equal(int64(2), rows[1].Event.Sequence)
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, revision, "mime"))
	rows = calendarRows(t, st, ids[0], secondID)
	requirements.Len(rows, 2)
	assertions.Equal(int64(2), rows[0].Event.Sequence)
	assertions.Equal(int64(2), rows[1].Event.Sequence)
	// METHOD may exist only on the MIME header. It must survive raw rebuild.
	cancel := strings.Replace(string(calendarDocument("CANCEL", "SEQUENCE:3")), "METHOD:CANCEL\r\n", "", 1)
	cancelMIME := strings.Replace(string(inviteEmail([]byte(cancel))), "Content-Type: text/calendar\r\n", "Content-Type: text/calendar; method=CANCEL\r\n", 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, []byte(cancelMIME), "mime"))
	assertions.Equal("cancelled", calendarRows(t, st, ids[0])[0].Event.Status)
	assertions.Equal("confirmed", calendarRows(t, st, secondID)[0].Event.Status)
	eventRaw, err := st.GetMessageRaw(ids[0])
	requirements.NoError(err)
	requirements.NoError(st.UpsertMessageRawWithFormat(ids[0], eventRaw, "ics"))
	assertions.Equal("cancelled", calendarRows(t, st, ids[0])[0].Event.Status)
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, []byte("Content-Type: text/plain\r\n\r\nReplacement"), "mime"))
	assertions.Equal(claimIDs, emailEvents(t, st, source), "events are independent archive artifacts")
}

func TestEmailCalendarClaimsAreScopedToSourceEmail(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)

	request := inviteEmail(calendarFamilyDocument())
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

	forged := inviteEmailFrom("attendee@example.net", calendarDocument("CANCEL", "SEQUENCE:999\r\nSUMMARY:Forged cancellation"))
	persistInviteEmail(t, st, "attendee-message", forged)
	allIDs := emailEvents(t, st, source)
	requirements.Len(allIDs, 3)
	assertions.Contains(allIDs, originalIDs[0])
	assertions.Equal("confirmed", calendarRows(t, st, originalIDs[0], originalIDs[1])[0].Event.Status)
	assertions.Equal("confirmed", calendarRows(t, st, originalIDs[0], originalIDs[1])[1].Event.Status)
}

func TestEmailCalendarRevisionDoesNotRequeueUnchangedException(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("PostgreSQL update-of trigger regression requires PostgreSQL")
	}

	family := strings.Replace(string(calendarFamilyDocument()),
		"UID:family@example.com\r\nDTSTART:",
		"UID:family@example.com\r\nSEQUENCE:1\r\nDTSTART:", 1)
	parent, source := persistInviteEmail(t, st, "family-revision", inviteEmail([]byte(family)))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 2)
	var exceptionID int64
	for _, id := range ids {
		rows := calendarRows(t, st, id)
		requirements.Len(rows, 1)
		if rows[0].Event.Kind == "exception" {
			exceptionID = id
		}
	}
	requirements.NotZero(exceptionID)

	_, err := st.db.Exec(`UPDATE activity_projection_queue SET processed_revision=revision WHERE message_id=?`, exceptionID)
	requirements.NoError(err)
	var beforeRevision, beforeProcessed int64
	requirements.NoError(st.db.QueryRow(`
		SELECT revision,processed_revision FROM activity_projection_queue WHERE message_id=?`, exceptionID,
	).Scan(&beforeRevision, &beforeProcessed))
	requirements.Positive(beforeRevision)
	assertions.Equal(beforeRevision, beforeProcessed)

	updated := strings.Replace(family, "SEQUENCE:1", "SEQUENCE:2", 1)
	updated = strings.Replace(updated, "SUMMARY:Original series", "SUMMARY:Updated series", 1)
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, inviteEmail([]byte(updated)), "mime"))

	var afterRevision, afterProcessed int64
	requirements.NoError(st.db.QueryRow(`
		SELECT revision,processed_revision FROM activity_projection_queue WHERE message_id=?`, exceptionID,
	).Scan(&afterRevision, &afterProcessed))
	assertions.Equal(beforeRevision, afterRevision,
		"an unchanged family cancellation state must not requeue its detached exception")
	assertions.Equal(beforeProcessed, afterProcessed)
}

func TestEmailCalendarRepairAdoptsMatchingLegacyClaimIdentity(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Legacy claim"))
	_, source := persistInviteEmail(t, st, "legacy-claim-email", raw)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	components, err := calevent.ParseICS(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Legacy claim"), "REQUEST", calevent.SourceContext{CalendarTimeZone: "UTC"})
	requirements.NoError(err)
	legacyID := calevent.ICSMessageID(components[0].Event)
	_, err = st.db.Exec(`UPDATE messages SET source_message_id=? WHERE id=?`, legacyID, ids[0])
	requirements.NoError(err)

	_, err = st.RepairEmailCalendarEventsContext(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(ids, emailEvents(t, st, source), "matching legacy email identity is adopted instead of duplicated")
	var migratedID string
	requirements.NoError(st.db.QueryRow(`SELECT source_message_id FROM messages WHERE id=?`, ids[0]).Scan(&migratedID))
	assertions.NotEqual(legacyID, migratedID)
}

func TestEmailCalendarLegacyExceptionProjectionRescopedBeforeSeriesCancellation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)

	request := string(calendarFamilyDocument())
	request = strings.Replace(request, "UID:family@example.com\r\nDTSTART:", "UID:family@example.com\r\nSEQUENCE:1\r\nDTSTART:", 1)
	request = strings.Replace(request, "UID:family@example.com\r\nRECURRENCE-ID:", "UID:family@example.com\r\nSEQUENCE:1\r\nRECURRENCE-ID:", 1)
	requestICS := []byte(request)
	parent, source := persistInviteEmail(t, st, "legacy-family", inviteEmail(requestICS))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 2)
	components, err := calevent.ParseICS(requestICS, "REQUEST", calevent.SourceContext{CalendarTimeZone: "UTC"})
	requirements.NoError(err)
	requirements.Len(components, 2)
	rows := calendarRows(t, st, ids...)
	requirements.Len(rows, 2)
	for i, component := range components {
		assertions.Equal(component.Event.Kind, rows[i].Event.Kind)
		_, err := st.db.Exec(`UPDATE messages SET source_message_id=? WHERE id=?`, calevent.ICSMessageID(component.Event), ids[i])
		requirements.NoError(err)
		_, err = st.db.Exec(`UPDATE calendar_events SET provider_event_id=?,series_provider_id=? WHERE message_id=?`, component.Event.ProviderEventID, component.Event.SeriesProviderID, ids[i])
		requirements.NoError(err)
	}

	cancel := calendarDocument("CANCEL", "SEQUENCE:2")
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, inviteEmail(cancel), "mime"))

	rows = calendarRows(t, st, ids...)
	requirements.Len(rows, 2)
	assertions.Equal("cancelled", rows[0].Event.Status)
	assertions.Equal("cancelled", rows[1].Event.Status, "the unchanged legacy exception follows its cancelled series")
	assertions.Equal(rows[0].Event.ProviderEventID, rows[1].Event.SeriesProviderID)
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

func TestEmailCalendarRejectsOversizedInMemoryRaw(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "oversized-in-memory", []byte("Subject: ordinary\r\n\r\nBody"))
	oversized := make([]byte, maxMeetingRawBytes+1)
	var skipped int
	err := st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		_, count, err := st.refreshEmailCalendarEventsWith(t.Context(), tx, parent, &oversized)
		skipped = count
		return err
	})
	requirements.NoError(err)
	assertions.Equal(1, skipped)
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Contains(metadata.String, "email raw could not be decoded within the 64 MiB limit")
	assertions.Empty(emailEvents(t, st, source))
}

func TestEmailCalendarSkipsNonInvitationMethodsWithoutDiagnostics(t *testing.T) {
	methods := []string{"ADD", "REFRESH", "REPLY", "COUNTER", "DECLINECOUNTER"}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			st := newRFC822IDBackfillBackendStore(t)
			raw := inviteEmail(calendarDocument(method, "DTSTART:20270302T120000Z"))
			parent, source := persistInviteEmail(t, st, "method-"+method, raw)
			assert.Empty(t, emailEvents(t, st, source))
			metadata, err := st.GetMessageMetadata(parent)
			require.NoError(t, err)
			assert.NotContains(t, metadata.String, "calendar_invite_errors")
		})
	}
}

func TestEmailCalendarInvalidAndAtomic(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART;TZID=Unknown/Zone:20270302T120000"))
	parent, source := persistInviteEmail(t, st, "invalid", raw)
	assertions.Empty(emailEvents(t, st, source))
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Contains(metadata.String, "calendar_invite_errors")
	requirements.NoError(st.SetMessageMetadata(parent, sql.NullString{String: `{"kept":"yes"}`, Valid: true}))
	rejectCalendarWrites(t, st)
	valid := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	requirements.Error(st.UpsertMessageRawWithFormat(parent, valid, "mime"))
	stored, err := st.GetMessageRaw(parent)
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	metadata, err = st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.JSONEq(`{"kept":"yes"}`, metadata.String)
}
func TestEmailCalendarRepair(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "old-email", []byte("Subject: Older email\r\n\r\nBody"))
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	// Exercise the real low-level writer to represent pre-upgrade stored MIME.
	requirements.NoError(st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		return upsertMessageRawWithFormat(boundQuerier{ctx: t.Context(), q: tx}, parent, raw, "mime")
	}))
	assertions.Empty(emailEvents(t, st, source))
	first, err := st.RepairEmailCalendarEventsContext(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(1, first.Changed)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	second, err := st.RepairEmailCalendarEventsContext(t.Context(), source)
	requirements.NoError(err)
	assertions.Zero(second.Changed)
	assertions.Equal(ids, emailEvents(t, st, source))
}

func TestEmailCalendarLegacyType(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("mbox", "legacy@example.com")
	require.NoError(t, err)
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	_, err = st.PersistMessageContext(t.Context(), &MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: "legacy", MessageType: ""}, Conversation: &ConversationPersistData{SourceConversationID: "legacy"}, RawMIME: raw})
	require.NoError(t, err)
	require.Len(t, emailEvents(t, st, source.ID), 1)
}

func TestEmailCalendarSkipsUnrelatedMIMEParsing(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	// The missing multipart boundary makes a full MIME parse fail, but there
	// is no calendar media type to interpret or diagnose in this email.
	plain := []byte("Content-Type: multipart/mixed\r\n\r\nOrdinary body")
	parent, source := persistInviteEmail(t, st, "ordinary", plain)
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "calendar_invite_errors")
	requirements.NoError(st.SetMessageMetadata(parent, sql.NullString{String: `{"kept":"yes","calendar_invite_errors":["old"]}`, Valid: true}))
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, plain, "mime"))
	metadata, err = st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.JSONEq(`{"kept":"yes"}`, metadata.String)
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	folded := []byte(strings.Replace(string(raw), "text/calendar\r\n", "TEXT/CALENDAR;\r\n method=REQUEST\r\n", 1))
	persistInviteEmail(t, st, "folded-calendar", folded)
	assertions.Len(emailEvents(t, st, source), 1)
}

func TestEmailCalendarWithParticipantBuilder(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	id, err := st.PersistMessageWithParticipantsContext(t.Context(), source, []ParticipantPersistData{
		{EmailAddress: "sender@example.org", Domain: "example.org"},
	}, func(ids []int64) *MessagePersistData {
		return &MessagePersistData{
			Message:      &Message{SourceID: source, SourceMessageID: "participant-invite", MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "participant-invite"},
			RawMIME:      raw,
		}
	})
	requirements.NoError(err)
	assert.Positive(t, id)
	assert.Len(t, emailEvents(t, st, source), 1)
}

func TestParticipantBuilderRejectsSourceMismatch(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source := icsSource(t, st)
	other, err := st.GetOrCreateSource("meeting_import", "other-synthetic-source")
	requirements.NoError(err)
	_, err = st.PersistMessageWithParticipantsContext(t.Context(), source, []ParticipantPersistData{
		{EmailAddress: "mismatch@example.org", Domain: "example.org"},
	}, func(ids []int64) *MessagePersistData {
		return &MessagePersistData{
			Message:      &Message{SourceID: other.ID, SourceMessageID: "mismatch", MessageType: "meeting_transcript", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "mismatch"},
		}
	})
	requirements.Error(err)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM participants WHERE email_address=?`, "mismatch@example.org").Scan(&count))
	assert.Zero(t, count, "the rejected builder must roll participant writes back")
}
