package store

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
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	documentSearchCursorVersion = 2
	documentSearchRRFConstant   = 60.0
	maxDocumentSearchQueryBytes = 1_024
	maxDocumentSearchTerms      = 20
	maxDocumentSearchPageSize   = 100
	maxDocumentSearchOffset     = 10_000
	maxDocumentSearchCandidates = 10_000
	maxDocumentSearchExcerpt    = 320
)

var (
	ErrDocumentSearchInvalidCursor  = errors.New("invalid document search cursor")
	ErrDocumentSearchCursorStale    = errors.New("document search cursor is stale")
	ErrDocumentSearchInvalidRequest = errors.New("invalid document search request")
	ErrDocumentSearchUnavailable    = errors.New("document search requires full-text search support")
)

type DocumentSearchRequest struct {
	Query        string
	SourceIDs    []int64
	MessageTypes []string
	AttachmentID int64
	MessageID    int64
	PageSize     int
	Cursor       string
}

type DocumentSearchResponse struct {
	Results    []DocumentSearchResult `json:"results"`
	NextCursor string                 `json:"next_cursor,omitempty"`
	Revision   int64                  `json:"revision"`
	Truncated  bool                   `json:"truncated,omitempty"`
}

type DocumentSearchResult struct {
	AttachmentID      int64    `json:"attachment_id"`
	MessageID         int64    `json:"message_id"`
	SourceID          int64    `json:"source_id"`
	OccurrenceKey     string   `json:"occurrence_key"`
	SourcePartKey     string   `json:"source_part_key,omitempty"`
	Filename          string   `json:"filename,omitempty"`
	ContainingTitle   string   `json:"containing_title,omitempty"`
	MIMEType          string   `json:"mime_type,omitempty"`
	CanonicalBlobHash string   `json:"canonical_blob_hash"`
	OtherLiveCopies   int      `json:"other_live_copies"`
	ChunkKey          string   `json:"chunk_key"`
	ChunkOrdinal      int      `json:"chunk_ordinal"`
	HeadingPath       []string `json:"heading_path,omitempty"`
	FirstUnitIndex    int      `json:"first_unit_index"`
	LastUnitIndex     int      `json:"last_unit_index"`
	Excerpt           string   `json:"excerpt"`
	HighlightStart    int      `json:"highlight_start"`
	HighlightEnd      int      `json:"highlight_end"`
	ProfileID         string   `json:"profile_id"`
	ExtractionID      string   `json:"extraction_id"`
	Provider          string   `json:"provider"`
	Model             string   `json:"model"`
	MatchedSignals    []string `json:"matched_signals"`
	Truncated         bool     `json:"truncated"`
	Rank              int      `json:"rank"`
}

type documentSearchCursor struct {
	Version        int    `json:"version"`
	Revision       int64  `json:"revision"`
	RequestHash    string `json:"request_hash"`
	Offset         int    `json:"offset"`
	CandidateLimit int    `json:"candidate_limit"`
}

type documentSearchRow struct {
	DocumentSearchResult

	Text         string
	ContentRank  int
	FilenameRank int
	Score        float64
}

func (s *Store) SearchDocuments(
	ctx context.Context,
	request DocumentSearchRequest,
) (DocumentSearchResponse, error) {
	prepared, terms, requestHash, offset, candidateLimit, revision, err := s.prepareDocumentSearch(ctx, request)
	if err != nil {
		return DocumentSearchResponse{}, err
	}
	if !s.fts5Available {
		return DocumentSearchResponse{}, ErrDocumentSearchUnavailable
	}
	contentRows, contentMore, err := s.searchDocumentContent(ctx, prepared, terms, candidateLimit)
	if err != nil {
		return DocumentSearchResponse{}, err
	}
	filenameRows, filenameMore, err := s.searchDocumentFilenames(ctx, prepared, terms, candidateLimit)
	if err != nil {
		return DocumentSearchResponse{}, err
	}
	rows := fuseDocumentSearchRows(contentRows, filenameRows, terms)
	moreCandidates := contentMore || filenameMore
	response := DocumentSearchResponse{
		Revision:  revision,
		Truncated: moreCandidates && candidateLimit == maxDocumentSearchCandidates,
	}
	if offset >= len(rows) {
		return response, nil
	}
	end := min(offset+prepared.PageSize, len(rows))
	response.Results = make([]DocumentSearchResult, 0, end-offset)
	for index := offset; index < end; index++ {
		result := rows[index].DocumentSearchResult
		result.Rank = index + 1
		response.Results = append(response.Results, result)
	}
	if err := s.populateDocumentLiveCopyCounts(ctx, response.Results); err != nil {
		return DocumentSearchResponse{}, err
	}
	if end < len(rows) {
		response.NextCursor, err = encodeDocumentSearchCursor(documentSearchCursor{
			Version: documentSearchCursorVersion, Revision: revision,
			RequestHash: requestHash, Offset: end, CandidateLimit: candidateLimit,
		})
		if err != nil {
			return DocumentSearchResponse{}, err
		}
	}
	return response, nil
}

func (s *Store) prepareDocumentSearch(
	ctx context.Context,
	request DocumentSearchRequest,
) (DocumentSearchRequest, []string, string, int, int, int64, error) {
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" || len(request.Query) > maxDocumentSearchQueryBytes || !utf8.ValidString(request.Query) {
		return request, nil, "", 0, 0, 0, fmt.Errorf("%w: requires a bounded UTF-8 query", ErrDocumentSearchInvalidRequest)
	}
	terms := strings.Fields(request.Query)
	if len(terms) == 0 || len(terms) > maxDocumentSearchTerms {
		return request, nil, "", 0, 0, 0, fmt.Errorf("%w: query has invalid term count", ErrDocumentSearchInvalidRequest)
	}
	if s.dialect.BuildFTSArg(terms) == "" {
		return request, nil, "", 0, 0, 0, fmt.Errorf("%w: query contains no searchable terms", ErrDocumentSearchInvalidRequest)
	}
	if request.PageSize == 0 {
		request.PageSize = 20
	}
	if request.PageSize < 1 || request.PageSize > maxDocumentSearchPageSize ||
		request.AttachmentID < 0 || request.MessageID < 0 {
		return request, nil, "", 0, 0, 0, fmt.Errorf("%w: request has invalid bounds", ErrDocumentSearchInvalidRequest)
	}
	var err error
	request.Query = strings.ToLower(strings.Join(terms, " "))
	request.SourceIDs, err = sortedUniquePositive(request.SourceIDs)
	if err != nil {
		return request, nil, "", 0, 0, 0, err
	}
	request.MessageTypes, err = sortedUniqueNonempty(request.MessageTypes)
	if err != nil {
		return request, nil, "", 0, 0, 0, err
	}
	requestHash, err := hashDocumentSearchRequest(request)
	if err != nil {
		return request, nil, "", 0, 0, 0, err
	}
	revision, err := s.GetDocumentIndexRevision(ctx)
	if err != nil {
		return request, nil, "", 0, 0, 0, err
	}
	offset := 0
	// RRF ranks are meaningful only for one fixed candidate set. Every page
	// therefore evaluates the same bounded set instead of widening it between
	// cursors, which could reorder earlier results and cause skips or repeats.
	candidateLimit := maxDocumentSearchCandidates
	if request.Cursor != "" {
		cursor, decodeErr := decodeDocumentSearchCursor(request.Cursor)
		if decodeErr != nil {
			return request, nil, "", 0, 0, 0, decodeErr
		}
		if cursor.RequestHash != requestHash {
			return request, nil, "", 0, 0, 0, ErrDocumentSearchInvalidCursor
		}
		if cursor.Revision != revision {
			return request, nil, "", 0, 0, 0, ErrDocumentSearchCursorStale
		}
		offset = cursor.Offset
		if cursor.CandidateLimit != candidateLimit {
			return request, nil, "", 0, 0, 0, ErrDocumentSearchInvalidCursor
		}
	}
	return request, terms, requestHash, offset, candidateLimit, revision, nil
}

func (s *Store) searchDocumentContent(
	ctx context.Context,
	request DocumentSearchRequest,
	terms []string,
	limit int,
) ([]documentSearchRow, bool, error) {
	ftsArg := s.dialect.BuildFTSArg(terms)
	conditions, scopeArgs := documentSearchScope(request, "m", "a")
	validity := documentSearchValidity("p", "c", "h", "o", "a", "m", "ds")
	var query string
	args := make([]any, 0, len(scopeArgs)+2)
	if s.IsPostgreSQL() {
		query = `
			WITH search_query AS (
				SELECT to_tsquery('simple', ?) AS value
			), ranked AS (
				SELECT ` + documentSearchRankedSelectColumns + `,
				       ROW_NUMBER() OVER (
				           PARTITION BY o.occurrence_key
				           ORDER BY ts_rank(dc.search_fts, sq.value) DESC, dc.id
				       ) AS occurrence_rank,
				       ts_rank(dc.search_fts, sq.value) AS search_rank
				FROM document_chunks dc
				JOIN document_extraction_heads h ON h.extraction_id = dc.extraction_id
				JOIN document_extraction_profiles p ON p.id = h.profile_id
				JOIN document_provider_consents c ON c.profile_id = p.id
				JOIN document_occurrences o ON o.canonical_blob_hash = h.canonical_blob_hash
				JOIN attachments a ON a.id = o.attachment_id
				JOIN messages m ON m.id = o.message_id
				JOIN conversations cv ON cv.id = m.conversation_id
				CROSS JOIN document_index_state ds
				CROSS JOIN search_query sq
				WHERE dc.search_fts @@ sq.value
				  AND ` + validity + conditions + `
			)
			SELECT ` + documentSearchOuterColumns + `
			FROM ranked
			WHERE occurrence_rank = 1
			ORDER BY search_rank DESC, occurrence_key
			LIMIT ?`
		args = append(args, ftsArg)
		args = append(args, scopeArgs...)
		args = append(args, limit+1)
	} else {
		query = `
			WITH matched AS MATERIALIZED (
				SELECT ` + documentSearchRankedSelectColumns + `,
				       bm25(document_chunks_fts) AS search_rank
				FROM document_chunks_fts
				JOIN document_chunks dc ON dc.id = document_chunks_fts.rowid
				JOIN document_extraction_heads h ON h.extraction_id = dc.extraction_id
				JOIN document_extraction_profiles p ON p.id = h.profile_id
				JOIN document_provider_consents c ON c.profile_id = p.id
				JOIN document_occurrences o ON o.canonical_blob_hash = h.canonical_blob_hash
				JOIN attachments a ON a.id = o.attachment_id
				JOIN messages m ON m.id = o.message_id
				JOIN conversations cv ON cv.id = m.conversation_id
				CROSS JOIN document_index_state ds
				WHERE document_chunks_fts MATCH ?
				  AND ` + validity + conditions + `
			), ranked AS (
				SELECT matched.*,
				       ROW_NUMBER() OVER (
				           PARTITION BY occurrence_key
				           ORDER BY search_rank, chunk_ordinal, chunk_key
				       ) AS occurrence_rank
				FROM matched
			)
			SELECT ` + documentSearchOuterColumns + `
			FROM ranked
			WHERE occurrence_rank = 1
			ORDER BY search_rank, occurrence_key
			LIMIT ?`
		args = append(args, ftsArg)
		args = append(args, scopeArgs...)
		args = append(args, limit+1)
		return s.scanDocumentSearchRows(ctx, query, args, true, limit)
	}
	return s.scanDocumentSearchRows(ctx, query, args, true, limit)
}

func (s *Store) searchDocumentFilenames(
	ctx context.Context,
	request DocumentSearchRequest,
	terms []string,
	limit int,
) ([]documentSearchRow, bool, error) {
	conditions, args := documentSearchScope(request, "m", "a")
	var filenameConditions strings.Builder
	for _, term := range terms {
		filenameConditions.WriteString(` AND LOWER(COALESCE(o.filename, '')) LIKE ? ESCAPE '!'`)
		args = append(args, "%"+escapeDocumentLike(strings.ToLower(term))+"%")
	}
	conditions += filenameConditions.String()
	query := `
		SELECT ` + documentSearchSelectColumns + `
		FROM document_extraction_heads h
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_chunks dc ON dc.extraction_id = h.extraction_id AND dc.ordinal = 0
		JOIN document_occurrences o ON o.canonical_blob_hash = h.canonical_blob_hash
		JOIN attachments a ON a.id = o.attachment_id
		JOIN messages m ON m.id = o.message_id
		JOIN conversations cv ON cv.id = m.conversation_id
		CROSS JOIN document_index_state ds
		WHERE ` + documentSearchValidity("p", "c", "h", "o", "a", "m", "ds") + conditions + `
		ORDER BY LOWER(COALESCE(o.filename, '')), o.occurrence_key
		LIMIT ?`
	args = append(args, limit+1)
	return s.scanDocumentSearchRows(ctx, query, args, false, limit)
}

const documentSearchSelectColumns = `
		a.id, m.id, m.source_id, o.occurrence_key,
		COALESCE(o.source_part_key, ''), COALESCE(o.filename, ''),
		COALESCE(NULLIF(m.subject, ''), NULLIF(cv.title, ''), ''),
		COALESCE(o.mime_type, ''), h.canonical_blob_hash,
		dc.chunk_key, dc.ordinal, CAST(dc.heading_path AS TEXT),
		dc.first_unit_index, dc.last_unit_index, dc.text,
		h.profile_id, h.extraction_id, p.provider, p.model,
		dc.truncated`

const documentSearchRankedSelectColumns = `
		a.id AS attachment_id, m.id AS message_id, m.source_id AS source_id,
		o.occurrence_key AS occurrence_key,
		COALESCE(o.source_part_key, '') AS source_part_key,
		COALESCE(o.filename, '') AS filename,
		COALESCE(NULLIF(m.subject, ''), NULLIF(cv.title, ''), '') AS containing_title,
		COALESCE(o.mime_type, '') AS mime_type,
		h.canonical_blob_hash AS canonical_blob_hash,
		dc.chunk_key AS chunk_key, dc.ordinal AS chunk_ordinal,
		CAST(dc.heading_path AS TEXT) AS heading_path,
		dc.first_unit_index AS first_unit_index,
		dc.last_unit_index AS last_unit_index, dc.text AS chunk_text,
		h.profile_id AS profile_id, h.extraction_id AS extraction_id,
		p.provider AS provider, p.model AS model, dc.truncated AS truncated`

const documentSearchOuterColumns = `
		attachment_id, message_id, source_id, occurrence_key,
		source_part_key, filename, containing_title, mime_type,
		canonical_blob_hash, chunk_key, chunk_ordinal, heading_path,
		first_unit_index, last_unit_index, chunk_text,
		profile_id, extraction_id, provider, model, truncated`

func documentSearchValidity(profile, consent, head, occurrence, attachment, message, state string) string {
	return profile + `.enabled = TRUE
		AND ` + profile + `.retired_at IS NULL
		AND ` + consent + `.profile_fingerprint = ` + profile + `.fingerprint
		AND ` + consent + `.retention_posture = ` + profile + `.retention_posture
		AND ` + consent + `.training_posture = ` + profile + `.training_posture
		AND ` + occurrence + `.attachment_role = 'standalone'
		AND ` + attachment + `.attachment_role = 'standalone'
		AND ` + occurrence + `.role_source IN ('mime_disposition', 'provider_explicit', 'importer_semantics', 'raw_mime_repair')
		AND ` + attachment + `.role_source IN ('mime_disposition', 'provider_explicit', 'importer_semantics', 'raw_mime_repair')
		AND ` + LiveMessagesWhere(message, true) + `
		AND (` + attachment + `.content_hash = ` + head + `.canonical_blob_hash
		     OR (COALESCE(` + attachment + `.content_hash, '') = ''
		         AND ` + attachment + `.storage_path =
		             SUBSTR(` + head + `.canonical_blob_hash, 1, 2) || '/' || ` + head + `.canonical_blob_hash))
		AND (
		    ` + head + `.profile_id = ` + state + `.target_profile_id
		    OR (
		        NOT EXISTS (
		            SELECT 1
		            FROM document_extraction_heads target_head
		            JOIN document_extraction_profiles target_profile
		              ON target_profile.id = target_head.profile_id
		            JOIN document_provider_consents target_consent
		              ON target_consent.profile_id = target_profile.id
		            WHERE target_head.canonical_blob_hash = ` + head + `.canonical_blob_hash
		              AND target_head.profile_id = ` + state + `.target_profile_id
		              AND target_profile.enabled = TRUE
		              AND target_profile.retired_at IS NULL
		              AND target_consent.profile_fingerprint = target_profile.fingerprint
		              AND target_consent.retention_posture = target_profile.retention_posture
		              AND target_consent.training_posture = target_profile.training_posture
		        )
		        AND NOT EXISTS (
		            SELECT 1
		            FROM document_extractions target_terminal
		            WHERE target_terminal.profile_id = ` + state + `.target_profile_id
		              AND target_terminal.canonical_blob_hash = ` + head + `.canonical_blob_hash
		              AND target_terminal.extraction_input_key = ` + head + `.extraction_input_key
		              AND target_terminal.state = 'terminal'
		        )
		        AND NOT EXISTS (
		            SELECT 1
		            FROM document_extraction_heads fallback_head
		            JOIN document_extraction_profiles fallback_profile
		              ON fallback_profile.id = fallback_head.profile_id
		            JOIN document_provider_consents fallback_consent
		              ON fallback_consent.profile_id = fallback_profile.id
		            WHERE fallback_head.canonical_blob_hash = ` + head + `.canonical_blob_hash
		              AND fallback_profile.enabled = TRUE
		              AND fallback_profile.retired_at IS NULL
		              AND fallback_consent.profile_fingerprint = fallback_profile.fingerprint
		              AND fallback_consent.retention_posture = fallback_profile.retention_posture
		              AND fallback_consent.training_posture = fallback_profile.training_posture
		              AND (
		                  fallback_consent.consented_at > ` + consent + `.consented_at
		                  OR (fallback_consent.consented_at = ` + consent + `.consented_at
		                      AND fallback_profile.id > ` + profile + `.id)
		              )
		        )
		    )
	)`
}

func documentSearchScope(request DocumentSearchRequest, message, attachment string) (string, []any) {
	var conditions strings.Builder
	var args []any
	if len(request.SourceIDs) > 0 {
		conditions.WriteString(` AND ` + message + `.source_id IN (`)
		conditions.WriteString(documentPlaceholders(len(request.SourceIDs)))
		conditions.WriteByte(')')
		for _, id := range request.SourceIDs {
			args = append(args, id)
		}
	}
	if len(request.MessageTypes) > 0 {
		conditions.WriteString(` AND ` + message + `.message_type IN (`)
		conditions.WriteString(documentPlaceholders(len(request.MessageTypes)))
		conditions.WriteByte(')')
		for _, messageType := range request.MessageTypes {
			args = append(args, messageType)
		}
	}
	if request.AttachmentID > 0 {
		conditions.WriteString(` AND ` + attachment + `.id = ?`)
		args = append(args, request.AttachmentID)
	}
	if request.MessageID > 0 {
		conditions.WriteString(` AND ` + message + `.id = ?`)
		args = append(args, request.MessageID)
	}
	return conditions.String(), args
}

func (s *Store) scanDocumentSearchRows(
	ctx context.Context,
	query string,
	args []any,
	contentSignal bool,
	uniqueOccurrenceLimit int,
) ([]documentSearchRow, bool, error) {
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return nil, false, fmt.Errorf("search document index: %w", err)
	}
	defer func() { _ = rows.Close() }()
	results := make([]documentSearchRow, 0)
	seenOccurrences := make(map[string]struct{})
	for rows.Next() {
		var row documentSearchRow
		var headingJSON string
		if err := rows.Scan(
			&row.AttachmentID, &row.MessageID, &row.SourceID, &row.OccurrenceKey,
			&row.SourcePartKey, &row.Filename, &row.ContainingTitle, &row.MIMEType,
			&row.CanonicalBlobHash, &row.ChunkKey, &row.ChunkOrdinal, &headingJSON,
			&row.FirstUnitIndex, &row.LastUnitIndex, &row.Text,
			&row.ProfileID, &row.ExtractionID, &row.Provider, &row.Model,
			&row.Truncated,
		); err != nil {
			return nil, false, fmt.Errorf("scan document search result: %w", err)
		}
		if err := json.Unmarshal([]byte(headingJSON), &row.HeadingPath); err != nil {
			return nil, false, fmt.Errorf("decode document search heading path: %w", err)
		}
		if contentSignal {
			if _, found := seenOccurrences[row.OccurrenceKey]; found {
				continue
			}
			seenOccurrences[row.OccurrenceKey] = struct{}{}
			row.ContentRank = len(results) + 1
		} else {
			row.FilenameRank = len(results) + 1
		}
		results = append(results, row)
		if uniqueOccurrenceLimit > 0 && len(results) > uniqueOccurrenceLimit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate document search results: %w", err)
	}
	hasMore := uniqueOccurrenceLimit > 0 && len(results) > uniqueOccurrenceLimit
	if hasMore {
		results = results[:uniqueOccurrenceLimit]
	}
	return results, hasMore, nil
}

func (s *Store) populateDocumentLiveCopyCounts(
	ctx context.Context,
	results []DocumentSearchResult,
) error {
	if len(results) == 0 {
		return nil
	}
	hashes := make([]string, 0, len(results))
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if _, found := seen[result.CanonicalBlobHash]; found {
			continue
		}
		seen[result.CanonicalBlobHash] = struct{}{}
		hashes = append(hashes, result.CanonicalBlobHash)
	}
	query := `
		SELECT h.canonical_blob_hash, COUNT(DISTINCT o.occurrence_key)
		FROM document_extraction_heads h
		JOIN document_extraction_profiles p ON p.id = h.profile_id
		JOIN document_provider_consents c ON c.profile_id = p.id
		JOIN document_occurrences o ON o.canonical_blob_hash = h.canonical_blob_hash
		JOIN attachments a ON a.id = o.attachment_id
		JOIN messages m ON m.id = o.message_id
		CROSS JOIN document_index_state ds
		WHERE h.canonical_blob_hash IN (` + documentPlaceholders(len(hashes)) + `)
		  AND ` + documentSearchValidity("p", "c", "h", "o", "a", "m", "ds") + `
		GROUP BY h.canonical_blob_hash`
	args := make([]any, len(hashes))
	for index := range hashes {
		args[index] = hashes[index]
	}
	rows, err := s.db.QueryContext(ctx, s.dialect.Rebind(query), args...)
	if err != nil {
		return fmt.Errorf("count live document copies: %w", err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int, len(hashes))
	for rows.Next() {
		var hash string
		var count int
		if err := rows.Scan(&hash, &count); err != nil {
			return fmt.Errorf("scan live document copy count: %w", err)
		}
		counts[hash] = count
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate live document copy counts: %w", err)
	}
	for index := range results {
		results[index].OtherLiveCopies = max(counts[results[index].CanonicalBlobHash]-1, 0)
	}
	return nil
}

func fuseDocumentSearchRows(
	contentRows []documentSearchRow,
	filenameRows []documentSearchRow,
	terms []string,
) []documentSearchRow {
	byOccurrence := make(map[string]documentSearchRow, len(contentRows)+len(filenameRows))
	for _, row := range contentRows {
		existing, found := byOccurrence[row.OccurrenceKey]
		if !found || row.ContentRank < existing.ContentRank {
			row.Score = 1 / (documentSearchRRFConstant + float64(row.ContentRank))
			row.MatchedSignals = []string{"content"}
			byOccurrence[row.OccurrenceKey] = row
		}
	}
	for _, filenameRow := range filenameRows {
		existing, found := byOccurrence[filenameRow.OccurrenceKey]
		if found {
			existing.FilenameRank = filenameRow.FilenameRank
			existing.Score += 1 / (documentSearchRRFConstant + float64(filenameRow.FilenameRank))
			existing.MatchedSignals = []string{"content", "filename"}
			byOccurrence[filenameRow.OccurrenceKey] = existing
			continue
		}
		filenameRow.Score = 1 / (documentSearchRRFConstant + float64(filenameRow.FilenameRank))
		filenameRow.MatchedSignals = []string{"filename"}
		byOccurrence[filenameRow.OccurrenceKey] = filenameRow
	}
	results := make([]documentSearchRow, 0, len(byOccurrence))
	for _, row := range byOccurrence {
		row.Excerpt, row.HighlightStart, row.HighlightEnd = documentSearchExcerpt(row.Text, terms)
		row.Text = ""
		results = append(results, row)
	}
	sort.Slice(results, func(i, j int) bool {
		if math.Abs(results[i].Score-results[j].Score) > 1e-12 {
			return results[i].Score > results[j].Score
		}
		if results[i].MessageID != results[j].MessageID {
			return results[i].MessageID < results[j].MessageID
		}
		return results[i].AttachmentID < results[j].AttachmentID
	})
	return results
}

func documentSearchExcerpt(text string, terms []string) (string, int, int) {
	runes := []rune(text)
	matchStart, matchEnd := 0, 0
	lower := []rune(strings.ToLower(text))
	for _, term := range terms {
		termRunes := []rune(strings.ToLower(term))
		if len(termRunes) == 0 {
			continue
		}
		for index := 0; index+len(termRunes) <= len(lower); index++ {
			if slices.Equal(lower[index:index+len(termRunes)], termRunes) {
				matchStart, matchEnd = index, index+len(termRunes)
				break
			}
		}
		if matchEnd > matchStart {
			break
		}
	}
	start := max(matchStart-maxDocumentSearchExcerpt/3, 0)
	end := min(start+maxDocumentSearchExcerpt, len(runes))
	if end-start < maxDocumentSearchExcerpt {
		start = max(end-maxDocumentSearchExcerpt, 0)
	}
	excerpt := strings.TrimSpace(string(runes[start:end]))
	trimmedPrefix := utf8.RuneCountInString(string(runes[start:end])) - utf8.RuneCountInString(strings.TrimLeftFunc(string(runes[start:end]), unicode.IsSpace))
	highlightStart := max(matchStart-start-trimmedPrefix, 0)
	highlightEnd := max(matchEnd-start-trimmedPrefix, highlightStart)
	if matchEnd == matchStart || highlightStart > utf8.RuneCountInString(excerpt) {
		highlightStart, highlightEnd = 0, 0
	} else {
		highlightEnd = min(highlightEnd, utf8.RuneCountInString(excerpt))
	}
	return excerpt, highlightStart, highlightEnd
}

func hashDocumentSearchRequest(request DocumentSearchRequest) (string, error) {
	payload := struct {
		Query        string   `json:"query"`
		SourceIDs    []int64  `json:"source_ids"`
		MessageTypes []string `json:"message_types"`
		AttachmentID int64    `json:"attachment_id"`
		MessageID    int64    `json:"message_id"`
		PageSize     int      `json:"page_size"`
	}{
		Query: request.Query, SourceIDs: request.SourceIDs, MessageTypes: request.MessageTypes,
		AttachmentID: request.AttachmentID, MessageID: request.MessageID, PageSize: request.PageSize,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode document search request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func encodeDocumentSearchCursor(cursor documentSearchCursor) (string, error) {
	encoded, err := json.Marshal(cursor)
	if err != nil {
		return "", fmt.Errorf("encode document search cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeDocumentSearchCursor(value string) (documentSearchCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > 1_024 {
		return documentSearchCursor{}, ErrDocumentSearchInvalidCursor
	}
	var cursor documentSearchCursor
	decoder := json.NewDecoder(strings.NewReader(string(decoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil || cursor.Version != documentSearchCursorVersion ||
		!validLowerSHA256(cursor.RequestHash) || cursor.Revision < 0 ||
		cursor.Offset <= 0 || cursor.Offset > maxDocumentSearchOffset ||
		cursor.CandidateLimit < 1 || cursor.CandidateLimit > maxDocumentSearchCandidates {
		return documentSearchCursor{}, ErrDocumentSearchInvalidCursor
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return documentSearchCursor{}, ErrDocumentSearchInvalidCursor
	}
	return cursor, nil
}

func sortedUniquePositive(values []int64) ([]int64, error) {
	result := slices.Clone(values)
	slices.Sort(result)
	result = slices.Compact(result)
	for _, value := range result {
		if value <= 0 {
			return nil, fmt.Errorf("%w: source IDs must be positive", ErrDocumentSearchInvalidRequest)
		}
	}
	return result, nil
}

func sortedUniqueNonempty(values []string) ([]string, error) {
	result := slices.Clone(values)
	for index := range result {
		result[index] = strings.ToLower(strings.TrimSpace(result[index]))
		if result[index] == "" {
			return nil, fmt.Errorf("%w: message types must be nonempty", ErrDocumentSearchInvalidRequest)
		}
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func escapeDocumentLike(value string) string {
	replacer := strings.NewReplacer(`!`, `!!`, `%`, `!%`, `_`, `!_`)
	return replacer.Replace(value)
}

func documentPlaceholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}
