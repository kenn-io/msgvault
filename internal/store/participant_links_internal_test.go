package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

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

// TestComponentOfDelegatesToComponentOfAdj proves the refactor preserved
// componentOf's behavior: building adjacency from edges and delegating must
// yield the same set as calling componentOfAdj against a separately built
// adjacency map, for every node in a multi-component graph.
func TestComponentOfDelegatesToComponentOfAdj(t *testing.T) {
	assert := assert.New(t)
	edges := []linkEdge{{1, 2}, {2, 3}, {5, 6}, {10, 11}}
	adj := buildAdjacency(edges)

	for _, id := range []int64{1, 2, 3, 5, 6, 8, 10, 11} {
		assert.Equal(
			setKeys(componentOfAdj(id, adj)),
			setKeys(componentOf(id, edges)),
			"componentOf(%d) must match componentOfAdj(%d)", id, id,
		)
	}
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

// TestClustersFromEdgesIsDeterministic pins that repeated calls on the same
// edge list return identical maps, so cache builds and identity refreshes are
// reproducible.
func TestClustersFromEdgesIsDeterministic(t *testing.T) {
	assert := assert.New(t)
	edges := []linkEdge{{1, 2}, {2, 3}, {5, 6}, {10, 11}}

	first := clustersFromEdges(edges)
	second := clustersFromEdges(edges)
	assert.Equal(first, second)
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
