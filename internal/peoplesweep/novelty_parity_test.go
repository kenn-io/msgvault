package peoplesweep

import (
	quickrand "math/rand"
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"testing/quick"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/novelty"
)

func TestNoveltyRecencyMatchesBriefSelectItems(t *testing.T) {
	random := rand.New(rand.NewPCG(5, 8)) //nolint:gosec // Deterministic synthetic fixtures, not cryptographic randomness.
	config := &quick.Config{
		MaxCount: 500,
		Values: func(args []reflect.Value, _ *quickrand.Rand) {
			var items []EvidenceItem
			for i := range 1 + random.IntN(60) {
				items = append(items, EvidenceItem{Ref: EvidenceRef{SourceID: 1, MessageID: int64(i + 1), SourceLane: SourceConversationText}, EventTime: time.Unix(int64(random.IntN(8)), 0).UTC()})
			}
			sortBriefItemsNewestFirst(items)
			request := BriefWindowRequest{MaxItems: 1 + random.IntN(20), OverlapItems: random.IntN(4)}
			if random.IntN(2) == 0 {
				request.Previous = &BriefBoundary{ThroughEventTime: time.Unix(int64(random.IntN(8)), 0).UTC()}
			}
			var mandatory *EvidenceItem
			if random.IntN(2) == 0 {
				mandatory = &items[random.IntN(len(items))]
			}
			args[0] = reflect.ValueOf(items)
			args[1] = reflect.ValueOf(mandatory)
			args[2] = reflect.ValueOf(request)
		},
	}
	require.NoError(t, quick.Check(func(items []EvidenceItem, mandatory *EvidenceItem, request BriefWindowRequest) bool {
		in := novelty.Input{Params: novelty.DefaultParams(request.MaxItems), OverlapCap: -1}
		if request.Previous != nil {
			in.OverlapCap = request.OverlapItems
		}
		ranks := map[string]int{}
		for rank, item := range items {
			key := packetEvidenceID(item)
			ranks[key] = rank
			in.Candidates = append(in.Candidates, novelty.Candidate{Key: key, MessageID: item.Ref.MessageID, RecencyRank: rank, Mandatory: mandatory != nil && key == packetEvidenceID(*mandatory), OverlapOld: request.Previous != nil && !item.EventTime.After(request.Previous.ThroughEventTime)})
		}
		want := []string{}
		for _, item := range briefSelectItems(items, mandatory, request) {
			want = append(want, packetEvidenceID(item))
		}
		recency, err := novelty.Recency(in)
		require.NoError(t, err, "recency input: %+v", in)
		fallback, err := novelty.Fallback(in, novelty.FallbackLowCoverage)
		require.NoError(t, err, "fallback input: %+v", in)
		for _, got := range []novelty.Result{recency, fallback} {
			slices.SortFunc(got.Picks, func(a, b novelty.Pick) int { return ranks[a.Key] - ranks[b.Key] })
			keys := []string{}
			for _, p := range got.Picks {
				keys = append(keys, p.Key)
			}
			assert.Equal(t, want, keys)
		}
		return !t.Failed()
	}, config))
}
