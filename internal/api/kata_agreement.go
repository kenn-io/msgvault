package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/kataevidence"
	"go.kenn.io/msgvault/internal/kataissues"
)

// Kept separate so existing custom Kata backends without agreement support
// continue serving their existing operations.
type kataAgreementOperations interface {
	Agreement(ctx context.Context, ref string) (kataissues.AgreementView, error)
	RecordAgreement(ctx context.Context, ref string, input kataissues.AgreementInput) (kataissues.AgreementView, error)
}

type KataAgreementRecordRequest kataissues.AgreementInput

type KataAgreementResponse struct {
	Agreement kataissues.AgreementView `json:"agreement"`
}

func (s *Server) registerKataAgreementRoutes(api huma.API) {
	for _, route := range []struct {
		id, method, summary string
		handler             http.HandlerFunc
	}{
		{"getKataAgreement", http.MethodGet, "Read reviewed agreement history and last accepted terms on a Kata issue", s.handleKataAgreement},
		{"recordKataAgreement", http.MethodPost, "Record a reviewed agreement event without changing unaccepted terms", s.handleRecordKataAgreement},
	} {
		operation := rawAPIV1Operation(route.id, route.method, "/integrations/kata/issues/{ref}/agreement", route.summary)
		operation.Parameters = append(operation.Parameters, &huma.Param{Name: "ref", In: pathKey, Required: true, Description: "Kata issue in the configured project", Schema: &huma.Schema{Type: huma.TypeString}})
		operation.Responses = jsonResponsesFor[KataAgreementResponse](api)
		if route.method == http.MethodPost {
			operation.RequestBody = jsonRequestBodyFor[KataAgreementRecordRequest](api)
		}
		addErrorResponses(api, operation.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
		registerRawHumaRoute(api, operation, route.handler)
	}
}

func (s *Server) agreementOperations(w http.ResponseWriter) kataAgreementOperations {
	operations, ok := s.kataIssueOperations.(kataAgreementOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "agreement_unavailable", "This Kata backend does not support agreement history")
		return nil
	}
	return operations
}

func (s *Server) handleKataAgreement(w http.ResponseWriter, r *http.Request) {
	operations := s.agreementOperations(w)
	if operations == nil {
		return
	}
	view, err := operations.Agreement(r.Context(), strings.TrimSpace(r.PathValue("ref")))
	if err != nil {
		s.writeAgreementError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, KataAgreementResponse{Agreement: view})
}

func (s *Server) handleRecordKataAgreement(w http.ResponseWriter, r *http.Request) {
	operations := s.agreementOperations(w)
	if operations == nil {
		return
	}
	var request KataAgreementRecordRequest
	if !decodeBoundedRequest(w, r, &request, "Invalid agreement event", kataevidence.MaxRequestBytes) {
		return
	}
	view, err := operations.RecordAgreement(r.Context(), strings.TrimSpace(r.PathValue("ref")), kataissues.AgreementInput(request))
	if err != nil {
		s.writeAgreementError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, KataAgreementResponse{Agreement: view})
}

func (s *Server) writeAgreementError(w http.ResponseWriter, err error) {
	for _, entry := range []struct {
		cause         error
		status        int
		code, message string
	}{
		{kataissues.ErrAgreementConflict, http.StatusConflict, "agreement_conflict", "Read current history; reuse an event ID only with its exact original input"},
		{kataissues.ErrUnsupportedAgreement, http.StatusConflict, "unsupported_agreement", "This agreement history is malformed or uses an unsupported format"},
		{kataissues.ErrAgreementClosed, http.StatusConflict, "agreement_closed", "Closed tasks allow agreement reads and exact retries, not new events"},
		{kataissues.ErrAgreementEvidenceUnlinked, http.StatusUnprocessableEntity, "agreement_evidence_unlinked", "Link every prepared citation to this task before recording an agreement event"},
		{kataissues.ErrAgreementFull, http.StatusUnprocessableEntity, "agreement_full", "The task cannot hold another agreement event without exceeding history or byte limits"},
		{kataissues.ErrInvalidRequest, http.StatusUnprocessableEntity, "invalid_agreement", "Review the event action, actor, target, terms and evidence; explicit review is required"},
	} {
		if errors.Is(err, entry.cause) {
			writeError(w, entry.status, entry.code, entry.message)
			return
		}
	}
	s.writeKataIssueError(w, err)
}

func (b *kataIssueBackend) Agreement(ctx context.Context, ref string) (kataissues.AgreementView, error) {
	service, err := b.service(ctx)
	if err != nil {
		return kataissues.AgreementView{}, err
	}
	return service.Agreement(ctx, ref)
}

func (b *kataIssueBackend) RecordAgreement(ctx context.Context, ref string, input kataissues.AgreementInput) (kataissues.AgreementView, error) {
	service, err := b.service(ctx)
	if err != nil {
		return kataissues.AgreementView{}, err
	}
	return service.RecordAgreement(ctx, ref, input)
}
