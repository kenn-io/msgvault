package store

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/calendarfixture"
)

func inviteEmail(calendar []byte) []byte {
	return calendarfixture.EmailFrom("organizer@example.com", calendar)
}

func TestEmailCalendarTokenRequiresMIMEType(t *testing.T) {
	assertions := assert.New(t)
	assertions.False(hasCalendarToken([]byte("Subject: Calendar reminder\r\n\r\nCheck your calendar")))
	assertions.False(hasCalendarToken([]byte("Content-Disposition: attachment; filename=calendar.ics\r\n")))
	assertions.True(hasCalendarToken([]byte("Content-Type: TeXt/CaLeNdAr; method=REQUEST\r\n")))
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
				raw := calendarfixture.EmailFrom(sender, calendarDocument(revision.method, revision.props+"\r\nORGANIZER:mailto:organizer@example.com"))
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

func TestEmailCalendarRejectsOversizedInMemoryRaw(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "oversized-in-memory", []byte("Subject: ordinary\r\n\r\nBody"))
	valid := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	// Pad the plain-text part, leaving the calendar below its separate parser limit.
	oversized := bytes.Replace(valid, []byte("Email body stays intact"), bytes.Repeat([]byte("x"), maxMeetingRawBytes+1), 1)
	requirements.Greater(len(oversized), maxMeetingRawBytes)
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
	assertions.Contains(metadata.String, "calendar_invite_errors")
	assertions.Contains(metadata.String, "meeting raw exceeds decode limit")
	assertions.Empty(emailEvents(t, st, source))
	err = st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		_, _, err := st.refreshEmailCalendarEventsWith(t.Context(), tx, parent, &valid)
		return err
	})
	requirements.NoError(err)
	assertions.Len(emailEvents(t, st, source), 1, "the same invitation below the raw limit must materialize")
	metadata, err = st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "calendar_invite_errors")
	stored, err := st.GetMessageRaw(parent)
	requirements.NoError(err)
	assertions.Equal([]byte("Subject: ordinary\r\n\r\nBody"), stored)
}

func TestEmailCalendarSkipsNonInvitationMethodsWithoutDiagnostics(t *testing.T) {
	methods := []string{"REPLY", "ADD", "REFRESH", "COUNTER", "DECLINECOUNTER"}
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
	requirements.NoError(st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		q := boundQuerier{ctx: t.Context(), q: tx}
		var conversationID int64
		if err := q.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, parent).Scan(&conversationID); err != nil {
			return err
		}
		for i := range 100 {
			id, err := upsertMessageWith(q, st.dialect, &Message{SourceID: source, ConversationID: conversationID, SourceMessageID: fmt.Sprintf("ordinary-%d", i), MessageType: "email"})
			if err != nil {
				return err
			}
			if err := upsertMessageRawWithFormat(q, id, []byte("Subject: Ordinary\r\n\r\nBody"), "mime"); err != nil {
				return err
			}
		}
		return nil
	}))
	var reports []string
	first, err := st.RepairEmailCalendarEventsContext(t.Context(), source, func(msg string) { reports = append(reports, msg) })
	assertions.Equal([]string{"calendar invites: 100 emails scanned", "calendar invites: 101 emails scanned"}, reports)
	requirements.NoError(err)
	assertions.Equal(1, first.Changed)
	assertions.Equal(101, first.Scanned)
	var count int
	var last time.Time
	requirements.NoError(st.db.QueryRow(`SELECT c.message_count,c.last_message_at FROM conversations c JOIN messages m ON m.conversation_id=c.id WHERE m.message_type='calendar_event' AND m.source_id=?`, source).Scan(&count, &last))
	assertions.Equal(1, count)
	assertions.Equal(time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC), last.UTC())
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	second, err := st.RepairEmailCalendarEventsContext(t.Context(), source, nil)
	requirements.NoError(err)
	assertions.Zero(second.Changed)
	assertions.Equal(ids, emailEvents(t, st, source))
}

func TestEmailCalendarRepairReportsUndecodableRaw(t *testing.T) {
	for _, staleDiagnostics := range []bool{false, true} {
		t.Run(fmt.Sprintf("stale diagnostics=%t", staleDiagnostics), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			parent, source := persistInviteEmail(t, st, "undecodable-raw", []byte("Subject: Older email\r\n\r\nBody"))
			if staleDiagnostics {
				requirements.NoError(st.SetMessageMetadata(parent, sql.NullString{String: `{"calendar_invite_errors":["stale failure"],"kept":true}`, Valid: true}))
			}
			// Represent an archived raw payload whose compressed bytes are corrupt.
			_, err := st.db.Exec(`UPDATE message_raw SET raw_data=?,compression='zlib' WHERE message_id=?`, []byte("invalid compressed email"), parent)
			requirements.NoError(err)
			summary, err := st.RepairEmailCalendarEventsContext(t.Context(), source, nil)
			requirements.NoError(err)
			assertions.Equal(EmailCalendarRepairSummary{Scanned: 1, Skipped: 1}, summary)
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			assertions.Contains(metadata.String, "calendar_invite_errors")
			assertions.Contains(metadata.String, "zlib reader")
			assertions.NotContains(metadata.String, "stale failure")
			if staleDiagnostics {
				var decoded map[string]any
				requirements.NoError(json.Unmarshal([]byte(metadata.String), &decoded))
				assertions.Equal(true, decoded["kept"])
			}
		})
	}
}

func TestEmailCalendarRepairSkipsArchiveOnlyInvitations(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	historySource, err := st.GetOrCreateSource("mbox", "history@example.com")
	requirements.NoError(err)
	liveSource, err := st.GetOrCreateSource("mbox", "live@example.com")
	requirements.NoError(err)
	parentID := persistInviteEmailForSource(t, st, historySource.ID, "archive-only-invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
	eventIDs := emailEvents(t, st, historySource.ID)
	requirements.Len(eventIDs, 1)
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: historySource.ID, IntoSourceID: liveSource.ID})
	requirements.NoError(err)
	mergedEventIDs := emailEvents(t, st, liveSource.ID)
	requirements.Equal(eventIDs, mergedEventIDs)
	var archiveOnly int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM source_merge_archive_only_messages WHERE message_id IN (?, ?)`, parentID, eventIDs[0]).Scan(&archiveOnly))
	assertions.Equal(2, archiveOnly)

	err = st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{liveSource.ID}}, func(tx *loggedTx) error {
		_, _, err := st.refreshEmailCalendarEventsWith(t.Context(), tx, parentID, nil)
		return err
	})
	requirements.NoError(err)
	var ordinaryEvents int
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages m WHERE m.source_id=? AND m.message_type='calendar_event'
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id=m.id)`, liveSource.ID).Scan(&ordinaryEvents))
	assertions.Zero(ordinaryEvents)

	summary, err := st.RepairEmailCalendarEventsContext(t.Context(), liveSource.ID, nil)
	requirements.NoError(err)
	assertions.Zero(summary.Scanned, "repair must skip merged historical email")
	assertions.Zero(summary.Changed)
	requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM messages m WHERE m.source_id=? AND m.message_type='calendar_event'
 AND NOT EXISTS (SELECT 1 FROM source_merge_archive_only_messages am WHERE am.message_id=m.id)`, liveSource.ID).Scan(&ordinaryEvents))
	assertions.Zero(ordinaryEvents)
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
		// Accepted outcome: continuity uses unauthenticated addresses. A stranger
		// who copies the visible ORGANIZER can cancel the event.
		{"AcceptedSpoofedOrganizerCancellation", "ORGANIZER:mailto:owner@example.com", "stranger@example.net", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER:mailto:owner@example.com", "cancelled", "", 1, false},
		{"missing original organizer", "", "owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER:mailto:owner@example.com", "confirmed", "update ignored: ORGANIZER does not match invite_organizer", 1, false},
		{"missing incoming organizer", "ORGANIZER:mailto:owner@example.com", "owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1", "confirmed", "update ignored: ORGANIZER does not match invite_organizer", 1, false},
		{"mismatched address with matching CN and SENT-BY", "ORGANIZER;CN=Owner:mailto:owner@example.com", "owner@example.com", "STATUS:CANCELLED\r\nSEQUENCE:1\r\nORGANIZER;CN=Owner;SENT-BY=\"mailto:owner@example.com\":mailto:other@example.net", "confirmed", "update ignored: ORGANIZER does not match invite_organizer", 1, false},
		{"forged", "", "attendee@example.net", "STATUS:CANCELLED\r\nSEQUENCE:999\r\nORGANIZER:mailto:attendee@example.net\r\nSUMMARY:Forged cancellation", "confirmed", "update ignored: ORGANIZER does not match invite_organizer", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			props := "DTSTART:20270302T120000Z\r\nSUMMARY:Original"
			if tc.organizer != "" {
				props += "\r\n" + tc.organizer
			}
			request := calendarfixture.EmailFrom("notifications@example.com", calendarDocument("REQUEST", props))
			if tc.forged {
				request = inviteEmail([]byte(strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\n")))
			}
			_, source := persistInviteEmail(t, st, "original", request)
			ids := emailEvents(t, st, source)
			requirements.Len(ids, tc.events)
			before := calendarRows(t, st, ids...)
			rawCancel := calendarfixture.EmailFrom(tc.sender, calendarDocument("CANCEL", tc.cancel))
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
				assertions.Contains(metadata.String, "calendar_invite_errors")
				assertions.Contains(metadata.String, tc.diagnostic)
				assertions.Equal(before, calendarRows(t, st, ids...))
			}
			retained, err := st.GetMessageRaw(cancel)
			requirements.NoError(err)
			assertions.Equal(rawCancel, retained)
		})
	}
}

func TestEmailCalendarMatchingOrganizerRespectsRevisionOrder(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	first := calendarfixture.EmailFrom("owner@example.com", calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:owner@example.com"))
	_, source := persistInviteEmail(t, st, "first-invite", first)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)

	sameSequence := calendarfixture.EmailFrom("owner@example.com", calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Same-sequence update\r\nORGANIZER:mailto:owner@example.com"))
	persistInviteEmail(t, st, "same-sequence", sameSequence)
	row := calendarRows(t, st, ids...)[0]
	assertions.Equal("Original", row.Subject)
	requirements.NotNil(row.Event.Start)
	assertions.Equal("2027-03-02T12:00:00Z", row.Event.Start.Format(time.RFC3339))
	var sentAt time.Time
	requirements.NoError(st.db.QueryRow(`SELECT sent_at FROM messages WHERE id=?`, ids[0]).Scan(&sentAt))
	assertions.Equal("2027-03-02T12:00:00Z", sentAt.UTC().Format(time.RFC3339))

	update := calendarfixture.EmailFrom("owner@example.com", calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Updated meeting\r\nORGANIZER:mailto:owner@example.com"))
	updateID, _ := persistInviteEmail(t, st, "update", update)
	cancel := calendarfixture.EmailFrom("owner@example.com", calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:3\r\nSUMMARY:Cancelled meeting\r\nORGANIZER:mailto:owner@example.com"))
	cancelID, _ := persistInviteEmail(t, st, "cancel", cancel)

	assertions.Equal(ids, emailEvents(t, st, source))
	row = calendarRows(t, st, ids...)[0]
	assertions.Equal("cancelled", row.Event.Status)
	assertions.Equal("Updated meeting", row.Subject)
	for _, id := range []int64{updateID, cancelID} {
		metadata, err := st.GetMessageMetadata(id)
		requirements.NoError(err)
		assertions.NotContains(metadata.String, "calendar_invite_errors")
	}
	for _, tc := range []struct {
		id  int64
		raw []byte
	}{{updateID, update}, {cancelID, cancel}} {
		stored, err := st.GetMessageRaw(tc.id)
		requirements.NoError(err)
		assertions.Equal(tc.raw, stored)
	}
}

func TestEmailCalendarMismatchedOrganizerPreservesInvite(t *testing.T) {
	for _, organizer := range []string{"", "ORGANIZER:mailto:owner@example.com\r\n"} {
		t.Run(organizer, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			first := calendarDocument("REQUEST", organizer+"DTSTART:20270302T120000Z\r\nSUMMARY:Original")
			_, source := persistInviteEmail(t, st, "invite", inviteEmail(first))
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			before := calendarRows(t, st, ids...)
			meta, err := st.GetMessageMetadata(ids[0])
			requirements.NoError(err)
			update := calendarfixture.EmailFrom("owner@example.com", calendarDocument("REQUEST", "SEQUENCE:999\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Replacement\r\nORGANIZER:mailto:other@example.net"))
			parent, _ := persistInviteEmail(t, st, "update", update)
			assertions.Equal(ids, emailEvents(t, st, source))
			assertions.Equal(before, calendarRows(t, st, ids...))
			storedMeta, err := st.GetMessageMetadata(ids[0])
			requirements.NoError(err)
			assertions.Equal(meta, storedMeta)
			raw, err := st.GetMessageRaw(ids[0])
			requirements.NoError(err)
			assertions.Equal(first, raw)
			retained, err := st.GetMessageRaw(parent)
			requirements.NoError(err)
			assertions.Equal(update, retained)
			diagnostic, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			assertions.Contains(diagnostic.String, "calendar_invite_errors")
			assertions.Contains(diagnostic.String, "update ignored: ORGANIZER does not match invite_organizer")
		})
	}
}

func TestEmailCalendarMalformedTextPreservesRevision(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com")
	_, source := persistInviteEmail(t, st, "original", inviteEmail(raw))
	id := emailEvents(t, st, source)[0]
	props := "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com\r\nSUMMARY:broken\\q"
	parent, _ := persistInviteEmail(t, st, "malformed", inviteEmail(calendarDocument("REQUEST", props)))
	stored, err := st.GetMessageRaw(id)
	requirements.NoError(err)
	assertions.Equal(raw, stored)
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Contains(metadata.String, "SUMMARY")
	assertions.Contains(metadata.String, "calendar_invite_errors")
}

func TestEmailCalendarRequiresSourceLockOnlyForCalendarMIME(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("mbox", "lock@example.org")
	requirements.NoError(err)
	persist := func(raw []byte) error {
		_, err := st.PersistMessageWithParticipantsContext(t.Context(), nil, func([]int64) *MessagePersistData {
			return &MessagePersistData{Message: &Message{SourceID: source.ID, SourceMessageID: "lock-contract", MessageType: "email"}, Conversation: &ConversationPersistData{SourceConversationID: "lock-contract", ConversationType: "email_thread"}, RawMIME: raw}
		})
		return err
	}
	requirements.NoError(persist([]byte("Subject: Ordinary email\r\n\r\nBody")))
	requirements.ErrorIs(persist(inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))), errAttributionLockMissing)
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE message_type='calendar_event'`).Scan(&count))
	assertions.Zero(count)
}

func TestEmailCalendarGuestRemovalLeavesMeeting(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	invite := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org\r\nATTENDEE:mailto:other@example.org"))
	_, source := persistInviteEmail(t, st, "invite", invite)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	before := calendarRows(t, st, ids...)
	removal := inviteEmail(calendarDocument("CANCEL", "SEQUENCE:1\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE:mailto:guest@example.org"))
	parent, _ := persistInviteEmail(t, st, "guest-removal", removal)
	assertions.Equal(before, calendarRows(t, st, ids...))
	assertions.Equal("confirmed", calendarRows(t, st, ids...)[0].Event.Status)
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Contains(metadata.String, "calendar_invite_errors")
	assertions.Contains(metadata.String, "guest removal ignored")
	var count int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE source_id=? AND message_type='email'`, source).Scan(&count))
	assertions.Equal(2, count)
	fullCancel, _ := persistInviteEmail(t, st, "full-cancel", inviteEmail(calendarDocument("CANCEL", "SEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com")))
	assertions.Equal("cancelled", calendarRows(t, st, ids...)[0].Event.Status)
	metadata, err = st.GetMessageMetadata(fullCancel)
	requirements.NoError(err)
	assertions.NotContains(metadata.String, "calendar_invite_errors")
}

func TestEmailCalendarReplyAndCancellationPreserveInviteRSVP(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE;PARTSTAT=ACCEPTED:mailto:guest@example.org")
	_, source := persistInviteEmail(t, st, "invite", inviteEmail(raw))
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	before := calendarRows(t, st, ids...)
	reply := calendarfixture.EmailFrom("guest@example.org", calendarDocument("REPLY", "ATTENDEE;PARTSTAT=DECLINED:mailto:guest@example.org"))
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

func TestEmailCalendarMIMEErrorsKeepDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, headers, content, diagnostic string
	}{
		{"parameters", "Content-Type: text/calendar; method", string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")), "invalid calendar Content-Type parameters"},
		{"decoding", "Content-Type: text/calendar\r\nContent-Transfer-Encoding: base64", "SGVsbG8gV29ybGQhA", "calendar MIME decoding failed"},
		{"method conflict", "Content-Type: text/calendar; method=REPLY", string(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")), "calendar METHOD conflicts with MIME method"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := []byte("From: organizer@example.com\r\n" + tc.headers + "\r\n\r\n" + tc.content)
			parent, source := persistInviteEmail(t, st, "invalid-mime", raw)
			assertions.Empty(emailEvents(t, st, source))
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			assertions.Contains(metadata.String, tc.diagnostic)
		})
	}
}

func TestEmailCalendarRepairAfterSourceMerge(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	parent, source := persistInviteEmail(t, st, "merged-invite", raw)
	dest, err := st.GetOrCreateSource("mbox", "destination@example.test")
	requirements.NoError(err)
	liveRaw := bytes.ReplaceAll(raw, []byte("family@example.com"), []byte("live@example.test"))
	_, err = st.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: dest.ID, SourceMessageID: "live-invite", MessageType: "email"},
		Conversation: &ConversationPersistData{SourceConversationID: "live-invite"},
		RawMIME:      liveRaw,
	})
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source, IntoSourceID: dest.ID})
	requirements.NoError(err)
	before := emailEvents(t, st, dest.ID)
	requirements.Len(before, 2)
	summary, err := st.RepairEmailCalendarEventsContext(t.Context(), dest.ID, nil)
	requirements.NoError(err)
	assertions.Equal(1, summary.Scanned)
	assertions.Zero(summary.Changed)
	assertions.Equal(before, emailEvents(t, st, dest.ID))
	requirements.NoError(st.UpsertMessageRawWithFormat(parent, raw, "mime"))
	assertions.Equal(before, emailEvents(t, st, dest.ID))
}

func TestEmailCalendarRejectsContradictoryEmbeddedZone(t *testing.T) {
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VTIMEZONE\r\nTZID:Eastern Standard Time\r\nBEGIN:STANDARD\r\nDTSTART:20261101T020000\r\nTZOFFSETFROM:-0400\r\nTZOFFSETTO:STANDARD_OFFSET\r\nEND:STANDARD\r\nBEGIN:DAYLIGHT\r\nDTSTART:20270314T020000\r\nTZOFFSETFROM:-0500\r\nTZOFFSETTO:DAYLIGHT_OFFSET\r\nEND:DAYLIGHT\r\nEND:VTIMEZONE\r\nBEGIN:VEVENT\r\nUID:zone@example.com\r\nDTSTART;TZID=Eastern Standard Time:20270102T090000\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	for _, tc := range []struct {
		observance, offset, diagnostic string
	}{
		{"STANDARD", "-0400", "VTIMEZONE STANDARD TZOFFSETTO -0400 contradicts America/New_York near DTSTART"},
		{"STANDARD", "-0500", ""},
		{"DAYLIGHT", "-0500", "VTIMEZONE"},
		{"DAYLIGHT", "-0400", ""},
	} {
		t.Run(tc.observance+tc.offset, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			input := strings.ReplaceAll(raw, tc.observance+"_OFFSET", tc.offset)
			input = strings.NewReplacer("STANDARD_OFFSET", "-0500", "DAYLIGHT_OFFSET", "-0400").Replace(input)
			if tc.observance == "DAYLIGHT" {
				input = strings.ReplaceAll(input, "20270102T090000", "20270702T090000")
			}
			parent, source := persistInviteEmail(t, st, "zone", inviteEmail([]byte(input)))
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			if tc.diagnostic != "" {
				assertions.Empty(emailEvents(t, st, source))
				assertions.Contains(metadata.String, "calendar_invite_errors")
				assertions.Contains(metadata.String, tc.diagnostic)
			} else {
				assertions.Len(emailEvents(t, st, source), 1)
				assertions.NotContains(metadata.String, "calendar_invite_errors")
			}
		})
	}
}

func TestEmailCalendarMIMEOnlyCancellationCreatesTombstone(t *testing.T) {
	for _, organizer := range []string{"ORGANIZER:mailto:organizer@example.com", ""} {
		t.Run(organizer, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			raw := []byte("From: organizer@example.com\r\nContent-Type: text/calendar; method=CANCEL\r\n\r\n" + strings.Replace(string(calendarDocument("", "SEQUENCE:2\r\n"+organizer)), "METHOD:\r\n", "", 1))
			parent, source := persistInviteEmail(t, st, "mime-cancel", raw)
			ids := emailEvents(t, st, source)
			metadata, err := st.GetMessageMetadata(parent)
			requirements.NoError(err)
			if organizer == "" {
				assertions.Empty(ids)
				assertions.Contains(metadata.String, "update ignored: ORGANIZER does not match invite_organizer")
			} else {
				requirements.Len(ids, 1)
				assertions.Equal("cancelled", calendarRows(t, st, ids...)[0].Event.Status)
				eventMetadata, err := st.GetMessageMetadata(ids[0])
				requirements.NoError(err)
				assertions.Contains(strings.ReplaceAll(eventMetadata.String, " ", ""), `"invite_organizer":"organizer@example.com"`)
				assertions.NotContains(metadata.String, "calendar_invite_errors")
			}
			retained, err := st.GetMessageRaw(parent)
			requirements.NoError(err)
			assertions.Equal(raw, retained)
		})
	}
}

func TestEmailCalendarReplayLeavesUnreadableCancellation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)

	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	requirements.NoError(err)
	cancelled, err := st.PersistICalendarContext(t.Context(), source.ID, calendarDocument("CANCEL", "SEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com"))
	requirements.NoError(err)
	ids := cancelled.MessageIDs
	requirements.Len(ids, 1)
	_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=NULL WHERE message_id=?`, []byte("broken calendar"), ids[0])
	requirements.NoError(err)

	active := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com"))
	replay, _ := persistInviteEmail(t, st, "active-replay-after-cancel", active)

	metadata, err := st.GetMessageMetadata(replay)
	requirements.NoError(err)
	assertions.Contains(metadata.String, "calendar_invite_errors")
	retained, err := st.GetMessageRaw(replay)
	requirements.NoError(err)
	assertions.Equal(active, retained)
	assertions.Equal("cancelled", calendarRows(t, st, ids...)[0].Event.Status)
}

func TestEmailCalendarNewerRestoreKeepsUnreadableCancellation(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)

	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	requirements.NoError(err)
	cancel, err := st.PersistICalendarContext(t.Context(), source.ID, calendarDocument("CANCEL", "STATUS:CANCELLED\r\nSEQUENCE:2\r\nORGANIZER:mailto:organizer@example.com"))
	requirements.NoError(err)
	ids := cancel.MessageIDs
	requirements.Len(ids, 1)
	_, err = st.db.Exec(`UPDATE message_raw SET raw_data=?,compression=NULL WHERE message_id=?`, []byte("broken calendar"), ids[0])
	requirements.NoError(err)

	restore := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:3\r\nORGANIZER:mailto:organizer@example.com\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Restored meeting"))
	parent, _ := persistInviteEmail(t, st, "newer-restore-after-unreadable-cancel", restore)
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Contains(metadata.String, "calendar_invite_errors")
	assertions.Contains(metadata.String, "decode iCalendar: ical: malformed content line")
	retained, err := st.GetMessageRaw(parent)
	requirements.NoError(err)
	assertions.Equal(restore, retained)
	assertions.Equal("cancelled", calendarRows(t, st, ids...)[0].Event.Status)
}

func TestEmailCalendarOrdinaryWriteSkipsDiagnosticsRead(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	parent, source := persistInviteEmail(t, st, "ordinary", []byte("Subject: plain\r\n\r\nbody"))
	var metadataReads int
	err := st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		rebind := tx.rebind
		tx.rebind = func(query string) string {
			if strings.Contains(query, "SELECT metadata FROM messages") {
				metadataReads++
			}
			return rebind(query)
		}
		_, _, err := st.refreshEmailCalendarEventsWith(t.Context(), tx, parent, nil)
		return err
	})
	requirements.NoError(err)
	assertions.Zero(metadataReads)
	_, err = st.db.Exec(`UPDATE messages SET metadata=? WHERE id=?`, `{"number":9007199254740993,"calendar_invite_errors":["old error"]}`, parent)
	requirements.NoError(err)
	err = st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source}}, func(tx *loggedTx) error {
		_, _, err := st.refreshEmailCalendarEventsWith(t.Context(), tx, parent, nil)
		return err
	})
	requirements.NoError(err)
	metadata, err := st.GetMessageMetadata(parent)
	requirements.NoError(err)
	assertions.Equal(`{"number":9007199254740993}`, strings.ReplaceAll(metadata.String, " ", ""))
}
