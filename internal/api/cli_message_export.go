package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/store"
)

// MessageExporter streams the msgvault-message-export/1 JSONL contract.
type MessageExporter interface {
	OpenMessageExport(ctx context.Context, request export.MessagesRequest) (*export.MessagesExport, error)
}

// handleCLIMessageExport streams export-messages output. Validation and
// source resolution finish before the first byte, so they return HTTP errors;
// a failure while streaming leaves the stream without its completion record.
func (s *Server) handleCLIMessageExport(w http.ResponseWriter, r *http.Request) {
	var open func(context.Context, export.MessagesRequest) (*export.MessagesExport, error)
	switch st := s.store.(type) {
	case *store.Store:
		// An embedded archive passes its native store.
		open = func(ctx context.Context, request export.MessagesRequest) (*export.MessagesExport, error) {
			return export.OpenMessages(ctx, st, request)
		}
	case MessageExporter:
		open = st.OpenMessageExport
	default:
		writeError(w, http.StatusServiceUnavailable, "message_export_unavailable", "Message export is not available for this archive")
		return
	}
	values := r.URL.Query()
	var personID *int64
	if raw := values.Get("person_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_request", "query parameter \"person_id\" must be a positive integer")
			return
		}
		personID = &id
	}
	request, err := export.ParseMessagesRequest(values.Get("start"), values.Get("end"), values["message_type"], values["source"], personID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if agentReadScoped(r) {
		request.Scope = agentReadSourceIDs(r)
	}
	messages, err := open(r.Context(), request)
	if err != nil {
		if errors.Is(err, export.ErrMessageSourceNotFound) {
			writeError(w, http.StatusNotFound, "source_not_found", err.Error())
			return
		}
		s.logger.Error("open message export", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	defer messages.Close()
	// The stream reads from the scoped snapshot after its header is written.
	holdReadSnapshot(r)
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	if err := messages.Write(w, s.daemonVersion); err != nil {
		s.logger.Warn("message export stream ended early", "error", err)
	}
}
