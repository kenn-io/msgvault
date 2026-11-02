package api

import (
	"bytes"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/mailkindworker"
)

const mailKindWarning = "Provider output is advisory and untrusted; a kind label does not verify the sender or transaction."

type MailKindRunRequest struct {
	DisclosureFingerprint string   `json:"disclosure_fingerprint"`
	Limit                 *int     `json:"limit,omitzero" nullable:"false" minimum:"1" maximum:"100"`
	MessageIDs            *[]int64 `json:"message_ids,omitzero" nullable:"false" minItems:"1" maxItems:"100"`
}

type MailKindRunResponse struct {
	mailkindworker.Counts

	Warning string `json:"warning"`
	Blocker string `json:"blocker,omitempty"`
}

type mailKindRunStore interface {
	mailKindConsentStore
	mailkindworker.Store
}

func (s *Server) registerMailKindRunRoute(api huma.API) {
	op := rawAPIV1Operation("classifyMailKind", http.MethodPost, "/mail/kind/classify", "Classify a bounded mail batch and apply local auto labels")
	op.RequestBody = jsonRequestBodyFor[MailKindRunRequest](api)
	op.RequestBody.Required = true
	op.Responses = jsonResponsesFor[MailKindRunResponse](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleMailKindRun)
}

func (s *Server) handleMailKindRun(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body MailKindRunRequest
	var rawBody bytes.Buffer
	r.Body = io.NopCloser(io.TeeReader(r.Body, &rawBody))
	if !decodeIdentityMatchJSON(w, r, &body) {
		return
	}
	var raw struct {
		MessageIDs jsontext.Value `json:"message_ids"`
	}
	if err := json.Unmarshal(rawBody.Bytes(), &raw); err == nil && bytes.Equal(bytes.TrimSpace(raw.MessageIDs), []byte("null")) {
		writeError(w, http.StatusBadRequest, "invalid_message_ids", "Provide a nonempty message ID list within the batch limit")
		return
	}
	st, ok := s.store.(mailKindRunStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "feature_unavailable", "Mail kind classification is unavailable")
		return
	}
	status, err := s.mailKindStatus(r.Context(), st)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "status_unavailable", "Mail kind status is unavailable")
		return
	}
	if !status.Ready {
		writeError(w, http.StatusConflict, status.Blocker, "Mail kind classification is not ready")
		return
	}
	if body.DisclosureFingerprint != status.DisclosureFingerprint {
		writeError(w, http.StatusConflict, "disclosure_changed", "Read and approve the current mail kind disclosure before classifying")
		return
	}
	limit := status.BatchSize
	if body.Limit != nil {
		limit = *body.Limit
	}
	if limit < 1 || limit > status.BatchSize || limit > 100 {
		writeError(w, http.StatusBadRequest, "invalid_limit", "Limit must be positive and no greater than the configured batch size")
		return
	}
	var ids []int64
	if body.MessageIDs != nil {
		ids = *body.MessageIDs
		if len(ids) == 0 || len(ids) > limit {
			writeError(w, http.StatusBadRequest, "invalid_message_ids", "Provide a nonempty message ID list within the batch limit")
			return
		}
		for _, id := range ids {
			if id < 1 || id > 1<<53-1 {
				writeError(w, http.StatusBadRequest, "invalid_message_ids", "Message IDs must be positive safe integers")
				return
			}
		}
	}
	worker := mailkindworker.Worker{Store: st, Config: s.cfg.Mail.KindClassification,
		Endpoint: s.mailKindEndpoint, BeginMutation: s.beginMailKindMutation}
	counts, err := worker.Run(r.Context(), limit, ids)
	response := MailKindRunResponse{Counts: counts, Warning: mailKindWarning}
	if err != nil {
		response.Blocker = "classification_stopped"
		if errors.Is(err, mailkindworker.ErrConsentRequired) {
			response.Blocker = "consent_required"
		} else if errors.Is(err, errMailKindGateBusy) {
			response.Blocker = "archive_busy"
		}
	}
	writeJSON(w, http.StatusOK, response)
}
