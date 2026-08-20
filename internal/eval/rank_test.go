package eval

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTruncateKeys pins the second half of the collapse-then-truncate rule.
func TestTruncateKeys(t *testing.T) {
	assert := assert.New(t)
	keys := []string{"a", "b", "c"}
	assert.Equal([]string{"a", "b"}, TruncateKeys(keys, 2))
	assert.Equal(keys, TruncateKeys(keys, 3))
	assert.Equal(keys, TruncateKeys(keys, 10), "a short list is not padded")
	assert.Empty(TruncateKeys(keys, 0))
	assert.Empty(TruncateKeys(keys, -1))
	assert.Empty(TruncateKeys(nil, 5))
}

// TestDedupeThenTruncate_Order is the ordering regression. Retrieving n
// messages and *then* collapsing them yields however many distinct threads
// happen to sit inside those n — here 2 — which is not what "-n 4" claims.
// Collapsing first and truncating after is what makes the depth mean what the
// metric labels say it means.
func TestDedupeThenTruncate_Order(t *testing.T) {
	// Four threads, three messages each, interleaved the way a real ranking
	// interleaves them: the first four raw hits cover only two threads.
	raw := []string{
		"t1", "t1", "t1",
		"t2", "t2", "t2",
		"t3", "t3", "t3",
		"t4", "t4", "t4",
	}
	const want = 4

	truncateFirst := DedupeKeys(TruncateKeys(raw, want))
	assert.Equal(t, []string{"t1", "t2"}, truncateFirst,
		"truncating the raw list first caps the result at the threads inside it")

	dedupeFirst := TruncateKeys(DedupeKeys(raw), want)
	assert.Equal(t, []string{"t1", "t2", "t3", "t4"}, dedupeFirst,
		"collapsing first yields the requested number of DISTINCT threads")
}

// TestOverFetchPlan_NonCollapsingKey: with a doc-key that is 1:1 with hits
// there is nothing to collapse, so the plan must not over-fetch. This command
// reports latency, and padding every query would inflate it for no gain.
func TestOverFetchPlan_NonCollapsingKey(t *testing.T) {
	assert.Equal(t, []int{100}, OverFetchPlan(100, false))
	assert.Equal(t, []int{1}, OverFetchPlan(1, false))
}

// TestOverFetchPlan_CollapsingKey: the plan grows geometrically and stops at
// the documented ceiling, so a query that cannot fill the depth costs a
// bounded amount of work.
func TestOverFetchPlan_CollapsingKey(t *testing.T) {
	assert := assert.New(t)
	assert.Equal([]int{400, 1600, 6400}, OverFetchPlan(100, true))
	assert.Equal([]int{4, 16, 64}, OverFetchPlan(1, true))

	plan := OverFetchPlan(10, true)
	require.NotEmpty(t, plan)
	assert.Equal(10*OverFetchFactor, plan[0], "the first attempt already over-fetches")
	assert.Equal(10*MaxOverFetchFactor, plan[len(plan)-1], "the last attempt is the documented ceiling")
	for i := 1; i < len(plan); i++ {
		assert.Greater(plan[i], plan[i-1], "each attempt must ask for strictly more")
	}
}

// TestOverFetchPlan_Bounded keeps the plan sane for degenerate depths: never
// empty, never smaller than the requested depth, and never overflowing the
// multiplication on an absurd --limit.
func TestOverFetchPlan_Bounded(t *testing.T) {
	for _, limit := range []int{0, 1, 7, 100, 1 << 19, 1 << 21, 1 << 40} {
		for _, collapses := range []bool{false, true} {
			plan := OverFetchPlan(limit, collapses)
			name := fmt.Sprintf("limit=%d collapses=%v", limit, collapses)
			require.NotEmpty(t, plan, name)
			for _, n := range plan {
				assert.GreaterOrEqual(t, n, limit, name+": a fetch is never shallower than the depth asked for")
			}
		}
	}
}
