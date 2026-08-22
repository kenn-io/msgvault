package document

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

const (
	searchCursorVersion = 1
	maxSearchPageSize   = 100
	maxSearchQueryBytes = 1024
	maxSearchQueryTerms = 20
)

var ErrSemanticSearchUnavailable = errors.New("semantic document search is unavailable")

// QueryEmbedder embeds one search query using the provider's query role.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, text string) ([]float32, error)
}

type SearchLedger interface {
	SearchDocuments(ctx context.Context, request store.DocumentSearchRequest) (store.DocumentSearchResponse, error)
	GetDocumentIndexRevision(ctx context.Context) (int64, error)
	GetActiveDocumentVectorGeneration(ctx context.Context) (*store.DocumentVectorGeneration, error)
	ResolveDocumentVectorSearchOccurrences(ctx context.Context, generationID int64, hits []store.DocumentVectorSearchHit, request store.DocumentSearchRequest, limit int) ([]store.DocumentSearchResult, bool, error)
}

var _ SearchLedger = (*store.Store)(nil)

type SearchDeps struct {
	Ledger   SearchLedger
	Embedder QueryEmbedder
	Backend  Backend
	// ExpectedFingerprint binds daemon capability to the currently configured
	// and consented policy. Empty preserves the standalone service contract.
	ExpectedFingerprint string
}

type SearchService struct{ deps SearchDeps }

type searchCursor struct {
	Version               int    `json:"version"`
	RequestHash           string `json:"request_hash"`
	Revision              int64  `json:"revision"`
	EffectiveMode         string `json:"effective_mode"`
	GenerationID          int64  `json:"generation_id"`
	GenerationFingerprint string `json:"generation_fingerprint"`
	CandidateLimit        int    `json:"candidate_limit"`
	CandidateDigest       string `json:"candidate_digest"`
	Offset                int    `json:"offset"`
}

func NewSearchService(deps SearchDeps) *SearchService { return &SearchService{deps: deps} }

func (s *SearchService) Search(ctx context.Context, request store.DocumentSearchRequest) (store.DocumentSearchResponse, error) {
	prepared, requestedMode, requestHash, err := normalizeSearchRequest(request)
	if err != nil {
		return store.DocumentSearchResponse{}, err
	}
	if s == nil || s.deps.Ledger == nil {
		return store.DocumentSearchResponse{}, errors.New("document search ledger is required")
	}
	revision, err := s.deps.Ledger.GetDocumentIndexRevision(ctx)
	if err != nil {
		return store.DocumentSearchResponse{}, fmt.Errorf("read document search revision: %w", err)
	}
	var generation *store.DocumentVectorGeneration
	if requestedMode == SearchModeSemantic || requestedMode == SearchModeHybrid {
		generation, err = s.deps.Ledger.GetActiveDocumentVectorGeneration(ctx)
		if err != nil {
			return store.DocumentSearchResponse{}, fmt.Errorf("read active document vector generation: %w", err)
		}
	}
	effectiveMode, err := s.effectiveMode(requestedMode, generation)
	if err != nil {
		return store.DocumentSearchResponse{}, err
	}
	generationID, generationFingerprint := int64(0), ""
	if effectiveMode != SearchModeLexical {
		generationID, generationFingerprint = generation.ID, generation.Fingerprint
	}
	offset, expectedDigest, err := validateSearchCursor(prepared.Cursor, requestHash, revision, effectiveMode, generationID, generationFingerprint, prepared.CandidateLimit)
	if err != nil {
		return store.DocumentSearchResponse{}, err
	}

	prepared.Cursor = ""
	var candidates []store.DocumentSearchResult
	truncated := false
	switch effectiveMode {
	case SearchModeLexical:
		lexical, more, searchErr := s.collectLexical(ctx, prepared)
		if searchErr != nil {
			return store.DocumentSearchResponse{}, searchErr
		}
		var fusionMore bool
		candidates, fusionMore = fuseSearchResults(lexical, nil, prepared.CandidateLimit)
		truncated = more || fusionMore
	case SearchModeSemantic, SearchModeHybrid:
		var lexical []store.DocumentSearchResult
		if effectiveMode == SearchModeHybrid {
			var more bool
			lexical, more, err = s.collectLexical(ctx, prepared)
			if err != nil {
				return store.DocumentSearchResponse{}, err
			}
			truncated = more
		}
		semantic, more, semanticErr := s.collectSemantic(ctx, prepared, *generation)
		if semanticErr != nil {
			return store.DocumentSearchResponse{}, semanticErr
		}
		truncated = truncated || more
		var fusionMore bool
		candidates, fusionMore = fuseSearchResults(lexical, semantic, prepared.CandidateLimit)
		truncated = truncated || fusionMore
	default:
		return store.DocumentSearchResponse{}, fmt.Errorf("%w: unsupported effective mode", store.ErrDocumentSearchInvalidRequest)
	}
	digest, err := digestSearchCandidates(candidates)
	if err != nil {
		return store.DocumentSearchResponse{}, err
	}
	if expectedDigest != "" && expectedDigest != digest {
		return store.DocumentSearchResponse{}, store.ErrDocumentSearchCursorStale
	}

	response := store.DocumentSearchResponse{
		Revision: revision, EffectiveMode: string(effectiveMode),
		VectorGenerationID: generationID, VectorGenerationFingerprint: generationFingerprint,
		Truncated: truncated,
	}
	if offset >= len(candidates) {
		return response, nil
	}
	end := min(offset+prepared.PageSize, len(candidates))
	response.Results = slices.Clone(candidates[offset:end])
	if end < len(candidates) {
		response.NextCursor, err = encodeSearchCursor(searchCursor{
			Version: searchCursorVersion, RequestHash: requestHash, Revision: revision,
			EffectiveMode: string(effectiveMode), GenerationID: generationID,
			GenerationFingerprint: generationFingerprint, CandidateLimit: prepared.CandidateLimit,
			CandidateDigest: digest, Offset: end,
		})
		if err != nil {
			return store.DocumentSearchResponse{}, err
		}
	}
	return response, nil
}

func (s *SearchService) effectiveMode(requested SearchMode, generation *store.DocumentVectorGeneration) (SearchMode, error) {
	semanticReady := generation != nil && s.deps.Embedder != nil && s.deps.Backend != nil &&
		(s.deps.ExpectedFingerprint == "" || generation.Fingerprint == s.deps.ExpectedFingerprint)
	switch requested {
	case SearchModeLexical:
		return SearchModeLexical, nil
	case SearchModeAuto:
		// Automatic searches stay local. Sending a query to the configured
		// provider requires an explicit semantic or hybrid request.
		return SearchModeLexical, nil
	case SearchModeSemantic, SearchModeHybrid:
		if !semanticReady {
			return "", ErrSemanticSearchUnavailable
		}
		return requested, nil
	default:
		return "", fmt.Errorf("%w: unsupported search mode", store.ErrDocumentSearchInvalidRequest)
	}
}

func (s *SearchService) collectLexical(ctx context.Context, request store.DocumentSearchRequest) ([]store.DocumentSearchResult, bool, error) {
	pageSize := min(request.CandidateLimit, maxSearchPageSize)
	lexicalRequest := request
	lexicalRequest.PageSize = pageSize
	lexicalRequest.Cursor = ""
	lexicalRequest.SearchMode = ""
	lexicalRequest.CandidateLimit = request.CandidateLimit
	results := make([]store.DocumentSearchResult, 0, request.CandidateLimit)
	for len(results) < request.CandidateLimit {
		response, err := s.deps.Ledger.SearchDocuments(ctx, lexicalRequest)
		if err != nil {
			return nil, false, err
		}
		remaining := request.CandidateLimit - len(results)
		if len(response.Results) > remaining {
			results = append(results, response.Results[:remaining]...)
			return results, true, nil
		}
		results = append(results, response.Results...)
		if response.NextCursor == "" {
			return results, response.Truncated, nil
		}
		if len(results) == request.CandidateLimit {
			return results, true, nil
		}
		lexicalRequest.Cursor = response.NextCursor
	}
	return results, true, nil
}

func (s *SearchService) collectSemantic(ctx context.Context, request store.DocumentSearchRequest, generation store.DocumentVectorGeneration) ([]store.DocumentSearchResult, bool, error) {
	query, err := s.deps.Embedder.EmbedQuery(ctx, request.Query)
	if err != nil {
		return nil, false, fmt.Errorf("embed document search query: %w", err)
	}
	if err := validateSearchVector(query, generation.Dimension); err != nil {
		return nil, false, err
	}
	hits, err := s.deps.Backend.Search(ctx, GenerationID(generation.ID), generation.Dimension, query, request.CandidateLimit)
	if err != nil {
		return nil, false, fmt.Errorf("search document vector backend: %w", err)
	}
	if len(hits) > request.CandidateLimit {
		return nil, false, fmt.Errorf("%w: backend returned too many hits", ErrInvalidVector)
	}
	storeHits := make([]store.DocumentVectorSearchHit, len(hits))
	for index := range hits {
		storeHits[index] = store.DocumentVectorSearchHit{Token: hits[index].Token, Score: hits[index].Score, Rank: hits[index].Rank}
	}
	results, expandedMore, err := s.deps.Ledger.ResolveDocumentVectorSearchOccurrences(
		ctx, generation.ID, storeHits, request, request.CandidateLimit,
	)
	if err != nil {
		return nil, false, err
	}
	return results, expandedMore || len(hits) == request.CandidateLimit, nil
}

func validateSearchVector(value []float32, dimension int) error {
	if dimension <= 0 || len(value) != dimension {
		return fmt.Errorf("%w: query vector length %d does not match dimension %d", vector.ErrDimensionMismatch, len(value), dimension)
	}
	norm := float64(0)
	for _, component := range value {
		if math.IsNaN(float64(component)) || math.IsInf(float64(component), 0) {
			return fmt.Errorf("%w: query vector contains a nonfinite component", ErrInvalidVector)
		}
		norm += float64(component) * float64(component)
	}
	if norm == 0 {
		return fmt.Errorf("%w: query vector has zero norm", ErrInvalidVector)
	}
	return nil
}

func normalizeSearchRequest(request store.DocumentSearchRequest) (store.DocumentSearchRequest, SearchMode, string, error) {
	mode, err := ParseSearchMode(request.SearchMode)
	if err != nil {
		return request, "", "", fmt.Errorf("%w: %w", store.ErrDocumentSearchInvalidRequest, err)
	}
	request.Query = strings.ToLower(strings.Join(strings.Fields(request.Query), " "))
	if request.Query == "" || len(request.Query) > maxSearchQueryBytes || !utf8.ValidString(request.Query) || len(strings.Fields(request.Query)) > maxSearchQueryTerms {
		return request, "", "", fmt.Errorf("%w: requires a bounded UTF-8 query", store.ErrDocumentSearchInvalidRequest)
	}
	if request.PageSize == 0 {
		request.PageSize = 20
	}
	if request.CandidateLimit == 0 {
		request.CandidateLimit = store.DefaultDocumentSearchCandidateLimit
	}
	if request.PageSize < 1 || request.PageSize > maxSearchPageSize || request.CandidateLimit < 1 || request.CandidateLimit > store.MaxDocumentSearchCandidateLimit || request.AttachmentID < 0 || request.MessageID < 0 {
		return request, "", "", fmt.Errorf("%w: request has invalid bounds", store.ErrDocumentSearchInvalidRequest)
	}
	request.SourceIDs = slices.Clone(request.SourceIDs)
	slices.Sort(request.SourceIDs)
	request.SourceIDs = slices.Compact(request.SourceIDs)
	for _, id := range request.SourceIDs {
		if id <= 0 {
			return request, "", "", fmt.Errorf("%w: source IDs must be positive", store.ErrDocumentSearchInvalidRequest)
		}
	}
	request.MessageTypes = slices.Clone(request.MessageTypes)
	for index := range request.MessageTypes {
		request.MessageTypes[index] = strings.ToLower(strings.TrimSpace(request.MessageTypes[index]))
		if request.MessageTypes[index] == "" {
			return request, "", "", fmt.Errorf("%w: message types must be nonempty", store.ErrDocumentSearchInvalidRequest)
		}
	}
	slices.Sort(request.MessageTypes)
	request.MessageTypes = slices.Compact(request.MessageTypes)
	request.SearchMode = string(mode)
	hash, err := hashSearchRequest(request)
	return request, mode, hash, err
}

func hashSearchRequest(request store.DocumentSearchRequest) (string, error) {
	request.Cursor = ""
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("encode document semantic search request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func digestSearchCandidates(results []store.DocumentSearchResult) (string, error) {
	type candidateIdentity struct {
		OccurrenceKey               string   `json:"occurrence_key"`
		ChunkKey                    string   `json:"chunk_key"`
		ExtractionID                string   `json:"extraction_id"`
		VectorToken                 string   `json:"vector_token"`
		VectorGenerationFingerprint string   `json:"vector_generation_fingerprint"`
		VectorEmbeddingProfile      string   `json:"vector_embedding_profile"`
		VectorModel                 string   `json:"vector_model"`
		AttachmentID                int64    `json:"attachment_id"`
		MessageID                   int64    `json:"message_id"`
		VectorGenerationID          int64    `json:"vector_generation_id"`
		LexicalRank                 int      `json:"lexical_rank"`
		SemanticRank                int      `json:"semantic_rank"`
		VectorDimension             int      `json:"vector_dimension"`
		SemanticScore               float64  `json:"semantic_score"`
		FusionScore                 float64  `json:"fusion_score"`
		MatchedSignals              []string `json:"matched_signals"`
	}
	identities := make([]candidateIdentity, len(results))
	for index, result := range results {
		identities[index] = candidateIdentity{
			OccurrenceKey: result.OccurrenceKey, ChunkKey: result.ChunkKey, ExtractionID: result.ExtractionID,
			VectorToken: result.VectorToken, VectorGenerationFingerprint: result.VectorGenerationFingerprint,
			VectorEmbeddingProfile: result.VectorEmbeddingProfile, VectorModel: result.VectorModel,
			AttachmentID: result.AttachmentID, MessageID: result.MessageID, VectorGenerationID: result.VectorGenerationID,
			LexicalRank: result.LexicalRank, SemanticRank: result.SemanticRank, VectorDimension: result.VectorDimension,
			SemanticScore: result.SemanticScore, FusionScore: result.FusionScore, MatchedSignals: result.MatchedSignals,
		}
	}
	encoded, err := json.Marshal(identities)
	if err != nil {
		return "", fmt.Errorf("encode document search candidate digest: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func validateSearchCursor(value, requestHash string, revision int64, mode SearchMode, generationID int64, generationFingerprint string, candidateLimit int) (int, string, error) {
	if value == "" {
		return 0, "", nil
	}
	cursor, err := decodeSearchCursor(value)
	if err != nil {
		return 0, "", err
	}
	if cursor.RequestHash != requestHash || cursor.CandidateLimit != candidateLimit {
		return 0, "", store.ErrDocumentSearchInvalidCursor
	}
	if cursor.Offset > cursor.CandidateLimit {
		return 0, "", store.ErrDocumentSearchInvalidCursor
	}
	if cursor.Revision != revision || cursor.EffectiveMode != string(mode) || cursor.GenerationID != generationID || cursor.GenerationFingerprint != generationFingerprint {
		return 0, "", store.ErrDocumentSearchCursorStale
	}
	return cursor.Offset, cursor.CandidateDigest, nil
}

func encodeSearchCursor(cursor searchCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode document semantic search cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeSearchCursor(value string) (searchCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 4096 {
		return searchCursor{}, store.ErrDocumentSearchInvalidCursor
	}
	var cursor searchCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != searchCursorVersion || !validSearchDigest(cursor.RequestHash) || !validSearchDigest(cursor.CandidateDigest) || cursor.Revision < 0 || cursor.Offset < 1 || cursor.Offset > store.MaxDocumentSearchCandidateLimit || cursor.CandidateLimit < 1 || cursor.CandidateLimit > store.MaxDocumentSearchCandidateLimit || (cursor.GenerationID == 0) != (cursor.GenerationFingerprint == "") || (cursor.GenerationFingerprint != "" && !validSearchDigest(cursor.GenerationFingerprint)) {
		return searchCursor{}, store.ErrDocumentSearchInvalidCursor
	}
	if _, err := ParseSearchMode(cursor.EffectiveMode); err != nil || cursor.EffectiveMode == string(SearchModeAuto) {
		return searchCursor{}, store.ErrDocumentSearchInvalidCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return searchCursor{}, store.ErrDocumentSearchInvalidCursor
	}
	return cursor, nil
}

func validSearchDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}
