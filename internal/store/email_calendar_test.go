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

func TestEmailCalendarSingleParticipantLockOrder(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL participant directory locks")
	}
	source, err := st.GetOrCreateSource("imap", "imap://owner@example.com:143")
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
	receipt := IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	done := make(chan error, 1)
	go func() {
		draft, persistErr := st.PersistIMAPDraftContext(ctx, receipt, []ParticipantPersistData{
			{EmailAddress: "organizer@example.com", DisplayName: "Organizer", Domain: "example.com"},
		}, func(ids []int64) *MessagePersistData {
			return &MessagePersistData{
				Message:      &Message{SourceID: source.ID, SourceMessageID: IMAPDraftSourceMessageID(receipt), MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}},
				Conversation: &ConversationPersistData{SourceConversationID: "single-invite"},
				RawMIME:      inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")),
			}
		})
		if persistErr == nil {
			id = draft.CurrentMessageID
		}
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
	// The revision row stays available while the writer waits on the directory.
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
	for _, sourceID := range []int64{source, gcalData.Message.SourceID, icsSourceID} {
		live, err := st.CountNativeMessagesForSource(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), live)
		raw, err := st.CountMessagesWithRaw(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), raw)
	}
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
				requirements.NoError(st.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
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
	for _, id := range originalIDs {
		assertions.NotEqual(parent, id)
	}
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
	t.Run("different organizer is ignored", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
		t.Cleanup(func() { slog.SetDefault(previous) })
		original := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com")
		_, from := persistInviteEmail(t, st, "original", inviteEmail(original))
		into, err := st.GetOrCreateSource("mbox", "destination@example.com")
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: from, IntoSourceID: into.ID})
		requirements.NoError(err)
		ids := emailEvents(t, st, into.ID)
		requirements.Len(ids, 1)
		update := calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270303T120000Z\r\nSUMMARY:Moved\r\nORGANIZER:mailto:replacement@example.com")
		persistInviteEmailForSource(t, st, into.ID, "update", inviteEmail(update))
		assertions.Equal(ids, emailEvents(t, st, into.ID))
		row := calendarRows(t, st, ids...)[0]
		assertions.Equal("Original", row.Subject)
		requirements.NotNil(row.Event.Start)
		assertions.Equal("2027-03-02T12:00:00Z", row.Event.Start.Format(time.RFC3339))
		stored, err := st.GetMessageRaw(ids[0])
		requirements.NoError(err)
		assertions.Equal(original, stored)
		assertions.Contains(logs.String(), "level=WARN")
		persistInviteEmailForSource(t, st, into.ID, "cancel", inviteEmail(calendarDocument("CANCEL", "SEQUENCE:2\r\nSTATUS:CANCELLED\r\nORGANIZER:mailto:replacement@example.com")))
		fromDate := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), fromDate, fromDate.AddDate(0, 0, 2))
		requirements.NoError(err)
		requirements.Len(page.Occurrences, 1)
		assertions.Equal("Original", page.Occurrences[0].Subject)
		assertions.Equal("confirmed", page.Occurrences[0].Status)
	})

	for _, merged := range []bool{false, true} {
		t.Run(fmt.Sprintf("merged=%t", merged), func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			master := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nORGANIZER:mailto:organizer@example.com")
			_, source := persistInviteEmail(t, st, "master", inviteEmail(master))
			if merged {
				into, err := st.GetOrCreateSource("mbox", "destination@example.com")
				requirements.NoError(err)
				_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source, IntoSourceID: into.ID})
				requirements.NoError(err)
				source = into.ID
			}
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

func TestEmailCalendarDuplicatePartsSummary(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	source, err := st.GetOrCreateSource("mbox", "owner@example.com")
	requirements.NoError(err)
	calendar := calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Invite")
	raw := "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/calendar\r\n\r\n" + string(calendar) + "\r\n--x\r\nContent-Type: application/ics\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(calendar) + "\r\n--x--\r\n"
	parsed, err := mime.Parse([]byte(raw))
	requirements.NoError(err)
	var summary ICalendarImportSummary
	err = st.withAttributionTxContext(t.Context(), attributionLock{Sources: []int64{source.ID}}, func(tx *loggedTx) error {
		var persistErr error
		summary, persistErr = st.persistEmailCalendarPartsWith(t.Context(), tx, 0, source.ID, parsed)
		return persistErr
	})
	requirements.NoError(err)
	assertions.Equal(1, summary.Changed)
	assertions.Equal(1, summary.Unchanged)
	assertions.Len(emailEvents(t, st, source.ID), 1)
}
