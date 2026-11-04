package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkMemberInsertLargeChat(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			s, err := OpenForTest(filepath.Join(b.TempDir(), "member-guard.db"))
			require.NoError(b, err)
			b.Cleanup(func() { require.NoError(b, s.Close()) })
			require.NoError(b, s.InitSchema())
			seedLiveMessages(b, s, size)
			conversationID, err := s.EnsureConversation(1, "beeper-chat", "Beeper chat")
			require.NoError(b, err)
			for _, id := range []string{"beeper-1", "beeper-2", "beeper-3"} {
				messageID, err := s.UpsertMessage(&Message{SourceID: 1, SourceMessageID: id, ConversationID: conversationID, MessageType: "beeper"})
				require.NoError(b, err)
				require.NoError(b, s.UpsertMessageBody(messageID, sql.NullString{String: "Synthetic body", Valid: true}, sql.NullString{}))
			}
			_, err = s.db.Exec("ANALYZE")
			require.NoError(b, err)
			next := 0
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				next++
				participantID, err := s.EnsureParticipant(fmt.Sprintf("member-%d@example.test", next), "Member", "example.test")
				require.NoError(b, err)
				b.StartTimer()
				err = s.EnsureConversationParticipantContext(b.Context(), 1, participantID, "member")
				b.StopTimer()
				require.NoError(b, err)
				b.StartTimer()
			}
		})
	}
}
