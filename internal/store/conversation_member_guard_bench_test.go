package store

import (
	"fmt"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func BenchmarkMemberInsertLargeChat(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(size), func(b *testing.B) {
			for _, stats := range []string{"no-stats", "analyzed"} {
				b.Run(stats, func(b *testing.B) {
					s, err := OpenForTest(filepath.Join(b.TempDir(), "member-guard.db"))
					require.NoError(b, err)
					b.Cleanup(func() { require.NoError(b, s.Close()) })
					require.NoError(b, s.InitSchema())
					seedMembershipEmbeddingGuard(b, s, size)
					if stats == "analyzed" {
						_, err = s.db.Exec("ANALYZE")
						require.NoError(b, err)
					}
					next := 0
					b.ReportAllocs()
					for b.Loop() {
						b.StopTimer()
						next++
						participantID, err := s.EnsureParticipant(fmt.Sprintf("member-%d@example.test", next), "Member", "example.test")
						require.NoError(b, err)
						b.StartTimer()
						err = s.EnsureConversationParticipantContext(b.Context(), 1, participantID, "member")
						require.NoError(b, err)
					}
				})
			}
		})
	}
}
