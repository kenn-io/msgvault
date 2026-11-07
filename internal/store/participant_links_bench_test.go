package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedDisjointParticipantLinks(tb testing.TB, st *Store, unlinked, edges int) {
	tb.Helper()
	override := ""
	if st.IsPostgreSQL() {
		override = " OVERRIDING SYSTEM VALUE"
	}
	_, err := st.db.ExecContext(tb.Context(), `WITH RECURSIVE ids(id) AS (
		SELECT 1 UNION ALL SELECT id + 1 FROM ids WHERE id < ?
	) INSERT INTO participants (id, email_address)`+override+`
	SELECT id, 'participant-' || id || '@example.com' FROM ids`, unlinked+2*edges)
	require.NoError(tb, err)
	_, err = st.db.ExecContext(tb.Context(), `INSERT INTO participant_links (participant_a, participant_b)
		SELECT id, id + 1 FROM participants WHERE id > ? AND id % 2 = 1`, unlinked)
	require.NoError(tb, err)
}

// BenchmarkLinkParticipantsUnrelatedEdges measures one link with a fixed number of unrelated edges.
func BenchmarkLinkParticipantsUnrelatedEdges(b *testing.B) {
	for _, edges := range []int{1000, 10000, 50000} {
		b.Run(strconv.Itoa(edges), func(b *testing.B) {
			require := require.New(b)
			st, err := OpenForTest(b.Context(), filepath.Join(b.TempDir(), "links.db"))
			require.NoError(err)
			b.Cleanup(func() { _ = st.Close() })
			require.NoError(st.InitSchema())
			seedDisjointParticipantLinks(b, st, 2, edges)
			b.ReportAllocs()
			for b.Loop() {
				_, err := st.LinkParticipants(1, 2)
				require.NoError(err)
				b.StopTimer()
				_, err = st.DB().ExecContext(b.Context(), `DELETE FROM participant_links WHERE participant_a = 1 AND participant_b = 2`)
				require.NoError(err)
				b.StartTimer()
			}
		})
	}
}

// seedDisjointOwnedParticipantLinks grows candidates, evidence, and owners with links.
func seedDisjointOwnedParticipantLinks(tb testing.TB, st *Store, unlinked, edges int) {
	tb.Helper()
	seedDisjointParticipantLinks(tb, st, unlinked, edges)
	source, err := st.GetOrCreateSource("slack", "unrelated@example.test")
	require.NoError(tb, err)
	for _, statement := range []string{
		`INSERT INTO identity_match_candidates (id, left_kind, left_id, right_kind, right_id, basis, state, source)
   SELECT participant_a, 'participant', participant_a, 'participant', participant_b, 'display_name', 'accepted', 'archive_observation' FROM participant_links`,
		`INSERT INTO identity_match_candidate_sources (candidate_id, source_id) SELECT id, ? FROM identity_match_candidates`,
		`INSERT INTO identity_match_evidence (id, candidate_id, evidence_kind, source) SELECT id, id, 'display_name', 'archive_observation' FROM identity_match_candidates`,
		`INSERT INTO identity_match_evidence_sources (evidence_id, source_id) SELECT id, ? FROM identity_match_evidence`,
		`UPDATE participant_links SET identity_match_candidate_id = participant_a`,
	} {
		if st.IsPostgreSQL() {
			statement = strings.Replace(statement, "source)", "source) OVERRIDING SYSTEM VALUE", 1)
		}
		var args []any
		if strings.Contains(statement, "?") {
			args = []any{source.ID}
		}
		_, err := st.db.ExecContext(tb.Context(), statement, args...)
		require.NoError(tb, err)
	}
	if st.IsPostgreSQL() {
		for _, table := range []string{"participants", "identity_match_candidates", "identity_match_evidence"} {
			_, err := st.DB().ExecContext(tb.Context(), `SELECT setval(pg_get_serial_sequence('`+table+`', 'id'), (SELECT max(id) FROM `+table+`))`)
			require.NoError(tb, err)
		}
	}
}

func BenchmarkMergeParticipantsUnrelatedEdges(b *testing.B) {
	for _, edges := range []int{1000, 10000, 100000} {
		b.Run(strconv.Itoa(edges), func(b *testing.B) {
			var st *Store
			if testDB := os.Getenv("MSGVAULT_TEST_DB"); IsPostgresURL(testDB) {
				st = newUninitializedPGStoreInternal(b, testDB)
			} else {
				var err error
				st, err = OpenForTest(b.Context(), filepath.Join(b.TempDir(), "merge.db"))
				require.NoError(b, err)
				b.Cleanup(func() { require.NoError(b, st.Close()) })
			}
			require.NoError(b, st.InitSchema())
			seedDisjointOwnedParticipantLinks(b, st, 0, edges)
			override := ""
			if st.IsPostgreSQL() {
				// Bulk seeding bypasses the statistics normally maintained by autovacuum.
				_, err := st.db.ExecContext(b.Context(), `ANALYZE participants, participant_links, identity_match_candidates`)
				require.NoError(b, err)
				override = " OVERRIDING SYSTEM VALUE"
			}
			next := int64(2*edges + 1)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				loser, winner, neighbor := next, next+1, next+2
				next += 3
				for _, id := range []int64{loser, winner, neighbor} {
					_, err := st.db.ExecContext(b.Context(), `INSERT INTO participants (id, email_address)`+override+` VALUES (?, ?)`, id, fmt.Sprintf("merge-%d@example.test", id))
					require.NoError(b, err)
				}
				_, err := st.db.ExecContext(b.Context(), `INSERT INTO participant_links (participant_a, participant_b) VALUES (?, ?), (?, ?)`, loser, neighbor, winner, neighbor)
				require.NoError(b, err)
				_, err = st.db.ExecContext(b.Context(), `INSERT INTO identity_match_candidates (id, left_kind, left_id, right_kind, right_id, basis, state, source)`+override+` VALUES (?, 'participant', ?, 'participant', ?, 'display_name', 'candidate', 'system')`, loser, loser, winner)
				require.NoError(b, err)
				b.StartTimer()
				err = st.MergeParticipants(loser, winner)
				b.StopTimer()
				require.NoError(b, err)
				b.StartTimer()
			}
		})
	}
}
