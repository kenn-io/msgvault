package store

import (
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConversationWindowConcurrentWriteSnapshot(t *testing.T) {
	for _, boundary := range []string{"SELECT (SELECT COUNT(*)", "WITH selected AS"} {
		for _, change := range []string{"insert", "hide"} {
			t.Run(boundary+"/"+change, func(t *testing.T) {
				requirements := require.New(t)
				assertions := assert.New(t)
				st := newSnapshotTestStore(t)
				source, err := st.GetOrCreateSource("gmail", "snapshot@example.com")
				requirements.NoError(err)
				conv, err := st.EnsureConversation(source.ID, "snapshot", "Snapshot")
				requirements.NoError(err)
				base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
				var ids []int64
				for i := range 3 {
					id, err := st.UpsertMessage(&Message{
						SourceID: source.ID, ConversationID: conv, SourceMessageID: strconv.Itoa(i),
						MessageType: "email", SentAt: sql.NullTime{Time: base.Add(time.Duration(i) * time.Minute), Valid: true},
					})
					requirements.NoError(err)
					ids = append(ids, id)
				}
				writer, err := st.DB().BeginTx(t.Context(), nil)
				requirements.NoError(err)
				defer func() { _ = writer.Rollback() }()
				if change == "insert" {
					_, err = writer.Exec(st.Rebind(`INSERT INTO messages (source_id, conversation_id, source_message_id, message_type, sent_at) VALUES (?, ?, 'new', 'email', ?)`), source.ID, conv, base.Add(-time.Minute))
				} else {
					_, err = writer.Exec(st.Rebind(`UPDATE messages SET deleted_at = ? WHERE id IN (?, ?)`), base, ids[0], ids[1])
				}
				requirements.NoError(err)

				fired := commitSnapshotWriterOnQuery(t, st, writer, boundary)
				window, err := st.GetConversationWindow(conv, ids[1], 25, 25)
				requirements.NoError(err)
				requirements.True(*fired, "the concurrent write must commit during the read")
				got := make([]int64, 0, len(window.Messages))
				for _, message := range window.Messages {
					got = append(got, message.ID)
				}
				assertions.Equal(ids, got)
				assertions.Equal(int64(3), window.Total)
				assertions.Equal(int64(2), window.AnchorPosition)
				fresh, err := st.GetConversationWindow(conv, ids[1], 25, 25)
				requirements.NoError(err)
				if change == "insert" {
					assertions.Len(fresh.Messages, 4)
					assertions.Equal(int64(4), fresh.Total)
					assertions.Equal(int64(3), fresh.AnchorPosition)
				} else {
					assertions.Empty(fresh.Messages)
					assertions.Zero(fresh.Total)
				}
			})
		}
	}
}
