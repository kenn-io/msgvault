package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUndoDedupRestoresCalendarMeetingCountAndInvalidatesCache(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("Parquet cache invalidation is SQLite-only")
	}

	event, _ := calendarLinkFixture(t, st, "primary@example.test", singleLinkRaw(
		"linked-event", "linked@example.test", "2027-03-14T10:00:00Z", "2027-03-14T11:00:00Z",
	))
	_, first := projectionFixture(t, st, "granola-one", "granola_json", `{"calendar_event":{"calendar_event_id":"linked-event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	_, second := projectionFixture(t, st, "granola-two", "granola_json", `{"calendar_event":{"calendar_event_id":"linked-event","scheduled_start_time":"2027-03-14T10:00:00Z"}}`)
	countLogicalMeetings := func(keys map[int64]string) int {
		unique := make(map[string]struct{}, len(keys))
		for _, key := range keys {
			unique[key] = struct{}{}
		}
		return len(unique)
	}
	visibleEvents := func() []CalendarEventRow {
		rows, err := calendarEventRowsByMessageIDs(t.Context(), st.DB(), OwnerScope(), []int64{event})
		requirements.NoError(err)
		return rows
	}

	initialKeys, err := st.LogicalMeetingKeysContext(t.Context(), []int64{first, second})
	requirements.NoError(err)
	assertions.Equal("ical:linked@example.test", initialKeys[first])
	assertions.Equal(initialKeys[first], initialKeys[second])
	assertions.Equal(1, countLogicalMeetings(initialKeys))
	requirements.Len(visibleEvents(), 1)

	beforeHide, err := st.DerivedDataRevision()
	requirements.NoError(err)
	result, err := st.db.Exec(st.Rebind(`UPDATE messages SET deleted_at = CURRENT_TIMESTAMP, delete_batch_id = ? WHERE id = ?`), "hidden-calendar-event", event)
	requirements.NoError(err)
	affected, err := result.RowsAffected()
	requirements.NoError(err)
	assertions.Equal(int64(1), affected)
	afterHide, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Greater(afterHide, beforeHide, "hiding a calendar event must invalidate cache data derived from event visibility")

	// A cache rebuild while the event is hidden stamps this revision and exports
	// two independent fallback meeting keys for the linked transcripts.
	hiddenCacheKeys, err := st.LogicalMeetingKeysContext(t.Context(), []int64{first, second})
	requirements.NoError(err)
	assertions.Equal(fmt.Sprintf("msg:%d", first), hiddenCacheKeys[first])
	assertions.Equal(fmt.Sprintf("msg:%d", second), hiddenCacheKeys[second])
	assertions.Equal(2, countLogicalMeetings(hiddenCacheKeys))
	assertions.Empty(visibleEvents())
	hiddenCacheRevision := afterHide

	restored, err := st.UndoDedup("hidden-calendar-event")
	requirements.NoError(err)
	assertions.Equal(int64(1), restored)
	afterUndo, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Greater(afterUndo, hiddenCacheRevision, "restoring a calendar event must stale a cache built while it was hidden")
	requirements.Len(visibleEvents(), 1)

	restoredKeys, err := st.LogicalMeetingKeysContext(t.Context(), []int64{first, second})
	requirements.NoError(err)
	assertions.Equal("ical:linked@example.test", restoredKeys[first])
	assertions.Equal(restoredKeys[first], restoredKeys[second])
	assertions.Equal(1, countLogicalMeetings(restoredKeys))
}

func TestMeetingCountEvidenceInvalidatesCache(t *testing.T) {
	cases := []struct{ name, statement string }{
		{"event uid", `UPDATE calendar_events SET ical_uid = 'changed@example.org' WHERE message_id = ?`},
		{"event cancellation", `UPDATE calendar_events SET status = 'cancelled' WHERE message_id = ?`},
		{"event attendees", `INSERT INTO calendar_event_attendees(message_id,ordinal,email,display_name,role,rsvp,is_copy_self) VALUES(?,0,'resource@example.com','','room','unknown',FALSE)`},
		{"calendar event source deletion", `UPDATE messages SET deleted_from_source_at = CURRENT_TIMESTAMP WHERE id = ?`},
		{"key change", `UPDATE meeting_link_keys SET value = 'another-event' WHERE message_id = ?`},
		{"key removal", `DELETE FROM meeting_link_keys WHERE message_id = ?`},
		{"member hidden", `UPDATE messages SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := newRFC822IDBackfillBackendStore(t)
			if st.IsPostgreSQL() {
				t.Skip("Parquet cache invalidation is SQLite-only")
			}
			event, _ := calendarLinkFixture(t, st, "primary@example.com", linkSeriesRaw)
			note := granolaLinkFixture(t, st, "series_20270314T100000Z")
			id := event
			if tc.name == "key change" || tc.name == "key removal" || tc.name == "member hidden" {
				id = note
			}
			before, err := st.DerivedDataRevision()
			requirements.NoError(err)
			result, err := st.DB().Exec(tc.statement, id)
			requirements.NoError(err)
			affected, err := result.RowsAffected()
			requirements.NoError(err)
			requirements.Positive(affected)
			after, err := st.DerivedDataRevision()
			requirements.NoError(err)
			assertions.Greater(after, before, "committed meeting evidence must make existing caches stale")
		})
	}
}

func TestOrdinaryMessageTimestampEditDoesNotInvalidateMeetingCounts(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := newRFC822IDBackfillBackendStore(t)
	if st.IsPostgreSQL() {
		t.Skip("Parquet cache invalidation is SQLite-only")
	}
	source, err := st.GetOrCreateSource("gmail", "owner@example.test")
	requirements.NoError(err)
	messageID, err := st.PersistMessage(&MessagePersistData{
		Message: &Message{
			SourceID: source.ID, SourceMessageID: "ordinary-email",
			MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		},
		Conversation: &ConversationPersistData{
			SourceConversationID: "ordinary-thread", ConversationType: "email_thread",
		},
	})
	requirements.NoError(err)

	before, err := st.DerivedDataRevision()
	requirements.NoError(err)
	_, err = st.DB().Exec(`UPDATE messages SET sent_at = '2027-01-02 00:00:00' WHERE id = ?`, messageID)
	requirements.NoError(err)
	after, err := st.DerivedDataRevision()
	requirements.NoError(err)
	assertions.Equal(before, after, "ordinary message timestamp edits do not affect meeting counts")
}
