package store

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkParticipantsReadsOnlyTouchedComponents(t *testing.T) {
	tests := []string{"link", "reject"}
	for _, operation := range tests {
		t.Run(operation, func(t *testing.T) {
			require := require.New(t)
			st := openTestStore(t)
			seedDisjointParticipantLinks(t, st, 4, 1000)
			_, err := st.DB().Exec(`INSERT INTO participant_links (participant_a, participant_b) VALUES (1, 4), (2, 4)`)
			require.NoError(err)
			orig := buildAdjacency
			t.Cleanup(func() { buildAdjacency = orig })
			var sizes []int
			buildAdjacency = func(edges []linkEdge) map[int64][]int64 {
				sizes = append(sizes, len(edges))
				return orig(edges)
			}
			var rejectedID int64
			if operation == "reject" {
				_, err = st.DB().Exec(`DELETE FROM participant_links WHERE participant_a = 1 AND participant_b = 4`)
				require.NoError(err)
				for _, pair := range [][2]int64{{1, 4}, {1, 2}} {
					candidate, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
						LeftKind: IdentityMatchParticipant, LeftID: pair[0], RightKind: IdentityMatchParticipant, RightID: pair[1],
						Basis: IdentityMatchStableProviderID, NormalizedValue: new("example-stable-id"),
						State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation,
					})
					require.NoError(err)
					_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), candidate.ID, "system", nil)
					require.NoError(err)
					if pair[1] == 4 {
						rejectedID = candidate.ID
					}
				}
			}

			switch operation {
			case "link":
				_, err = st.LinkParticipants(2, 3)
			case "reject":
				_, err = st.DecideIdentityMatchCandidateContext(t.Context(), rejectedID, IdentityMatchStateRejected, "user", nil)
			}
			require.NoError(err)
			require.NotEmpty(sizes)
			for _, size := range sizes {
				assert.LessOrEqual(t, size, 2, "operation must read only the endpoint components")
			}
		})
	}
}

// setKeys returns the sorted-independent key set of a component result as a
// plain map[int64]bool, so assert.Equal can compare against a literal without
// caring about the empty-struct value type.
func setKeys(component map[int64]struct{}) map[int64]bool {
	out := make(map[int64]bool, len(component))
	for id := range component {
		out[id] = true
	}
	return out
}

// TestComponentOfAdjReturnsExactComponent feeds componentOfAdj a prebuilt
// adjacency map spanning several disconnected components and pins the exact
// membership returned for a root in each one, including an isolated node with
// no edges. This is the wrong-root/wrong-membership regression guard: a BFS
// that leaked across components or dropped a reachable node would change these
// literals.
func TestComponentOfAdjReturnsExactComponent(t *testing.T) {
	assert := assert.New(t)
	// Components: {1-2-3}, {5-6}, {10-11}; node 8 is isolated (no edges).
	edges := []linkEdge{{1, 2}, {2, 3}, {5, 6}, {10, 11}}
	adj := buildAdjacency(edges)

	assert.Equal(map[int64]bool{1: true, 2: true, 3: true}, setKeys(componentOfAdj(1, adj)))
	assert.Equal(map[int64]bool{1: true, 2: true, 3: true}, setKeys(componentOfAdj(3, adj)))
	assert.Equal(map[int64]bool{5: true, 6: true}, setKeys(componentOfAdj(5, adj)))
	assert.Equal(map[int64]bool{10: true, 11: true}, setKeys(componentOfAdj(11, adj)))
	assert.Equal(map[int64]bool{8: true}, setKeys(componentOfAdj(8, adj)),
		"an id absent from adj is its own single-element component")
}

// TestClustersFromEdgesSmallestRootAcrossComponents pins the smallest-id root
// invariant across multiple disconnected components, a chain, and a star in
// one edge list. The {10,11} component is discovered after the larger {1,2,3}
// component in ascending-id order, which pins that the canonical root is the
// smallest member of each component and not merely the first node visited.
func TestClustersFromEdgesSmallestRootAcrossComponents(t *testing.T) {
	assert := assert.New(t)
	edges := []linkEdge{
		{1, 2}, {2, 3}, // chain 1-2-3 rooted at 1
		{5, 6},                       // pair rooted at 5
		{10, 11},                     // pair rooted at 10 (discovered after 1..6)
		{20, 21}, {20, 22}, {20, 23}, // star centered on 20, rooted at 20
	}

	got := clustersFromEdges(edges)

	want := map[int64]int64{
		1: 1, 2: 1, 3: 1,
		5: 5, 6: 5,
		10: 10, 11: 10,
		20: 20, 21: 20, 22: 20, 23: 20,
	}
	assert.Equal(want, got)
}

// TestClustersFromEdgesMultiHopChainAndStar exercises multi-hop BFS over the
// shared adjacency map: a long chain (1-2-3-4-5) collapses to a single root,
// and a star (10 linked to 11..14) collapses to its center, all resolved from
// one adjacency build.
func TestClustersFromEdgesMultiHopChainAndStar(t *testing.T) {
	assert := assert.New(t)
	edges := []linkEdge{
		{1, 2}, {2, 3}, {3, 4}, {4, 5}, // chain
		{10, 11}, {10, 12}, {10, 13}, {10, 14}, // star
	}

	got := clustersFromEdges(edges)

	want := map[int64]int64{
		1: 1, 2: 1, 3: 1, 4: 1, 5: 1,
		10: 10, 11: 10, 12: 10, 13: 10, 14: 10,
	}
	assert.Equal(want, got)
}

// TestClustersFromEdgesBuildsAdjacencyOnce is the single-pass guard. It swaps
// buildAdjacency for a counting wrapper and asserts clustersFromEdges builds
// adjacency exactly once for a four-component graph. The pre-fix code called
// componentOf(id, edges) per component, which rebuilt adjacency once per
// component (four times here); the fix builds it once and traverses the shared
// map with componentOfAdj, so this pins that the quadratic per-component
// rebuild is gone.
func TestClustersFromEdgesBuildsAdjacencyOnce(t *testing.T) {
	assert := assert.New(t)
	orig := buildAdjacency
	t.Cleanup(func() { buildAdjacency = orig })

	calls := 0
	buildAdjacency = func(edges []linkEdge) map[int64][]int64 {
		calls++
		return orig(edges)
	}

	// Four disconnected components: a per-component rebuild would call
	// buildAdjacency four times.
	edges := []linkEdge{{1, 2}, {5, 6}, {10, 11}, {20, 21}}
	got := clustersFromEdges(edges)

	assert.Equal(1, calls, "adjacency must be built exactly once for the whole graph")
	assert.Equal(map[int64]int64{1: 1, 2: 1, 5: 5, 6: 5, 10: 10, 11: 10, 20: 20, 21: 20}, got)
}

// BenchmarkClustersFromEdgesDisjointComponents documents that resolving many
// disconnected components stays linear: adjacency is built once and each node
// is visited once, rather than rebuilding adjacency per component.
func BenchmarkClustersFromEdgesDisjointComponents(b *testing.B) {
	const components = 2000
	edges := make([]linkEdge, 0, components)
	for i := range int64(components) {
		lo := 2*i + 1
		edges = append(edges, linkEdge{a: lo, b: lo + 1})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if got := clustersFromEdges(edges); len(got) != 2*components {
			b.Fatalf("expected %d members, got %d", 2*components, len(got))
		}
	}
}

// Explain every emitted statement so an added scan fails without a timing threshold.
func TestMergeParticipantsUsesIndexedLinkAndCandidateQueries(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(strconv.FormatBool(legacy), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st := openTestStore(t)
			seedDisjointOwnedParticipantLinks(t, st, 4, 3000)
			_, err := st.db.Exec(`INSERT INTO participant_links (participant_a, participant_b) VALUES (1, 4), (2, 4)`)
			require.NoError(err)
			// Exercise both the self-link clear and the collision owner move.
			_, err = st.db.Exec(`INSERT INTO identity_match_candidates (id, left_kind, left_id, right_kind, right_id, basis, state, source)
    VALUES (1, 'participant', 1, 'participant', 2, 'display_name', 'accepted', 'system'),
     (2, 'participant', 1, 'participant', 4, 'display_name', 'accepted', 'system'),
     (3, 'participant', 2, 'participant', 4, 'display_name', 'accepted', 'system')`)
			require.NoError(err)
			_, err = st.db.Exec(`UPDATE participant_links SET identity_match_candidate_id = CASE participant_a WHEN 1 THEN 2 ELSE 3 END WHERE participant_a IN (1, 2)`)
			require.NoError(err)
			var statements []string
			rebind := st.db.rebind
			st.db.rebind = func(query string) string { statements = append(statements, query); return rebind(query) }
			if legacy {
				tx, err := st.db.BeginTx(t.Context(), nil)
				require.NoError(err)
				require.NoError(st.mergeParticipant(t.Context(), tx, 2, 1))
				require.NoError(tx.Commit())
			} else {
				require.NoError(st.MergeParticipants(1, 2))
			}
			st.db.rebind = rebind
			require.NotEmpty(statements)
			tables := regexp.MustCompile(`(?i)\b(?:FROM|JOIN|UPDATE|INTO)\s+(participant_links|identity_match_candidates|identity_match_evidence)(?:\s+(?:AS\s+)?(\w+))?`)
			scans := regexp.MustCompile(`(?i)\bSCAN (?:TABLE )?(\w+)`)
			for _, statement := range statements {
				targets := map[string]bool{"participant_links": true, "identity_match_candidates": true, "identity_match_evidence": true}
				for _, match := range tables.FindAllStringSubmatch(statement, -1) {
					switch strings.ToUpper(match[2]) {
					case "WHERE", "SET", "ORDER", "ON", "VALUES", "SELECT", "":
					default:
						targets[match[2]] = true
					}
				}
				args := make([]any, strings.Count(statement, "?"))
				rows, err := st.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+statement, args...)
				require.NoError(err, statement)
				for rows.Next() {
					var id, parent, unused int
					var detail string
					require.NoError(rows.Scan(&id, &parent, &unused, &detail))
					if scan := scans.FindStringSubmatch(detail); scan != nil {
						assert.False(targets[scan[1]], "%s\n%s", statement, detail)
					}
				}
				require.NoError(rows.Err())
				require.NoError(rows.Close())
			}
		})
	}
}

func TestInitSchemaAddsCandidateRightIndexBeforeLegacyMerge(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "existing.db")
	st, err := OpenForTest(path)
	require.NoError(err)
	require.NoError(st.InitSchema())
	_, err = st.db.Exec(`DROP INDEX idx_identity_match_candidates_right;
  DROP INDEX idx_participants_phone;
  DELETE FROM applied_migrations WHERE name = '` + migrationPhoneUniqueIndex + `';
  INSERT INTO participants (phone_number, display_name) VALUES ('+15550000001', 'First'), ('+15550000001', 'Second')`)
	require.NoError(err)
	require.NoError(st.Close())
	st, err = OpenForTest(path)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })
	rebind := st.db.rebind
	checked := false
	st.db.rebind = func(query string) string {
		if !checked && strings.Contains(query, "JOIN participant_links") {
			checked = true
			var count int
			require.NoError(st.db.DB.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_identity_match_candidates_right'`).Scan(&count))
			require.Equal(1, count, "the index must exist when legacy merges begin")
		}
		return rebind(query)
	}
	require.NoError(st.InitSchema())
	require.True(checked)
	var count int
	require.NoError(st.db.QueryRow(`SELECT count(*) FROM participants`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestMergeParticipantsReappliesAcceptedMatchAfterScopedCleanup(t *testing.T) {
	for _, transfer := range []bool{false, true} {
		t.Run(strconv.FormatBool(transfer), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			st, observer := filenameObservedStore(t)
			seedDisjointOwnedParticipantLinks(t, st, 6, 1000)
			source, err := st.GetOrCreateSource("slack", "merge-support@example.test")
			require.NoError(err)
			for participant, provider := range map[int64]string{1: "previous-provider", 2: "survivor-provider", 3: "previous-provider"} {
				_, err := st.RecordContactObservationContext(t.Context(), participant, ParticipantContactObservationInput{
					SourceID: &source.ID, AddressKind: ContactAddressEmail,
					OriginalValue: "shared@example.test", ProviderUserID: new(provider),
					Envelope: ValueEnvelopeInput{Source: ProvenanceArchiveObservation},
				})
				require.NoError(err)
			}
			lost, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
				LeftKind: IdentityMatchParticipant, LeftID: 1, RightKind: IdentityMatchParticipant, RightID: 3,
				Basis: IdentityMatchStableProviderID, NormalizedValue: new("previous-provider"),
				State: IdentityMatchStateCandidate, Source: ProvenanceArchiveObservation, SourceID: &source.ID,
			})
			require.NoError(err)
			_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), lost.ID, "system", nil)
			require.NoError(err)
			var expectedOwner int64
			var ownerA, ownerB int64
			if transfer {
				replacement, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
					LeftKind: IdentityMatchParticipant, LeftID: 1, RightKind: IdentityMatchParticipant, RightID: 3,
					Basis: IdentityMatchStableProviderID, NormalizedValue: new("independent-provider"),
					State: IdentityMatchStateCandidate, Source: ProvenanceSystem,
				})
				require.NoError(err)
				_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), replacement.ID, "system", nil)
				require.NoError(err)
				expectedOwner, ownerA, ownerB = replacement.ID, 2, 3
			} else {
				_, err = st.LinkParticipants(3, 4)
				require.NoError(err)
				_, err = st.LinkParticipants(2, 5)
				require.NoError(err)
				redundant, _, err := st.UpsertIdentityMatchCandidateContext(t.Context(), IdentityMatchCandidateInput{
					LeftKind: IdentityMatchParticipant, LeftID: 4, RightKind: IdentityMatchParticipant, RightID: 5,
					Basis: IdentityMatchStableProviderID, NormalizedValue: new("retained-provider"), State: IdentityMatchStateCandidate, Source: ProvenanceSystem,
				})
				require.NoError(err)
				_, _, err = st.AcceptIdentityMatchCandidateContext(t.Context(), redundant.ID, "system", nil)
				require.NoError(err)
				expectedOwner, ownerA, ownerB = redundant.ID, 4, 5
			}
			before, err := st.IdentityRevisionContext(t.Context())
			require.NoError(err)
			originalBuildAdjacency := buildAdjacency
			t.Cleanup(func() { buildAdjacency = originalBuildAdjacency })
			var traversals int
			buildAdjacency = func(edges []linkEdge) map[int64][]int64 {
				traversals++
				return originalBuildAdjacency(edges)
			}
			observer.all = true
			require.NoError(st.MergeParticipants(1, 2))
			assert.LessOrEqual(traversals, 20, "cleanup must not traverse unrelated accepted matches")
			for _, query := range observer.queries {
				if strings.Contains(query.sql, "SELECT id, left_id, right_id") && strings.Contains(query.sql, "FROM identity_match_candidates") {
					assert.LessOrEqual(query.rows, 3, "cleanup must not load unrelated accepted matches")
				}
			}
			_, err = st.GetIdentityMatchCandidateContext(t.Context(), lost.ID)
			require.ErrorIs(err, ErrIdentityMatchNotFound)
			var owner int64
			require.NoError(st.db.QueryRowContext(t.Context(), `SELECT identity_match_candidate_id FROM participant_links WHERE participant_a = ? AND participant_b = ?`, ownerA, ownerB).Scan(&owner))
			assert.Equal(expectedOwner, owner)
			var unrelated int
			require.NoError(st.db.QueryRowContext(t.Context(), `SELECT count(*) FROM participant_links WHERE participant_a > 6 AND identity_match_candidate_id IS NOT NULL`).Scan(&unrelated))
			assert.Equal(1000, unrelated)
			after, err := st.IdentityRevisionContext(t.Context())
			require.NoError(err)
			assert.Equal(before+2, after, "cleanup and the participant merge each bump the identity revision")
		})
	}
}
