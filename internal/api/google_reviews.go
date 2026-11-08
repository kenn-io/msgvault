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
	"net/url"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/googlereviews"
)

func (s *Server) registerGoogleReviewsRoute(api huma.API) {
	op := rawAPIV1Operation("controlGoogleReviews", http.MethodPost, "/google-reviews/control", "Read live Google reviews, check archive backfill status, start a backfill, or confirm an exact owner reply")
	op.Tags = []string{"Google Reviews"}
	op.RequestBody = jsonRequestBodyFor[googlereviews.Request](api)
	op.Responses = jsonResponsesFor[googlereviews.Result](api)
	op.Errors = []int{400, 401, 403, 404, 409, 413, 415, 429, 500, 502, 503}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleGoogleReviews)

	search := rawAPIV1Operation("searchGoogleReviewsArchive", http.MethodGet, "/google-reviews/archive/search", "Search archived Google reviews")
	search.Tags = []string{"Google Reviews"}
	query := queryStringParam("q", "Full-text query over archived review text, reviewer names, ratings, and replies (maximum 256 UTF-8 bytes)", true)
	minimumQueryLength := 1
	query.Schema.MinLength = &minimumQueryLength
	target := queryStringParam("target", "Restrict results to one archive target", false)
	page := queryIntegerParam("page", "One-based result page (default 1)")
	pageMinimum, pageMaximum := float64(1), float64(100000)
	page.Schema.Minimum, page.Schema.Maximum, page.Schema.Default = &pageMinimum, &pageMaximum, 1
	pageSize := queryIntegerParam("page_size", "Results per page (default 20, maximum 100)")
	pageSizeMinimum, pageSizeMaximum := float64(1), float64(100)
	pageSize.Schema.Minimum, pageSize.Schema.Maximum, pageSize.Schema.Default = &pageSizeMinimum, &pageSizeMaximum, 20
	search.Parameters = []*huma.Param{query, target, page, pageSize}
	search.Responses = jsonResponsesFor[googlereviews.ArchiveSearchResponse](api)
	addErrorResponses(api, search.Responses, http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, search, s.handleGoogleReviewsArchiveSearch)
}
func (s *Server) handleGoogleReviews(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth := s.requestAuthentication(r)
	if auth.Mode == AuthModeDelegated || auth.Mode == AuthModeRemoteClient || auth.Mode == AuthModeCaller {
		writeError(w, http.StatusForbidden, "google_reviews_denied", "Google Reviews require owner access")
		return
	}
	var request googlereviews.Request
	if !decodeGoogleReviewsRequest(w, r, &request) {
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
	if principal == "" && (request.Action == "preview_reply" || request.Action == "confirm_reply" || request.Action == "receipt" || request.Action == "invalidate_target" || request.Action == "backfill") {
		message := "Configure owner authentication to preview or confirm replies and read receipts"
		if request.Action == "backfill" {
			message = "Owner authentication is required to start a Google Reviews archive backfill"
		}
		writeError(w, http.StatusForbidden, "google_reviews_denied", message)
		return
	}
	if s.googleReviews == nil {
		writeError(w, http.StatusServiceUnavailable, "google_reviews_unavailable", googlereviews.ErrUnavailable.Error())
		return
	}
	result, err := s.googleReviews.Execute(r.Context(), principal, request)
	if err != nil {
		status, code := http.StatusBadGateway, "google_reviews_unavailable"
		switch {
		case errors.Is(err, googlereviews.ErrInvalid):
			status, code = http.StatusBadRequest, "invalid_google_review_request"
		case errors.Is(err, googlereviews.ErrDenied):
			status, code = http.StatusForbidden, "google_reviews_denied"
		case errors.Is(err, googlereviews.ErrConflict):
			status, code = http.StatusConflict, "google_reviews_preview_changed"
		case errors.Is(err, googlereviews.ErrNotFound):
			status, code = http.StatusNotFound, "google_reviews_not_found"
		case errors.Is(err, googlereviews.ErrCapacity):
			status, code = http.StatusServiceUnavailable, "google_reviews_capacity"
		case errors.Is(err, googlereviews.ErrArchiveBusy):
			status, code = http.StatusConflict, "google_reviews_backfill_running"
		case errors.Is(err, googlereviews.ErrArchive), errors.Is(err, googlereviews.ErrArchiveCycle):
			status, code = http.StatusServiceUnavailable, "google_reviews_archive_unavailable"
		case errors.Is(err, googlereviews.ErrCredentials):
			status, code = http.StatusBadGateway, "google_reviews_credentials"
		case errors.Is(err, googlereviews.ErrPermission):
			status, code = http.StatusForbidden, "google_reviews_permission"
		case errors.Is(err, googlereviews.ErrQuota):
			status, code = http.StatusTooManyRequests, "google_reviews_quota"
		case errors.Is(err, googlereviews.ErrTemporary):
			status, code = http.StatusServiceUnavailable, "google_reviews_temporary"
		case errors.Is(err, googlereviews.ErrRejected):
			status, code = http.StatusBadRequest, "google_reviews_rejected"
		}
		// The service/provider return fixed classified messages only. Do not return
		// unexpected factory errors or upstream content through this route.
		message := googlereviews.ErrUnavailable.Error()
		for _, known := range []error{googlereviews.ErrInvalid, googlereviews.ErrDenied, googlereviews.ErrConflict, googlereviews.ErrNotFound, googlereviews.ErrCapacity, googlereviews.ErrArchiveBusy, googlereviews.ErrArchive, googlereviews.ErrArchiveCycle, googlereviews.ErrUnknown, googlereviews.ErrCredentials, googlereviews.ErrPermission, googlereviews.ErrQuota, googlereviews.ErrTemporary, googlereviews.ErrRejected} {
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
func decodeGoogleReviewsRequest(w http.ResponseWriter, r *http.Request, request *googlereviews.Request) bool {
	invalid := func() bool {
		writeError(w, http.StatusBadRequest, "invalid_google_review_request", googlereviews.ErrInvalid.Error())
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
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large", "Google Reviews request exceeds 32 KiB")
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
	case "invalidate_target", "archive_status", "backfill":
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

func (s *Server) handleGoogleReviewsArchiveSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth := s.requestAuthentication(r)
	if auth.Mode == AuthModeDelegated || auth.Mode == AuthModeRemoteClient || auth.Mode == AuthModeCaller {
		writeError(w, http.StatusForbidden, "google_reviews_denied", "Google Reviews require owner access")
		return
	}
	if s.googleReviews == nil {
		writeError(w, http.StatusServiceUnavailable, "google_reviews_unavailable", googlereviews.ErrArchive.Error())
		return
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_google_review_search", googlereviews.ErrSearch.Error())
		return
	}
	allowed := map[string]bool{"q": true, "target": true, "page": true, "page_size": true}
	for key, value := range values {
		if !allowed[key] || len(value) != 1 {
			writeError(w, http.StatusBadRequest, "invalid_google_review_search", googlereviews.ErrSearch.Error())
			return
		}
	}
	request := googlereviews.ArchiveSearchRequest{Query: values.Get("q"), Page: 1, PageSize: 20}
	if values.Has("target") {
		request.Target = values.Get("target")
	}
	if values.Has("page") {
		request.Page, err = strconv.Atoi(values.Get("page"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_google_review_search", googlereviews.ErrSearch.Error())
			return
		}
	}
	if values.Has("page_size") {
		request.PageSize, err = strconv.Atoi(values.Get("page_size"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_google_review_search", googlereviews.ErrSearch.Error())
			return
		}
	}
	response, err := s.googleReviews.SearchArchive(r.Context(), request)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "google_reviews_archive_unavailable"
		if errors.Is(err, googlereviews.ErrSearch) || errors.Is(err, googlereviews.ErrInvalid) {
			status, code = http.StatusBadRequest, "invalid_google_review_search"
		} else if errors.Is(err, googlereviews.ErrDenied) {
			status, code = http.StatusForbidden, "google_reviews_denied"
		}
		writeError(w, status, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, response)
}
