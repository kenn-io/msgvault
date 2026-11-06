package store

import (
	"database/sql"
	"fmt"
	"os"
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
				var s *Store
				if dbURL := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(dbURL) {
					s = newUninitializedPGStoreInternal(b, dbURL)
				} else {
					var err error
					s, err = OpenForTest(filepath.Join(b.TempDir(), "growing.db"))
					require.NoError(b, err)
					b.Cleanup(func() {
						if s.DB().Ping() == nil {
							_ = s.Close()
						}
					})
				}
				require.NoError(b, s.InitSchema())
				source, err := s.GetOrCreateSource("gmail", "bench@example.com")
				require.NoError(b, err)
				conversationID, err := s.EnsureConversationWithType(source.ID, "growing", "group_chat", "Growing Chat")
				require.NoError(b, err)
				members := make([]int64, size/100)
				for member := range members {
					members[member], err = s.EnsureParticipant(fmt.Sprintf("member%d@example.com", member), "Member", "example.com")
					require.NoError(b, err)
				}
				b.StartTimer()
				for batch := range size / 100 {
					start := time.Now()
					for i := batch * 100; i < (batch+1)*100; i++ {
						_, err := s.UpsertMessageContext(b.Context(), &Message{
							ConversationID: conversationID, SourceID: source.ID,
							SourceMessageID: fmt.Sprintf("m%d", i), MessageType: "chat",
							SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute), Valid: true},
						})
						require.NoError(b, err)
					}
					messageTime += time.Since(start)
					start = time.Now()
					require.NoError(b, s.EnsureConversationParticipantContext(b.Context(), conversationID, members[batch], "member"))
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
