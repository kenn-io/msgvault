package store_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEmailCalendarArchiveOnlyMarker(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "marker@example.org")
	requirements.NoError(err)
	raw := []byte("Content-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:marker@example.org\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Invite\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	parent, err := st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "parent", MessageType: "email"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "parent"}, RawMIME: raw,
	})
	requirements.NoError(err)
	var eventID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND message_type='calendar_event'`), source.ID).Scan(&eventID))
	engine := query.NewEngine(st.DB(), st.IsPostgreSQL())
	targets, err := engine.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{SourceID: &source.ID})
	requirements.NoError(err)
	requirements.Len(targets, 1)
	assertions.Equal(parent, targets[0].MessageID)
	var marker int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_merge_archive_only_messages WHERE message_id=?`), eventID).Scan(&marker))
	assertions.Equal(1, marker)
	_, err = st.ReconcileSourceMessageSnapshot(t.Context(), source.ID, map[string]struct{}{"parent": {}})
	requirements.NoError(err)
	var deleted sql.NullTime
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT deleted_from_source_at FROM messages WHERE id=?`), eventID).Scan(&deleted))
	assertions.False(deleted.Valid)
	key, err := st.GetMessageSourceID(eventID)
	requirements.NoError(err)
	requirements.ErrorIs(st.RejectArchiveOnlyDeletionTargetsContext(t.Context(), source.ID, []string{key}), store.ErrArchiveOnlyDeletionTarget)
	from := time.Date(2027, 3, 2, 0, 0, 0, 0, time.UTC)
	page, err := st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	dest, err := st.GetOrCreateSource("gmail", "destination@example.org")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: dest.ID})
	requirements.NoError(err)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	requirements.Len(page.Occurrences, 1)
	key, err = st.GetMessageSourceID(eventID)
	requirements.NoError(err)
	requirements.ErrorIs(st.RejectArchiveOnlyDeletionTargetsContext(t.Context(), dest.ID, []string{key}), store.ErrArchiveOnlyDeletionTarget)
	targets, err = engine.GetDeletionTargetsByFilter(t.Context(), query.MessageFilter{SourceID: &dest.ID})
	requirements.NoError(err)
	assertions.Empty(targets)
	_, err = st.ReconcileSourceMessageSnapshot(t.Context(), dest.ID, map[string]struct{}{})
	requirements.NoError(err)
	page, err = st.ListCalendarOccurrencesContext(t.Context(), from, from.AddDate(0, 0, 1))
	requirements.NoError(err)
	assertions.Len(page.Occurrences, 1)
}
