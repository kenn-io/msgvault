package timeline

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

// The cap selects a prefix of the readers' shared (FirstAt, Key) ordering,
// before the day renderer applies its separate tier/exchange ranking.
func TestMergeBurstsSelectsCombinedReaderOrder(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newTimelineFixture(t)
	var want []string
	for i, cold := range []bool{true, false, true, false} {
		conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("ordered-%d", i), "Ordered")
		requirements.NoError(err)
		at := f.now.Add(time.Duration(i/2) * time.Minute)
		f.add(at, false, func(m *store.Message) { m.ConversationID = conv })
		if cold {
			requirements.NoError(f.seedConversationPolicy(t.Context(), f.Source.ID, fmt.Sprintf("ordered-%d", i)))
		}
		want = append(want, fmt.Sprintf("burst:%d:%d:2026-10-22", f.Source.ID, conv))
	}
	f.project()
	q := store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Filters: f.src.Filters, Limit: 5000}
	warm, err := f.src.Activity.ListActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	cold, err := f.src.Cold.ListColdActivityBurstsContext(t.Context(), q)
	requirements.NoError(err)
	requirements.Len(warm.Bursts, 2)
	requirements.Len(cold.Bursts, 2)
	// Equal-time ties use lexical keys, rather than source preference.
	slices.Sort(want[:2])
	slices.Sort(want[2:])
	merged := mergeBursts(warm.Bursts, cold.Bursts)
	got := []string{}
	for _, b := range merged[:3] {
		got = append(got, b.Key)
	}
	assertions.Equal(want[:3], got)
	// The deduplication path must retain the same selection order.
	merged = mergeBursts(warm.Bursts, append(cold.Bursts, warm.Bursts...))
	requirements.Len(merged, 4)
	assertions.Equal(want[0], merged[0].Key)
}
