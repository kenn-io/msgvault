package store_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarEventFromExcludedSenderRemainsVisible(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("noreply@example.com", "Calendar sender", "example.com")
	id := f.add(t, f.ConvID, f.Source.ID, tierAsOf, false, "")
	conversationID := f.ConvID
	_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='calendar_event' WHERE id=?`), id)
	require.NoError(err)
	projectTierSignals(t, f.Fixture, "UTC", 0)
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	assert.Zero(page.NoiseOmitted)
	require.Len(page.Bursts, 1)
	assert.Equal(id, page.Bursts[0].AnchorMessageID)
	assert.Zero(page.Bursts[0].Inbound)
	found := false
	for _, cp := range page.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			found = true
			assert.Equal("automated_address", cp.Excluded)
			assert.False(cp.Counted)
		}
	}
	assert.True(found)
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conversationID, Limit: 10})
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(id, messages[0].MessageID)
	contact := timelineContactQuery(f)
	contact.Now = tierAsOf
	aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
	require.NoError(err)
	assert.Zero(aggregate.Count)
}

func TestImportedMIMEInvitationFromExcludedSenderIsNoise(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("noreply@example.com", "Calendar sender", "example.com")
	var id, conversationID int64
	raw := []byte("From: Calendar sender <noreply@example.com>\r\n" +
		"To: Owner <owner@example.com>\r\n" +
		"Date: Sat, 17 Oct 2026 12:00:00 +0000\r\n" +
		"Message-ID: <invitation@example.com>\r\n" +
		"Subject: Planning invitation\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=invite\r\n\r\n" +
		"--invite\r\nContent-Type: text/plain\r\n\r\nCan you join?\r\n" +
		"--invite\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\n" +
		"BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Calendar//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:planning@example.com\r\nDTSTAMP:20261017T120000Z\r\n" +
		"DTSTART:20261018T120000Z\r\nDTEND:20261018T130000Z\r\n" +
		"ORGANIZER:mailto:noreply@example.com\r\nATTENDEE:mailto:owner@example.com\r\n" +
		"SUMMARY:Planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n--invite--\r\n")
	require.NoError(importer.IngestRawMessage(t.Context(), f.Store, f.Source.ID, "owner@example.com", "", nil, "invitation", "invitation-hash", raw, tierAsOf, slog.Default()))
	var messageType string
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id,conversation_id,message_type FROM messages WHERE source_id=? AND source_message_id='invitation'`), f.Source.ID).Scan(&id, &conversationID, &messageType))
	require.Equal("email", messageType)
	projectTierSignals(t, f.Fixture, "UTC", 0)
	query := timelineBurstQuery()
	page, err := f.Store.ListActivityBurstsContext(t.Context(), query)
	require.NoError(err)
	assert.Empty(page.Bursts)
	assert.Equal(1, page.NoiseOmitted)

	query.Filters.IncludeNoise = true
	page, err = f.Store.ListActivityBurstsContext(t.Context(), query)
	require.NoError(err)
	require.Len(page.Bursts, 1)
	assert.Equal(id, page.Bursts[0].AnchorMessageID)
	assert.Equal(conversationID, page.Bursts[0].ConversationID)
	assert.Zero(page.Bursts[0].Inbound)
}

func TestCalendarFileInvitationFromExcludedSenderRemainsVisible(t *testing.T) {
	require, assert := require.New(t), assert.New(t)
	f := newTimelineActivityFixture(t)
	f.peer = f.EnsureParticipant("calendar-notification@example.com", "Calendar sender", "example.com")
	calendar := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Calendar//EN\r\nMETHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\nUID:planning@example.com\r\nDTSTAMP:20261017T120000Z\r\n" +
		"DTSTART:20261018T120000Z\r\nDTEND:20261018T130000Z\r\n" +
		"ORGANIZER:mailto:calendar-notification@example.com\r\nATTENDEE:mailto:owner@example.com\r\n" +
		"SUMMARY:Planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	raw := []byte("From: Calendar sender <calendar-notification@example.com>\r\n" +
		"To: Owner <owner@example.com>\r\nDate: Sat, 17 Oct 2026 12:00:00 +0000\r\n" +
		"Message-ID: <calendar-file@example.com>\r\nSubject: Planning invitation\r\nMIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=mixed\r\n\r\n" +
		"--mixed\r\nContent-Type: multipart/alternative; boundary=invite\r\n\r\n" +
		"--invite\r\nContent-Type: text/plain\r\n\r\nCan you join?\r\n" +
		"--invite\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\n" + calendar + "--invite--\r\n" +
		"--mixed\r\nContent-Type: application/ics; name=\"invite.ics\"\r\n" +
		"Content-Disposition: attachment; filename=\"invite.ics\"\r\n\r\n" + calendar + "--mixed--\r\n")
	require.NoError(importer.IngestRawMessage(t.Context(), f.Store, f.Source.ID, "owner@example.com", t.TempDir(), nil, "calendar-file", "calendar-file-hash", raw, tierAsOf, slog.Default()))
	var id, conversationID int64
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id,conversation_id FROM messages WHERE source_id=? AND source_message_id='calendar-file'`), f.Source.ID).Scan(&id, &conversationID))
	var attachments int
	require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=?`), id).Scan(&attachments))
	require.Equal(1, attachments)
	projectTierSignals(t, f.Fixture, "UTC", 0)
	page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
	require.NoError(err)
	assert.Zero(page.NoiseOmitted)
	require.Len(page.Bursts, 1)
	assert.Equal(id, page.Bursts[0].AnchorMessageID)
	assert.Zero(page.Bursts[0].Inbound)
	found := false
	for _, cp := range page.Bursts[0].Counterparts {
		if cp.ParticipantID == f.peer {
			found = true
			assert.Equal("automated_address", cp.Excluded)
			assert.False(cp.Counted)
		}
	}
	assert.True(found)
	messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conversationID, Limit: 10})
	require.NoError(err)
	require.Len(messages, 1)
	assert.Equal(id, messages[0].MessageID)
	contact := timelineContactQuery(f)
	contact.Now = tierAsOf
	aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), contact)
	require.NoError(err)
	assert.Zero(aggregate.Count)
}
