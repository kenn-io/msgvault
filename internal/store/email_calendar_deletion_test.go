package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestEmailCalendarArchiveOnlyMarker(t *testing.T) {
	assertions, requirements := assert.New(t), require.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "marker@example.org")
	requirements.NoError(err)
	raw := []byte("Content-Type: text/calendar\r\n\r\nBEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:marker@example.org\r\nDTSTART:20270302T120000Z\r\nSUMMARY:Invite\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n")
	_, err = st.PersistMessageContext(t.Context(), &store.MessagePersistData{
		Message:      &store.Message{SourceID: source.ID, SourceMessageID: "parent", MessageType: "email"},
		Conversation: &store.ConversationPersistData{SourceConversationID: "parent"}, RawMIME: raw,
	})
	requirements.NoError(err)
	var eventID int64
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT id FROM messages WHERE source_id=? AND message_type='calendar_event'`), source.ID).Scan(&eventID))
	var marker int
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_merge_archive_only_messages WHERE message_id=?`), eventID).Scan(&marker))
	assertions.Equal(1, marker)
	dest, err := st.GetOrCreateSource("gmail", "destination@example.org")
	requirements.NoError(err)
	_, err = st.MergeSourcesContext(t.Context(), store.MergeSourcesRequest{FromSourceID: source.ID, IntoSourceID: dest.ID})
	requirements.NoError(err)
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT COUNT(*) FROM source_merge_archive_only_messages a JOIN messages m ON m.id=a.message_id WHERE m.id=? AND m.source_id=?`), eventID, dest.ID).Scan(&marker))
	assertions.Equal(1, marker)
}
