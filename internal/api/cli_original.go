package api

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// cliOriginalMessageResponse carries one message's provenance and its
// original MIME bytes (base64 in JSON) exactly as the provider delivered them.
type cliOriginalMessageResponse struct {
	Message query.MessageRecord `json:"message"`
	MIME    []byte              `json:"mime"`
}

func (s *Server) originalMessageReader(w http.ResponseWriter, r *http.Request) (query.OriginalMessageReader, bool) {
	reader, ok := s.queryEngineForContext(r.Context()).(query.OriginalMessageReader)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "original_export_unavailable",
			"Original message export is not available for this archive engine")
		return nil, false
	}
	return reader, true
}

// cliMessageRefFromRequest reads id, source_message_id and account. The
// numeric id names only an internal message ID; a provider ID is always
// passed as source_message_id so it can never collide with one.
func cliMessageRefFromRequest(r *http.Request) (query.MessageRef, error) {
	values := r.URL.Query()
	ref := query.MessageRef{
		SourceMessageID: values.Get("source_message_id"),
		Account:         values.Get("account"),
	}
	if raw := values.Get("id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			return ref, newParamError("id", "query parameter \"id\" must be a positive integer")
		}
		ref.ID = id
	}
	return ref, nil
}

func (s *Server) handleCLIMessageOriginal(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.originalMessageReader(w, r)
	if !ok {
		return
	}
	ref, err := cliMessageRefFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	maxBytes, present, err := queryInt64(r, "max_bytes")
	if err != nil || (present && (maxBytes <= 0 || maxBytes == math.MaxInt64)) {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameter max_bytes must be positive and less than 9223372036854775807")
		return
	}
	if (maxBytes == 0 || maxBytes > remoteMessageBytes) && s.remoteClientRequest(r) {
		maxBytes = remoteMessageBytes
	}
	original, err := reader.ReadOriginalMessage(r.Context(), ref, maxBytes)
	if err != nil {
		s.writeOriginalExportError(w, "read original message", err)
		return
	}
	writeJSON(w, http.StatusOK, cliOriginalMessageResponse{Message: original.MessageRecord, MIME: original.MIME})
}

func (s *Server) handleCLIMessageThread(w http.ResponseWriter, r *http.Request) {
	reader, ok := s.originalMessageReader(w, r)
	if !ok {
		return
	}
	ref, err := cliMessageRefFromRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	q := query.ThreadQuery{MessageRef: ref, ThreadID: r.URL.Query().Get("thread_id"), Limit: query.ThreadDefaultLimit}
	q.All, _, err = queryBool(r, "all")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if s.remoteClientRequest(r) {
		q.MaxMembers = query.ThreadMaxLimit
	}
	if q.All && (r.URL.Query().Has("limit") || r.URL.Query().Has("offset")) {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameter \"all\" cannot be combined with \"limit\" or \"offset\"")
		return
	}
	if value, present, err := queryInt(r, "limit"); err != nil || (present && (value < 1 || value > query.ThreadMaxLimit)) {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameter \"limit\" must be between 1 and 500")
		return
	} else if present {
		q.Limit = value
	}
	if value, present, err := queryInt(r, "offset"); err != nil || (present && value < 0) {
		writeError(w, http.StatusBadRequest, "invalid_request", "query parameter \"offset\" must be a non-negative integer")
		return
	} else if present {
		q.Offset = value
	}
	delegated := s.requestAuthentication(r).Grant != nil
	if delegated {
		q.SourceIDs = agentReadSourceIDs(r)
	}
	page, err := reader.ListThread(r.Context(), q)
	if err != nil {
		if delegated && errors.Is(err, query.ErrAmbiguousReference) {
			writeError(w, http.StatusConflict, "message_ambiguous", "Reference matches several accounts; select an account")
			return
		}
		s.writeOriginalExportError(w, "list thread", err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) writeOriginalExportError(w http.ResponseWriter, operation string, err error) {
	var ambiguous *query.AmbiguousError
	switch {
	case errors.Is(err, query.ErrInvalidMessageRef):
		writeError(w, http.StatusBadRequest, "invalid_request",
			"Provide exactly one of id, source_message_id, or thread_id")
	case errors.Is(err, store.ErrMessageNotFound):
		writeError(w, http.StatusNotFound, cliErrorMessageNotFound, "Message not found")
	case errors.Is(err, query.ErrThreadTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "remote_thread_too_large",
			fmt.Sprintf("Thread exceeds the remote client limit of %d messages", query.ThreadMaxLimit))
	case errors.Is(err, query.ErrThreadNotFound):
		writeError(w, http.StatusNotFound, "thread_not_found", "Thread not found")
	case errors.Is(err, query.ErrOriginalMessageTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "original_message_too_large", "Original MIME exceeds the requested byte limit")
	case errors.Is(err, query.ErrOriginalMIMEUnavailable):
		writeError(w, http.StatusNotFound, "original_mime_unavailable", "The archive holds no original MIME for this message")
	case errors.Is(err, query.ErrOriginalExportUnsupported):
		writeError(w, http.StatusServiceUnavailable, "original_export_unavailable",
			"Original message export is not available for this archive engine")
	case errors.As(err, &ambiguous):
		writeError(w, http.StatusConflict, "message_ambiguous", ambiguous.Error())
	case s.writeIfContextError(w, err):
	default:
		s.logger.Error(operation, "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Could not "+operation)
	}
}
