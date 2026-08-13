package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
)

// DocumentSearchStore is the optional dedicated extracted-document search
// surface. It stays separate from MessageStore so lightweight API consumers
// do not have to implement document indexing.
type DocumentSearchStore interface {
	SearchDocuments(ctx context.Context, request store.DocumentSearchRequest) (store.DocumentSearchResponse, error)
}

// DocumentStatusStore is the optional scoped status surface for extracted
// documents. Keeping it separate lets the daemon expose the same read path as
// the CLI without expanding the core MessageStore contract.
type DocumentStatusStore interface {
	GetDocumentIndexStatusForScope(
		ctx context.Context,
		profileID string,
		extractionInputKey string,
		allowedMediaTypes []string,
		allowedMessageTypes []string,
	) (store.DocumentIndexStatus, error)
	GetActiveDocumentExtractionRebuild(
		ctx context.Context,
		profileID string,
		extractionInputKey string,
	) (store.DocumentExtractionRebuild, error)
	CountIncompleteDocumentExtractionRebuild(
		ctx context.Context,
		rebuild store.DocumentExtractionRebuild,
		allowedMediaTypes []string,
		allowedMessageTypes []string,
	) (int64, error)
}

type documentOccurrenceSearchCatalog interface {
	documentindex.DocumentOccurrenceCatalog
	GetAttachmentChangeConsumer(
		ctx context.Context,
		consumerKey string,
	) (store.AttachmentChangeConsumer, error)
}

type documentOccurrenceStatusReconciler interface {
	ReconcileDocumentOccurrences(ctx context.Context) error
}

var _ DocumentSearchStore = (*store.Store)(nil)
var _ DocumentStatusStore = (*store.Store)(nil)

func (s *Server) registerDocumentSearchRoute(api huma.API) {
	registerAPIV1RawHumaJSONRouteWithErrors[store.DocumentSearchResponse](
		api, "searchDocuments", http.MethodGet, "/documents/search",
		"Search extracted document attachments", s.handleDocumentSearch,
		http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable,
	)
	registerAPIV1RawHumaJSONRouteWithErrors[store.DocumentIndexStatusResponse](
		api, "getDocumentIndexStatus", http.MethodGet, "/documents/status",
		"Get extracted document index status", s.handleDocumentIndexStatus,
		http.StatusBadRequest, http.StatusServiceUnavailable,
	)
}

func (s *Server) handleDocumentIndexStatus(w http.ResponseWriter, r *http.Request) {
	statusStore, ok := s.store.(DocumentStatusStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "document_status_unavailable",
			"Document attachment status is unavailable")
		return
	}
	request, err := parseDocumentIndexStatusRequest(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if reconciler, ok := s.store.(documentOccurrenceStatusReconciler); ok {
		if err := reconciler.ReconcileDocumentOccurrences(r.Context()); err != nil {
			s.logger.Error("reconcile document status index", "error", err)
			writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
			return
		}
	}
	status, err := statusStore.GetDocumentIndexStatusForScope(
		r.Context(), request.ProfileID, request.ExtractionInputKey,
		request.AllowedMediaTypes, request.AllowedMessageTypes,
	)
	if err != nil {
		s.logger.Error("read document index status", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
		return
	}
	response := store.DocumentIndexStatusResponse{Status: status}
	active, err := statusStore.GetActiveDocumentExtractionRebuild(
		r.Context(), request.ProfileID, request.ExtractionInputKey,
	)
	if err == nil {
		remaining, countErr := statusStore.CountIncompleteDocumentExtractionRebuild(
			r.Context(), active, request.AllowedMediaTypes, request.AllowedMessageTypes,
		)
		if countErr != nil {
			s.logger.Error("count document rebuild status", "error", countErr)
			writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
			return
		}
		response.ActiveRebuild = &store.DocumentIndexRebuildStatus{
			SnapshotOwners: active.SnapshotOwners, RemainingOwners: remaining,
		}
	} else if !errors.Is(err, store.ErrDocumentExtractionRebuildMissing) {
		s.logger.Error("read document rebuild status", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Document status failed")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func parseDocumentIndexStatusRequest(r *http.Request) (store.DocumentIndexStatusRequest, error) {
	request := store.DocumentIndexStatusRequest{
		ProfileID:           r.URL.Query().Get("profile_id"),
		ExtractionInputKey:  r.URL.Query().Get("input_key"),
		AllowedMediaTypes:   append([]string(nil), r.URL.Query()["media_type"]...),
		AllowedMessageTypes: append([]string(nil), r.URL.Query()["message_type"]...),
	}
	if request.ProfileID == "" || request.ExtractionInputKey == "" || len(request.AllowedMediaTypes) == 0 {
		return request, errors.New("profile_id, input_key, and at least one media_type are required")
	}
	return request, nil
}

func (s *Server) handleDocumentSearch(w http.ResponseWriter, r *http.Request) {
	searcher, ok := s.store.(DocumentSearchStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "document_search_unavailable",
			"Document attachment search is unavailable")
		return
	}
	request, err := parseDocumentSearchRequest(r)
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	if catalog, ok := s.store.(documentOccurrenceSearchCatalog); ok {
		if _, lookupErr := catalog.GetAttachmentChangeConsumer(
			r.Context(), documentindex.DocumentAttachmentConsumerKey,
		); lookupErr == nil {
			reconciler, reconcileErr := documentindex.NewReconciler(catalog, documentindex.ReconcilerConfig{
				AttachmentPageSize: 1000,
				ChangePageSize:     1000,
			})
			if reconcileErr != nil {
				s.logger.Error("configure document search reconciliation", "error", reconcileErr)
				writeError(w, http.StatusInternalServerError, "internal_error", "Document search failed")
				return
			}
			if _, reconcileErr = reconciler.Reconcile(r.Context()); reconcileErr != nil {
				s.logger.Error("reconcile document search index", "error", reconcileErr)
				writeError(w, http.StatusInternalServerError, "internal_error", "Document search failed")
				return
			}
		} else if !errors.Is(lookupErr, store.ErrAttachmentChangeConsumerMissing) {
			s.logger.Error("read document search reconciliation state", "error", lookupErr)
			writeError(w, http.StatusInternalServerError, "internal_error", "Document search failed")
			return
		}
	}
	response, err := searcher.SearchDocuments(r.Context(), request)
	if err != nil {
		s.writeDocumentSearchError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) writeDocumentSearchError(w http.ResponseWriter, err error) {
	switch {
	case s.writeIfContextError(w, err):
		return
	case errors.Is(err, store.ErrDocumentSearchInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid_cursor", "The document search cursor is invalid")
	case errors.Is(err, store.ErrDocumentSearchCursorStale):
		writeError(w, http.StatusConflict, "document_index_changed",
			"The document index changed; restart pagination")
	case errors.Is(err, store.ErrDocumentSearchInvalidRequest):
		writeError(w, http.StatusBadRequest, "invalid_document_search", err.Error())
	case errors.Is(err, store.ErrDocumentSearchUnavailable):
		writeError(w, http.StatusServiceUnavailable, "document_search_unavailable",
			"Document search requires full-text search support")
	default:
		s.logger.Error("document search failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Document search failed")
	}
}

func parseDocumentSearchRequest(r *http.Request) (store.DocumentSearchRequest, error) {
	request := store.DocumentSearchRequest{
		Query:  r.URL.Query().Get("q"),
		Cursor: r.URL.Query().Get("cursor"),
	}
	var err error
	request.SourceIDs, _, err = queryInt64s(r, "source_id")
	if err != nil {
		return request, err
	}
	for _, raw := range r.URL.Query()["message_type"] {
		for value := range strings.SplitSeq(raw, ",") {
			request.MessageTypes = append(request.MessageTypes, strings.TrimSpace(value))
		}
	}
	if request.PageSize, _, err = queryInt(r, "limit"); err != nil {
		return request, err
	}
	if request.AttachmentID, _, err = queryInt64(r, "attachment_id"); err != nil {
		return request, err
	}
	if request.MessageID, _, err = queryInt64(r, "message_id"); err != nil {
		return request, err
	}
	return request, nil
}
