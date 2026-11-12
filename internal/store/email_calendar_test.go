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

func TestEmailCalendarOctetStreamAttachment(t *testing.T) {
	st := newRFC822IDBackfillBackendStore(t)
	calendar := calendarDocument("REQUEST", "DTSTART:20270302T120000Z")
	raw := "From: organizer@example.com\r\nMIME-Version: 1.0\r\nContent-Type: application/octet-stream; name=invite.ics\r\nContent-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(calendar)
	_, source := persistInviteEmail(t, st, "encoded-invite", []byte(raw))
	require.Len(t, emailEvents(t, st, source), 1)
}

func TestPlainEmailSingleParticipantDoesNotWaitForDirectory(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL participant directory locks")
	}
	source, err := st.GetOrCreateSource("imap", "imap://owner@example.com:143")
	requirements.NoError(err)
	blocker, err := st.db.BeginTx(t.Context(), nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	requirements.NoError(st.lockParticipantDirectoryMutationTxContext(t.Context(), blocker))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	receipt := IMAPDraftReceipt{SourceID: source.ID, Mailbox: "Drafts", UIDValidity: 1, UID: 1}
	_, err = st.PersistIMAPDraftContext(ctx, receipt, []ParticipantPersistData{
		{EmailAddress: "sender@example.com", Domain: "example.com"},
	}, func(ids []int64) *MessagePersistData {
		return &MessagePersistData{
			Message:      &Message{SourceID: source.ID, SourceMessageID: IMAPDraftSourceMessageID(receipt), MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "plain-email"},
			RawMIME:      []byte("From: sender@example.com\r\nSubject: Hello\r\n\r\nHello"),
		}
	})
	requirements.NoError(err)
}

func TestEmailCalendarSingleParticipantLockOrder(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if !st.IsPostgreSQL() {
		t.Skip("requires PostgreSQL participant directory locks")
	}
	source, err := st.GetOrCreateSource("imap", "imap://owner@example.com:143")
	requirements.NoError(err)
	_, err = st.db.Exec(`INSERT INTO archive_metadata (key, value) VALUES ('participant_display_name_revision', '0') ON CONFLICT (key) DO NOTHING`)
	requirements.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	blocker, err := st.db.BeginTx(ctx, nil)
	requirements.NoError(err)
	defer func() { _ = blocker.Rollback() }()
	requirements.NoError(st.lockParticipantDirectoryMutationTxContext(ctx, blocker))
	done := make(chan error, 1)
	go func() {
		_, persistErr := st.persistMessageWithParticipantsTransaction(ctx, attributionLock{Sources: []int64{source.ID}}, nil, []ParticipantPersistData{
			{EmailAddress: "organizer@example.com", DisplayName: "Organizer", Domain: "example.com"},
		}, func(ids []int64) *MessagePersistData {
			return &MessagePersistData{
				Message:      &Message{SourceID: source.ID, SourceMessageID: "single-invite", MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}},
				Conversation: &ConversationPersistData{SourceConversationID: "single-invite"},
				RawMIME:      inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z")),
			}
		}, nil, nil)
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
	var count int
	requirements.NoError(st.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE source_id=? AND source_message_id='single-invite'`, source.ID).Scan(&count))
	requirements.Equal(1, count, "email must commit before waiting on the directory")
	requirements.NoError(blocker.Rollback())
	err = <-done
	joined = true
	requirements.NoError(err)
	requirements.Len(emailEvents(t, st, source.ID), 1)
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

func TestEmailCalendarSourceCountsAndActivityCandidates(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	raw := calendarDocument("REQUEST", "DTSTART:20270302T120000Z")
	_, mailSource := persistInviteEmail(t, st, "activity-invite", inviteEmail(raw))
	mailEvents := emailEvents(t, st, mailSource)
	requirements.Len(mailEvents, 1)
	icsSource := icsSource(t, st)
	_, err := importICalendarForTest(t, st, icsSource, raw)
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
	for _, sourceID := range []int64{mailSource, gcalData.Message.SourceID, icsSource} {
		count, err := st.CountNativeMessagesForSource(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), count)
		rawCount, err := st.CountMessagesWithRaw(sourceID)
		requirements.NoError(err)
		assertions.Equal(int64(1), rawCount)
	}
	count, err := st.CountNativeMessagesForSource(imported.ID)
	requirements.NoError(err)
	assertions.Equal(int64(1), count)
	for _, source := range []struct {
		id  int64
		typ string
	}{{icsSource, "ics"}, {gcalData.Message.SourceID, "gcal"}, {imported.ID, "message_import"}} {
		destination, err := st.GetOrCreateSource(source.typ, "merged-"+source.typ)
		requirements.NoError(err)
		_, err = st.MergeSourcesContext(t.Context(), MergeSourcesRequest{FromSourceID: source.id, IntoSourceID: destination.ID})
		requirements.NoError(err)
		if source.typ == "message_import" {
			count, err = st.CountNativeMessagesForSource(destination.ID)
			requirements.NoError(err)
			assertions.Equal(int64(1), count)
		}
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
	setup := func(t *testing.T) (*Store, int64, int64, []int64, []byte) {
		t.Helper()
		st := newRFC822IDBackfillBackendStore(t)
		request := inviteEmail([]byte(strings.ReplaceAll(string(calendarFamilyDocument()), "UID:family@example.com\r\n", "UID:family@example.com\r\nORGANIZER:mailto:organizer@example.com\r\nATTENDEE:mailto:guest@example.org\r\n")))
		parent, source := persistInviteEmail(t, st, "organizer-message", request)
		ids := emailEvents(t, st, source)
		require.Len(t, ids, 2)
		return st, parent, source, ids, request
	}
	t.Run("email retained", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st, parent, source, _, _ := setup(t)
		email, err := st.GetMessage(parent)
		requirements.NoError(err)
		assertions.Equal("Original email", email.Subject)
		assertions.Equal("Email body stays intact", email.BodyText)
		var emails int
		requirements.NoError(st.db.QueryRow(`SELECT count(*) FROM messages WHERE source_id=? AND message_type='email'`, source).Scan(&emails))
		assertions.Equal(1, emails)
	})
	t.Run("event participants and search", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st, _, _, ids, _ := setup(t)
		assertions.Len(calendarRows(t, st, ids...)[0].Event.Attendees, 2)
		matches, _, err := st.SearchMessagesQuery(search.Parse("from:organizer@example.com"), 0, 10)
		requirements.NoError(err)
		var found bool
		for _, match := range matches {
			if match.ID == ids[0] {
				found = true
			}
		}
		assertions.True(found)
	})
	t.Run("rebuild preserves events", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st, _, source, ids, request := setup(t)
		_, err := st.PersistMessageContext(t.Context(), &MessagePersistData{
			Message:      &Message{SourceID: source, SourceMessageID: "organizer-message", MessageType: "email", Subject: sql.NullString{String: "Original email", Valid: true}},
			Conversation: &ConversationPersistData{SourceConversationID: "organizer-message", Title: "Original email"},
			RawMIME:      request,
		})
		requirements.NoError(err)
		assertions.Equal(ids, emailEvents(t, st, source), "rebuilding the same email keeps its claim identity")
	})
	t.Run("garbage collection", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		st, _, source, _, _ := setup(t)
		if st.IsPostgreSQL() {
			t.Skip("garbage collection requires SQLite")
		}
		requirements.NoError(st.MarkMessageDeletedContext(t.Context(), source, "organizer-message"))
		plan, err := st.PlanGCContext(t.Context())
		requirements.NoError(err)
		deleted, err := st.ExecuteGCContext(t.Context(), plan)
		requirements.NoError(err)
		assertions.Equal(int64(1), deleted)
		from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 2))
		requirements.NoError(err)
		assertions.Len(page.Occurrences, 2)
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
	for _, tc := range []struct {
		name string
		part []byte
	}{
		{"unsupported method", calendarDocument("ADD", "DTSTART:20270302T120000Z")},
		{"invalid calendar", []byte("invalid calendar")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions, requirements := assert.New(t), require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })
			raw := inviteEmail(tc.part)
			id, source := persistInviteEmail(t, st, "invalid-part", raw)
			assertions.Empty(emailEvents(t, st, source))
			stored, err := st.GetMessageRaw(id)
			requirements.NoError(err)
			assertions.Equal(raw, stored)
			assertions.Contains(logs.String(), "calendar invite ignored")
			assertions.Contains(logs.String(), "error=")
			assertions.NotContains(logs.String(), `error=""`)
			assertions.Contains(logs.String(), "level=WARN")
			assertions.Contains(logs.String(), fmt.Sprintf("message_id=%d", id))
		})
	}
}

func TestEmailCalendarConflictingMethodPreservesEvent(t *testing.T) {
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	initial := inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Meeting"))
	first, source := persistInviteEmail(t, st, "request", initial)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	conflict := []byte(strings.Replace(string(inviteEmail(calendarDocument("CANCEL", "SEQUENCE:2"))), "text/calendar", "text/calendar; method=REPLY", 1))
	second := persistInviteEmailForSource(t, st, source, "reply", conflict)
	ids := emailEvents(t, st, source)
	requirements.Len(ids, 1)
	requirements.Equal("confirmed", calendarRows(t, st, ids[0])[0].Event.Status)
	for id, raw := range map[int64][]byte{first: initial, second: conflict} {
		stored, err := st.GetMessageRaw(id)
		requirements.NoError(err)
		requirements.Equal(raw, stored)
	}
	requirements.Contains(logs.String(), "calendar invite ignored")
	requirements.Contains(logs.String(), fmt.Sprintf("message_id=%d", second))
}

func TestEmailCalendarRevisionAfterSourceMerge(t *testing.T) {
	t.Run("master and exception retain renamed conversation", func(t *testing.T) {
		assertions, requirements := assert.New(t), require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		family := strings.Replace(string(calendarFamilyDocument()), "SUMMARY:Original series", "ORGANIZER:mailto:organizer@example.com\r\nSUMMARY:Original series", 1)
		_, from := persistInviteEmail(t, st, "family", inviteEmail([]byte(family)))
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
		var subject string
		requirements.NoError(st.db.QueryRow(`SELECT subject FROM messages WHERE id=?`, ids[0]).Scan(&subject))
		assertions.Equal("Updated series", subject)
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
		persistInviteEmailForSource(t, st, into.ID, "foreign-master", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nORGANIZER:mailto:organizer@example.com")))
		assertions.Len(emailEvents(t, st, into.ID), 2)
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
	oldID, from := persistInviteEmail(t, st, "original", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com")))
	persistInviteEmailForSource(t, st, from, "revision", inviteEmail(calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T130000Z\r\nSUMMARY:Accepted\r\nORGANIZER:mailto:organizer@example.com")))
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

func TestEmailCalendarChangedOrganizerUpdatesEvent(t *testing.T) {
	r := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	_, source := persistInviteEmail(t, st, "original", emailFrom("first@example.com", calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:first@example.com")))
	persistInviteEmailForSource(t, st, source, "update", emailFrom("FIRST@example.com", calendarDocument("REQUEST", "SEQUENCE:1\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Updated\r\nORGANIZER:mailto:other@example.com")))
	ids := emailEvents(t, st, source)
	r.Len(ids, 1)
	r.Equal("Updated", calendarRows(t, st, ids[0])[0].Subject)
	persistInviteEmailForSource(t, st, source, "cancel", emailFrom("other@example.com", calendarDocument("CANCEL", "SEQUENCE:2\r\nORGANIZER:mailto:first@example.com")))
	r.Equal("cancelled", calendarRows(t, st, ids[0])[0].Event.Status)
}

func TestEmailCalendarRejectsOtherSenders(t *testing.T) {
	for _, method := range []string{"REQUEST", "CANCEL"} {
		t.Run(method, func(t *testing.T) {
			r := require.New(t)
			st := newRFC822IDBackfillBackendStore(t)
			_, source := persistInviteEmail(t, st, "original", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nSUMMARY:Original\r\nORGANIZER:mailto:organizer@example.com")))
			id := persistInviteEmailForSource(t, st, source, "spoof", emailFrom("attendee@example.org", calendarDocument(method, "SEQUENCE:9\r\nDTSTART:20270302T140000Z\r\nSUMMARY:Changed\r\nORGANIZER:mailto:organizer@example.com")))
			ids := emailEvents(t, st, source)
			r.Len(ids, 1)
			row := calendarRows(t, st, ids[0])[0]
			r.Equal("Original", row.Subject)
			r.Equal("confirmed", row.Event.Status)
			r.Equal(12, row.Event.Start.Hour())
			_, err := st.GetMessage(id)
			r.NoError(err)
		})
	}
	t.Run("unedited occurrence", func(t *testing.T) {
		r := require.New(t)
		st := newRFC822IDBackfillBackendStore(t)
		_, source := persistInviteEmail(t, st, "series", inviteEmail(calendarDocument("REQUEST", "DTSTART:20270302T120000Z\r\nDURATION:PT1H\r\nRRULE:FREQ=DAILY;COUNT=2\r\nORGANIZER:mailto:organizer@example.com")))
		persistInviteEmailForSource(t, st, source, "cancel", emailFrom("attendee@example.org", calendarDocument("CANCEL", "RECURRENCE-ID:20270303T120000Z\r\nSEQUENCE:9")))
		r.Len(emailEvents(t, st, source), 1)
		from := time.Date(2027, 3, 3, 0, 0, 0, 0, time.UTC)
		page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
		r.NoError(err)
		r.Len(page.Occurrences, 1)
		r.Equal("confirmed", page.Occurrences[0].Status)
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
