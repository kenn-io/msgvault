package document

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"go.kenn.io/msgvault/internal/vector"
)

func TestSearchServiceRejectsCandidateLimitAboveBound(t *testing.T) {
	service := NewSearchService(SearchDeps{})

	_, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query:          "synthetic query",
		SearchMode:     string(SearchModeLexical),
		CandidateLimit: 1001,
	})
	require.ErrorIs(t, err, store.ErrDocumentSearchInvalidRequest)
}

func TestSearchServiceDefaultsCandidateLimitToSharedContract(t *testing.T) {
	fixture := seedSemanticSearch(t, "default candidate evidence")
	embedder := &searchQueryEmbedder{vector: []float32{1, 0, 0}}
	backend := &searchBackend{hits: []Hit{{Token: fixture.claims[0].Token, Score: .9, Rank: 1}}}
	service := NewSearchService(SearchDeps{
		Ledger: fixture.store.Store, Embedder: embedder, Backend: backend,
	})

	_, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "candidate", SearchMode: string(SearchModeSemantic),
	})
	require.NoError(t, err)
	require.Len(t, backend.searches, 1)
	assert.Equal(t, store.DefaultDocumentSearchCandidateLimit, backend.searches[0].k)
}

func TestSearchServiceAutoFallsBackToLexicalWhenSemanticCapabilityIsAbsent(t *testing.T) {
	fixture := storetest.New(t)
	service := NewSearchService(SearchDeps{Ledger: fixture.Store})

	response, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "synthetic query", SearchMode: string(SearchModeAuto),
	})

	require.NoError(t, err)
	assert.Equal(t, string(SearchModeLexical), response.EffectiveMode)
	assert.Empty(t, response.Results)
}

func TestSearchServiceExplicitSemanticDoesNotMasqueradeAsLexical(t *testing.T) {
	fixture := storetest.New(t)
	service := NewSearchService(SearchDeps{Ledger: fixture.Store})

	for _, mode := range []SearchMode{SearchModeSemantic, SearchModeHybrid} {
		_, err := service.Search(t.Context(), store.DocumentSearchRequest{
			Query: "synthetic query", SearchMode: string(mode),
		})
		require.ErrorIs(t, err, ErrSemanticSearchUnavailable)
		assert.NotErrorIs(t, err, store.ErrDocumentSearchUnavailable)
	}
}

func TestSearchServiceSemanticReturnsAuthoritativeOccurrenceProvenance(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := seedSemanticSearch(t, "alpha evidence", "beta evidence")
	embedder := &searchQueryEmbedder{vector: []float32{1, 0, 0}}
	backend := &searchBackend{hits: []Hit{{Token: fixture.claims[1].Token, Score: .75, Rank: 1}}}
	service := NewSearchService(SearchDeps{Ledger: fixture.store.Store, Embedder: embedder, Backend: backend})

	response, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "  FIND beta  ", SearchMode: string(SearchModeSemantic), CandidateLimit: 10,
	})

	requirements.NoError(err)
	requirements.Len(response.Results, 1)
	result := response.Results[0]
	assertions.Equal(string(SearchModeSemantic), response.EffectiveMode)
	assertions.Equal(fixture.generation.ID, response.VectorGenerationID)
	assertions.Equal(fixture.generation.Fingerprint, response.VectorGenerationFingerprint)
	assertions.Equal(fixture.claims[1].Token, result.VectorToken)
	assertions.Equal(fixture.claims[1].ChunkKey, result.ChunkKey)
	assertions.Equal(fixture.claims[1].ChunkOrdinal, result.ChunkOrdinal)
	assertions.Equal(fixture.claims[1].ExtractionID, result.ExtractionID)
	assertions.Equal(fixture.claims[1].ExtractionProfileID, result.ProfileID)
	assertions.Equal(fixture.generation.EmbeddingProfile, result.VectorEmbeddingProfile)
	assertions.Equal(fixture.generation.Model, result.VectorModel)
	assertions.Equal(fixture.generation.Dimension, result.VectorDimension)
	assertions.Equal(1, result.SemanticRank)
	assertions.Equal(1, result.Rank)
	assertions.Equal([]string{"semantic"}, result.MatchedSignals)
	assertions.Equal([]string{"find beta"}, embedder.queries)
	requirements.Len(backend.searches, 1)
	assertions.Equal(fixture.generation.ID, int64(backend.searches[0].generationID))
	assertions.Equal(3, backend.searches[0].dimension)
	assertions.Equal([]float32{1, 0, 0}, backend.searches[0].query)
	assertions.Equal(10, backend.searches[0].k)
}

func TestSearchServiceAutoKeepsQueryLocalWhenSemanticCapabilityIsReady(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := seedSemanticSearch(t, "nebula evidence")
	embedder := &searchQueryEmbedder{vector: []float32{1, 0, 0}}
	backend := &searchBackend{hits: []Hit{{Token: fixture.claims[0].Token, Score: .9, Rank: 1}}}
	service := NewSearchService(SearchDeps{Ledger: fixture.store.Store, Embedder: embedder, Backend: backend})

	for _, mode := range []string{"", string(SearchModeAuto)} {
		response, err := service.Search(t.Context(), store.DocumentSearchRequest{
			Query: "nebula", SearchMode: mode, CandidateLimit: 10,
		})

		requirements.NoError(err)
		requirements.Len(response.Results, 1)
		assertions.Equal(string(SearchModeLexical), response.EffectiveMode)
		assertions.Equal([]string{"content"}, response.Results[0].MatchedSignals)
	}
	assertions.Empty(embedder.queries)
	assertions.Empty(backend.searches)
}

func TestSearchServiceExplicitLexicalPreservesStoreRankingWithoutProviderWork(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := seedSemanticSearch(t, "nebula evidence")
	copyMessageID := fixture.store.CreateMessage("lexical-ranking-copy")
	copyAttachmentID := addSemanticSearchAttachment(
		t, fixture.store, copyMessageID, fixture.claims[0].CanonicalBlobHash, "nebula-copy.pdf", "provider:lexical-copy",
	)
	_, eligible, err := fixture.store.Store.ReconcileDocumentOccurrence(t.Context(), copyAttachmentID, 2)
	requirements.NoError(err)
	requirements.True(eligible)
	direct, err := fixture.store.Store.SearchDocuments(t.Context(), store.DocumentSearchRequest{Query: "nebula"})
	requirements.NoError(err)
	embedder := &searchQueryEmbedder{vector: []float32{1, 0, 0}}
	backend := &searchBackend{hits: []Hit{{Token: fixture.claims[0].Token, Score: .9, Rank: 1}}}
	service := NewSearchService(SearchDeps{Ledger: fixture.store.Store, Embedder: embedder, Backend: backend})

	response, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", SearchMode: string(SearchModeLexical), CandidateLimit: 10,
	})

	requirements.NoError(err)
	requirements.Len(response.Results, len(direct.Results))
	for index := range direct.Results {
		assertions.Equal(direct.Results[index].OccurrenceKey, response.Results[index].OccurrenceKey)
		assertions.Equal(direct.Results[index].MatchedSignals, response.Results[index].MatchedSignals)
		assertions.Equal(direct.Results[index].Rank, response.Results[index].Rank)
	}
	assertions.Empty(embedder.queries)
	assertions.Empty(backend.searches)
}

func TestSearchServiceExplicitSemanticModesDoNotFallbackAfterProviderAttempt(t *testing.T) {
	fixture := seedSemanticSearch(t, "nebula evidence")
	providerErr := errors.New("synthetic provider unavailable")
	service := NewSearchService(SearchDeps{
		Ledger: fixture.store.Store, Embedder: &searchQueryEmbedder{err: providerErr}, Backend: &searchBackend{},
	})

	_, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", SearchMode: string(SearchModeHybrid), CandidateLimit: 10,
	})
	require.ErrorIs(t, err, providerErr)

	backendErr := errors.New("synthetic backend unavailable")
	service = NewSearchService(SearchDeps{
		Ledger: fixture.store.Store, Embedder: &searchQueryEmbedder{vector: []float32{1, 0, 0}},
		Backend: &searchBackend{err: backendErr},
	})
	_, err = service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "nebula", SearchMode: string(SearchModeSemantic), CandidateLimit: 10,
	})
	require.ErrorIs(t, err, backendErr)
}

func TestSearchServiceRejectsInvalidQueryVectorBeforeBackendWork(t *testing.T) {
	fixture := seedSemanticSearch(t, "vector evidence")
	tests := []struct {
		name    string
		vector  []float32
		wantErr error
	}{
		{name: "dimension", vector: []float32{1, 0}, wantErr: vector.ErrDimensionMismatch},
		{name: "zero norm", vector: []float32{0, 0, 0}, wantErr: ErrInvalidVector},
		{name: "nonfinite", vector: []float32{1, float32(math.NaN()), 0}, wantErr: ErrInvalidVector},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backend := &searchBackend{}
			service := NewSearchService(SearchDeps{
				Ledger: fixture.store.Store, Embedder: &searchQueryEmbedder{vector: test.vector}, Backend: backend,
			})
			_, err := service.Search(t.Context(), store.DocumentSearchRequest{
				Query: "vector", SearchMode: string(SearchModeSemantic), CandidateLimit: 10,
			})
			require.ErrorIs(t, err, test.wantErr)
			assert.Empty(t, backend.searches)
		})
	}
}

func TestSearchServiceCursorBindsFixedCandidatesAndChecksRevisionBeforeEmbedding(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := seedSemanticSearch(t, "cursor evidence")
	copyMessageID := fixture.store.CreateMessage("semantic-cursor-copy")
	copyAttachmentID := addSemanticSearchAttachment(
		t, fixture.store, copyMessageID, fixture.claims[0].CanonicalBlobHash, "cursor-copy.pdf", "provider:cursor-copy",
	)
	_, eligible, err := fixture.store.Store.ReconcileDocumentOccurrence(t.Context(), copyAttachmentID, 2)
	requirements.NoError(err)
	requirements.True(eligible)
	embedder := &searchQueryEmbedder{vector: []float32{1, 0, 0}}
	backend := &searchBackend{hits: []Hit{{Token: fixture.claims[0].Token, Score: .9, Rank: 1}}}
	service := NewSearchService(SearchDeps{Ledger: fixture.store.Store, Embedder: embedder, Backend: backend})
	request := store.DocumentSearchRequest{
		Query: "cursor", SearchMode: string(SearchModeSemantic), CandidateLimit: 10, PageSize: 1,
	}

	first, err := service.Search(t.Context(), request)
	requirements.NoError(err)
	requirements.Len(first.Results, 1)
	requirements.NotEmpty(first.NextCursor)
	providerCalls := len(embedder.queries)
	_, err = service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "different", SearchMode: string(SearchModeSemantic), CandidateLimit: 10,
		PageSize: 1, Cursor: first.NextCursor,
	})
	requirements.ErrorIs(err, store.ErrDocumentSearchInvalidCursor)
	assertions.Len(embedder.queries, providerCalls, "mismatched request must fail before provider work")

	request.Cursor = first.NextCursor
	second, err := service.Search(t.Context(), request)
	requirements.NoError(err)
	requirements.Len(second.Results, 1)
	assertions.NotEqual(first.Results[0].OccurrenceKey, second.Results[0].OccurrenceKey)
	assertions.Equal(2, second.Results[0].Rank)
	assertions.Empty(second.NextCursor)

	request.Cursor = first.NextCursor
	backend.hits[0].Score = .7
	_, err = service.Search(t.Context(), request)
	requirements.ErrorIs(err, store.ErrDocumentSearchCursorStale)

	backend.hits[0].Score = .9
	fresh, err := service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "cursor", SearchMode: string(SearchModeSemantic), CandidateLimit: 10, PageSize: 1,
	})
	requirements.NoError(err)
	providerCalls = len(embedder.queries)
	thirdMessageID := fixture.store.CreateMessage("semantic-cursor-revision")
	thirdAttachmentID := addSemanticSearchAttachment(
		t, fixture.store, thirdMessageID, fixture.claims[0].CanonicalBlobHash, "third.pdf", "provider:third",
	)
	_, eligible, err = fixture.store.Store.ReconcileDocumentOccurrence(t.Context(), thirdAttachmentID, 3)
	requirements.NoError(err)
	requirements.True(eligible)
	_, err = service.Search(t.Context(), store.DocumentSearchRequest{
		Query: "cursor", SearchMode: string(SearchModeSemantic), CandidateLimit: 10,
		PageSize: 1, Cursor: fresh.NextCursor,
	})
	requirements.ErrorIs(err, store.ErrDocumentSearchCursorStale)
	assertions.Len(embedder.queries, providerCalls, "stale revision must fail before provider work")
}

func TestSearchCursorRejectsOffsetBeyondCandidateSet(t *testing.T) {
	cursor, err := encodeSearchCursor(searchCursor{
		Version: searchCursorVersion, RequestHash: strings.Repeat("a", 64), Revision: 1,
		EffectiveMode: string(SearchModeLexical), CandidateLimit: 2,
		CandidateDigest: strings.Repeat("b", 64), Offset: 3,
	})
	require.NoError(t, err)

	_, _, err = validateSearchCursor(cursor, strings.Repeat("a", 64), 1, SearchModeLexical, 0, "", 2)
	require.ErrorIs(t, err, store.ErrDocumentSearchInvalidCursor)
}

type semanticSearchFixture struct {
	store      *storetest.Fixture
	generation store.DocumentVectorGeneration
	claims     []store.DocumentVectorChunkClaim
}

func seedSemanticSearch(t *testing.T, chunks ...string) semanticSearchFixture {
	t.Helper()
	requirements := require.New(t)
	fixture := storetest.New(t)
	profileFingerprint := strings.Repeat("a", 64)
	profile := store.DocumentExtractionProfile{
		ID: "profile-" + profileFingerprint, Fingerprint: profileFingerprint,
		Provider: "mistral", Endpoint: "https://api.example.invalid/v1/ocr", Region: "test",
		Model: "ocr-test", RetentionPosture: "standard", TrainingPosture: "opted-out",
		AllowedMediaTypes: []string{"application/pdf"}, PolicyJSON: []byte(`{"policy":1}`),
	}
	_, err := fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	requirements.NoError(err)
	requirements.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))
	messageID := fixture.CreateMessage("semantic-search-document")
	hash := strings.Repeat("b", 64)
	attachmentID := addSemanticSearchAttachment(t, fixture, messageID, hash, "semantic.pdf", "mime:1")
	occurrence, eligible, err := fixture.Store.ReconcileDocumentOccurrence(t.Context(), attachmentID, 1)
	requirements.NoError(err)
	requirements.True(eligible)
	claim, err := fixture.Store.ClaimDocumentExtraction(t.Context(), store.DocumentExtractionClaimInput{
		ExtractionID: "semantic-extraction", ProfileID: profile.ID, CanonicalBlobHash: hash,
		ExtractionInputKey: "original", OccurrenceAttachmentID: occurrence.AttachmentID,
		OccurrenceMIMEType: occurrence.MIMEType, OccurrenceMessageType: "email",
		LeaseOwner: "semantic-extractor", LeaseUntil: time.Now().UTC().Add(time.Minute),
		LocalBytes: 128, SourceSequence: occurrence.SourceSequence,
	})
	requirements.NoError(err)
	unitText := strings.Join(chunks, " ")
	publication := store.DocumentExtractionPublication{
		ExtractionID: claim.ExtractionID, ProfileID: claim.ProfileID,
		CanonicalBlobHash: claim.CanonicalBlobHash, ExtractionInputKey: claim.ExtractionInputKey,
		OccurrenceAttachmentID: claim.OccurrenceAttachmentID, OccurrenceMIMEType: claim.OccurrenceMIMEType,
		OccurrenceMessageType: claim.OccurrenceMessageType, LeaseOwner: claim.LeaseOwner, LeaseFence: claim.LeaseFence,
		ReturnedModel: profile.Model, UnitsProcessed: 1, RequestCount: 1,
		ManifestChecksum: strings.Repeat("c", 64),
		Units: []store.DocumentPublishedUnit{{
			Index: 0, Kind: "page", Text: unitText, Checksum: strings.Repeat("d", 64), CharCount: len([]rune(unitText)),
		}},
	}
	offset := 0
	for index, text := range chunks {
		length := len([]rune(text))
		publication.Chunks = append(publication.Chunks, store.DocumentPublishedChunk{
			Key: fmt.Sprintf("chunk-%d", index), Ordinal: index, Text: text,
			FirstUnitIndex: 0, LastUnitIndex: 0, Checksum: fmt.Sprintf("%064x", index+1),
			CharCount: length,
			Spans:     []store.DocumentPublishedSpan{{UnitIndex: 0, CharStart: offset, CharEnd: offset + length}},
		})
		offset += length + 1
	}
	requirements.NoError(fixture.Store.PublishDocumentExtraction(t.Context(), publication))
	generation, _, err := fixture.Store.EnsureDocumentVectorGeneration(t.Context(), store.DocumentVectorGenerationSpec{
		Fingerprint: strings.Repeat("f", 64), TargetExtractionProfileID: profile.ID,
		EmbeddingProfile: "vector.embeddings", Model: "embed-test", Dimension: 3,
	})
	requirements.NoError(err)
	now := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	claims := make([]store.DocumentVectorChunkClaim, 0, len(chunks))
	for range chunks {
		vectorClaim, claimErr := fixture.Store.ClaimDocumentVectorChunk(
			t.Context(), generation.ID, 0, 1000, "semantic-worker", now, time.Minute,
		)
		requirements.NoError(claimErr)
		requirements.NotNil(vectorClaim)
		requirements.NoError(fixture.Store.CommitDocumentVectorPublication(
			t.Context(), generation.ID, vectorClaim.Token, vectorClaim.LeaseOwner,
			vectorClaim.LeaseFence, now.Add(time.Second),
		))
		claims = append(claims, *vectorClaim)
	}
	requirements.NoError(fixture.Store.ActivateDocumentVectorGeneration(t.Context(), generation.ID, now.Add(2*time.Second)))
	return semanticSearchFixture{store: fixture, generation: generation, claims: claims}
}

func addSemanticSearchAttachment(
	t *testing.T, fixture *storetest.Fixture, messageID int64, hash, filename, sourcePartKey string,
) int64 {
	t.Helper()
	require.NoError(t, fixture.Store.UpsertAttachmentRecord(t.Context(), messageID, store.AttachmentWrite{
		Filename: filename, MIMEType: "application/pdf", Size: 128,
		StoragePath: hash[:2] + "/" + hash, ContentHash: hash,
		Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics,
		SourcePartKey: sourcePartKey,
	}))
	var attachmentID int64
	require.NoError(t, fixture.Store.DB().QueryRow(fixture.Store.Rebind(
		`SELECT id FROM attachments WHERE message_id = ?`), messageID).Scan(&attachmentID))
	return attachmentID
}

type searchQueryEmbedder struct {
	vector  []float32
	err     error
	queries []string
}

func (e *searchQueryEmbedder) EmbedQuery(_ context.Context, query string) ([]float32, error) {
	e.queries = append(e.queries, query)
	return e.vector, e.err
}

type searchBackendCall struct {
	generationID GenerationID
	dimension    int
	query        []float32
	k            int
}

type searchBackend struct {
	hits     []Hit
	err      error
	searches []searchBackendCall
}

func (*searchBackend) PutUnpublished(context.Context, GenerationID, int, []Embedding) error {
	return nil
}
func (*searchBackend) DeleteTokens(context.Context, GenerationID, []string) error { return nil }
func (b *searchBackend) Search(
	_ context.Context, generationID GenerationID, dimension int, query []float32, k int,
) ([]Hit, error) {
	b.searches = append(b.searches, searchBackendCall{
		generationID: generationID, dimension: dimension, query: append([]float32(nil), query...), k: k,
	})
	return append([]Hit(nil), b.hits...), b.err
}
