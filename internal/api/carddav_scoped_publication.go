package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/store"
)

const scopedCardDAVPublicationPath = "/api/v1/carddav/scoped/publications/"

// ScopedCardDAVPublicationOperations supports reviewed mapped updates with
// current native authorization and durable receipt admission.
type ScopedCardDAVPublicationOperations interface {
	PreviewPublicationAuthorized(ctx context.Context, personID int64, authorize store.PersonEditAuthorizer) (*carddav.PublicationPreview, error)
	PublishReviewedPersonWithReceipt(ctx context.Context, personID int64, token, principal, key string, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, error)
}

// ScopedCardDAVPublicationRecoveryOperations observes an existing reviewed
// update without admitting or dispatching another publication.
type ScopedCardDAVPublicationRecoveryOperations interface {
	ReconcileReviewedPersonWithReceipt(ctx context.Context, personID int64, token, principal, key string, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, error)
}

type scopedCardDAVReceiptStore interface {
	WithReviewedCardDAVPublicationReceipt(ctx context.Context, principal, key string, personID int64, token string) (context.Context, error)
	ReviewedCardDAVPublicationReceiptContext(ctx context.Context, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, error)
	ReviewedCardDAVPublicationRecoveryContext(ctx context.Context, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReceipt, *store.CardDAVPublication, error)
	LoadCardDAVPublicationReviewSourceAuthorizedContext(ctx context.Context, personID int64, authorize store.PersonEditAuthorizer) (*store.CardDAVPublicationReviewSource, error)
}

type CardDAVScopedPublicationApprovalRequest struct {
	ApprovalToken  string `json:"approval_token" minLength:"1" maxLength:"256"`
	IdempotencyKey string `json:"idempotency_key" minLength:"1" maxLength:"256"`
}

type CardDAVScopedPublicationReceiptResponse struct {
	Receipt *store.CardDAVPublicationReceipt `json:"receipt"`
}

func (s *Server) registerScopedCardDAVPublicationRoutes(api huma.API) {
	registerCardDAVIDJSONRoute[CardDAVPublicationPreviewResponse](api, "previewScopedCardDAVPublication", http.MethodGet, "/carddav/scoped/publications/{person_id}/preview", "person_id", "Preview one authorized mapped CardDAV update", s.handleScopedCardDAVPublicationPreview, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable, http.StatusInternalServerError)
	op := cardDAVIDOperation("approveScopedCardDAVPublication", http.MethodPost, "/carddav/scoped/publications/{person_id}/approve", "person_id", "Approve one mapped CardDAV update with a durable receipt")
	op.RequestBody = jsonRequestBodyFor[CardDAVScopedPublicationApprovalRequest](api)
	op.Responses = jsonResponsesFor[CardDAVScopedPublicationReceiptResponse](api)
	op.Responses[httpStatusKey(http.StatusAccepted)] = op.Responses[httpStatusKey(http.StatusOK)]
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusBadGateway)
	addCardDAVRetryAfterHeader(op.Responses)
	registerRawHumaRoute(api, op, s.handleScopedCardDAVPublicationApprove)
	op = cardDAVIDOperation("reconcileScopedCardDAVPublication", http.MethodPost, "/carddav/scoped/publications/{person_id}/reconcile", "person_id", "Observe an existing reviewed CardDAV update without replaying it")
	op.RequestBody = jsonRequestBodyFor[CardDAVScopedPublicationApprovalRequest](api)
	op.Responses = jsonResponsesFor[CardDAVScopedPublicationReceiptResponse](api)
	op.Responses[httpStatusKey(http.StatusAccepted)] = op.Responses[httpStatusKey(http.StatusOK)]
	addErrorResponses(api, op.Responses, http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable, http.StatusInternalServerError, http.StatusBadGateway)
	addCardDAVRetryAfterHeader(op.Responses)
	registerRawHumaRoute(api, op, s.handleScopedCardDAVPublicationReconcile)
}

func (s *Server) handleScopedCardDAVPublicationReconcile(w http.ResponseWriter, r *http.Request) {
	id, err := cardDAVPositivePathID(r, "person_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "A positive person_id is required")
		return
	}
	var request CardDAVScopedPublicationApprovalRequest
	if !decodeCardDAV(w, r, &request) {
		return
	}
	principal, authorize, err := s.scopedCardDAVPublicationAuthority(r, id)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	backend, ok := s.store.(scopedCardDAVReceiptStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "carddav_scope_unavailable", "Native scoped CardDAV publication is unavailable")
		return
	}
	bound, err := backend.WithReviewedCardDAVPublicationReceipt(r.Context(), principal, request.IdempotencyKey, id, request.ApprovalToken)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	receipt, pending, err := backend.ReviewedCardDAVPublicationRecoveryContext(bound, authorize)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	if pending == nil {
		s.writeScopedCardDAVPublicationReceipt(w, receipt)
		return
	}
	operations := s.cardDAVService(w)
	if operations == nil {
		return
	}
	service, ok := operations.(ScopedCardDAVPublicationRecoveryOperations)
	if !ok {
		writeError(w, http.StatusNotImplemented, "carddav_scope_unavailable", "Native scoped CardDAV recovery is unavailable")
		return
	}
	if s.operationGate != nil {
		release, ok := beginGateWorkBounded(r.Context(), s.operationGate, "reviewed CardDAV recovery")
		if !ok {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		defer release()
	}
	receipt, err = service.ReconcileReviewedPersonWithReceipt(bound, id, request.ApprovalToken, principal, request.IdempotencyKey, authorize)
	if receipt != nil {
		s.writeScopedCardDAVPublicationReceipt(w, receipt)
		return
	}
	s.writeScopedCardDAVPublicationError(w, err)
}

func (s *Server) scopedCardDAVPublicationAuthority(r *http.Request, personID int64) (string, store.PersonEditAuthorizer, error) {
	principal, err := identityOperationPrincipal(s.classifyAPIRequestDirect(r))
	if err != nil {
		return "", nil, errPersonScopeDenied
	}
	check := s.personMutationAuthorization(r, agentgrant.PermissionPersonRead)
	if principal == "owner" {
		check = func(context.Context, *store.IdentityGrantSelection) error {
			current, err := identityOperationPrincipal(s.classifyAPIRequestDirect(r))
			if err != nil || current != "owner" {
				return errPersonScopeDenied
			}
			return nil
		}
	}
	authorize := func(ctx context.Context, scope *store.IdentityGrantSelection) error {
		if scope == nil || len(scope.Persons) != 1 || scope.Persons[0].ID != personID || len(scope.AddressBooks) != 1 {
			return errPersonScopeDenied
		}
		return check(ctx, scope)
	}
	return principal, authorize, nil
}

func (s *Server) scopedCardDAVPublicationService(w http.ResponseWriter) ScopedCardDAVPublicationOperations {
	operations := s.cardDAVService(w)
	if operations == nil {
		return nil
	}
	scoped, ok := operations.(ScopedCardDAVPublicationOperations)
	if !ok {
		writeError(w, http.StatusNotImplemented, "carddav_scope_unavailable", "Native scoped CardDAV publication is unavailable")
		return nil
	}
	return scoped
}

func (s *Server) writeScopedCardDAVPublicationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPersonScopeDenied):
		writeError(w, http.StatusForbidden, "person_scope_denied", "Current credentials do not authorize this person and address book")
	case errors.Is(err, store.ErrCardDAVInvalidPlan):
		writeError(w, http.StatusBadRequest, "bad_request", "A mapped person, approval token, and idempotency key are required")
	case errors.Is(err, store.ErrCardDAVPublicationReceiptNotFound):
		writeError(w, http.StatusNotFound, "carddav_receipt_not_found", "No reviewed publication receipt exists for this request")
	default:
		s.writeCardDAVOperationError(w, err, "CardDAV publication failed")
	}
}

func (s *Server) handleScopedCardDAVPublicationPreview(w http.ResponseWriter, r *http.Request) {
	id, err := cardDAVPositivePathID(r, "person_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "A positive person_id is required")
		return
	}
	_, authorize, err := s.scopedCardDAVPublicationAuthority(r, id)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	backend, ok := s.store.(scopedCardDAVReceiptStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "carddav_scope_unavailable", "Native scoped CardDAV publication is unavailable")
		return
	}
	if _, err := backend.LoadCardDAVPublicationReviewSourceAuthorizedContext(r.Context(), id, authorize); err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	service := s.scopedCardDAVPublicationService(w)
	if service == nil {
		return
	}
	preview, err := service.PreviewPublicationAuthorized(r.Context(), id, authorize)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, CardDAVPublicationPreviewResponse{PersonID: preview.PersonID, AddressBook: addressBookIdentityResponse(preview.AddressBook), Kind: preview.Kind, VCard: preview.VCard, ApprovalToken: preview.ApprovalToken, ReviewRequired: preview.ReviewRequired, ConflictID: preview.ConflictID})
}

func (s *Server) handleScopedCardDAVPublicationApprove(w http.ResponseWriter, r *http.Request) {
	id, err := cardDAVPositivePathID(r, "person_id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "A positive person_id is required")
		return
	}
	var request CardDAVScopedPublicationApprovalRequest
	if !decodeCardDAV(w, r, &request) {
		return
	}
	principal, authorize, err := s.scopedCardDAVPublicationAuthority(r, id)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	backend, ok := s.store.(scopedCardDAVReceiptStore)
	if !ok {
		writeError(w, http.StatusNotImplemented, "carddav_scope_unavailable", "Native scoped CardDAV publication is unavailable")
		return
	}
	bound, err := backend.WithReviewedCardDAVPublicationReceipt(r.Context(), principal, request.IdempotencyKey, id, request.ApprovalToken)
	if err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	receipt, err := backend.ReviewedCardDAVPublicationReceiptContext(bound, authorize)
	if err == nil {
		s.writeScopedCardDAVPublicationReceipt(w, receipt)
		return
	}
	if !errors.Is(err, store.ErrCardDAVPublicationReceiptNotFound) {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	if _, err := backend.LoadCardDAVPublicationReviewSourceAuthorizedContext(bound, id, authorize); err != nil {
		s.writeScopedCardDAVPublicationError(w, err)
		return
	}
	service := s.scopedCardDAVPublicationService(w)
	if service == nil {
		return
	}
	if s.operationGate != nil {
		release, ok := beginGateWorkBounded(r.Context(), s.operationGate, "reviewed CardDAV publication")
		if !ok {
			writeOperationGateBusy(w, r, s.operationGate)
			return
		}
		defer release()
	}
	receipt, err = service.PublishReviewedPersonWithReceipt(bound, id, request.ApprovalToken, principal, request.IdempotencyKey, authorize)
	if receipt != nil {
		s.writeScopedCardDAVPublicationReceipt(w, receipt)
		return
	}
	s.writeScopedCardDAVPublicationError(w, err)
}

func (s *Server) writeScopedCardDAVPublicationReceipt(w http.ResponseWriter, receipt *store.CardDAVPublicationReceipt) {
	status := http.StatusOK
	if receipt.State == "dispatching" {
		status = http.StatusAccepted
	}
	writeJSON(w, status, CardDAVScopedPublicationReceiptResponse{Receipt: receipt})
}
