package api

import (
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/businessreviews"
)

func (s *Server) registerBusinessReviewsRoute(api huma.API) {
	op := rawAPIV1Operation("controlBusinessReviews", http.MethodPost, "/business-reviews/control", "Inspect live business reviews or confirm an exact owner reply")
	op.Tags = []string{"Business reviews"}
	op.RequestBody = jsonRequestBodyFor[businessreviews.Request](api)
	op.Responses = jsonResponsesFor[businessreviews.Result](api)
	op.Errors = []int{400, 401, 403, 404, 409, 413, 415, 429, 500, 502, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleBusinessReviews)
}
func (s *Server) handleBusinessReviews(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth := s.requestAuthentication(r)
	if auth.Mode == AuthModeDelegated || auth.Mode == AuthModeRemoteClient || auth.Mode == AuthModeCaller {
		writeError(w, http.StatusForbidden, "business_reviews_denied", "Business reviews require owner access")
		return
	}
	var request businessreviews.Request
	if !decodeBusinessReviewsRequest(w, r, &request) {
		return
	}
	principal := ""
	switch auth.Mode {
	case AuthModeAPIKey:
		principal = fmt.Sprintf("owner-key:%x", sha256.Sum256([]byte(s.cfg.Server.AuthenticationKey())))
	case AuthModeSession:
		principal = "session:" + auth.SessionID
	case AuthModeLoopback, AuthModeRequired, AuthModeCaller, AuthModeDelegated, AuthModeRemoteClient:
		// These modes do not establish an owner principal.
	}
	if principal == "" && (request.Action == "preview_reply" || request.Action == "confirm_reply" || request.Action == "receipt" || request.Action == "invalidate_target") {
		writeError(w, http.StatusForbidden, "business_reviews_denied", "Configure an owner API key to preview or confirm replies and read receipts")
		return
	}
	if s.businessReviews == nil {
		writeError(w, http.StatusServiceUnavailable, "business_reviews_unavailable", businessreviews.ErrUnavailable.Error())
		return
	}
	result, err := s.businessReviews.Execute(r.Context(), principal, request)
	if err != nil {
		status, code := http.StatusBadGateway, "business_reviews_unavailable"
		switch {
		case errors.Is(err, businessreviews.ErrInvalid):
			status, code = http.StatusBadRequest, "invalid_business_review_request"
		case errors.Is(err, businessreviews.ErrDenied):
			status, code = http.StatusForbidden, "business_reviews_denied"
		case errors.Is(err, businessreviews.ErrConflict):
			status, code = http.StatusConflict, "business_reviews_preview_changed"
		case errors.Is(err, businessreviews.ErrNotFound):
			status, code = http.StatusNotFound, "business_reviews_not_found"
		case errors.Is(err, businessreviews.ErrCapacity):
			status, code = http.StatusServiceUnavailable, "business_reviews_capacity"
		case errors.Is(err, businessreviews.ErrCredentials):
			status, code = http.StatusBadGateway, "business_reviews_credentials"
		case errors.Is(err, businessreviews.ErrPermission):
			status, code = http.StatusForbidden, "business_reviews_permission"
		case errors.Is(err, businessreviews.ErrQuota):
			status, code = http.StatusTooManyRequests, "business_reviews_quota"
		case errors.Is(err, businessreviews.ErrTemporary):
			status, code = http.StatusServiceUnavailable, "business_reviews_temporary"
		case errors.Is(err, businessreviews.ErrRejected):
			status, code = http.StatusBadRequest, "business_reviews_rejected"
		}
		// The service/provider return fixed classified messages only. Do not return
		// unexpected factory errors or upstream content through this route.
		message := businessreviews.ErrUnavailable.Error()
		for _, known := range []error{businessreviews.ErrInvalid, businessreviews.ErrDenied, businessreviews.ErrConflict, businessreviews.ErrNotFound, businessreviews.ErrCapacity, businessreviews.ErrUnknown, businessreviews.ErrCredentials, businessreviews.ErrPermission, businessreviews.ErrQuota, businessreviews.ErrTemporary, businessreviews.ErrRejected} {
			if errors.Is(err, known) {
				message = known.Error()
				break
			}
		}
		writeError(w, status, code, message)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func decodeBusinessReviewsRequest(w http.ResponseWriter, r *http.Request, request *businessreviews.Request) bool {
	invalid := func() bool {
		writeError(w, http.StatusBadRequest, "invalid_business_review_request", businessreviews.ErrInvalid.Error())
		return false
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != applicationJSONMediaType {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 32769))
	if err != nil {
		return invalid()
	}
	if len(data) > 32768 {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Business review request exceeds 32 KiB")
		return false
	}
	var fields map[string]jsontext.Value
	if json.Unmarshal(data, &fields) != nil || fields == nil {
		return invalid()
	}
	for _, value := range fields {
		if string(value) == "null" {
			return invalid()
		}
	}
	if json.Unmarshal(data, request, json.RejectUnknownMembers(true)) != nil {
		return invalid()
	}
	allowed := map[string]bool{"action": true}
	switch request.Action {
	case "invalidate_target":
		allowed["target"] = true
	case "list":
		allowed["target"] = true
		allowed["page_token"] = true
		allowed["page_size"] = true
	case "get":
		allowed["target"] = true
		allowed["review_id"] = true
	case "preview_reply", "assist_reply":
		allowed["target"] = true
		allowed["review_id"] = true
		allowed["comment"] = true
	case "confirm_reply":
		allowed["confirmation_token"] = true
		allowed["confirmed"] = true
	case "receipt":
		allowed["receipt_id"] = true
	}
	for key := range fields {
		if !allowed[key] {
			return invalid()
		}
	}
	if request.Validate() != nil {
		return invalid()
	}
	return true
}
