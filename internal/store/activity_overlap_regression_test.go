package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverlappingMailboxThreadsKeepMailboxConversations(t *testing.T) {
	for _, shape := range []struct {
		name       string
		threads    [][]int
		wantCounts []int
		count      int
	}{
		{"partial overlap", [][]int{{0, 1}, {1, 2}}, []int{2, 1}, 3},
		{"contained thread", [][]int{{0}, {0, 1}}, []int{0, 2}, 2},
		{"divergent replies with incomplete bridge", [][]int{{0, 1}, {0, 2}, {0}}, []int{2, 1, 0}, 3},
	} {
		t.Run(shape.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			f := newTimelineActivityFixture(t)
			q := timelineContactQuery(f)
			at := q.Now.Add(-2 * time.Hour)
			convs := []int64{f.ConvID}
			for i, messages := range shape.threads {
				source, conv := f.Source.ID, f.ConvID
				if i > 0 {
					other, err := f.Store.GetOrCreateSource("gmail", fmt.Sprintf("account-%d@example.com", i))
					require.NoError(err)
					require.NoError(f.Store.AddAccountIdentity(other.ID, "owner@example.com", "manual"))
					source = other.ID
					conv, err = f.Store.EnsureConversation(source, "overlap", "Overlapping thread")
					require.NoError(err)
					convs = append(convs, conv)
				}
				for _, message := range messages {
					f.add(t, conv, source, at.Add(time.Duration(message)*time.Hour), false, fmt.Sprintf("<message-%d@example.com>", message))
				}
			}
			projectTimelineActivity(t, f, "UTC")
			page, err := f.Store.ListActivityBurstsContext(t.Context(), timelineBurstQuery())
			require.NoError(err)
			counts := map[int64]int{}
			seen := map[int64]bool{}
			for _, burst := range page.Bursts {
				assert.Zero(burst.Outbound)
				counts[burst.ConversationID] += burst.Inbound
				for _, id := range burst.LatestMessageIDs {
					assert.False(seen[id])
					seen[id] = true
				}
			}
			rows := 0
			for i, want := range shape.wantCounts {
				assert.Equal(want, counts[convs[i]])
				if want > 0 {
					rows++
				}
			}
			assert.Len(page.Bursts, rows)
			assert.Len(seen, shape.count)
			aggregate, err := f.Store.MessageContactAggregateContext(t.Context(), q)
			require.NoError(err)
			assert.Equal(shape.count, aggregate.Count)
			assert.Equal(1, aggregate.InitiatedByContact)
			assert.Zero(aggregate.InitiatedByOwner)
		})
	}
}
