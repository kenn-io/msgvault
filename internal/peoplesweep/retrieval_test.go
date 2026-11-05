package peoplesweep

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

type retrievalArchive struct {
	candidates []int64
	lexical    []EvidenceItem
	hydrated   map[int64]EvidenceItem
}

func (f retrievalArchive) ListPersonSweepHistoricalCandidates(context.Context, HistoricalCandidateRequest) ([]int64, error) {
	return f.candidates, nil
}
func (f retrievalArchive) SearchPersonSweepMessages(context.Context, ContextRequest) ([]EvidenceItem, error) {
	return f.lexical, nil
}
func (f retrievalArchive) HydratePersonSweepMessages(_ context.Context, _ int64, ids []int64, _ int64) ([]EvidenceItem, error) {
	items := make([]EvidenceItem, 0, len(ids))
	for _, id := range ids {
		if item, ok := f.hydrated[id]; ok {
			items = append(items, item)
		}
	}
	return items, nil
}

func TestPersonSweepContextRetrieverDeduplicatesEachSignalBeforeFusion(t *testing.T) {
	one := retrievalItem(1)
	two := retrievalItem(2)
	three := retrievalItem(3)
	archive := retrievalArchive{
		candidates: []int64{1, 2, 3},
		lexical:    []EvidenceItem{one, one, two},
		hydrated:   map[int64]EvidenceItem{2: two, 3: three},
	}
	retriever := NewContextRetriever(archive)

	got, err := retriever.RetrievePersonSweepContext(t.Context(), ContextRequest{
		PersonID: 9, CandidateMessageIDs: []int64{1, 2, 3}, Limit: 3,
		Target: personfacts.TargetDescriptor{Description: "role history"},
	})
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []int64{1, 2}, []int64{
		got[0].Ref.MessageID, got[1].Ref.MessageID,
	})
}

func retrievalItem(id int64) EvidenceItem {
	return EvidenceItem{PersonID: 9, SourceClass: SourceConversationText, Ref: EvidenceRef{SourceLane: SourceConversationText, SourceID: 1, MessageID: id}}
}

func TestPersonSweepContextRetrieverRejectsCrossPersonArchiveResults(t *testing.T) {
	other := retrievalItem(1)
	other.PersonID = 10
	retriever := NewContextRetriever(retrievalArchive{
		candidates: []int64{1}, lexical: []EvidenceItem{retrievalItem(1), other},
	})
	_, err := retriever.RetrievePersonSweepContext(t.Context(), ContextRequest{
		PersonID: 9, CandidateMessageIDs: []int64{1}, Limit: 3,
		Target: personfacts.TargetDescriptor{Description: "role history"},
	})
	assert.ErrorContains(t, err, "person")
}
