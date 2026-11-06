package query

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func BenchmarkSQLiteConversationMessagesSnapshot(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			s, err := store.OpenForTest(filepath.Join(b.TempDir(), "timeline.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			require.NoError(b, s.InitSchema())
			_, err = s.DB().Exec(`
				INSERT INTO sources (id, source_type, identifier) VALUES (1, 'imessage', 'bench@example.com');
				INSERT INTO conversations (id, source_id, source_conversation_id, conversation_type)
					VALUES (1, 1, 'chat-1', 'direct_chat');
				INSERT INTO participants (id, email_address) VALUES (1, 'sender@example.com');
				INSERT INTO conversation_participants (conversation_id, participant_id) VALUES (1, 1);
			`)
			require.NoError(b, err)
			tx, err := s.DB().Begin()
			require.NoError(b, err)
			stmt, err := tx.Prepare(`INSERT INTO messages
				(id, conversation_id, source_id, source_message_id, message_type, sent_at, snippet, sender_id)
				VALUES (?, 1, 1, ?, 'imessage', datetime('2020-01-01', '+' || ? || ' seconds'), 'benchmark message', 1)`)
			require.NoError(b, err)
			defer func() { _ = stmt.Close() }()
			for id := 1; id <= size; id++ {
				_, err = stmt.Exec(id, strconv.Itoa(id), id)
				require.NoError(b, err)
			}
			require.NoError(b, stmt.Close())
			require.NoError(b, tx.Commit())
			_, err = s.DB().Exec(`UPDATE messages SET content_changed_at = '2020-01-01 00:00:00'`)
			require.NoError(b, err)
			engine := NewSQLiteEngine(s.DB())
			for _, tc := range []struct {
				name   string
				offset int
			}{{"early", 0}, {"deep", size / 2}} {
				b.Run(tc.name, func(b *testing.B) {
					filter := TextFilter{
						ParticipantIDs: []int64{1},
						Pagination:     Pagination{Limit: 100, Offset: tc.offset},
						SortDirection:  SortDesc,
					}
					b.ReportAllocs()
					for b.Loop() {
						page, _, err := engine.ListConversationMessagesSnapshot(b.Context(), 1, filter)
						require.NoError(b, err)
						require.Len(b, page, 100)
						require.Equal(b, int64(size-tc.offset), page[0].ID)
					}
				})
			}
			b.Run("probe", func(b *testing.B) {
				id := int64(1)
				scope := TextSnapshotScope{ConversationID: &id, Filter: TextFilter{ParticipantIDs: []int64{1}, SortDirection: SortDesc}}
				b.ReportAllocs()
				for b.Loop() {
					_, err := engine.TextSnapshotRevision(b.Context(), scope)
					require.NoError(b, err)
				}
			})
		})
	}
}
