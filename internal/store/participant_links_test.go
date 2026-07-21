package store_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestLinkParticipantsCreatesEdgeAndBumpsRevision(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	rev, err := f.Store.LinkParticipants(b, a) // reversed order: must normalize
	require.NoError(err)
	assert.Equal(int64(1), rev)

	clusters, err := f.Store.ParticipantClusters()
	require.NoError(err)
	assert.Equal(map[int64]int64{a: min(a, b), b: min(a, b)}, clusters)
}

func TestLinkParticipantsExactEdgeIsIdempotent(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	b := f.EnsureParticipant("alice@personal.example", "Alice P", "personal.example")

	rev1, err := f.Store.LinkParticipants(a, b)
	require.NoError(err)

	rev2, err := f.Store.LinkParticipants(b, a) // reversed, same edge
	require.NoError(err)
	assert.Equal(t, rev1, rev2)
}

func TestLinkParticipantsRejectsSelfAndUnknown(t *testing.T) {
	f := storetest.New(t)
	a := f.EnsureParticipant("alice@example.com", "Alice", "example.com")

	_, err := f.Store.LinkParticipants(a, a)
	require.ErrorIs(t, err, store.ErrInvalidParticipantID)

	_, err = f.Store.LinkParticipants(a, 999999)
	require.ErrorIs(t, err, store.ErrParticipantNotFound)
}

func TestLinkParticipantsRedundantIndirectEdgeIsAlreadyLinked(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")
	b := f.EnsureParticipant("b@example.com", "B", "example.com")
	c := f.EnsureParticipant("c@example.com", "C", "example.com")

	_, err := f.Store.LinkParticipants(a, b)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(b, c)
	require.NoError(err)

	_, err = f.Store.LinkParticipants(a, c)
	assert.ErrorIs(t, err, store.ErrAlreadyLinked)
}

func TestUnlinkParticipantsSplitsClusterDeterministically(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")
	b := f.EnsureParticipant("b@example.com", "B", "example.com")
	c := f.EnsureParticipant("c@example.com", "C", "example.com")

	_, err := f.Store.LinkParticipants(a, b)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(b, c)
	require.NoError(err)

	revBefore, err := f.Store.IdentityRevision()
	require.NoError(err)

	rev, err := f.Store.UnlinkParticipants(b, c)
	require.NoError(err)
	assert.Equal(revBefore+1, rev)

	clusters, err := f.Store.ParticipantClusters()
	require.NoError(err)
	assert.Equal(map[int64]int64{a: min(a, b), b: min(a, b)}, clusters)

	members, err := f.Store.ClusterMembers(c)
	require.NoError(err)
	assert.Equal([]int64{c}, members)
}

func TestUnlinkParticipantsMissingEdgeIsIdempotent(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")
	b := f.EnsureParticipant("b@example.com", "B", "example.com")

	revBefore, err := f.Store.IdentityRevision()
	require.NoError(err)

	rev, err := f.Store.UnlinkParticipants(a, b)
	require.NoError(err)
	assert.Equal(t, revBefore, rev)
}

// TestUnlinkParticipantsRejectsUnknown covers Finding 2: unlinking a pair
// where one ID does not exist must fail the same way LinkParticipants does
// (ErrParticipantNotFound, 400 at the API layer) instead of silently
// succeeding as a no-op, and must not bump the identity revision.
func TestUnlinkParticipantsRejectsUnknown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")

	revBefore, err := f.Store.IdentityRevision()
	require.NoError(err)

	_, err = f.Store.UnlinkParticipants(a, 999999)
	require.ErrorIs(err, store.ErrParticipantNotFound)

	revAfter, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revBefore, revAfter, "rejected unlink must not bump the identity revision")
}

func TestClusterMembersForUnlinkedParticipant(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")

	members, err := f.Store.ClusterMembers(a)
	require.NoError(err)
	assert.Equal(t, []int64{a}, members)
}

// TestClusterEdgesFiltersToTheRequestedComponent covers the seam
// internal/api/people.go relies on to render a person detail's cluster
// block: ClusterEdges must return exactly the edges within id's own
// component, excluding a wholly disjoint cluster's edges even though both
// clusters share one participant_links table.
func TestClusterEdgesFiltersToTheRequestedComponent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")
	b := f.EnsureParticipant("b@example.com", "B", "example.com")
	c := f.EnsureParticipant("c@example.com", "C", "example.com")
	d := f.EnsureParticipant("d@example.com", "D", "example.com")
	e := f.EnsureParticipant("e@example.com", "E", "example.com")

	_, err := f.Store.LinkParticipants(a, b)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(b, c)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(d, e)
	require.NoError(err)

	loA, hiA := normalizeEdgeForTest(a, b)
	loB, hiB := normalizeEdgeForTest(b, c)
	edges, err := f.Store.ClusterEdges(a)
	require.NoError(err)
	assert.ElementsMatch([]store.LinkEdge{{A: loA, B: hiA}, {A: loB, B: hiB}}, edges,
		"must include every edge in a's component, and only those")

	unlinked := f.EnsureParticipant("f@example.com", "F", "example.com")
	empty, err := f.Store.ClusterEdges(unlinked)
	require.NoError(err)
	assert.Empty(empty, "an unlinked participant has no cluster edges")
}

// normalizeEdgeForTest mirrors the unexported normalizeEdge in
// participant_links.go (participant_a < participant_b), since this test is
// in package store_test and cannot call it directly.
func normalizeEdgeForTest(a, b int64) (int64, int64) {
	if a > b {
		return b, a
	}
	return a, b
}

// TestMergeParticipantsRewritesLinkEdges covers the simple case: the loser
// of a merge has a link edge to a third participant. After the merge, that
// edge must be repointed onto the winner instead of being dropped by the
// participant_links ON DELETE CASCADE, and both the identity revision and
// the account-identity revision must bump: the merge repoints
// messages.sender_id, which can leave a stale is_from_me baked into message
// Parquet shards, so the staleness check must force a full rebuild (not the
// cheap identity-only refresh) — see cache_staleness.go.
func TestMergeParticipantsRewritesLinkEdges(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com") // a < b < c
	b := f.EnsureParticipant("b@example.com", "B", "example.com")
	c := f.EnsureParticipant("c@example.com", "C", "example.com")

	_, err := f.Store.LinkParticipants(b, c)
	require.NoError(err)

	revBefore, err := f.Store.IdentityRevision()
	require.NoError(err)
	acctRevBefore, err := f.Store.AccountIdentityRevision()
	require.NoError(err)

	require.NoError(f.Store.MergeParticipants(b, a)) // b (loser) merges into a (winner)

	revAfter, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revBefore+1, revAfter, "identity revision must bump when a merge touches links")
	acctRevAfter, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(acctRevBefore+1, acctRevAfter, "account identity revision must bump on every merge")

	clusters, err := f.Store.ParticipantClusters()
	require.NoError(err)
	assert.Equal(map[int64]int64{a: a, c: a}, clusters, "edge must repoint from b to a")

	var loserCount int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT COUNT(*) FROM participants WHERE id = ?`), b).
		Scan(&loserCount))
	assert.Equal(0, loserCount, "merged-away participant must be gone")
}

// TestMergeParticipantsPathContractionKeepsForest covers the case the spec
// calls out explicitly: contracting the endpoints of a path can create a
// cycle. Links a-x, x-y, y-b form a path; merging b into a contracts the
// path's endpoints together, which would yield cycle a-x-y-a if the edges
// were merely repointed without rebuilding the cluster as a star.
func TestMergeParticipantsPathContractionKeepsForest(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com") // a < x < y < b
	x := f.EnsureParticipant("x@example.com", "X", "example.com")
	y := f.EnsureParticipant("y@example.com", "Y", "example.com")
	b := f.EnsureParticipant("b@example.com", "B", "example.com")

	_, err := f.Store.LinkParticipants(a, x)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(x, y)
	require.NoError(err)
	_, err = f.Store.LinkParticipants(y, b)
	require.NoError(err)

	require.NoError(f.Store.MergeParticipants(b, a)) // b (loser) merges into a (winner)

	// Cluster {a, x, y} must remain intact and rooted at a (the smallest
	// surviving member).
	members, err := f.Store.ClusterMembers(a)
	require.NoError(err)
	assert.Equal([]int64{a, x, y}, members)

	// Exactly 2 edges, forming a star: no self-edges, no duplicate edges.
	var edgeCount int
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(
		`SELECT COUNT(*) FROM participant_links WHERE participant_a IN (?, ?, ?) OR participant_b IN (?, ?, ?)`),
		a, x, y, a, x, y).Scan(&edgeCount))
	assert.Equal(2, edgeCount, "3-node cluster must have exactly 2 edges (a star, not a cycle)")

	// Unlinking any single edge splits exactly one member off.
	_, err = f.Store.UnlinkParticipants(a, x)
	require.NoError(err)
	xMembers, err := f.Store.ClusterMembers(x)
	require.NoError(err)
	assert.Equal([]int64{x}, xMembers, "unlinking a-x must split x off on its own")
	aMembers, err := f.Store.ClusterMembers(a)
	require.NoError(err)
	assert.Equal([]int64{a, y}, aMembers, "unlinking a-x must leave a and y still linked")
}

// TestMergeParticipantsWithoutLinksStillBumpsRevisionButRewritesNoLinks
// covers the common case: neither side of the merge has ever been linked.
// The merge must still bump the identity revision unconditionally (a merge
// can change owner_participants' content even without touching the link
// graph, per Finding 2 of the relationships-backend review), but since
// there was no link edge to repoint, participant_links must remain empty.
// It must also bump the account-identity revision unconditionally: the
// merge repoints messages.sender_id regardless of whether a link edge
// existed, so a stale is_from_me can only be repaired by a full rebuild.
func TestMergeParticipantsWithoutLinksStillBumpsRevisionButRewritesNoLinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	f := storetest.New(t)
	a := f.EnsureParticipant("a@example.com", "A", "example.com")
	b := f.EnsureParticipant("b@example.com", "B", "example.com")

	revBefore, err := f.Store.IdentityRevision()
	require.NoError(err)
	acctRevBefore, err := f.Store.AccountIdentityRevision()
	require.NoError(err)

	require.NoError(f.Store.MergeParticipants(b, a))

	revAfter, err := f.Store.IdentityRevision()
	require.NoError(err)
	assert.Equal(revBefore+1, revAfter, "merge must bump the identity revision unconditionally")
	acctRevAfter, err := f.Store.AccountIdentityRevision()
	require.NoError(err)
	assert.Equal(acctRevBefore+1, acctRevAfter, "merge must bump the account identity revision unconditionally")

	var edgeCount int
	require.NoError(f.Store.DB().QueryRow(`SELECT COUNT(*) FROM participant_links`).Scan(&edgeCount))
	assert.Equal(0, edgeCount, "merge without pre-existing links must not create any link edge")
}

// TestLinkParticipants_ConcurrentDisjointClusters covers the race that
// lockIdentityMutationTx serializes against: two LinkParticipants calls
// that each try to connect two already-linked, previously-disjoint
// clusters (e.g. link(b,c) racing link(a,d) where {a,b} and {c,d} already
// exist). Without a lock taken before the edge snapshot read, both calls
// could see the pre-merge snapshot, both pass the connectivity check, and
// both commit a new edge — producing a cycle and breaking the
// participant_links forest invariant. With the lock, exactly one call
// merges the clusters and the other observes the merge and returns
// ErrAlreadyLinked.
//
// This runs against the file-backed store from storetest.New (real
// separate connections, real SQLite/PostgreSQL locking), following the
// pattern of TestEnsureParticipant_Concurrent.
func TestLinkParticipants_ConcurrentDisjointClusters(t *testing.T) {
	require := require.New(t)
	f := storetest.New(t)

	const groups = 10
	type group struct{ a, b, c, d int64 }
	groupIDs := make([]group, groups)
	for i := range groups {
		a := f.EnsureParticipant(fmt.Sprintf("a%d@example.com", i), "A", "example.com")
		b := f.EnsureParticipant(fmt.Sprintf("b%d@example.com", i), "B", "example.com")
		c := f.EnsureParticipant(fmt.Sprintf("c%d@example.com", i), "C", "example.com")
		d := f.EnsureParticipant(fmt.Sprintf("d%d@example.com", i), "D", "example.com")
		_, err := f.Store.LinkParticipants(a, b)
		require.NoError(err)
		_, err = f.Store.LinkParticipants(c, d)
		require.NoError(err)
		groupIDs[i] = group{a: a, b: b, c: c, d: d}
	}

	var wg sync.WaitGroup
	errs1 := make([]error, groups)
	errs2 := make([]error, groups)
	for i, g := range groupIDs {
		wg.Add(2)
		go func(idx int, g group) {
			defer wg.Done()
			_, err := f.Store.LinkParticipants(g.b, g.c)
			errs1[idx] = err
		}(i, g)
		go func(idx int, g group) {
			defer wg.Done()
			_, err := f.Store.LinkParticipants(g.a, g.d)
			errs2[idx] = err
		}(i, g)
	}
	wg.Wait()

	for i, g := range groupIDs {
		// Exactly one of the two merge attempts must succeed and the
		// other must observe the merge as redundant. Two successes
		// would mean both edges landed, creating a 4-cycle.
		succeeded := 0
		for _, err := range []error{errs1[i], errs2[i]} {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, store.ErrAlreadyLinked):
				// expected outcome for the loser
			default:
				require.NoError(err, "group %d: unexpected error", i)
			}
		}
		assert.Equal(t, 1, succeeded, "group %d: exactly one link call should succeed", i)

		members, err := f.Store.ClusterMembers(g.a)
		require.NoError(err)
		assert.ElementsMatch(t, []int64{g.a, g.b, g.c, g.d}, members,
			"group %d: all four participants must end up in one cluster", i)

		var edgeCount int
		require.NoError(f.Store.DB().QueryRow(
			f.Store.Rebind(`SELECT COUNT(*) FROM participant_links
				WHERE participant_a IN (?, ?, ?, ?) AND participant_b IN (?, ?, ?, ?)`),
			g.a, g.b, g.c, g.d, g.a, g.b, g.c, g.d,
		).Scan(&edgeCount), "count group %d edges", i)
		assert.Equal(t, 3, edgeCount, "group %d: 4-node cluster must have exactly 3 edges (a tree, not a cycle)", i)
	}
}
