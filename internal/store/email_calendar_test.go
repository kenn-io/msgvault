package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/search"
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

func TestEmailCalendarEncodedFilename(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendar := calendarDocument("REQUEST", "DTSTART:20270302T120000Z")
	raw := "From: organizer@example.com\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment;\r\n filename*=UTF-8''invite%2Eics\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(calendar)
	_, source := persistInviteEmail(t, st, "encoded-invite", []byte(raw))
	require.Len(t, emailEvents(t, st, source), 1)
}

func TestEmailCalendarEmptyRawFormatDoesNotLockDirectory(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL participant directory locks")
	}
	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	requirements.NoError(err)
	id, err := st.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: source.ID, SourceMessageID: "raw", MessageType: "email"},
		Conversation: &ConversationPersistData{SourceConversationID: "raw"},
	})
	requirements.NoError(err)
	blocker, err := st.db.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	requirements.NoError(st.lockParticipantDirectoryMutationTxContext(t.Context(), blocker))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	requirements.NoError(st.UpsertMessageRawWithFormatContext(ctx, id, []byte("Content-Type: text/calendar\r\n\r\n"), ""))
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

func TestEmailCalendarSourceCountsExcludeDerivedEvents(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	email, source := persistInviteEmail(t, st, "count-invite", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
	requirements.Len(emailEvents(t, st, source), 1)
	countAll, err := st.CountAllMessagesForSourceContext(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(int64(2), countAll)
	_, err = st.db.Exec(`UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?`, email)
	requirements.NoError(err)
	countAll, err = st.CountAllMessagesForSourceContext(t.Context(), source)
	requirements.NoError(err)
	assertions.Equal(int64(2), countAll)
	_, err = st.db.Exec(`UPDATE messages SET deleted_from_source_at=NULL WHERE id=?`, email)
	requirements.NoError(err)
	gcalData, _ := calendarFixture(t, st, "count-gcal", calendarRaw)
	_, err = st.PersistMessageContext(t.Context(), gcalData)
	requirements.NoError(err)
	icsSourceID := icsSource(t, st)
	_, err = st.PersistICalendarContext(t.Context(), icsSourceID, calendarDocument("REQUEST", "DTSTART:20270302T120000Z"))
	requirements.NoError(err)
	for _, sourceID := range []int64{source, gcalData.Message.SourceID, icsSourceID} {
		live, err := st.CountNativeMessagesForSource(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), live)
		raw, err := st.CountMessagesWithRaw(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), raw)
	}
	imported, err := st.GetOrCreateSource("message_import", "count-import")
	requirements.NoError(err)
	_, err = st.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: imported.ID, SourceMessageID: "imported-event", MessageType: "calendar_event"},
		Conversation: &ConversationPersistData{SourceConversationID: "imported-event"},
	})
	requirements.NoError(err)
	count, err := st.CountNativeMessagesForSource(imported.ID)
	requirements.NoError(err)
	assertions.Equal(int64(1), count)
	destination, err := st.GetOrCreateSource("message_import", "merged-count-import")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: imported.ID, IntoSourceID: destination.ID})
	requirements.NoError(err)
	count, err = st.CountNativeMessagesForSource(destination.ID)
	requirements.NoError(err)
	assertions.Equal(int64(1), count)
}

func TestEmailCalendarActivityCandidates(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z")
	_, mailSource := persistInviteEmail(t, st, "activity-invite", inviteEmail(raw))
	mailEvents := emailEvents(t, st, mailSource)
	requirements.Len(mailEvents, 1)
	gmail, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	persistInviteEmailForSource(t, st, gmail.ID, "activity-invite", inviteEmail(raw))
	gmailEvents := emailEvents(t, st, gmail.ID)
	requirements.Len(gmailEvents, 1)
	icsSource := icsSource(t, st)
	_, err = st.PersistICalendarContext(t.Context(), icsSource, raw)
	requirements.NoError(err)
	icsEvents := emailEvents(t, st, icsSource)
	requirements.Len(icsEvents, 1)
	gcalData, gcalID := calendarFixture(t, st, "activity-gcal", calendarRaw)
	gcalData.Message.SentAt = sql.NullTime{Time: time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC), Valid: true}
	_, err = st.PersistMessageContext(t.Context(), gcalData)
	requirements.NoError(err)
	imported, err := st.GetOrCreateSource("message_import", "activity-import")
	requirements.NoError(err)
	importedID, err := st.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: imported.ID, SourceMessageID: "imported-event", MessageType: "calendar_event", SentAt: sql.NullTime{Time: time.Date(2027, 3, 2, 12, 0, 0, 0, time.UTC), Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: "imported-event"},
	})
	requirements.NoError(err)
	for _, source := range []struct {
		id  int64
		typ string
	}{{icsSource, "ics"}, {gcalData.Message.SourceID, "gcal"}, {imported.ID, "message_import"}} {
		destination, err := st.GetOrCreateSource(source.typ, "merged-"+source.typ)
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.id, IntoSourceID: destination.ID})
		requirements.NoError(err)
	}
	for _, id := range []int64{icsEvents[0], gcalID, importedID} {
		var marked bool
		requirements.NoError(st.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM source_merge_archive_only_messages WHERE message_id=?)`, id).Scan(&marked))
		requirements.True(marked)
	}

	candidates, err := st.LoadQueuedActivityCandidatesContext(t.Context(), 100)
	requirements.NoError(err)
	eligible := make(map[int64]bool)
	for _, candidate := range candidates {
		eligible[candidate.MessageID] = candidate.Eligible
	}
	requirements.Contains(eligible, mailEvents[0])
	assertions.False(eligible[mailEvents[0]])
	requirements.Contains(eligible, gmailEvents[0])
	assertions.False(eligible[gmailEvents[0]])
	assertions.True(eligible[icsEvents[0]])
	assertions.True(eligible[gcalID])
	assertions.True(eligible[importedID])
}

func TestEmailCalendarEventsUseConfirmedMailAccountScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		confirmed bool
		// confirmLater confirms the mailbox after the invitation is archived.
		confirmLater bool
		want         string
	}{
		{name: "confirmed Gmail", confirmed: true, want: "owner@example.com"},
		{name: "unconfirmed Gmail"},
		{name: "Gmail confirmed after archival", confirmLater: true, want: "owner@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			source, err := st.GetOrCreateSource("gmail", "owner@example.com")
			requirements.NoError(err)
			if tc.confirmed {
				requirements.NoError(st.AddAccountIdentityContext(t.Context(), source.ID, "owner@example.com", "manual"))
			}
			persistInviteEmailForSource(t, st, source.ID, "calendar-account-scope", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")))
			if tc.confirmLater {
				requirements.NoError(st.AddAccountIdentityContext(t.Context(), source.ID, "owner@example.com", "manual"))
			}
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
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	request := inviteEmail([]byte(strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE:mailto:guest@example.org\r\n")))
	parent, source := persistInviteEmail(t, st, "organizer-message", request)
	email, err := st.GetMessage(parent)
	requirements.NoError(err)
	assertions.Equal("Original email", email.Subject)
	assertions.Equal("Email body stays intact", email.BodyText)
	var emails int
	requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE source_id=? AND message_type='email'`, source).Scan(&emails))
	assertions.Equal(1, emails)
	originalIDs := emailEvents(t, st, source)
	requirements.Len(originalIDs, 2)
	assertions.Len(calendarRows(t, st, originalIDs...)[0].Event.Attendees, 2)
	matches, _, err := st.SearchMessagesQuery(search.Parse("from:organizer@example.com"), 0, 10)
	requirements.NoError(err)
	var found bool
	for _, match := range matches {
		if match.ID == originalIDs[0] {
			found = true
		}
	}
	assertions.True(found)
	_, err = st.PersistMessageContext(t.Context(), &MessagePersistData{
		Message:      &Message{SourceID: source, SourceMessageID: "organizer-message", MessageType: "email", Subject: sql.NullString{String: "Original email", Valid: true}},
		Conversation: &ConversationPersistData{SourceConversationID: "organizer-message", Title: "Original email"},
		RawMIME:      request,
	})
	requirements.NoError(err)
	assertions.Equal(originalIDs, emailEvents(t, st, source), "rebuilding the same email keeps its claim identity")
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
	t.Run("invalid calendar", func(t *testing.T) {
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
	})
	t.Run("method conflict", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, source := persistInviteEmail(t, st, "method-request", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z")))
		cancel := calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED")
		mail := []byte(strings.Replace(string(inviteEmail(cancel)), "text/calendar", "text/calendar; method=REPLY", 1))
		id := persistInviteEmailForSource(t, st, source, "method-conflict", mail)
		stored, err := st.GetMessageRaw(id)
		requirements.NoError(err)
		assertions.Equal(mail, stored)
		from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		requirements.NoError(err)
		requirements.Len(page.Occurrences, 1)
		assertions.Equal("confirmed", page.Occurrences[0].Status)
	})
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
	t.Run("masterless exception retains conversation", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		unrelated, err := st.GetOrCreateSource("mbox", "unrelated@example.com")
		requirements.NoError(err)
		persistInviteEmailForSource(t, st, unrelated.ID, "other-source", inviteEmail(calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T130000Z\r\nORGANIZER:mailto:other@example.com")))
		other, err := st.GetOrCreateSource("mbox", "other-destination@example.com")
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: unrelated.ID, IntoSourceID: other.ID})
		requirements.NoError(err)
		_, from := persistInviteEmail(t, st, "exception", inviteEmail(calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T130000Z\r\nSUMMARY:Exception\r\nORGANIZER:mailto:organizer@example.com")))
		into, err := st.GetOrCreateSource("mbox", "destination@example.com")
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
		requirements.NoError(err)
		ids := emailEvents(t, st, into.ID)
		requirements.Len(ids, 1)
		var conversationID int64
		requirements.NoError(st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, ids[0]).Scan(&conversationID))
		persistInviteEmailForSource(t, st, into.ID, "update", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nRECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T140000Z\r\nSUMMARY:Moved\r\nORGANIZER:mailto:organizer@example.com")))
		assertions.Equal(ids, emailEvents(t, st, into.ID))
		var gotConversation int64
		requirements.NoError(st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, ids[0]).Scan(&gotConversation))
		assertions.Equal(conversationID, gotConversation)
		persistInviteEmailForSource(t, st, into.ID, "foreign-master", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:other@example.com")))
		assertions.Equal(ids, emailEvents(t, st, into.ID))
		persistInviteEmailForSource(t, st, into.ID, "sibling", inviteEmail(calendarDocument("REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270304T130000Z\r\nORGANIZER:mailto:organizer@example.com")))
		persistInviteEmailForSource(t, st, into.ID, "master", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com")))
		family := emailEvents(t, st, into.ID)
		requirements.Len(family, 3)
		for _, id := range family {
			requirements.NoError(st.db.QueryRow(`SELECT conversation_id FROM messages WHERE id=?`, id).Scan(&gotConversation))
			assertions.Equal(conversationID, gotConversation)
		}
		var count int
		requirements.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE source_id=? AND conversation_type='calendar'`, into.ID).Scan(&count))
		assertions.Equal(1, count)
	})
}

func TestEmailCalendarSubsetAfterSourceMerge(t *testing.T) {
	t.Setenv("MSGVAULT_TEST_DB", "")
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	oldID, from := persistInviteEmail(t, st, "original", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original")))
	persistInviteEmailForSource(t, st, from, "revision", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Accepted")))
	into, err := st.GetOrCreateSource("mbox", "destination@example.com")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
	requirements.NoError(err)
	ids := emailEvents(t, st, into.ID)
	requirements.Len(ids, 1)
	_, err = st.db.Exec(`UPDATE messages SET sent_at=CASE WHEN id IN (?, ?) THEN '2030-01-01T00:00:00Z' ELSE '2020-01-01T00:00:00Z' END`, oldID, ids[0])
	requirements.NoError(err)
	destination := filepath.Join(t.TempDir(), "subset")
	_, err = CopySubset(t.Context(), st.dbPath, destination, 2, false)
	requirements.NoError(err)
	subset, err := Open(filepath.Join(destination, "msgvault.db"))
	requirements.NoError(err)
	t.Cleanup(func() { _ = subset.Close() })
	item, err := subset.ScanArchivedEmailContext(t.Context(), into.ID, oldID-1, oldID)
	requirements.NoError(err)
	requirements.NotNil(item)
	parsed, err := mime.Parse(item.RawData)
	requirements.NoError(err)
	_, err = subset.RefreshArchivedEmailCalendarContext(t.Context(), into.ID, item, parsed)
	requirements.NoError(err)
	assertions.Equal(ids, emailEvents(t, subset, into.ID))
	var subject, start string
	requirements.NoError(subset.db.QueryRow(`SELECT subject, metadata ->> 'start' FROM messages WHERE id=?`, ids[0]).Scan(&subject, &start))
	assertions.Equal("Accepted", subject)
	assertions.Equal("2027-03-02T13:00:00Z", start)
}

func TestEmailCalendarFamilyOrganizer(t *testing.T) {
	t.Run("cancellation establishes organizer", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		original := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original")
		_, source := persistInviteEmail(t, st, "original", inviteEmail(original))
		cancel := calendarDocument("CANCEL", "SEQUENCE:1\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:first@example.com")
		persistInviteEmailForSource(t, st, source, "cancel", inviteEmail(cancel))
		ids := emailEvents(t, st, source)
		requirements.Len(ids, 1)
		foreign := calendarDocument("REQUEST", "SEQUENCE:2\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Foreign\r\nORGANIZER:mailto:other@example.com")
		persistInviteEmailForSource(t, st, source, "foreign", inviteEmail(foreign))
		row := calendarRows(t, st, ids[0])[0]
		assertions.Equal("Original", row.Subject)
		assertions.Equal("cancelled", row.Event.Status)
		stored, err := st.GetMessageRaw(ids[0])
		requirements.NoError(err)
		assertions.Equal(original, stored)
	})
	t.Run("blank original adopts family organizer", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		_, source := persistInviteEmail(t, st, "original", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original")))
		persistInviteEmailForSource(t, st, source, "first", inviteEmail(calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T120000Z\r\nORGANIZER:mailto:first@example.com")))
		ids := emailEvents(t, st, source)
		requirements.Len(ids, 2)
		persistInviteEmailForSource(t, st, source, "second", inviteEmail(calendarDocument("REQUEST", "RECURRENCE-ID:20270304T120000Z\r\nDTSTART:20270304T120000Z\r\nORGANIZER:mailto:second@example.com")))
		persistInviteEmailForSource(t, st, source, "foreign-original", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Foreign\r\nORGANIZER:mailto:second@example.com")))
		assertions.Equal(ids, emailEvents(t, st, source))
		assertions.Equal("Original", calendarRows(t, st, ids[0])[0].Subject)
		assertions.Contains(logs.String(), "organizer differs")
	})

	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprintf("merged=%t", merged), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			master := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com")
			_, source := persistInviteEmail(t, st, "master", inviteEmail(master))
			if merged {
				into, err := st.GetOrCreateSource("mbox", "destination@example.com")
				requirements.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source, IntoSourceID: into.ID})
				requirements.NoError(err)
				source = into.ID
			}
			ids := emailEvents(t, st, source)
			requirements.Len(ids, 1)
			update := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270303T120000Z\r\nSUMMARY:Moved\r\nORGANIZER:mailto:other@example.com")
			persistInviteEmailForSource(t, st, source, "foreign-original", inviteEmail(update))
			persistInviteEmailForSource(t, st, source, "foreign-original-cancel", inviteEmail(calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:other@example.com")))
			assertions.Equal(ids, emailEvents(t, st, source))
			row := calendarRows(t, st, ids[0])[0]
			assertions.Equal("Original", row.Subject)
			requirements.NotNil(row.Event.Start)
			assertions.Equal("2027-03-02T12:00:00Z", row.Event.Start.Format(time.RFC3339))
			assertions.Equal("confirmed", row.Event.Status)
			stored, err := st.GetMessageRaw(ids[0])
			requirements.NoError(err)
			assertions.Equal(master, stored)
			assertions.Contains(logs.String(), "level=WARN")
			cancel := calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nORGANIZER:mailto:other@example.com")
			persistInviteEmailForSource(t, st, source, "foreign-cancel", inviteEmail(cancel))
			move := calendarDocument("REQUEST", "RECURRENCE-ID:20270303T120000Z\r\nDTSTART:20270303T150000Z\r\nORGANIZER:mailto:other@example.com")
			persistInviteEmailForSource(t, st, source, "foreign-move", inviteEmail(move))
			assertions.Len(emailEvents(t, st, source), 1)
			from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
			page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 2)
			assertions.Equal("confirmed", page.Occurrences[1].Status)
			cancel = calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nORGANIZER:mailto:organizer@example.com")
			persistInviteEmailForSource(t, st, source, "own-cancel", inviteEmail(cancel))
			page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 3))
			requirements.NoError(err)
			requirements.Len(page.Occurrences, 2)
			assertions.Equal("cancelled", page.Occurrences[1].Status)
		})
	}

	t.Run("exception first", func(t *testing.T) {
		assertions := assert.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		exception := calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nORGANIZER:mailto:organizer@example.com")
		_, source := persistInviteEmail(t, st, "exception-first", inviteEmail(exception))
		master := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:other@example.com")
		persistInviteEmailForSource(t, st, source, "foreign-master", inviteEmail(master))
		assertions.Len(emailEvents(t, st, source), 1)
		master = calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com")
		persistInviteEmailForSource(t, st, source, "own-master", inviteEmail(master))
		assertions.Len(emailEvents(t, st, source), 2)
	})
}

func TestEmailCalendarDuplicateParts(t *testing.T) {
	assertions := assert.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	calendar := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Invite")
	raw := "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/calendar\r\n\r\n" + string(calendar) + "\r\n--x\r\nContent-Type: application/ics\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(calendar) + "\r\n--x--\r\n"
	_, source := persistInviteEmail(t, st, "duplicate-parts", []byte(raw))
	assertions.Len(emailEvents(t, st, source), 1)
}
