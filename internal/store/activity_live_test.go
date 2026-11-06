package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestLiveEligibilityExcludesChangesAfterProjection(t *testing.T) {
	for _, change := range []string{"is_sent=FALSE", "deleted_at=CURRENT_TIMESTAMP", "deleted_from_source_at=CURRENT_TIMESTAMP", `metadata='{"status":"cancelled"}'`} {
		t.Run(change, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newTimelineActivityFixture(t)
			now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
			conversationID := f.ConvID
			var id int64
			calendar := change == `metadata='{"status":"cancelled"}'`
			if calendar {
				source, err := f.Store.GetOrCreateSource("gcal", "owner@example.com")
				requirements.NoError(err)
				requirements.NoError(f.Store.AddAccountIdentity(source.ID, "owner@example.com", "manual"))
				requirements.NoError(f.Store.UpdateSourceSyncConfig(source.ID, `{"calendar_id":"owner@example.com","time_zone":"UTC"}`))
				message := f.NewMessage().WithSentAt(now).Build()
				message.SourceID, message.ConversationID = source.ID, 0
				message.MessageType = "calendar_event"
				message.SenderID = sql.NullInt64{Int64: f.peer, Valid: true}
				id, err = f.Store.PersistMessage(&store.MessagePersistData{
					Message:      message,
					Conversation: &store.ConversationPersistData{SourceConversationID: "planning", ConversationType: "calendar", Title: "Planning"},
					RawMIME:      []byte(`{"id":"planning","status":"confirmed","start":{"dateTime":"2026-10-22T12:00:00Z"},"end":{"dateTime":"2026-10-22T13:00:00Z"},"iCalUID":"planning@example.com","organizer":{"email":"alice@example.com"},"attendees":[{"email":"owner@example.com","responseStatus":"accepted"}]}`),
					RawFormat:    "gcal_json",
					Metadata:     &sql.NullString{String: `{"calendar_id":"owner@example.com"}`, Valid: true},
				})
				requirements.NoError(err)
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "from", []int64{f.peer}, nil))
				requirements.NoError(f.Store.ReplaceMessageRecipients(id, "to", []int64{f.owner}, nil))
				requirements.NoError(f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`SELECT conversation_id FROM messages WHERE id=?`), id).Scan(&conversationID))
			} else {
				id = f.add(t, f.ConvID, f.Source.ID, now, false, "")
			}
			ids := []int64{id}
			if !calendar {
				transcript := f.add(t, f.ConvID, f.Source.ID, now.Add(-time.Minute), false, "")
				_, err := f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET message_type='meeting_transcript' WHERE id=?`), transcript)
				requirements.NoError(err)
				ids = append(ids, transcript)
			}
			projectTierSignals(t, f.Fixture, "UTC", 0)
			q := timelineContactQuery(f)
			summary, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			if calendar {
				assertions.Zero(summary.Count)
				page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
				requirements.NoError(err)
				requirements.Len(page.Bursts, 1)
				assertions.Equal(id, page.Bursts[0].AnchorMessageID)
				// Remove decoded status so the live reader must use message metadata.
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`DELETE FROM calendar_events WHERE message_id=?`), id)
				requirements.NoError(err)
				page, err = f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
				requirements.NoError(err)
				requirements.Len(page.Bursts, 1, "missing decoded status alone must keep the event visible")
			} else {
				assertions.Equal(1, summary.Count)
			}
			for _, messageID := range ids {
				_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`UPDATE messages SET `+change+` WHERE id=?`), messageID)
				requirements.NoError(err)
			}
			for _, includeNoise := range []bool{false, true} {
				query := timelineBurstQuery()
				query.Filters.IncludeNoise = includeNoise
				bursts, err := f.Store.ListActivityBurstsContext(t.Context(), query)
				requirements.NoError(err)
				assertions.Empty(bursts.Bursts)
			}
			messages, err := f.Store.ListActivityMessagesContext(t.Context(), store.MessageQuery{ConversationID: conversationID, Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
			requirements.NoError(err)
			assertions.Empty(messages)
			summary, err = f.Store.MessageContactAggregateContext(t.Context(), q)
			requirements.NoError(err)
			assertions.Zero(summary.Count)
			latest, err := f.Store.LatestEligibleMessageContext(t.Context(), conversationID)
			requirements.NoError(err)
			assertions.Zero(latest)
			display, err := f.Store.MessageDisplayContext(t.Context(), ids, store.ActivityFilters{})
			requirements.NoError(err)
			assertions.Empty(display)
		})
	}
}
