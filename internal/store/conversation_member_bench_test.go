package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// BenchmarkGrowingChatImport adds a newly seen member after each 100 messages.
// Each metric is the total time spent on that write type per complete import.
func BenchmarkGrowingChatImport(b *testing.B) {
	for _, size := range []int{1000, 10000, 30000} {
		b.Run(fmt.Sprintf("%d_messages_%d_members", size, size/100), func(b *testing.B) {
			var messageTime, memberTime time.Duration
			for range b.N {
				b.StopTimer()
				s, err := OpenForTest(filepath.Join(b.TempDir(), "growing.db"))
				require.NoError(b, err)
				require.NoError(b, s.InitSchema())
				seedLiveMessages(b, s, 0)
				_, err = s.db.Exec(`UPDATE conversations SET conversation_type = 'group_chat' WHERE id = 1`)
				require.NoError(b, err)
				for member := 1; member <= size/100; member++ {
					_, err = s.db.Exec(`INSERT INTO participants (id, email_address) VALUES (?, ?)`, member+1, fmt.Sprintf("member%d@example.com", member))
					require.NoError(b, err)
				}
				b.StartTimer()
				for batch := 0; batch < size/100; batch++ {
					start := time.Now()
					for i := batch * 100; i < (batch+1)*100; i++ {
						_, err := s.UpsertMessageContext(b.Context(), &Message{
							ConversationID: 1, SourceID: 1,
							SourceMessageID: fmt.Sprintf("m%d", i), MessageType: "chat",
							SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute), Valid: true},
						})
						require.NoError(b, err)
					}
					messageTime += time.Since(start)
					start = time.Now()
					require.NoError(b, s.EnsureConversationParticipantContext(b.Context(), 1, int64(batch+2), "member"))
					memberTime += time.Since(start)
				}
				b.StopTimer()
				require.NoError(b, s.Close())
			}
			b.ReportMetric(float64(memberTime.Nanoseconds())/float64(b.N), "member-insert-ns/op")
			b.ReportMetric(float64(messageTime.Nanoseconds())/float64(b.N), "message-insert-ns/op")
		})
	}
}
