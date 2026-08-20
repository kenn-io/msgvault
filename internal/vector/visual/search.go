package visual

import (
	"go.kenn.io/docbank/document/media"
	"strconv"
	"sync"

	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
)

const MaxQueryImageBytes int64 = 20 << 20

var (
	ErrSearchNotReady = errors.New("visual attachment search is not ready")
	ErrInvalidQuery   = errors.New("visual attachment search requires exactly one of text or image")
	ErrInvalidCursor  = errors.New("visual attachment search cursor does not match the active query")
)

type SearchQuery struct {
	Text  string
	Image *MediaInput
	// QueryVector, when set, is the already-embedded query: paging callers
	// embed once via EmbedQueryVector and reuse the vector so each cursor
	// continuation does not pay another hosted embedding request. It never
	// participates in the cursor's query hash — the text or image does.
	QueryVector []float32
	Limit       int
	Cursor      string
	Person      *personscope.Scope
	SourceID    int64
	MessageID   int64
	Filename    string
	MIMEPrefix  string
	After       *time.Time
	Before      *time.Time
}

type AttachmentSearchResult struct {
	AttachmentID     int64                   `json:"attachment_id"`
	MessageID        int64                   `json:"message_id"`
	ConversationID   int64                   `json:"conversation_id"`
	SourceID         int64                   `json:"source_id"`
	SourceMessageID  string                  `json:"source_message_id"`
	BlobHash         string                  `json:"blob_hash"`
	Filename         string                  `json:"filename"`
	MIMEType         string                  `json:"mime_type"`
	Size             int64                   `json:"size"`
	SentAt           time.Time               `json:"sent_at"`
	Score            float64                 `json:"score"`
	Rank             int                     `json:"rank"`
	PersonProvenance *personscope.Provenance `json:"person_provenance,omitempty"`
}

type SearchResponse struct {
	GenerationID int64                    `json:"generation_id"`
	Model        string                   `json:"model"`
	QueryMode    string                   `json:"query_mode"`
	Results      []AttachmentSearchResult `json:"results"`
	Usage        Usage                    `json:"usage"`
	NextCursor   string                   `json:"next_cursor,omitempty"`
}

type searchCursor struct {
	GenerationID int64   `json:"generation_id"`
	QueryHash    string  `json:"query_hash"`
	Score        float64 `json:"score"`
	Token        string  `json:"token"`
}

type searchQueryHashPayload struct {
	Text       string             `json:"text"`
	ImageHash  string             `json:"image_hash"`
	Filename   string             `json:"filename"`
	MIMEPrefix string             `json:"mime_prefix"`
	Person     *personscope.Scope `json:"person"`
	SourceID   int64              `json:"source_id"`
	MessageID  int64              `json:"message_id"`
	After      *time.Time         `json:"after"`
	Before     *time.Time         `json:"before"`
}

type SearchService struct {
	archive             *store.Store
	provider            Provider
	backend             Backend
	allowImageQueries   bool
	expectedFingerprint string
	// vectorCache reuses recently embedded query vectors for cursor
	// continuations, keyed by generation and query hash: re-embedding per
	// page pays a hosted request each time and provider nondeterminism can
	// re-rank pages mid-pagination. Bounded FIFO, ~4 KiB per entry.
	vectorCache queryVectorCache
	// scopeCheck re-resolves the configured account scope before serving:
	// the text lane latches searches stale when its scope no longer
	// resolves to the same sources, and the visual lane must not keep
	// serving a generation whose scope drifted (account deleted, recreated,
	// or remapped) until a restart.
	scopeCheck func(context.Context) error
}

// DecodeQueryImage validates uploaded bytes with the same detection and
// pixel ceiling used for indexed media, without persisting the query. Still
// images only; whether the format's query capability was probed is enforced
// by the provider when the query is embedded.
func DecodeQueryImage(data []byte) (*MediaInput, error) {
	if len(data) == 0 || int64(len(data)) > MaxQueryImageBytes {
		return nil, errors.New("invalid visual query image size")
	}
	metadata, reason := media.InspectBytes(data, "", media.Policy{
		MaxBytes: MaxQueryImageBytes, MaxPixels: defaultMaxPixels, AllowStill: true,
	})
	if reason != media.ReasonEligible || metadata.Kind != media.KindImage || metadata.Animated {
		return nil, errors.New("unsupported visual query image")
	}
	input := mediaInputFrom(metadata)
	input.Bytes = data
	return input, nil
}

func NewSearchService(archive *store.Store, provider Provider, backend Backend, allowImageQueries bool) (*SearchService, error) {
	if archive == nil || provider == nil || backend == nil {
		return nil, errors.New("visual search requires archive, provider, and backend")
	}
	return &SearchService{archive: archive, provider: provider, backend: backend, allowImageQueries: allowImageQueries}, nil
}

// SetScopeCheck installs the live account-scope preflight; a drift error
// reports not-ready instead of serving the outdated generation.
func (s *SearchService) SetScopeCheck(check func(context.Context) error) {
	s.scopeCheck = check
}

// ExpectFingerprint pins the generation the service may search. After a
// scope or policy change, the previously active generation would otherwise
// keep serving now-excluded attachments while the replacement build awaits
// consent; a mismatch reports not-ready instead.
func (s *SearchService) ExpectFingerprint(fingerprint string) {
	s.expectedFingerprint = fingerprint
}

func (s *SearchService) activeGeneration(ctx context.Context) (store.VisualGeneration, error) {
	if s.scopeCheck != nil {
		if err := s.scopeCheck(ctx); err != nil {
			return store.VisualGeneration{}, ErrSearchNotReady
		}
	}
	generation, err := s.archive.ActiveVisualGeneration(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.VisualGeneration{}, ErrSearchNotReady
		}
		return store.VisualGeneration{}, err
	}
	if s.expectedFingerprint != "" && generation.Fingerprint != s.expectedFingerprint {
		return store.VisualGeneration{}, ErrSearchNotReady
	}
	return generation, nil
}

func (s *SearchService) validateQueryMode(query SearchQuery) (imageSet bool, err error) {
	textSet := strings.TrimSpace(query.Text) != ""
	imageSet = query.Image != nil
	if textSet == imageSet {
		return false, ErrInvalidQuery
	}
	if imageSet {
		if !s.allowImageQueries {
			return false, errors.New("visual image queries are disabled")
		}
		if query.Image.Kind != MediaKindImage || int64(len(query.Image.Bytes)) > MaxQueryImageBytes {
			return false, errors.New("invalid visual query image")
		}
	}
	return imageSet, nil
}

// QueryVectorForContinuation returns the cached vector a pagination
// continuation must reuse, or ErrInvalidCursor when it is gone (eviction or
// daemon restart): re-embedding would let provider nondeterminism reorder
// the fused results mid-pagination, silently skipping or duplicating rows.
func (s *SearchService) QueryVectorForContinuation(ctx context.Context, query SearchQuery) ([]float32, error) {
	if _, err := s.validateQueryMode(query); err != nil {
		return nil, err
	}
	generation, err := s.activeGeneration(ctx)
	if err != nil {
		return nil, err
	}
	queryHash, err := visualSearchQueryHash(query)
	if err != nil {
		return nil, err
	}
	vector := s.vectorCache.get(generation.ID, queryHash)
	if len(vector) == 0 {
		return nil, ErrInvalidCursor
	}
	return vector, nil
}

// EmbedQueryVector embeds the query once so callers paging through results
// can reuse the vector via SearchQuery.QueryVector instead of paying one
// hosted embedding request per cursor page.
func (s *SearchService) EmbedQueryVector(ctx context.Context, query SearchQuery) ([]float32, Usage, error) {
	if _, err := s.validateQueryMode(query); err != nil {
		return nil, Usage{}, err
	}
	// Search validates the active generation before embedding; a paging
	// caller must not pay a hosted embedding either when no generation is
	// searchable yet or the active one no longer matches the configured
	// policy.
	generation, err := s.activeGeneration(ctx)
	if err != nil {
		return nil, Usage{}, err
	}
	queryHash, err := visualSearchQueryHash(query)
	if err != nil {
		return nil, Usage{}, err
	}
	if vector := s.vectorCache.get(generation.ID, queryHash); len(vector) > 0 {
		return vector, Usage{}, nil
	}
	vector, usage, err := s.provider.EmbedQuery(ctx, QueryInput{Text: query.Text, Image: query.Image})
	if err != nil {
		return nil, Usage{}, fmt.Errorf("embed visual query: %w", err)
	}
	s.vectorCache.put(generation.ID, queryHash, vector)
	if canonical := s.vectorCache.get(generation.ID, queryHash); len(canonical) > 0 {
		vector = canonical
	}
	return vector, usage, nil
}

func (s *SearchService) Search(ctx context.Context, query SearchQuery) (SearchResponse, error) {
	imageSet, err := s.validateQueryMode(query)
	if err != nil {
		return SearchResponse{}, err
	}
	if query.Limit == 0 {
		query.Limit = 20
	}
	if query.Limit < 1 || query.Limit > 100 {
		return SearchResponse{}, errors.New("visual search limit must be between 1 and 100")
	}
	if query.SourceID < 0 || query.MessageID < 0 ||
		(query.After != nil && query.Before != nil && !query.After.Before(*query.Before)) {
		return SearchResponse{}, errors.New("invalid visual search filters")
	}
	if query.Person != nil {
		if err := personscope.Validate(*query.Person); err != nil {
			return SearchResponse{}, fmt.Errorf("invalid visual person scope: %w", err)
		}
	}
	generation, err := s.activeGeneration(ctx)
	if err != nil {
		return SearchResponse{}, err
	}
	queryHash, err := visualSearchQueryHash(query)
	if err != nil {
		return SearchResponse{}, err
	}
	var cursor searchCursor
	if query.Cursor != "" {
		cursor, err = decodeSearchCursor(query.Cursor)
		if err != nil || cursor.GenerationID != generation.ID || cursor.QueryHash != queryHash || cursor.Token == "" {
			return SearchResponse{}, ErrInvalidCursor
		}
	}
	vector, usage := query.QueryVector, Usage{}
	if len(vector) == 0 {
		// The cached vector is CANONICAL for a (generation, query) pair:
		// cursorless repeats must rank with the same vector their earlier
		// pages cached, or a nondeterministic re-embedding hands page one a
		// different ordering than the continuation cursor was built from.
		vector = s.vectorCache.get(generation.ID, queryHash)
	}
	if len(vector) == 0 && query.Cursor != "" {
		// The cursor's score boundary was computed from the original
		// embedding. Re-embedding here would pay another provider request
		// AND apply the boundary to a possibly different vector, silently
		// skipping or duplicating results — reject so the caller restarts
		// pagination deterministically.
		return SearchResponse{}, ErrInvalidCursor
	}
	if len(vector) == 0 {
		vector, usage, err = s.provider.EmbedQuery(ctx, QueryInput{Text: query.Text, Image: query.Image})
		if err != nil {
			return SearchResponse{}, fmt.Errorf("embed visual query: %w", err)
		}
	}
	// put is put-if-absent; ranking with the post-put canonical entry means
	// two racing cache misses both rank with the same winning vector.
	s.vectorCache.put(generation.ID, queryHash, vector)
	if canonical := s.vectorCache.get(generation.ID, queryHash); len(canonical) > 0 {
		vector = canonical
	}
	request := SearchRequest{GenerationID: GenerationID(generation.ID), Vector: vector, Limit: query.Limit + 1,
		SourceID: query.SourceID, MessageID: query.MessageID,
		Person:   query.Person,
		Filename: strings.TrimSpace(query.Filename), MIMEPrefix: strings.ToLower(strings.TrimSpace(query.MIMEPrefix)),
		After: query.After, Before: query.Before}
	if query.Cursor != "" {
		request.AfterScore = &cursor.Score
		request.AfterToken = VectorToken(cursor.Token)
	}
	hits, err := s.backend.Search(ctx, request)
	if err != nil {
		return SearchResponse{}, fmt.Errorf("search visual vectors: %w", err)
	}
	response := SearchResponse{GenerationID: generation.ID, Model: generation.Model, QueryMode: "text", Usage: usage,
		Results: make([]AttachmentSearchResult, 0, min(query.Limit, len(hits)))}
	if imageSet {
		response.QueryMode = "image"
	}
	for _, hit := range hits[:min(query.Limit, len(hits))] {
		occurrence, resolveErr := s.archive.ResolveVisualSearchOccurrence(ctx, generation.ID, string(hit.Token))
		if errors.Is(resolveErr, sql.ErrNoRows) {
			continue
		}
		if resolveErr != nil {
			return SearchResponse{}, resolveErr
		}
		response.Results = append(response.Results, AttachmentSearchResult{
			AttachmentID: occurrence.AttachmentID, MessageID: occurrence.MessageID,
			ConversationID: occurrence.ConversationID, SourceID: occurrence.SourceID,
			SourceMessageID: occurrence.SourceMessageID, BlobHash: occurrence.BlobHash,
			Filename: occurrence.Filename, MIMEType: occurrence.MIMEType, Size: occurrence.Size,
			SentAt: occurrence.SentAt, Score: hit.Score, Rank: len(response.Results) + 1,
		})
	}
	if query.Person != nil && len(response.Results) > 0 {
		messageIDs := make([]int64, len(response.Results))
		for i := range response.Results {
			messageIDs[i] = response.Results[i].MessageID
		}
		provenance, provenanceErr := s.archive.PersonProvenanceForMessages(ctx, messageIDs, *query.Person)
		if provenanceErr != nil {
			return SearchResponse{}, provenanceErr
		}
		for i := range response.Results {
			response.Results[i].PersonProvenance = provenance[response.Results[i].MessageID]
			if response.Results[i].PersonProvenance == nil {
				return SearchResponse{}, fmt.Errorf("visual person provenance missing for message %d", response.Results[i].MessageID)
			}
		}
	}
	if len(hits) > query.Limit {
		last := hits[query.Limit-1]
		response.NextCursor, err = encodeSearchCursor(searchCursor{
			GenerationID: generation.ID, QueryHash: queryHash, Score: last.Score, Token: string(last.Token),
		})
		if err != nil {
			return SearchResponse{}, err
		}
	}
	return response, nil
}

type queryVectorCache struct {
	mu      sync.Mutex
	entries map[string][]float32
	order   []string
}

const queryVectorCacheCap = 32

func (c *queryVectorCache) key(generationID int64, queryHash string) string {
	return strconv.FormatInt(generationID, 10) + "|" + queryHash
}

func (c *queryVectorCache) get(generationID int64, queryHash string) []float32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[c.key(generationID, queryHash)]
}

func (c *queryVectorCache) put(generationID int64, queryHash string, vector []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := c.key(generationID, queryHash)
	if c.entries == nil {
		c.entries = make(map[string][]float32, queryVectorCacheCap)
	}
	if _, exists := c.entries[key]; exists {
		return
	}
	if len(c.order) >= queryVectorCacheCap {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
	c.entries[key] = vector
	c.order = append(c.order, key)
}

func visualSearchQueryHash(query SearchQuery) (string, error) {
	imageHash := ""
	if query.Image != nil {
		digest := sha256.Sum256(query.Image.Bytes)
		imageHash = hex.EncodeToString(digest[:])
	}
	payload, err := json.Marshal(searchQueryHashPayload{
		Text: strings.TrimSpace(query.Text), ImageHash: imageHash,
		Filename:   strings.TrimSpace(query.Filename),
		MIMEPrefix: strings.ToLower(strings.TrimSpace(query.MIMEPrefix)),
		Person:     query.Person, SourceID: query.SourceID, MessageID: query.MessageID,
		After: query.After, Before: query.Before,
	})
	if err != nil {
		return "", fmt.Errorf("marshal visual search query: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func encodeSearchCursor(cursor searchCursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeSearchCursor(raw string) (searchCursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return searchCursor{}, fmt.Errorf("decode visual search cursor: %w", err)
	}
	var cursor searchCursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return searchCursor{}, err
	}
	return cursor, nil
}
