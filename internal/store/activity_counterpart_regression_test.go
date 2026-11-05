package store_test

import (
	"database/sql"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/importer"
	"go.kenn.io/msgvault/internal/store"
)

func TestCalendarInvitationsFromExcludedSender(t *testing.T) {
	for _, tc := range []struct {
		name, sender                         string
		event, attachment, readContact, list bool
		noise                                int
	}{
		{name: "calendar event", sender: "noreply@example.com", event: true},
		{name: "calendar event with list ID", sender: "noreply@example.com", event: true, list: true},
		{name: "calendar file", sender: "calendar-notification@example.com", attachment: true, readContact: true},
		{name: "newsletter calendar file", sender: "vendor@example.com", attachment: true, list: true, noise: 1},
		{name: "inline MIME", sender: "noreply@example.com", noise: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require, assert := require.New(t), assert.New(t)
			f := newTimelineActivityFixture(t)
			f.peer = f.EnsureParticipant(tc.sender, "Calendar sender", "example.com")
			var id, conversationID int64
			if tc.event {
				src, err := f.Store.GetOrCreateSource("gcal", "owner@example.com")
				require.NoError(err)
				require.NoError(f.Store.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
				require.NoError(f.Store.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
				message := f.NewMessage().WithSentAt(tierAsOf).Build()
				message.SourceID, message.ConversationID = src.ID, 0
				message.MessageType = "calendar_event"
				message.SenderID = sql.NullInt64{Int64: f.peer, Valid: true}
				id, err = f.Store.PersistMessage(&store.MessagePersistData{
					Message:      message,
					Conversation: &store.ConversationPersistData{SourceConversationID: "planning", ConversationType: "calendar", Title: "Planning"},
					RawMIME:      []byte(`{"id":"planning","status":"confirmed","start":{"dateTime":"2026-10-17T12:00:00Z"},"end":{"dateTime":"2026-10-17T13:00:00Z"},"iCalUID":"planning@example.com","organizer":{"email":"noreply@example.com"},"attendees":[{"email":"owner@example.com","responseStatus":"accepted"}]}`),
					RawFormat:    "gcal_json",
					Metadata:     &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
				})
				require.NoError(err)
				require.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.peer}, nil))
				require.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner}, nil))
				var events int
				require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM calendar_events WHERE message_id=?`), id).Scan(&events))
				require.Equal(1, events)
			} else {
				calendar := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//Example//Calendar//EN\r\nMETHOD:REQUEST\r\n" +
					"BEGIN:VEVENT\r\nUID:planning@example.com\r\nDTSTAMP:20261017T120000Z\r\n" +
					"DTSTART:20261018T120000Z\r\nDTEND:20261018T130000Z\r\n" +
					"ORGANIZER:mailto:" + tc.sender + "\r\nATTENDEE:mailto:owner@example.com\r\n" +
					"SUMMARY:Planning\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
				body := "Content-Type: multipart/alternative; boundary=invite\r\n\r\n" +
					"--invite\r\nContent-Type: text/plain\r\n\r\nCan you join?\r\n" +
					"--invite\r\nContent-Type: text/calendar; method=REQUEST\r\n\r\n" + calendar + "--invite--\r\n"
				if tc.attachment {
					body = "Content-Type: multipart/mixed; boundary=mixed\r\n\r\n--mixed\r\n" + body +
						"--mixed\r\nContent-Type: application/ics; name=\"invite.ics\"\r\n" +
						"Content-Disposition: attachment; filename=\"invite.ics\"\r\n\r\n" + calendar + "--mixed--\r\n"
				}
				listHeader := ""
				if tc.list {
					listHeader = "List-Id: Newsletter <newsletter.example.com>\r\n"
				}
				raw := []byte(listHeader + "From: Calendar sender <" + tc.sender + ">\r\n" +
					"To: Owner <owner@example.com>\r\nDate: Sat, 17 Oct 2026 12:00:00 +0000\r\n" +
					"Message-ID: <invitation@example.com>\r\nSubject: Planning invitation\r\nMIME-Version: 1.0\r\n" + body)
				require.NoError(importer.IngestRawMessage(t.Context(), f.Store, f.Source.ID, "owner@example.com", t.TempDir(), nil, "invitation", "invitation-hash", raw, tierAsOf, slog.Default()))
				var messageType string
				require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT id,message_type FROM messages WHERE source_id=? AND source_message_id='invitation'`), f.Source.ID).Scan(&id, &messageType))
				require.Equal("email", messageType)
				if tc.attachment {
					var attachments int
					require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT COUNT(*) FROM attachments WHERE message_id=?`), id).Scan(&attachments))
					require.Equal(1, attachments)
				}
			}
			if tc.event && tc.list {
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET list_id='newsletter.example.com' WHERE id=?`), id)
				require.NoError(err)
			}
			require.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), id).Scan(&conversationID))
			projectTierSignals(t, f.Fixture, "UTC", 0)
			query := timelineBurstQuery()
			page, err := f.Store.ListActivityBurstsContext(t.Context(), query)
			require.NoError(err)
			assert.Equal(tc.noise, page.NoiseOmitted)
			messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conversationID, Limit: 10})
			require.NoError(err)
			if tc.noise > 0 {
				assert.Empty(page.Bursts)
				assert.Empty(messages)
				query.Filters.IncludeNoise = true
				page, err = f.Store.ListActivityBurstsContext(t.Context(), query)
				require.NoError(err)
			} else {
				require.Len(messages, 1)
				assert.Equal(id, messages[0].MessageID)
			}
			require.Len(page.Bursts, 1)
			assert.Equal(id, page.Bursts[0].AnchorMessageID)
			assert.Equal(conversationID, page.Bursts[0].ConversationID)
			assert.Zero(page.Bursts[0].Inbound)
			if tc.noise == 0 {
				found := false
				for _, cp := range page.Bursts[0].Counterparts {
					if cp.ParticipantID == f.peer {
						found = true
						assert.Equal("automated_address", cp.Excluded)
						assert.False(cp.Counted)
					}
				}
				assert.True(found)
			}
			if tc.readContact {
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
		})
	}
}
