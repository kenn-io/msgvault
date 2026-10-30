package store_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	docbankdocument "go.kenn.io/docbank/document"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/document"
	"go.kenn.io/msgvault/internal/vector/embed"
)

type coldDocumentProvider func(context.Context, []vector.DocumentInput) ([][][]float32, error)

func (f coldDocumentProvider) EmbedDocuments(ctx context.Context, in []vector.DocumentInput) ([][][]float32, error) {
	return f(ctx, in)
}

type coldDocumentBackend struct{ puts int }

func (b *coldDocumentBackend) PutUnpublished(context.Context, document.GenerationID, int, []document.Embedding) error {
	b.puts++
	return nil
}
func (*coldDocumentBackend) DeleteTokens(context.Context, document.GenerationID, []string) error {
	return nil
}
func (*coldDocumentBackend) Search(context.Context, document.GenerationID, int, []float32, int) ([]document.Hit, error) {
	return nil, nil
}

func TestDocumentColdPolicyWorkerKeepsOriginalAuthority(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "policy-worker-normalized", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original", LeaseOwner: "extract", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1,
	}))
	requirements.NoError(err)
	policy, err := docbankdocument.NewNormalizePolicy(10000)
	requirements.NoError(err)
	normalized, err := docbankdocument.NormalizeDocument(docbankdocument.SourceDocument{Family: "pdf", UnitKind: "page", Units: []docbankdocument.SourceUnit{{Index: 0, Markdown: "Synthetic document worker evidence"}}}, policy)
	requirements.NoError(err)
	requirements.Len(normalized.Chunks, 1)
	unit, chunk := normalized.Units[0], normalized.Chunks[0]
	publication := publicationFor(t, claim, chunk.Text, chunk.Checksum)
	publication.ManifestChecksum = normalized.Checksum
	publication.Units[0] = store.DocumentPublishedUnit{Index: unit.Index, Kind: unit.Kind, Text: unit.Text, Checksum: unit.Checksum, CharCount: unit.CharCount, HeadingMarks: unit.HeadingMarks}
	publication.Chunks[0] = store.DocumentPublishedChunk{Key: chunk.Key, Ordinal: chunk.Ordinal, Text: chunk.Text, Checksum: chunk.Checksum, CharCount: chunk.CharCount, HeadingPath: chunk.HeadingPath, Spans: []store.DocumentPublishedSpan{{UnitIndex: chunk.Spans[0].UnitIndex, CharStart: chunk.Spans[0].CharStart, CharEnd: chunk.Spans[0].CharEnd}}}
	requirements.NoError(f.Store.PublishDocumentExtraction(t.Context(), publication))
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("f", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768})
	requirements.NoError(err)
	calls := 0
	provider := coldDocumentProvider(func(ctx context.Context, in []vector.DocumentInput) ([][][]float32, error) {
		calls++
		require.NoError(t, f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Cold))
		require.NoError(t, f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Full))
		out := make([][][]float32, len(in))
		for i, input := range in {
			for range input.Chunks {
				embedding := make([]float32, 768)
				embedding[0] = 1
				out[i] = append(out[i], embedding)
			}
		}
		return out, nil
	})
	backend := &coldDocumentBackend{}
	worker := document.NewWorker(document.WorkerDeps{Ledger: f.Store, Provider: provider, Backend: backend, Owner: "policy-worker", Dimension: 768, MaxInputChars: 1000, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, RetryDelay: time.Minute, MaxAttempts: 3})
	result, err := worker.Run(t.Context(), document.GenerationID(generation.ID), 10)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	assertions.Equal(1, calls)
	assertions.Zero(backend.puts)
	assertions.Zero(result.Published)
}

type mutatePolicyOnNormalizedLoad struct {
	*store.Store

	mutate func()
}

func (l mutatePolicyOnNormalizedLoad) LoadNormalizedDocument(ctx context.Context, extractionID string) (docbankdocument.NormalizedDocument, error) {
	l.mutate()
	return l.Store.LoadNormalizedDocument(ctx, extractionID)
}

func TestDocumentColdPolicyWorkerReleasesClaimOnAdmissionStale(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f := storetest.New(t)
	profile, hash := seedDocumentPublicationAuthority(t, f)
	claim, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "policy-worker-admission", ProfileID: profile.ID, CanonicalBlobHash: hash, ExtractionInputKey: "original", LeaseOwner: "extract", LeaseUntil: time.Now().UTC().Add(time.Minute), LocalBytes: 128, SourceSequence: 1,
	}))
	requirements.NoError(err)
	policy, err := docbankdocument.NewNormalizePolicy(10000)
	requirements.NoError(err)
	normalized, err := docbankdocument.NormalizeDocument(docbankdocument.SourceDocument{Family: "pdf", UnitKind: "page", Units: []docbankdocument.SourceUnit{{Index: 0, Markdown: "Synthetic document worker evidence"}}}, policy)
	requirements.NoError(err)
	unit, chunk := normalized.Units[0], normalized.Chunks[0]
	publication := publicationFor(t, claim, chunk.Text, chunk.Checksum)
	publication.ManifestChecksum = normalized.Checksum
	publication.Units[0] = store.DocumentPublishedUnit{Index: unit.Index, Kind: unit.Kind, Text: unit.Text, Checksum: unit.Checksum, CharCount: unit.CharCount, HeadingMarks: unit.HeadingMarks}
	publication.Chunks[0] = store.DocumentPublishedChunk{Key: chunk.Key, Ordinal: chunk.Ordinal, Text: chunk.Text, Checksum: chunk.Checksum, CharCount: chunk.CharCount, HeadingPath: chunk.HeadingPath, Spans: []store.DocumentPublishedSpan{{UnitIndex: chunk.Spans[0].UnitIndex, CharStart: chunk.Spans[0].CharStart, CharEnd: chunk.Spans[0].CharEnd}}}
	requirements.NoError(f.Store.PublishDocumentExtraction(t.Context(), publication))
	generation, _, err := f.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{Fingerprint: strings.Repeat("f", 64), TargetExtractionProfileID: profile.ID, EmbeddingProfile: "vector.embeddings", Model: "embed-v1", Dimension: 768})
	requirements.NoError(err)
	providerCalls := 0
	provider := coldDocumentProvider(func(context.Context, []vector.DocumentInput) ([][][]float32, error) {
		providerCalls++
		return nil, nil
	})
	backend := &coldDocumentBackend{}
	ledger := mutatePolicyOnNormalizedLoad{Store: f.Store, mutate: func() {
		requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Cold))
		requirements.NoError(f.Store.SetConversationPolicyOverride(t.Context(), f.Source.ID, "default-thread", conversationpolicy.Full))
	}}
	worker := document.NewWorker(document.WorkerDeps{Ledger: ledger, Provider: provider, Backend: backend, Owner: "policy-worker-admission", Dimension: 768, MaxInputChars: 1000, LeaseDuration: time.Minute, HeartbeatInterval: 10 * time.Second, RetryDelay: time.Minute, MaxAttempts: 3})
	result, err := worker.Run(t.Context(), document.GenerationID(generation.ID), 10)
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	assertions.Zero(providerCalls)
	assertions.Zero(backend.puts)
	assertions.Zero(result.Retry)
	assertions.Zero(result.Terminal)
	reclaimed, err := f.Store.ClaimDocumentVectorChunk(t.Context(), generation.ID, 0, 1, "successor", time.Now().UTC(), time.Minute)
	requirements.NoError(err)
	requirements.NotNil(reclaimed, "stale admission must release its pending claim")
	assertions.Equal("successor", reclaimed.LeaseOwner)
}

func TestDocumentColdPolicyHTTPRetryChecksExactPendingInput(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	f, generation := seedDocumentVectorGenerationWithChunks(t, 1)
	ctx, err := f.Store.CaptureConversationPolicyWork(t.Context())
	requirements.NoError(err)
	claim, err := f.Store.ClaimDocumentVectorChunk(ctx, generation.ID, 0, 10, "retry-worker", time.Now().UTC(), time.Minute)
	requirements.NoError(err)
	requirements.NotNil(claim)
	ctx = store.WithConversationPolicyDocumentTokens(ctx, generation.ID, []string{claim.Token})
	// A replacement extraction for the same document publishes during the
	// first HTTP attempt, superseding the head the pending input came from.
	var hash string
	requirements.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`SELECT canonical_blob_hash FROM document_extraction_heads WHERE profile_id=? LIMIT 1`), generation.TargetExtractionProfileID).Scan(&hash))
	replacement, err := f.Store.ClaimDocumentExtraction(t.Context(), documentClaimInputForHash(t, f, store.DocumentExtractionClaimInput{
		ExtractionID: "extraction-replacement", ProfileID: generation.TargetExtractionProfileID,
		CanonicalBlobHash: hash, ExtractionInputKey: "original",
		LeaseOwner: "replacement-worker", LeaseUntil: time.Now().UTC().Add(10 * time.Minute),
		LocalBytes: 128, SourceSequence: 1,
	}))
	requirements.NoError(err)
	publication := publicationFor(t, replacement, "replacement document text", strings.Repeat("e", 64))
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			assert.NoError(t, f.Store.PublishDocumentExtraction(r.Context(), publication))
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	provider := embed.NewVoyageClient(embed.VoyageConfig{Endpoint: server.URL, Model: "synthetic", Dimension: 768, MaxRetries: 2})
	_, err = provider.EmbedDocuments(ctx, []vector.DocumentInput{{Chunks: []string{"synthetic document"}}})
	requirements.ErrorIs(err, store.ErrConversationPolicyStale)
	assertions.Equal(int64(1), requests.Load(), "a superseded extraction prevents retry even without a global revision change")
}
