package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mark3labs/mcp-go/mcp"
	"go.kenn.io/msgvault/internal/deletion"
	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/internal/vector/chunkmatch"
	"go.kenn.io/msgvault/internal/vector/embed"
	"go.kenn.io/msgvault/internal/vector/hybrid"
)

const (
	maxLimit               = 1000
	maxSearchMessagesLimit = 50
	defaultSearchLimit     = 20
	// searchContextChars is the max byte length of each matches[] snippet in
	// search_message_bodies and search_in_message.
	searchContextChars = 300
	defaultBodyChars   = 2000
	bodyFormatAuto     = "auto"
	bodyFormatText     = "text"
	bodyFormatHTML     = "html"
	// maxBodyChars caps the body slice returned by get_message regardless of what
	// the caller requests via max_chars. Prevents a single tool call from flooding
	// the context window; callers page forward using offset.
	maxBodyChars = 4000
	// maxContextSnippets is the maximum number of match excerpts returned for a single message.
	maxContextSnippets = 5
	// totalCountUnknown is returned when the backend cannot report a full match
	// count (hybrid/vector ranking depth, or list_messages without a separate
	// count query). Clients should use has_more for paging.
	totalCountUnknown = -1
)

type paginatedResponse[T any] struct {
	Data     []T   `json:"data"`
	Total    int64 `json:"total"`
	Returned int   `json:"returned"`
	Offset   int   `json:"offset"`
	HasMore  bool  `json:"has_more"`
}

func newPaginatedResponse[T any](data []T, total int64, offset int) paginatedResponse[T] {
	if data == nil {
		data = []T{}
	}
	returned := len(data)
	return paginatedResponse[T]{
		Data:     data,
		Total:    total,
		Returned: returned,
		Offset:   offset,
		HasMore:  int64(offset+returned) < total,
	}
}

// newPaginatedResponseNoTotal builds a page when the backend cannot report a
// total match count. total is always totalCountUnknown; use has_more to page.
func newPaginatedResponseNoTotal[T any](data []T, offset int, hasMore bool) paginatedResponse[T] {
	if data == nil {
		data = []T{}
	}
	return paginatedResponse[T]{
		Data:     data,
		Total:    totalCountUnknown,
		Returned: len(data),
		Offset:   offset,
		HasMore:  hasMore,
	}
}

func searchLimitArg(args map[string]any) int {
	limit := limitArg(args, "limit", defaultSearchLimit)
	if limit <= 0 {
		return defaultSearchLimit
	}
	if limit > maxSearchMessagesLimit {
		return maxSearchMessagesLimit
	}
	return limit
}

func listLimitArg(args map[string]any) int {
	return searchLimitArg(args)
}

type handlers struct {
	engine           query.Engine
	attachmentsDir   string
	attachmentReader AttachmentReader
	manifestSaver    DeletionManifestSaver
	hybridSearcher   HybridSearcher
	similarSearcher  SimilarSearcher
	dataDir          string

	// Optional vector-search wiring. When hybridEngine is nil, the
	// search_message_bodies handler rejects mode=vector and mode=hybrid with
	// a vector_not_enabled error. backend is additionally required by
	// the find_similar_messages handler to load seed vectors and
	// resolve the active generation.
	hybridEngine *hybrid.Engine
	vectorCfg    vector.Config
	backend      vector.Backend
}

// AttachmentReader fetches content-addressed attachment bytes. It is optional:
// local MCP servers can read from attachmentsDir, while daemon-routed MCP
// servers can fetch the bytes over HTTP.
type AttachmentReader interface {
	ReadAttachment(ctx context.Context, contentHash string) ([]byte, error)
}

// DeletionManifestSaver persists staged deletion manifests. It is optional:
// direct/local MCP servers can save under dataDir, while daemon-routed MCP
// servers save through the selected daemon.
type DeletionManifestSaver interface {
	SaveManifest(ctx context.Context, manifest *deletion.Manifest) error
}

// HybridSearcher runs vector/hybrid searches outside the MCP process. The
// daemon-backed CLI uses this so MCP does not open local vector stores.
type HybridSearcher interface {
	SearchHybrid(ctx context.Context, req HybridSearchRequest) (*HybridSearchResult, error)
}

type HybridSearchRequest struct {
	Query          string
	Mode           string
	Account        string
	Limit          int
	Offset         int
	IncludeMatches bool
	MinScore       float64
}

type HybridSearchMatch struct {
	CharOffset *int
	Snippet    string
	Line       *int
	Score      float64
}

type HybridSearchHit struct {
	ID               int64
	RRFScore         *float64
	BM25Score        *float64
	VectorScore      *float64
	SubjectBoosted   bool
	Matches          []HybridSearchMatch
	MatchesTruncated bool
}

type HybridSearchResult struct {
	Hits          []HybridSearchHit
	PoolSaturated bool
	Generation    HybridGeneration
	HasMore       bool
}

type SimilarSearcher interface {
	FindSimilar(ctx context.Context, req SimilarSearchRequest) (*SimilarSearchResult, error)
}

type SimilarSearchRequest struct {
	MessageID     int64
	Limit         int
	Account       string
	MessageType   string
	After         *time.Time
	Before        *time.Time
	HasAttachment *bool
}

type SimilarSearchResult struct {
	SeedMessageID int64
	Generation    HybridGeneration
	Messages      []query.MessageSummary
}

// translateVectorErr maps well-known vector sentinel errors to MCP tool
// error results. Returns nil if the error is not a known sentinel
// (callers should wrap it themselves).
func translateVectorErr(err error) *mcp.CallToolResult {
	switch {
	case errors.Is(err, vector.ErrNotEnabled):
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured",
		)
	case errors.Is(err, vector.ErrIndexStale):
		return mcp.NewToolResultError(
			"index_stale: the vector index does not match the configured model; " +
				"run `msgvault embeddings build --full-rebuild`",
		)
	case errors.Is(err, vector.ErrIndexBuilding):
		return mcp.NewToolResultError(
			"index_building: the initial vector index is still being built",
		)
	case errors.Is(err, vector.ErrIndexScopeMismatch):
		return mcp.NewToolResultError(
			"index_scope_mismatch: the vector index scope does not cover this query; " +
				"add a matching message_type filter or rebuild embeddings for the requested scope",
		)
	case errors.Is(err, vector.ErrNoActiveGeneration):
		return mcp.NewToolResultError(
			"no_active_generation: vector search has no active index yet; " +
				"run `msgvault embeddings build` to build one",
		)
	case errors.Is(err, vector.ErrEmbeddingTimeout):
		return mcp.NewToolResultError(
			"embedding_timeout: the embedding endpoint did not respond in time; " +
				"retry, or raise [vector.embeddings].timeout in config",
		)
	}
	return nil
}

// getAccountID looks up a source ID by email address.
// Returns nil if account is empty (no filter), or an error if not found.
func (h *handlers) getAccountID(ctx context.Context, account string) (*int64, error) {
	if account == "" {
		return nil, nil //nolint:nilnil // empty input -> no filter, not an error
	}
	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list accounts: %w", err)
	}
	for _, acc := range accounts {
		if acc.Identifier == account {
			return &acc.ID, nil
		}
	}
	return nil, fmt.Errorf("account not found: %s", account)
}

// getIDArg extracts a required positive integer ID from the arguments map.
func getIDArg(args map[string]any, key string) (int64, error) {
	v, ok := args[key].(float64)
	if !ok {
		return 0, fmt.Errorf("%s parameter is required", key)
	}
	if v != math.Trunc(v) || v < 1 || v > math.MaxInt64 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return int64(v), nil
}

// getDateArg extracts an optional date (YYYY-MM-DD) from the arguments map.
func getDateArg(args map[string]any, key string) (*time.Time, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return nil, nil //nolint:nilnil // absent optional arg is not an error
	}
	t, err := time.Parse("2006-01-02", v)
	if err != nil {
		return nil, fmt.Errorf("invalid %s date %q: expected YYYY-MM-DD", key, v)
	}
	return &t, nil
}

func (h *handlers) readAttachment(ctx context.Context, contentHash string) ([]byte, error) {
	if h.attachmentReader != nil {
		return h.readAttachmentFromReader(ctx, contentHash)
	}
	return h.readAttachmentFile(contentHash)
}

func (h *handlers) readAttachmentFromReader(ctx context.Context, contentHash string) ([]byte, error) {
	if err := export.ValidateContentHash(contentHash); err != nil {
		return nil, errors.New("attachment has invalid content hash")
	}
	data, err := h.attachmentReader.ReadAttachment(ctx, contentHash)
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if int64(len(data)) > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", len(data), maxAttachmentSize)
	}
	return data, nil
}

// readAttachmentFile reads the content-addressed attachment file after
// validating the hash and checking size limits.
func (h *handlers) readAttachmentFile(contentHash string) ([]byte, error) {
	filePath, err := export.StoragePath(h.attachmentsDir, contentHash)
	if err != nil {
		return nil, errors.New("attachment has invalid content hash")
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if info.Size() > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", info.Size(), maxAttachmentSize)
	}

	data, err := io.ReadAll(io.LimitReader(f, maxAttachmentSize+1))
	if err != nil {
		return nil, fmt.Errorf("attachment file not available: %w", err)
	}
	if int64(len(data)) > maxAttachmentSize {
		return nil, fmt.Errorf("attachment too large: %d bytes (max %d)", len(data), maxAttachmentSize)
	}

	return data, nil
}

// searchMessageItem carries a message summary plus body match excerpts.
// Used by search_message_bodies for keyword, vector, and hybrid results.
// Score is present only when mode=vector/hybrid and explain=true.
type searchMessageItem struct {
	query.MessageSummary

	// MatchesTruncated is true when more than maxContextSnippets (5) match
	// excerpts were found; only the first 5 are returned.
	Matches          []messageMatch        `json:"matches,omitempty"`
	MatchesTruncated bool                  `json:"matches_truncated,omitempty"`
	Score            *hybridScoreBreakdown `json:"score,omitempty"`
}

// searchMessages preserves the legacy combined search tool while clients
// migrate to the split tools. An omitted mode retains metadata-search
// semantics; vector and hybrid modes delegate to semantic_search_messages.
func (h *handlers) searchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	mode, _ := req.GetArguments()["mode"].(string)
	switch mode {
	case "":
		return h.searchMetadata(ctx, req)
	case searchModeVector, searchModeHybrid:
		return h.semanticSearchMessages(ctx, req)
	default:
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: must be %s or %s (or omit for metadata search)", mode, searchModeVector, searchModeHybrid),
		), nil
	}
}

// searchMetadata searches message metadata only (subject, sender, recipients,
// labels, dates). Use search_message_bodies for full-body keyword, vector, or
// hybrid search.
func (h *handlers) searchMetadata(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	queryStr, _ := args["query"].(string)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	q := search.Parse(queryStr)
	if err := q.Err(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := unsupportedSearchOperatorMessage(q); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}

	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if sourceID != nil {
		q.AccountIDs = []int64{*sourceID}
	}

	filter := query.MessageFilter{SourceID: sourceID}

	results, err := h.engine.SearchFast(ctx, q, filter, limit, offset)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	totalMatched, err := h.engine.SearchFastCount(ctx, q, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search count failed: %v", err)), nil
	}

	return jsonResult(newPaginatedResponse(results, totalMatched, offset))
}

func unsupportedSearchOperatorMessage(q *search.Query) string {
	if len(q.UnsupportedOperators) == 0 {
		return ""
	}

	names := make([]string, 0, len(q.UnsupportedOperators))
	seen := make(map[string]bool, len(q.UnsupportedOperators))
	for _, op := range q.UnsupportedOperators {
		name := op.Name + ":"
		if !seen[name] {
			names = append(names, name)
			seen[name] = true
		}
	}

	return fmt.Sprintf(
		"unsupported_search_operator: %s is Gmail-only syntax; msgvault does not index List-ID locally. "+
			"Use the Gmail connector for List-ID validation, or use msgvault-supported operators.",
		strings.Join(names, ", "),
	)
}

// searchMessageBodies searches message bodies by keyword, vector, or hybrid.
// It returns messages whose body matches the query, plus matches — short
// excerpts centered on each matched term. Requires at least one free-text term
// for keyword mode; use search_metadata for filter-only queries.
func (h *handlers) searchMessageBodies(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	queryStr, _ := args["query"].(string)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = searchModeKeyword
	}

	switch mode {
	case searchModeKeyword:
	case searchModeVector, searchModeHybrid:
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: search_message_bodies is keyword-only; use semantic_search_messages for vector or hybrid search", mode),
		), nil
	default:
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: search_message_bodies only supports keyword search; use semantic_search_messages for vector or hybrid search", mode),
		), nil
	}

	q := search.Parse(queryStr)
	if err := q.Err(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := unsupportedSearchOperatorMessage(q); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}

	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	if sourceID != nil {
		q.AccountIDs = []int64{*sourceID}
	}

	if len(q.TextTerms) == 0 {
		return mcp.NewToolResultError(
			"search_message_bodies requires at least one free-text term (bare word or quoted phrase); " +
				"Gmail operators such as from: or subject: are metadata filters and do not count — " +
				"use search_metadata for filter-only queries",
		), nil
	}

	bodySearcher, ok := h.engine.(query.MessageBodySearcher)
	if !ok {
		return mcp.NewToolResultError("search_message_bodies is unavailable: the query engine does not support exact body-only search"), nil
	}
	results, err := bodySearcher.SearchMessageBodies(ctx, q, limit+1, offset)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	data := make([]searchMessageItem, 0, len(results))
	for _, r := range results {
		item := searchMessageItem{MessageSummary: r}
		switch {
		case len(r.BodyContextSnippets) > 0:
			item.Matches, item.MatchesTruncated = bodyContextSnippetsToMatches(r.BodyContextSnippets, r.BodyContextSnippetsTruncated)
		case r.BodyContextSnippetsTruncated:
			item.Matches = nil
			item.MatchesTruncated = true
		default:
			return mcp.NewToolResultError(fmt.Sprintf(
				"body context unavailable for message %d: search backend returned no context", r.ID,
			)), nil
		}
		data = append(data, item)
	}

	return jsonResult(searchMessageBodiesResponse{
		paginatedResponse: newPaginatedResponseNoTotal(data, offset, hasMore),
		Mode:              searchModeKeyword,
	})
}

// semanticSearchMessages runs vector/hybrid body search. Unlike
// searchMessageBodies (keyword), mode defaults to hybrid and keyword is
// rejected. Vector availability, the free-text requirement, and index
// staleness are all enforced by the shared searchMessageBodiesHybrid path,
// which returns vector_not_enabled when vector search is not configured.
func (h *handlers) semanticSearchMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	queryStr, _ := args["query"].(string)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = searchModeHybrid
	}
	switch mode {
	case searchModeVector, searchModeHybrid:
	default:
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: must be %s or %s (default %s); use search_message_bodies for keyword search",
				mode, searchModeVector, searchModeHybrid, searchModeHybrid),
		), nil
	}
	explain, _ := args["explain"].(bool)

	q := search.Parse(queryStr)
	if err := q.Err(); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if msg := unsupportedSearchOperatorMessage(q); msg != "" {
		return mcp.NewToolResultError(msg), nil
	}

	return h.searchMessageBodiesHybrid(ctx, args, queryStr, q, mode, explain)
}

// hybridScoreBreakdown exposes fused-score components for debugging.
// All score fields are pointer-typed so "not present in this signal"
// can be distinguished from a legitimate 0.0 score. RRF is omitted in
// mode=vector (only one signal, nothing to fuse).
type hybridScoreBreakdown struct {
	RRF            *float64 `json:"rrf,omitempty"`
	BM25           *float64 `json:"bm25,omitempty"`
	Vector         *float64 `json:"vector,omitempty"`
	SubjectBoosted bool     `json:"subject_boosted,omitempty"`
}

// HybridGeneration describes the active vector-index generation used to answer
// a hybrid/vector query.
type HybridGeneration struct {
	ID          int64  `json:"id"`
	Model       string `json:"model"`
	Dimension   int    `json:"dimension"`
	Fingerprint string `json:"fingerprint"`
	State       string `json:"state"`
}

type hybridGenerationSummary = HybridGeneration

// searchMessageBodiesResponse is the paginated body for search_message_bodies.
// It is returned for all modes (keyword, vector, hybrid); Mode/PoolSaturated/Generation
// are only meaningful for vector/hybrid.
type searchMessageBodiesResponse struct {
	paginatedResponse[searchMessageItem]

	Mode          string                  `json:"mode"`
	PoolSaturated bool                    `json:"pool_saturated"`
	Generation    hybridGenerationSummary `json:"generation"`
}

// searchMessageBodiesHybrid runs vector or hybrid search via the configured
// hybrid engine. Mirrors api/handlers.go handleHybridSearch: returns
// descriptive errors when the engine is not configured or the index is
// stale/building, otherwise returns RRF-ranked hits hydrated via
// GetMessageSummariesByIDs (body omitted — use search_message_bodies or
// search_in_message for body content).
func (h *handlers) searchMessageBodiesHybrid(
	ctx context.Context, args map[string]any,
	queryStr string, parsed *search.Query, mode string, explain bool,
) (*mcp.CallToolResult, error) {
	if h.hybridSearcher != nil {
		return h.searchMessageBodiesHybridViaSearcher(ctx, args, queryStr, parsed, mode, explain)
	}
	if h.hybridEngine == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}

	// Resolve account filter to a source ID for the structured Filter.
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	freeText := strings.Join(parsed.TextTerms, " ")

	// mode=vector|hybrid requires at least one free-text term; filter-only
	// queries have no query vector to rank by. Callers that want pure
	// structured filtering should omit mode (metadata search).
	if freeText == "" {
		return mcp.NewToolResultError(
			"missing_free_text: mode=" + mode +
				" requires at least one free-text term; use search_metadata for filter-only queries",
		), nil
	}

	subjectTerms := make([]string, 0, len(parsed.TextTerms))
	for _, t := range parsed.TextTerms {
		subjectTerms = append(subjectTerms, strings.ToLower(t))
	}

	filter, err := h.hybridEngine.BuildFilter(ctx, parsed)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("filter resolution failed: %v", err)), nil
	}
	if sourceID != nil {
		filter.SourceIDs = []int64{*sourceID}
	}

	maxPage := h.vectorCfg.Search.MaxPageSizeHybridClamp()
	requestedEnd := offset + limit
	wantedFetch := requestedEnd + 1 // probe one past the page end for has_more
	fetchLimit := wantedFetch
	hitMaxPageCap := false
	if maxPage > 0 {
		if offset >= maxPage {
			return mcp.NewToolResultError(fmt.Sprintf(
				"pagination_limit: offset %d exceeds hybrid ranking window (max %d); "+
					"use search_metadata or search_message_bodies for deeper pagination",
				offset, maxPage,
			)), nil
		}
		if fetchLimit > maxPage {
			fetchLimit = maxPage
			hitMaxPageCap = wantedFetch > maxPage
		}
	}

	req := hybrid.SearchRequest{
		Mode:         hybrid.Mode(mode),
		FreeText:     freeText,
		Filter:       filter,
		Limit:        fetchLimit,
		SubjectTerms: subjectTerms,
		Explain:      explain,
	}

	hits, meta, err := h.hybridEngine.Search(ctx, req)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Bulk-hydrate hits in one round-trip instead of looping
	// GetMessage per result (which fetches body, From, To, Cc, Bcc,
	// labels, and attachments for each id and was the dominant search
	// latency cost).
	hitIDs := make([]int64, len(hits))
	for i, h := range hits {
		hitIDs[i] = h.MessageID
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, hitIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate hybrid hits failed: ids=%d error=%v\n",
			len(hitIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	items := make([]searchMessageItem, 0, len(hits))
	for _, hit := range hits {
		msg, ok := byID[hit.MessageID]
		if !ok {
			continue
		}
		item := searchMessageItem{MessageSummary: msg}
		if explain {
			sb := &hybridScoreBreakdown{SubjectBoosted: hit.SubjectBoosted}
			if !math.IsNaN(hit.RRFScore) {
				v := hit.RRFScore
				sb.RRF = &v
			}
			if !math.IsNaN(hit.BM25Score) {
				v := hit.BM25Score
				sb.BM25 = &v
			}
			if !math.IsNaN(hit.VectorScore) {
				v := hit.VectorScore
				sb.Vector = &v
			}
			item.Score = sb
		}
		items = append(items, item)
	}

	var page []searchMessageItem
	if offset < len(items) {
		end := min(offset+limit, len(items))
		page = items[offset:end]
	}

	minScore := floatArg(args, "min_score", 0)
	h.attachVectorChunkMatches(ctx, meta.Generation.ID, meta.QueryVector, page, minScore)

	nextPageServable := maxPage == 0 || requestedEnd < maxPage
	hasMore := false
	if nextPageServable {
		if requestedEnd < len(items) {
			hasMore = true
		} else if !hitMaxPageCap && meta.PoolSaturated && len(hits) >= fetchLimit {
			hasMore = true
		}
	}

	return jsonResult(searchMessageBodiesResponse{
		paginatedResponse: newPaginatedResponseNoTotal(page, offset, hasMore),
		Mode:              mode,
		PoolSaturated:     meta.PoolSaturated,
		Generation: hybridGenerationSummary{
			ID:          int64(meta.Generation.ID),
			Model:       meta.Generation.Model,
			Dimension:   meta.Generation.Dimension,
			Fingerprint: meta.Generation.Fingerprint,
			State:       string(meta.Generation.State),
		},
	})
}

func (h *handlers) searchMessageBodiesHybridViaSearcher(
	ctx context.Context, args map[string]any,
	queryStr string, parsed *search.Query, mode string, explain bool,
) (*mcp.CallToolResult, error) {
	limit := searchLimitArg(args)
	offset := limitArg(args, "offset", 0)

	freeText := strings.Join(parsed.TextTerms, " ")
	if freeText == "" {
		return mcp.NewToolResultError(
			"missing_free_text: mode=" + mode +
				" requires at least one free-text term; use search_metadata for filter-only queries",
		), nil
	}

	account, _ := args["account"].(string)
	result, err := h.hybridSearcher.SearchHybrid(ctx, HybridSearchRequest{
		Query:          queryStr,
		Mode:           mode,
		Account:        account,
		Limit:          limit,
		Offset:         offset,
		IncludeMatches: true,
		MinScore:       floatArg(args, "min_score", 0),
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}
	if result == nil {
		result = &HybridSearchResult{}
	}

	hits := result.Hits
	hasMore := result.HasMore
	pageHits := hits

	hitIDs := make([]int64, len(pageHits))
	for i, hit := range pageHits {
		hitIDs[i] = hit.ID
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, hitIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate daemon hybrid hits failed: ids=%d error=%v\n",
			len(hitIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}

	items := make([]searchMessageItem, 0, len(pageHits))
	for _, hit := range pageHits {
		msg, ok := byID[hit.ID]
		if !ok {
			continue
		}
		item := searchMessageItem{MessageSummary: msg}
		if explain {
			item.Score = &hybridScoreBreakdown{
				RRF:            hit.RRFScore,
				BM25:           hit.BM25Score,
				Vector:         hit.VectorScore,
				SubjectBoosted: hit.SubjectBoosted,
			}
		}
		if len(hit.Matches) > 0 {
			item.Matches = make([]messageMatch, len(hit.Matches))
			for i, match := range hit.Matches {
				score := match.Score
				item.Matches[i] = messageMatch{
					CharOffset: match.CharOffset,
					Snippet:    match.Snippet,
					Line:       match.Line,
					Score:      &score,
				}
			}
		}
		item.MatchesTruncated = hit.MatchesTruncated
		items = append(items, item)
	}

	return jsonResult(searchMessageBodiesResponse{
		paginatedResponse: newPaginatedResponseNoTotal(items, offset, hasMore),
		Mode:              mode,
		PoolSaturated:     result.PoolSaturated,
		Generation:        result.Generation,
	})
}

// similarMessagesResponse is the full response body for
// find_similar_messages.
type similarMessagesResponse struct {
	SeedMessageID int64                   `json:"seed_message_id"`
	Returned      int                     `json:"returned"`
	Generation    hybridGenerationSummary `json:"generation"`
	Messages      []query.MessageSummary  `json:"messages"`
}

// findSimilarMessages returns nearest-neighbour messages to a seed
// message using the active vector index. The seed is excluded from
// results. Structured filters (account, after, before, has_attachment)
// are applied at the backend level.
func (h *handlers) findSimilarMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if h.similarSearcher != nil {
		return h.findSimilarMessagesViaSearcher(ctx, req)
	}
	if h.backend == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}
	args := req.GetArguments()

	seedID, err := getIDArg(args, "message_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := limitArg(args, "limit", 20)
	if maxPage := h.vectorCfg.Search.MaxPageSizeHybridClamp(); maxPage > 0 && limit > maxPage {
		limit = maxPage
	}

	filter, err := h.filterFromFindSimilarArgs(ctx, args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	active, err := vector.ResolveActiveForFingerprint(ctx, h.backend, h.vectorCfg.GenerationFingerprint())
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("active generation: %v", err)), nil
	}
	if err := hybrid.ValidateBuildScope(h.vectorCfg.Embed.Scope.BuildScope(), filter); err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(err.Error()), nil
	}

	seed, err := h.backend.LoadVector(ctx, seedID)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("load seed vector: %v", err)), nil
	}

	// +1 so we can drop the seed itself from results without coming up short.
	hits, err := h.backend.Search(ctx, active.ID, seed, limit+1, filter)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
	}

	// Bulk-hydrate keeping rank order. Drop the seed first so the +1
	// over-fetch is paid for in the size budget rather than the
	// hydration round-trip.
	wantIDs := make([]int64, 0, limit)
	for _, hit := range hits {
		if hit.MessageID == seedID {
			continue
		}
		if len(wantIDs) >= limit {
			break
		}
		wantIDs = append(wantIDs, hit.MessageID)
	}
	summaries, err := h.engine.GetMessageSummariesByIDs(ctx, wantIDs)
	if err != nil {
		fmt.Fprintf(os.Stderr,
			"mcp: hydrate similar hits failed: ids=%d error=%v\n",
			len(wantIDs), err)
		summaries = nil
	}
	byID := make(map[int64]query.MessageSummary, len(summaries))
	for _, s := range summaries {
		byID[s.ID] = s
	}
	messages := make([]query.MessageSummary, 0, len(wantIDs))
	for _, id := range wantIDs {
		if msg, ok := byID[id]; ok {
			messages = append(messages, msg)
		}
	}

	return jsonResult(similarMessagesResponse{
		SeedMessageID: seedID,
		Returned:      len(messages),
		Generation: hybridGenerationSummary{
			ID:          int64(active.ID),
			Model:       active.Model,
			Dimension:   active.Dimension,
			Fingerprint: active.Fingerprint,
			State:       string(active.State),
		},
		Messages: messages,
	})
}

func (h *handlers) findSimilarMessagesViaSearcher(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	seedID, err := getIDArg(args, "message_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	limit := limitArg(args, "limit", 20)
	if limit < 1 {
		limit = 20
	}
	account, _ := args["account"].(string)
	messageType, _ := args["message_type"].(string)
	after, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	before, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	var hasAttachment *bool
	if v, ok := args["has_attachment"].(bool); ok {
		hasAttachment = &v
	}

	result, err := h.similarSearcher.FindSimilar(ctx, SimilarSearchRequest{
		MessageID:     seedID,
		Limit:         limit,
		Account:       account,
		MessageType:   messageType,
		After:         after,
		Before:        before,
		HasAttachment: hasAttachment,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("find similar failed: %v", err)), nil
	}
	if result == nil {
		result = &SimilarSearchResult{SeedMessageID: seedID}
	}
	if result.SeedMessageID == 0 {
		result.SeedMessageID = seedID
	}

	return jsonResult(similarMessagesResponse{
		SeedMessageID: result.SeedMessageID,
		Returned:      len(result.Messages),
		Generation:    result.Generation,
		Messages:      result.Messages,
	})
}

// filterFromFindSimilarArgs builds a vector.Filter from the
// find_similar_messages args. Returns an error if account lookup fails.
// Sender/label filters are intentionally not exposed — resolving
// participant/label names to IDs requires a main-DB handle that the
// MCP handlers struct does not currently hold. A future task that
// wires the DB through can extend both the schema and this helper.
func (h *handlers) filterFromFindSimilarArgs(ctx context.Context, args map[string]any) (vector.Filter, error) {
	var f vector.Filter

	account, _ := args["account"].(string)
	srcID, err := h.getAccountID(ctx, account)
	if err != nil {
		return f, err
	}
	if srcID != nil {
		f.SourceIDs = []int64{*srcID}
	}
	if messageType, _ := args["message_type"].(string); messageType != "" {
		f.MessageTypes = vector.NewBuildScope([]string{messageType}).MessageTypes
	}

	if v, ok := args["has_attachment"].(bool); ok && v {
		tr := true
		f.HasAttachment = &tr
	}
	after, err := getDateArg(args, "after")
	if err != nil {
		return f, err
	}
	if after != nil {
		f.After = after
	}
	before, err := getDateArg(args, "before")
	if err != nil {
		return f, err
	}
	if before != nil {
		f.Before = before
	}
	return f, nil
}

// bodyByteSliceRange returns a UTF-8-safe subslice of body[start:end] and the
// adjusted byte offsets actually used. adjEnd is exclusive; callers use it for
// has_more and sequential paging via offset += body_returned.
func bodyByteSliceRange(body string, start, end int) (text string, adjStart, adjEnd int) {
	if start < 0 {
		start = 0
	}
	if end > len(body) {
		end = len(body)
	}
	if start >= len(body) {
		return "", len(body), len(body)
	}
	if start >= end {
		return oneRuneSlice(body, start)
	}

	adjStart, adjEnd = start, end
	for adjStart < adjEnd && !utf8.RuneStart(body[adjStart]) {
		adjStart++
	}
	for adjEnd > adjStart && adjEnd < len(body) && !utf8.RuneStart(body[adjEnd]) {
		adjEnd--
	}
	for adjEnd > adjStart {
		s := body[adjStart:adjEnd]
		if utf8.ValidString(s) {
			return s, adjStart, adjEnd
		}
		adjEnd--
	}
	return oneRuneSlice(body, adjStart)
}

// oneRuneSlice returns a single rune starting at or after start so tiny windows
// and mid-rune offsets still advance sequential paging.
func oneRuneSlice(body string, start int) (text string, adjStart, adjEnd int) {
	adjStart = start
	for adjStart < len(body) && !utf8.RuneStart(body[adjStart]) {
		adjStart++
	}
	if adjStart >= len(body) {
		return "", len(body), len(body)
	}
	_, size := utf8.DecodeRuneInString(body[adjStart:])
	if size <= 0 {
		return "", adjStart, adjStart
	}
	adjEnd = min(len(body), adjStart+size)
	return body[adjStart:adjEnd], adjStart, adjEnd
}

// bodyByteSlice returns body[start:end], nudging boundaries inward so the
// result is always valid UTF-8. MCP body APIs use byte offsets; without
// this, a window can split a multibyte rune (emoji, CJK, accented letters).
func bodyByteSlice(body string, start, end int) string {
	text, _, _ := bodyByteSliceRange(body, start, end)
	return text
}

// contextWindow returns byte offsets [start, end) for a window of up to
// contextChars bytes centered on a match at pos with byte length termLen.
func contextWindow(bodyLen, pos, termLen, contextChars int) (start, end int) {
	start = pos - (contextChars-termLen)/2
	end = start + contextChars
	if start < 0 {
		start = 0
		end = min(bodyLen, contextChars)
	} else if end > bodyLen {
		end = bodyLen
		start = max(0, end-contextChars)
	}
	return start, end
}

func lineNumberAt(body string, byteOffset int) int {
	if byteOffset <= 0 {
		return 1
	}
	if byteOffset > len(body) {
		byteOffset = len(body)
	}
	return 1 + strings.Count(body[:byteOffset], "\n")
}

type getMessageResponse struct {
	ID                   int64                  `json:"id"`
	SourceMessageID      string                 `json:"source_message_id"`
	ConversationID       int64                  `json:"conversation_id"`
	SourceConversationID string                 `json:"source_conversation_id"`
	Subject              string                 `json:"subject"`
	MessageType          string                 `json:"message_type,omitempty"`
	Snippet              string                 `json:"snippet"`
	SentAt               time.Time              `json:"sent_at"`
	ReceivedAt           *time.Time             `json:"received_at,omitempty"`
	DeletedAt            *time.Time             `json:"deleted_at,omitempty"`
	SizeEstimate         int64                  `json:"size_estimate"`
	HasAttachments       bool                   `json:"has_attachments"`
	From                 []query.Address        `json:"from"`
	To                   []query.Address        `json:"to"`
	Cc                   []query.Address        `json:"cc"`
	Bcc                  []query.Address        `json:"bcc"`
	BodyText             string                 `json:"body_text"`
	BodyHTML             string                 `json:"body_html"`
	BodyFormat           string                 `json:"body_format,omitempty"`
	BodyLength           int                    `json:"body_length"`
	BodyReturned         int                    `json:"body_returned"`
	Offset               int                    `json:"offset"`
	HasMore              bool                   `json:"has_more"`
	Labels               []string               `json:"labels"`
	Attachments          []query.AttachmentInfo `json:"attachments"`
}

func (h *handlers) getMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}
	if msg == nil {
		return mcp.NewToolResultError("message not found"), nil
	}

	maxChars := intArg(args, "max_chars", defaultBodyChars)
	if maxChars <= 0 {
		maxChars = defaultBodyChars
	} else if maxChars > maxBodyChars {
		maxChars = maxBodyChars
	}

	requestedBodyFormat, _ := args["body_format"].(string)
	if requestedBodyFormat == "" {
		requestedBodyFormat = bodyFormatAuto
	}

	fullBody := msg.BodyText
	bodyFormat := bodyFormatText
	switch requestedBodyFormat {
	case bodyFormatAuto:
		if fullBody == "" && msg.BodyHTML != "" {
			fullBody = msg.BodyHTML
			bodyFormat = bodyFormatHTML
		}
	case bodyFormatText:
	case bodyFormatHTML:
		fullBody = msg.BodyHTML
		bodyFormat = bodyFormatHTML
	default:
		return mcp.NewToolResultError("body_format must be one of auto, text, html"), nil
	}
	bodyLen := len(fullBody)

	var start, end int
	fullBodyRequested, _ := args["full_body"].(bool)
	if fullBodyRequested {
		start, end = 0, bodyLen
	} else if centerAt := intArg(args, "center_at", -1); centerAt >= 0 {
		// Center the window on the given byte offset. contextWindow handles
		// clamping to body boundaries.
		start, end = contextWindow(bodyLen, centerAt, 0, maxChars)
	} else {
		start = min(intArg(args, "offset", 0), bodyLen)
		end = min(start+maxChars, bodyLen)
	}

	bodySlice, sliceStart, sliceEnd := bodyByteSliceRange(fullBody, start, end)
	bodyText := bodySlice
	bodyHTML := ""
	if bodyFormat == bodyFormatHTML {
		bodyText = ""
		bodyHTML = bodySlice
	}

	return jsonResult(getMessageResponse{
		ID:                   msg.ID,
		SourceMessageID:      msg.SourceMessageID,
		ConversationID:       msg.ConversationID,
		SourceConversationID: msg.SourceConversationID,
		Subject:              msg.Subject,
		MessageType:          msg.MessageType,
		Snippet:              msg.Snippet,
		SentAt:               msg.SentAt,
		ReceivedAt:           msg.ReceivedAt,
		DeletedAt:            msg.DeletedAt,
		SizeEstimate:         msg.SizeEstimate,
		HasAttachments:       msg.HasAttachments,
		From:                 msg.From,
		To:                   msg.To,
		Cc:                   msg.Cc,
		Bcc:                  msg.Bcc,
		BodyText:             bodyText,
		BodyHTML:             bodyHTML,
		BodyFormat:           bodyFormat,
		BodyLength:           bodyLen,
		BodyReturned:         len(bodySlice),
		Offset:               sliceStart,
		HasMore:              sliceEnd < bodyLen,
		Labels:               msg.Labels,
		Attachments:          msg.Attachments,
	})
}

func (h *handlers) attachVectorChunkMatches(
	ctx context.Context,
	genID vector.GenerationID,
	queryVec []float32,
	items []searchMessageItem,
	minScore float64,
) {
	scorer, ok := h.backend.(vector.ChunkScoringBackend)
	if !ok || len(queryVec) == 0 || len(items) == 0 {
		return
	}
	for i := range items {
		msg, err := h.engine.GetMessage(ctx, items[i].ID)
		if err != nil || msg == nil {
			continue
		}
		chunkHits, err := scorer.ScoreMessageChunks(ctx, genID, msg.ID, queryVec)
		if err != nil {
			continue
		}
		matches, truncated := chunkmatch.Build(
			msg.Subject, embed.BodyTextForEmbedding(msg.BodyText, msg.BodyHTML), h.vectorCfg, chunkHits,
			minScore, maxContextSnippets, searchContextChars,
		)
		items[i].Matches = messageMatchesFromChunks(matches)
		items[i].MatchesTruncated = truncated
	}
}

func (h *handlers) vectorMatchesInMessage(
	ctx context.Context,
	messageID int64,
	queryStr string,
	minScore float64,
	limit, offset int,
) (*mcp.CallToolResult, error) {
	if h.hybridEngine == nil || h.backend == nil {
		return mcp.NewToolResultError(
			"vector_not_enabled: vector search is not configured on this server",
		), nil
	}
	scorer, ok := h.backend.(vector.ChunkScoringBackend)
	if !ok {
		return mcp.NewToolResultError(
			"vector_not_enabled: chunk scoring is not available on this backend",
		), nil
	}

	active, err := vector.ResolveActiveForFingerprint(ctx, h.backend, h.vectorCfg.GenerationFingerprint())
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("vector index: %v", err)), nil
	}

	queryVec, err := h.hybridEngine.EmbedQuery(ctx, queryStr)
	if err != nil {
		if r := translateVectorErr(err); r != nil {
			return r, nil
		}
		return mcp.NewToolResultError(fmt.Sprintf("embed query: %v", err)), nil
	}

	msg, err := h.engine.GetMessage(ctx, messageID)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}
	if msg == nil {
		return mcp.NewToolResultError("message not found"), nil
	}

	chunkHits, err := scorer.ScoreMessageChunks(ctx, active.ID, messageID, queryVec)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("score chunks: %v", err)), nil
	}

	chunkMatches, _ := chunkmatch.Build(
		msg.Subject, embed.BodyTextForEmbedding(msg.BodyText, msg.BodyHTML), h.vectorCfg, chunkHits,
		minScore, len(chunkHits), searchContextChars,
	)
	allMatches := messageMatchesFromChunks(chunkMatches)

	total := int64(len(allMatches))
	if offset >= len(allMatches) {
		return jsonResult(newPaginatedResponse([]messageMatch{}, total, offset))
	}
	end := min(offset+limit, len(allMatches))
	page := allMatches[offset:end]
	// Re-cap page length after pagination.
	if len(page) > limit {
		page = page[:limit]
	}
	return jsonResult(newPaginatedResponse(page, total, offset))
}

func (h *handlers) searchInMessage(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	queryStr, _ := args["query"].(string)
	queryStr = strings.TrimSpace(queryStr)
	if queryStr == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	mode, _ := args["mode"].(string)
	limit := limitArg(args, "limit", 10)
	offset := limitArg(args, "offset", 0)

	switch mode {
	case "", "keyword":
		// default: literal term search
	case searchModeVector:
		return h.vectorMatchesInMessage(ctx, id, queryStr, floatArg(args, "min_score", 0), limit, offset)
	default:
		return mcp.NewToolResultError(
			fmt.Sprintf("invalid mode %q: must be keyword (default) or %s", mode, searchModeVector),
		), nil
	}

	msg, err := h.engine.GetMessage(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("message not found: %v", err)), nil
	}
	if msg == nil {
		return mcp.NewToolResultError("message not found"), nil
	}

	allMatches := findTermMatches(msg.BodyText, queryStr)
	total := int64(len(allMatches))
	if offset >= len(allMatches) {
		return jsonResult(newPaginatedResponse([]messageMatch{}, total, offset))
	}
	end := min(offset+limit, len(allMatches))
	return jsonResult(newPaginatedResponse(allMatches[offset:end], total, offset))
}

func findTermMatches(body, term string) []messageMatch {
	if body == "" || term == "" {
		return nil
	}
	lowerBody := strings.ToLower(body)
	lowerTerm := strings.ToLower(term)
	termLen := len(term)
	var matches []messageMatch
	searchFrom := 0
	for {
		idx := strings.Index(lowerBody[searchFrom:], lowerTerm)
		if idx < 0 {
			break
		}
		pos := searchFrom + idx
		searchFrom = pos + 1
		start, end := contextWindow(len(body), pos, termLen, searchContextChars)
		charOffset := pos
		line := lineNumberAt(body, pos)
		matches = append(matches, messageMatch{
			CharOffset: &charOffset,
			Snippet:    bodyByteSlice(body, start, end),
			Line:       &line,
		})
	}
	return matches
}

const maxAttachmentSize = 50 * 1024 * 1024 // 50MB

func (h *handlers) getAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "attachment_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	att, err := h.engine.GetAttachment(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get attachment failed: %v", err)), nil
	}
	if att == nil {
		return mcp.NewToolResultError("attachment not found"), nil
	}

	if h.attachmentReader == nil && h.attachmentsDir == "" {
		return mcp.NewToolResultError("attachments directory not configured"), nil
	}

	if att.Size > maxAttachmentSize {
		return mcp.NewToolResultError(fmt.Sprintf("attachment too large: %d bytes (max %d)", att.Size, maxAttachmentSize)), nil
	}

	data, err := h.readAttachment(ctx, att.ContentHash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	mimeType := att.MimeType
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	metaObj := struct {
		Filename string `json:"filename"`
		MimeType string `json:"mime_type"`
		Size     int64  `json:"size"`
	}{
		Filename: att.Filename,
		MimeType: mimeType,
		Size:     att.Size,
	}
	metaJSON, err := json.Marshal(metaObj)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal metadata: %v", err)), nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			mcp.TextContent{
				Type: "text",
				Text: string(metaJSON),
			},
			mcp.EmbeddedResource{
				Type: "resource",
				Resource: mcp.BlobResourceContents{
					URI:      fmt.Sprintf("attachment:///%d/%s", att.ID, url.PathEscape(att.Filename)),
					MIMEType: mimeType,
					Blob:     base64.StdEncoding.EncodeToString(data),
				},
			},
		},
	}, nil
}

func (h *handlers) exportAttachment(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	id, err := getIDArg(args, "attachment_id")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	att, err := h.engine.GetAttachment(ctx, id)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("get attachment failed: %v", err)), nil
	}
	if att == nil {
		return mcp.NewToolResultError("attachment not found"), nil
	}

	if h.attachmentReader == nil && h.attachmentsDir == "" {
		return mcp.NewToolResultError("attachments directory not configured"), nil
	}

	if att.Size > maxAttachmentSize {
		return mcp.NewToolResultError(fmt.Sprintf("attachment too large: %d bytes (max %d)", att.Size, maxAttachmentSize)), nil
	}

	data, err := h.readAttachment(ctx, att.ContentHash)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Determine destination directory.
	destDir, _ := args["destination"].(string)
	if destDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("cannot determine home directory: %v", err)), nil
		}
		destDir = filepath.Join(home, "Downloads")
	}

	info, err := os.Stat(destDir)
	if err != nil || !info.IsDir() {
		return mcp.NewToolResultError("destination directory does not exist: " + destDir), nil //nolint:nilerr // MCP convention: tool errors flow via ToolResultError, not Go error
	}

	// Sanitize and deduplicate filename.
	filename := export.SanitizeFilename(filepath.Base(att.Filename))
	if filename == "" || filename == "." {
		filename = att.ContentHash
	}
	f, outPath, err := export.CreateExclusiveFile(filepath.Join(destDir, filename), 0600)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", err)), nil
	}
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		_ = os.Remove(outPath)
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", writeErr)), nil
	}
	if closeErr != nil {
		_ = os.Remove(outPath)
		return mcp.NewToolResultError(fmt.Sprintf("write failed: %v", closeErr)), nil
	}

	resp := struct {
		Path     string `json:"path"`
		Filename string `json:"filename"`
		Size     int64  `json:"size"`
	}{
		Path:     outPath,
		Filename: filepath.Base(outPath),
		Size:     int64(len(data)),
	}
	return jsonResult(resp)
}

func (h *handlers) listMessages(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	filter := query.MessageFilter{
		SourceID: sourceID,
		Pagination: query.Pagination{
			Limit:  listLimitArg(args) + 1,
			Offset: limitArg(args, "offset", 0),
		},
	}

	if v, ok := args["from"].(string); ok && v != "" {
		// If it looks like an email address, filter by email; otherwise by display name.
		if strings.Contains(v, "@") || strings.HasPrefix(v, "+") {
			filter.Sender = v
		} else {
			filter.SenderName = v
		}
	}
	if v, ok := args["to"].(string); ok && v != "" {
		filter.Recipient = v
	}
	if v, ok := args["label"].(string); ok && v != "" {
		filter.Label = v
	}
	if v, ok := args["has_attachment"].(bool); ok && v {
		filter.WithAttachmentsOnly = true
	}
	if filter.After, err = getDateArg(args, "after"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if filter.Before, err = getDateArg(args, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if v, ok := args["conversation_id"].(float64); ok && v != 0 {
		v2 := int64(v)
		filter.ConversationID = &v2
	}

	results, err := h.engine.ListMessages(ctx, filter)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("list failed: %v", err)), nil
	}

	pageLimit := listLimitArg(args)
	offset := filter.Pagination.Offset
	hasMore := len(results) > pageLimit
	if hasMore {
		results = results[:pageLimit]
	}

	return jsonResult(newPaginatedResponseNoTotal(results, offset, hasMore))
}

// getStatsResponse is the JSON body returned by the get_stats MCP tool.
// VectorSearch is omitempty so archives without vector search do not
// surface an empty sub-object to callers.
type getStatsResponse struct {
	Stats        *query.TotalStats   `json:"stats"`
	Accounts     []query.AccountInfo `json:"accounts"`
	VectorSearch *vector.StatsView   `json:"vector_search,omitempty"`
}

func (h *handlers) getStats(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	stats, err := h.engine.GetTotalStats(ctx, query.StatsOptions{})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("stats failed: %v", err)), nil
	}

	accounts, err := h.engine.ListAccounts(ctx)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("accounts failed: %v", err)), nil
	}

	// Vector stats are best-effort: partial failures are logged here but
	// still attached to the response so callers see whatever succeeded.
	vs, vsErr := vector.CollectStats(ctx, h.backend)
	if vsErr != nil {
		fmt.Fprintf(os.Stderr, "mcp: vector stats failed: %v\n", vsErr)
	}

	return jsonResult(getStatsResponse{
		Stats:        stats,
		Accounts:     accounts,
		VectorSearch: vs,
	})
}

func (h *handlers) aggregate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	groupBy, _ := args["group_by"].(string)
	if groupBy == "" {
		return mcp.NewToolResultError("group_by parameter is required"), nil
	}

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	opts := query.AggregateOptions{
		SourceID: sourceID,
		Limit:    limitArg(args, "limit", 50),
	}

	if opts.After, err = getDateArg(args, "after"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if opts.Before, err = getDateArg(args, "before"); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	viewTypeMap := map[string]query.ViewType{
		"sender":    query.ViewSenders,
		"recipient": query.ViewRecipients,
		"domain":    query.ViewDomains,
		"label":     query.ViewLabels,
		"time":      query.ViewTime,
	}

	viewType, ok := viewTypeMap[groupBy]
	if !ok {
		return mcp.NewToolResultError("invalid group_by: " + groupBy), nil
	}

	rows, err := h.engine.Aggregate(ctx, viewType, opts)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("aggregate failed: %v", err)), nil
	}

	return jsonResult(rows)
}

// limitArg extracts a non-negative integer limit from a map, with a default.
// JSON numbers arrive as float64. Clamps to maxLimit to prevent excessive
// result sets.
// intArg extracts a non-negative integer from args without the maxLimit clamp
// used by limitArg. Suitable for body-text offsets and similar unbounded values.
func intArg(args map[string]any, key string, def int) int {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	if math.IsNaN(v) || v < 0 || math.IsInf(v, 1) || v > float64(math.MaxInt) {
		return def
	}
	return int(v)
}

func limitArg(args map[string]any, key string, def int) int {
	v, ok := args[key].(float64)
	if !ok {
		return def
	}
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if math.IsInf(v, 1) || v > float64(maxLimit) {
		return maxLimit
	}
	return int(v)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("marshal error: %v", err)), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

// maxStageDeletionResults limits how many messages can be staged in one call.
const maxStageDeletionResults = 100000

func (h *handlers) stageDeletion(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	// Look up account filter
	account, _ := args["account"].(string)
	sourceID, err := h.getAccountID(ctx, account)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	// Check for query vs structured filters
	queryStr, _ := args["query"].(string)
	queryStr = strings.TrimSpace(queryStr)
	hasQuery := queryStr != ""

	// Check for any structured filter
	fromStr, _ := args["from"].(string)
	domainStr, _ := args["domain"].(string)
	labelStr, _ := args["label"].(string)
	hasAttachment, _ := args["has_attachment"].(bool)
	afterDate, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	beforeDate, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	hasStructuredFilter := fromStr != "" || domainStr != "" || labelStr != "" ||
		hasAttachment || afterDate != nil || beforeDate != nil

	// Validate: must have either query or structured filters, but not both
	if hasQuery && hasStructuredFilter {
		return mcp.NewToolResultError("use either 'query' or structured filters (from, domain, label, etc.), not both"), nil
	}
	if !hasQuery && !hasStructuredFilter {
		return mcp.NewToolResultError("must provide either 'query' or at least one filter (from, domain, label, after, before, has_attachment)"), nil
	}

	var gmailIDs []string
	var description string

	if hasQuery {
		// Query-based search
		q := search.Parse(queryStr)
		if msg := unsupportedSearchOperatorMessage(q); msg != "" {
			return mcp.NewToolResultError(msg), nil
		}
		if sourceID != nil {
			q.AccountIDs = []int64{*sourceID}
		}

		// Try fast search first
		filter := query.MessageFilter{SourceID: sourceID}
		results, err := h.engine.SearchFast(ctx, q, filter, maxStageDeletionResults, 0)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
		}

		// Fall back to FTS if no results and query has text terms
		if len(results) == 0 && len(q.TextTerms) > 0 {
			results, err = h.engine.Search(ctx, q, maxStageDeletionResults, 0)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("search failed: %v", err)), nil
			}
		}

		for _, msg := range results {
			gmailIDs = append(gmailIDs, msg.SourceMessageID)
		}
		description = "query: " + queryStr
		if len(description) > 50 {
			description = description[:50]
		}
	} else {
		// Structured filter
		filter := query.MessageFilter{
			SourceID:            sourceID,
			Sender:              fromStr,
			Domain:              domainStr,
			Label:               labelStr,
			WithAttachmentsOnly: hasAttachment,
			After:               afterDate,
			Before:              beforeDate,
			Pagination: query.Pagination{
				Limit: maxStageDeletionResults,
			},
		}

		var err error
		gmailIDs, err = h.engine.GetGmailIDsByFilter(ctx, filter)
		if err != nil {
			return mcp.NewToolResultError(fmt.Sprintf("filter failed: %v", err)), nil
		}

		// Build description from filters
		var parts []string
		if fromStr != "" {
			parts = append(parts, "from:"+fromStr)
		}
		if domainStr != "" {
			parts = append(parts, "domain:"+domainStr)
		}
		if labelStr != "" {
			parts = append(parts, "label:"+labelStr)
		}
		if hasAttachment {
			parts = append(parts, "has:attachment")
		}
		if afterDate != nil {
			parts = append(parts, "after:"+afterDate.Format("2006-01-02"))
		}
		if beforeDate != nil {
			parts = append(parts, "before:"+beforeDate.Format("2006-01-02"))
		}
		description = "filter: " + strings.Join(parts, " ")
		if len(description) > 50 {
			description = description[:50]
		}
	}

	if len(gmailIDs) == 0 {
		return mcp.NewToolResultError("no messages match the specified criteria"), nil
	}

	manifest := deletion.NewManifest(description, gmailIDs)
	manifest.CreatedBy = "mcp"

	// Set filter metadata for execution
	manifest.Filters.Account = account
	if fromStr != "" {
		manifest.Filters.Senders = []string{fromStr}
	}
	if domainStr != "" {
		manifest.Filters.SenderDomains = []string{domainStr}
	}
	if labelStr != "" {
		manifest.Filters.Labels = []string{labelStr}
	}
	if afterDate != nil {
		manifest.Filters.After = afterDate.Format("2006-01-02")
	}
	if beforeDate != nil {
		manifest.Filters.Before = beforeDate.Format("2006-01-02")
	}

	if err := h.saveDeletionManifest(ctx, manifest); err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("save manifest: %v", err)), nil
	}

	resp := struct {
		BatchID      string `json:"batch_id"`
		MessageCount int    `json:"message_count"`
		Status       string `json:"status"`
		NextStep     string `json:"next_step"`
	}{
		BatchID:      manifest.ID,
		MessageCount: len(gmailIDs),
		Status:       string(manifest.Status),
		NextStep:     "Run 'MSGVAULT_ENABLE_REMOTE_DELETE=1 msgvault delete-staged' to execute deletion (gated for v1), or 'msgvault cancel-deletion " + manifest.ID + "' to cancel",
	}

	return jsonResult(resp)
}

func (h *handlers) saveDeletionManifest(ctx context.Context, manifest *deletion.Manifest) error {
	if h.manifestSaver != nil {
		return h.manifestSaver.SaveManifest(ctx, manifest)
	}
	deletionsDir := filepath.Join(h.dataDir, "deletions")
	manager, err := deletion.NewManager(deletionsDir)
	if err != nil {
		return fmt.Errorf("create deletion manager: %w", err)
	}
	return manager.SaveManifest(manifest)
}

func (h *handlers) searchByDomains(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	args := req.GetArguments()

	domainsStr, _ := args["domains"].(string)
	domainsStr = strings.TrimSpace(domainsStr)
	if domainsStr == "" {
		return mcp.NewToolResultError("domains is required"), nil
	}

	// Split and clean domain list
	var domains []string
	for d := range strings.SplitSeq(domainsStr, ",") {
		d = strings.TrimSpace(d)
		if d != "" {
			domains = append(domains, d)
		}
	}
	if len(domains) == 0 {
		return mcp.NewToolResultError("at least one domain is required"), nil
	}

	limit := limitArg(args, "limit", 100)
	offset := limitArg(args, "offset", 0)

	afterDate, err := getDateArg(args, "after")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	beforeDate, err := getDateArg(args, "before")
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	results, err := h.engine.SearchByDomains(ctx, domains, afterDate, beforeDate, limit, offset)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("search by domains failed: %v", err)), nil
	}

	return jsonResult(results)
}
