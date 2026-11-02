package store_test

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// BenchmarkLinkParticipantsUnrelatedEdges measures one link with a fixed number
// of unrelated edges. At 1k, 10k, and 50k edges, full-table reads took 1.23,
// 11.90, and 63.35 ms per link; component reads took 0.30, 0.28, and 0.27 ms.
func BenchmarkLinkParticipantsUnrelatedEdges(b *testing.B) {
	for _, edges := range []int{1000, 10000, 50000} {
		b.Run(strconv.Itoa(edges), func(b *testing.B) {
			require := require.New(b)
			st, err := store.OpenForTest(filepath.Join(b.TempDir(), "links.db"))
			require.NoError(err)
			b.Cleanup(func() { _ = st.Close() })
			require.NoError(st.InitSchema())
			_, err = st.DB().Exec(`WITH RECURSIVE ids(id) AS (
				SELECT 1 UNION ALL SELECT id + 1 FROM ids WHERE id < ?
			) INSERT INTO participants (id, email_address)
			SELECT id, 'participant-' || id || '@example.com' FROM ids`, 2*edges+2)
			require.NoError(err)
			_, err = st.DB().Exec(`INSERT INTO participant_links (participant_a, participant_b)
				SELECT id, id + 1 FROM participants WHERE id > 2 AND id % 2 = 1`)
			require.NoError(err)
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
