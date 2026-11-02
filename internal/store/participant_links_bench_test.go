package store

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedDisjointParticipantLinks(tb testing.TB, st *Store, unlinked, edges int) {
	tb.Helper()
	_, err := st.DB().Exec(`WITH RECURSIVE ids(id) AS (
		SELECT 1 UNION ALL SELECT id + 1 FROM ids WHERE id < ?
	) INSERT INTO participants (id, email_address)
	SELECT id, 'participant-' || id || '@example.com' FROM ids`, unlinked+2*edges)
	require.NoError(tb, err)
	_, err = st.DB().Exec(`INSERT INTO participant_links (participant_a, participant_b)
		SELECT id, id + 1 FROM participants WHERE id > ? AND id % 2 = 1`, unlinked)
	require.NoError(tb, err)
}

// BenchmarkLinkParticipantsUnrelatedEdges measures one link with a fixed number of unrelated edges.
func BenchmarkLinkParticipantsUnrelatedEdges(b *testing.B) {
	for _, edges := range []int{1000, 10000, 50000} {
		b.Run(strconv.Itoa(edges), func(b *testing.B) {
			require := require.New(b)
			st, err := OpenForTest(filepath.Join(b.TempDir(), "links.db"))
			require.NoError(err)
			b.Cleanup(func() { _ = st.Close() })
			require.NoError(st.InitSchema())
			seedDisjointParticipantLinks(b, st, 2, edges)
			b.ReportAllocs()
			for b.Loop() {
				_, err := st.LinkParticipants(1, 2)
				require.NoError(err)
				b.StopTimer()
				_, err = st.DB().Exec(`DELETE FROM participant_links WHERE participant_a = 1 AND participant_b = 2`)
				require.NoError(err)
				b.StartTimer()
			}
		})
	}
}
