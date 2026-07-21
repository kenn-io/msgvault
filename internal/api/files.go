package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const filesMaxLimit = 500

type FileContentState string

const (
	FileContentMetadataOnly FileContentState = "metadata_only"
	FileContentURLOnly      FileContentState = "url_only"
	FileContentMissingBlob  FileContentState = "missing_blob"
	FileContentLocal        FileContentState = "local_content"
)

type FileSearchSort struct {
	Field     string `json:"field"`
	Direction string `json:"direction"`
}

type FileSearchHTTPRequest struct {
	Predicate     ExploreHTTPRequest     `json:"predicate"`
	FilenameQuery string                 `json:"filename_query,omitempty"`
	MIMEFamilies  []query.FileMIMEFamily `json:"mime_families,omitempty"`
	Sort          FileSearchSort         `json:"sort"`
	Cursor        string                 `json:"cursor,omitempty"`
	Limit         int                    `json:"limit,omitempty" minimum:"0" maximum:"500"`
}

type FileSearchRow struct {
	ID                 int64                `json:"id"`
	Key                string               `json:"key"`
	EntryKey           string               `json:"entry_key"`
	MessageID          int64                `json:"message_id"`
	ConversationID     int64                `json:"conversation_id"`
	OccurredAt         time.Time            `json:"occurred_at"`
	SourceID           int64                `json:"source_id"`
	SourceType         string               `json:"source_type"`
	SourceIdentifier   string               `json:"source_identifier"`
	ContainingTitle    string               `json:"containing_title"`
	Filename           string               `json:"filename"`
	MimeType           string               `json:"mime_type"`
	MIMEFamily         query.FileMIMEFamily `json:"mime_family"`
	Size               int64                `json:"size_bytes"`
	ParticipantIDs     []int64              `json:"participant_ids,omitempty"`
	ParticipantLabels  []string             `json:"participant_labels,omitempty"`
	ParticipantDomains []string             `json:"participant_domains,omitempty"`
	ContentState       FileContentState     `json:"content_state" enum:"metadata_only,url_only,missing_blob,local_content"`
	ContentAvailable   bool                 `json:"content_available"`
}

type FileSearchHTTPResponse struct {
	Files               []FileSearchRow        `json:"files"`
	TotalCount          int64                  `json:"total_count"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	NextCursor          string                 `json:"next_cursor,omitempty"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type FileGroupsHTTPRequest struct {
	Predicate     ExploreHTTPRequest      `json:"predicate"`
	FilenameQuery string                  `json:"filename_query,omitempty"`
	MIMEFamilies  []query.FileMIMEFamily  `json:"mime_families,omitempty"`
	Grouping      []ExploreGroupDimension `json:"grouping" minItems:"1" maxItems:"1"`
	Sort          []ExploreGroupSort      `json:"sort,omitempty" maxItems:"1"`
	Cursor        string                  `json:"cursor,omitempty"`
	Limit         int                     `json:"limit,omitempty" minimum:"0" maximum:"500"`
}

type FileGroupsHTTPResponse struct {
	Rows                []query.ExploreGroupRow `json:"rows"`
	TotalCount          int64                   `json:"total_count"`
	CacheRevision       string                  `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance  `json:"search_provenance"`
	NextCursor          string                  `json:"next_cursor,omitempty"`
	CandidateSnapshotID string                  `json:"candidate_snapshot_id,omitempty"`
}

type FileMetadataResponse struct {
	ID               int64            `json:"id"`
	MessageID        int64            `json:"message_id"`
	ConversationID   int64            `json:"conversation_id"`
	Filename         string           `json:"filename"`
	MimeType         string           `json:"mime_type"`
	Size             int64            `json:"size_bytes"`
	ContentHash      string           `json:"content_hash,omitempty"`
	URL              string           `json:"url,omitempty"`
	ContentState     FileContentState `json:"content_state" enum:"metadata_only,url_only,missing_blob,local_content"`
	ContentAvailable bool             `json:"content_available"`
}

type fileMetadataCatalog interface {
	GetFileMetadata(ctx context.Context, id int64) (*store.FileMetadata, error)
	GetFileMetadataBatch(ctx context.Context, ids []int64) (map[int64]store.FileMetadata, error)
}

func (s *Server) registerFilesRoutes(api huma.API) {
	registerExploreRoute[FileSearchHTTPRequest, FileSearchHTTPResponse](
		api, "searchFiles", "/files/search", "Search analytical files", s.handleSearchFiles,
	)
	registerExploreRoute[FileSearchHTTPRequest, FileSearchHTTPResponse](
		api, "searchPersonFiles", "/people/{id}/files/search", "Search one person's analytical files", s.handleSearchPersonFiles,
	)
	registerExploreRoute[FileSearchHTTPRequest, FileSearchHTTPResponse](
		api, "searchDomainFiles", "/domains/{domain}/files/search", "Search one domain's analytical files", s.handleSearchDomainFiles,
	)
	registerExploreRoute[FileGroupsHTTPRequest, FileGroupsHTTPResponse](
		api, "groupFiles", "/files/groups", "Group analytical files", s.handleGroupFiles,
	)
	registerAPIV1RawHumaJSONRoute[FileMetadataResponse](
		api, "getFile", http.MethodGet, "/files/{id}", "Get authoritative file metadata", s.handleGetFile,
	)
}

func (s *Server) handleGroupFiles(w http.ResponseWriter, r *http.Request) {
	var request FileGroupsHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	predicate, err := prepareExplorePredicate(request.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_files_predicate", err.Error())
		return
	}
	if len(request.Grouping) != 1 {
		writeError(w, http.StatusBadRequest, "invalid_grouping", "exactly one grouping dimension is required")
		return
	}
	dimension := ExploreGroupDimension(strings.ToLower(strings.TrimSpace(string(request.Grouping[0]))))
	if !slices.Contains(exploreGroupDimensions, dimension) {
		writeError(w, http.StatusBadRequest, "invalid_grouping", fmt.Sprintf("unknown grouping dimension %q", dimension))
		return
	}
	request.FilenameQuery = strings.TrimSpace(request.FilenameQuery)
	for i := range request.MIMEFamilies {
		request.MIMEFamilies[i] = query.FileMIMEFamily(strings.ToLower(strings.TrimSpace(string(request.MIMEFamilies[i]))))
	}
	slices.Sort(request.MIMEFamilies)
	request.MIMEFamilies = slices.Compact(request.MIMEFamilies)
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > filesMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", filesMaxLimit))
		return
	}
	sortSpec := query.SortSpec{Field: "count", Direction: apiSortDirectionDesc}
	if len(request.Sort) > 1 {
		writeError(w, http.StatusBadRequest, "invalid_sort", "at most one group sort is supported")
		return
	}
	if len(request.Sort) == 1 {
		sortSpec = query.SortSpec{
			Field:     strings.ToLower(strings.TrimSpace(request.Sort[0].Field)),
			Direction: strings.ToLower(strings.TrimSpace(request.Sort[0].Direction)),
		}
		if !slices.Contains([]string{"key", "count", "estimated_bytes", "latest_at"}, sortSpec.Field) ||
			!slices.Contains([]string{"asc", apiSortDirectionDesc}, sortSpec.Direction) {
			writeError(w, http.StatusBadRequest, "invalid_sort", "unknown group sort field or direction")
			return
		}
		request.Sort = []ExploreGroupSort{{Field: sortSpec.Field, Direction: sortSpec.Direction}}
	}
	request.Predicate = predicate.request
	request.Grouping = []ExploreGroupDimension{dimension}
	canonical := request
	canonical.Cursor = ""
	requestHash := hashCanonicalValue(canonical, false)
	offset, ok := s.parseExploreCursor(w, request.Cursor, requestHash)
	if !ok {
		return
	}
	searchRequest := predicate.request
	var cursor exploreCursor
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
		if searchRequest.SearchMode == exploreSearchModeSemantic || searchRequest.SearchMode == exploreSearchModeHybrid {
			if cursor.Snapshot == "" {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "semantic cursor is missing its candidate snapshot")
				return
			}
			searchRequest.CandidateSnapshotID = cursor.Snapshot
		}
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, searchRequest)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return
	}
	if request.Cursor != "" && cursor.SearchRevision != exploreResolvedSearchRevision(searchSpec) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The resolved search index revision changed; restart pagination")
		return
	}
	grouper, ok := s.engine.(query.FileGrouper)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := grouper.GroupFiles(r.Context(), query.FileGroupRequest{
		Explore:       query.ExploreRequest{Context: predicate.query.Context, Search: searchSpec},
		FilenameQuery: request.FilenameQuery, MIMEFamilies: request.MIMEFamilies,
		Dimension: string(dimension), Sort: sortSpec,
		Page: query.PageSpec{Limit: request.Limit, Offset: offset},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if request.Cursor != "" && cursor.Revision != result.CacheRevision {
		writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
		return
	}
	response := FileGroupsHTTPResponse{
		Rows: result.Rows, TotalCount: result.TotalCount, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: snapshotID,
	}
	if next := offset + len(result.Rows); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(searchSpec), Snapshot: snapshotID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleSearchFiles(w http.ResponseWriter, r *http.Request) {
	s.handleSearchFilesWithScope(w, r, nil)
}

func (s *Server) handleSearchPersonFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := positivePersonPathID(w, r)
	if !ok {
		return
	}
	scope := ExploreFilter{Dimension: "participant", Values: []string{strconv.FormatInt(id, 10)}}
	s.handleSearchFilesWithScope(w, r, &scope)
}

func (s *Server) handleSearchDomainFiles(w http.ResponseWriter, r *http.Request) {
	domain, ok := domainPathFactSuffix(w, r, "/files/search")
	if !ok {
		return
	}
	scope := ExploreFilter{Dimension: "domain", Values: []string{domain}}
	s.handleSearchFilesWithScope(w, r, &scope)
}

func (s *Server) handleSearchFilesWithScope(w http.ResponseWriter, r *http.Request, scope *ExploreFilter) {
	var request FileSearchHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	predicate, err := prepareExplorePredicate(request.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_files_predicate", err.Error())
		return
	}
	if request.Limit == 0 {
		request.Limit = 100
	}
	if request.Limit < 1 || request.Limit > filesMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", filesMaxLimit))
		return
	}
	if request.Sort.Field == "" {
		request.Sort = FileSearchSort{Field: "occurred_at", Direction: apiSortDirectionDesc}
	}
	request.Predicate = predicate.request
	requestHash := canonicalScopedFileSearchHash(request, scope)
	offset, ok := s.parseExploreCursor(w, request.Cursor, requestHash)
	if !ok {
		return
	}
	var cursor exploreCursor
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
		if predicate.request.SearchMode == exploreSearchModeSemantic || predicate.request.SearchMode == exploreSearchModeHybrid {
			if cursor.Snapshot == "" {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "semantic cursor is missing its candidate snapshot")
				return
			}
			predicate.request.CandidateSnapshotID = cursor.Snapshot
		}
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, predicate.request)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return
	}
	if request.Cursor != "" && cursor.SearchRevision != exploreResolvedSearchRevision(searchSpec) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The resolved search index revision changed; restart pagination")
		return
	}
	predicate.query.Search = searchSpec
	if scope != nil {
		if err := applyIdentityScope(&predicate.query.Context, *scope); err != nil {
			writeError(w, http.StatusConflict, "identity_scope_conflict", err.Error())
			return
		}
	}
	searcher, ok := s.engine.(query.FileSearcher)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := searcher.SearchFiles(r.Context(), query.FileSearchRequest{
		Explore: predicate.query, FilenameQuery: strings.TrimSpace(request.FilenameQuery),
		MIMEFamilies: request.MIMEFamilies,
		Sort:         query.SortSpec{Field: request.Sort.Field, Direction: request.Sort.Direction},
		Page:         query.PageSpec{Limit: request.Limit, Offset: offset},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if request.Cursor != "" {
		if cursor.Revision != result.CacheRevision {
			writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
			return
		}
	}
	catalog, ok := s.store.(fileMetadataCatalog)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "file_metadata_unavailable", "Authoritative file metadata is unavailable")
		return
	}
	ids := make([]int64, len(result.Files))
	for i, file := range result.Files {
		ids[i] = file.ID
	}
	metadata, err := catalog.GetFileMetadataBatch(r.Context(), ids)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		writeError(w, http.StatusServiceUnavailable, "file_metadata_unavailable", "Authoritative file metadata is unavailable")
		return
	}
	response := FileSearchHTTPResponse{
		Files: make([]FileSearchRow, 0, len(result.Files)), TotalCount: result.TotalCount,
		CacheRevision: result.CacheRevision, SearchProvenance: result.SearchProvenance,
		CandidateSnapshotID: snapshotID,
	}
	for _, file := range result.Files {
		authority, found := metadata[file.ID]
		if !found {
			writeError(w, http.StatusConflict, "file_metadata_changed", "The authoritative file metadata changed; refresh the Files workspace")
			return
		}
		state, available := fileContentState(authority)
		response.Files = append(response.Files, FileSearchRow{
			ID: file.ID, Key: file.Key, EntryKey: file.EntryKey, MessageID: file.MessageID,
			ConversationID: file.ConversationID, OccurredAt: file.OccurredAt,
			SourceID: file.SourceID, SourceType: file.SourceType, SourceIdentifier: file.SourceIdentifier,
			ContainingTitle: file.ContainingTitle, Filename: file.Filename, MimeType: file.MimeType,
			MIMEFamily: file.MIMEFamily, Size: file.Size, ParticipantIDs: file.ParticipantIDs,
			ParticipantLabels: file.ParticipantLabels, ParticipantDomains: file.ParticipantDomains,
			ContentState: state, ContentAvailable: available,
		})
	}
	if next := offset + len(result.Files); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(searchSpec), Snapshot: snapshotID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func canonicalScopedFileSearchHash(request FileSearchHTTPRequest, scope *ExploreFilter) string {
	request.Cursor = ""
	if scope == nil {
		return hashCanonicalValue(request, false)
	}
	return hashCanonicalValue(struct {
		Request FileSearchHTTPRequest `json:"request"`
		Scope   ExploreFilter         `json:"identity_scope"`
	}{Request: request, Scope: *scope}, false)
}

func (s *Server) handleGetFile(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "File ID must be a positive integer")
		return
	}
	catalog, ok := s.store.(fileMetadataCatalog)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "file_metadata_unavailable", "Authoritative file metadata is unavailable")
		return
	}
	file, err := catalog.GetFileMetadata(r.Context(), id)
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		writeError(w, http.StatusServiceUnavailable, "file_metadata_unavailable", "Authoritative file metadata is unavailable")
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "file_not_found", "File not found")
		return
	}
	state, available := fileContentState(*file)
	writeJSON(w, http.StatusOK, FileMetadataResponse{
		ID: file.ID, MessageID: file.MessageID, ConversationID: file.ConversationID,
		Filename: file.Filename, MimeType: file.MimeType, Size: file.Size,
		ContentHash: file.ContentHash, URL: file.URL, ContentState: state, ContentAvailable: available,
	})
}

func (s *Server) handleGetFileContent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_id", "File ID must be a positive integer")
		return
	}
	catalog, ok := s.store.(fileMetadataCatalog)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "file_metadata_unavailable", "Authoritative file metadata is unavailable")
		return
	}
	file, err := catalog.GetFileMetadata(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "file_metadata_unavailable", "Authoritative file metadata is unavailable")
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "file_not_found", "File not found")
		return
	}
	if state, available := fileContentState(*file); state != FileContentLocal || !available {
		writeError(w, http.StatusNotFound, "file_content_unavailable", "File content is not available")
		return
	}
	var content io.ReadCloser
	var length int64
	if s.blobStore != nil {
		content, length, err = s.blobStore.OpenStream(r.Context(), file.ContentHash)
	}
	if s.blobStore == nil || errors.Is(err, os.ErrNotExist) {
		content, length, err = openLooseAttachmentContent(s.cfg.AttachmentsDir(), file.ContentHash, file.StoragePath)
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, "file_content_unavailable", "File content is not available")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to open file content")
		return
	}
	contentType := file.MimeType
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(file.Filename))
	w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, copyErr := io.Copy(w, content)
	if err := errors.Join(copyErr, content.Close()); err != nil {
		s.logger.Error("failed to stream file", "error", err, "file_id", id)
	}
}

func fileContentState(file store.FileMetadata) (FileContentState, bool) {
	if file.URL != "" {
		return FileContentURLOnly, false
	}
	if file.ContentHash == "" {
		return FileContentMetadataOnly, false
	}
	if file.StoragePath == "" {
		return FileContentMissingBlob, false
	}
	return FileContentLocal, true
}
