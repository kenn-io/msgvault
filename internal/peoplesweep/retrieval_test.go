package peoplesweep

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personfacts"
)

type retrievalArchive struct {
	candidates  []int64
	lexical     []EvidenceItem
	hydrated    map[int64]EvidenceItem
	documents   []EvidenceItem
	docRequests chan DocumentContextRequest
}

func (f retrievalArchive) ListPersonSweepHistoricalCandidates(context.Context, HistoricalCandidateRequest) ([]int64, error) {
	return f.candidates, nil
}
func (f retrievalArchive) SearchPersonSweepMessages(context.Context, ContextRequest) ([]EvidenceItem, error) {
	return f.lexical, nil
}
func (f retrievalArchive) HydratePersonSweepMessages(_ context.Context, _ int64, ids []int64) ([]EvidenceItem, error) {
	items := make([]EvidenceItem, 0, len(ids))
	for _, id := range ids {
		if item, ok := f.hydrated[id]; ok {
			items = append(items, item)
		}
	}
	return items, nil
}
func (f retrievalArchive) SearchPersonSweepDocuments(_ context.Context, request DocumentContextRequest) ([]EvidenceItem, error) {
	if f.docRequests != nil {
		f.docRequests <- request
	}
	return f.documents, nil
}

func retrievalItem(id int64, lane SourceClass, chunk string) EvidenceItem {
	ref := EvidenceRef{SourceLane: lane, SourceID: 1, MessageID: id, ChunkKey: chunk}
	if lane == SourceDocumentText {
		ref.AttachmentID, ref.OccurrenceKey = 10+id, "occ"
	}
	return EvidenceItem{PersonID: 9, SourceClass: lane, Ref: ref}
}

func TestPersonSweepContextRetrieverCombinesLexicalAndDocumentResults(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	one := retrievalItem(1, SourceConversationText, "")
	two := retrievalItem(2, SourceConversationText, "")
	three := retrievalItem(3, SourceConversationText, "")
	doc := retrievalItem(1, SourceDocumentText, "doc-1")
	archive := retrievalArchive{
		candidates: []int64{1, 2, 3}, lexical: []EvidenceItem{one, two},
		hydrated: map[int64]EvidenceItem{2: two, 3: three}, documents: []EvidenceItem{doc},
	}
	retriever := NewContextRetriever(archive)

	got, err := retriever.RetrievePersonSweepContext(t.Context(), ContextRequest{
		PersonID: 9, CandidateMessageIDs: []int64{1, 2, 3}, Limit: 4,
		Target: personfacts.TargetDescriptor{Description: "role history"},
	})
	requirements.NoError(err)
	requirements.Len(got, 3)
	checks.Equal([]int64{1, 2, 1}, []int64{
		got[0].Ref.MessageID, got[1].Ref.MessageID, got[2].Ref.MessageID,
	})
	checks.Equal(SourceDocumentText, got[2].SourceClass)
}

func TestPersonSweepContextRetrieverRestrictsDocumentsToMessageCandidates(t *testing.T) {
	requests := make(chan DocumentContextRequest, 1)
	retriever := NewContextRetriever(retrievalArchive{
		candidates: []int64{1, 2}, docRequests: requests,
		documents: []EvidenceItem{retrievalItem(1, SourceDocumentText, "doc-1")},
	})

	_, err := retriever.RetrievePersonSweepContext(t.Context(), ContextRequest{
		PersonID: 9, Limit: 4, Target: personfacts.TargetDescriptor{Description: "role history"},
	})
	require.NoError(t, err)
	assert.Equal(t, []int64{1, 2}, (<-requests).CandidateMessageIDs)
}

func TestPersonSweepContextRetrieverDeduplicatesEachSignalBeforeFusion(t *testing.T) {
	one := retrievalItem(1, SourceConversationText, "")
	two := retrievalItem(2, SourceConversationText, "")
	three := retrievalItem(3, SourceConversationText, "")
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

func TestPersonSweepContextRetrieverRejectsCrossPersonArchiveResults(t *testing.T) {
	tests := []struct {
		name    string
		archive retrievalArchive
	}{
		{
			name: "lexical",
			archive: retrievalArchive{candidates: []int64{1}, lexical: []EvidenceItem{
				retrievalItem(1, SourceConversationText, ""),
				func() EvidenceItem {
					item := retrievalItem(1, SourceConversationText, "")
					item.PersonID = 10
					return item
				}(),
			}},
		},
		{
			name: "document",
			archive: retrievalArchive{candidates: []int64{1}, documents: []EvidenceItem{
				func() EvidenceItem {
					item := retrievalItem(1, SourceDocumentText, "chunk")
					item.PersonID = 10
					return item
				}(),
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			retriever := NewContextRetriever(test.archive)
			_, err := retriever.RetrievePersonSweepContext(t.Context(), ContextRequest{
				PersonID: 9, CandidateMessageIDs: []int64{1}, Limit: 3,
				Target: personfacts.TargetDescriptor{Description: "role history"},
			})
			assert.ErrorContains(t, err, "person")
		})
	}
}
