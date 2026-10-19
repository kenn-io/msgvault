package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/persontiers"
	"go.kenn.io/msgvault/internal/store"
)

// PersonTierStore supplies tier planning and explicit automation preferences.
type PersonTierStore interface {
	PlanPersonTiersContext(ctx context.Context, req store.PersonTierPageRequest) (*store.PersonTierPage, error)
	ApplyPersonTiersPageContext(ctx context.Context, req store.PersonTierApplyRequest) (*store.PersonTierPage, error)
	ExplainPersonTierContext(ctx context.Context, personID int64, req store.PersonTierPageRequest) (*store.PersonTierExplanation, error)
	ResetPersonTiersContext(ctx context.Context, actor string) (*store.PersonTierResetResult, error)
	PersonTierHistoryContext(ctx context.Context, personID int64, limit, offset int) ([]store.PersonTierEvent, error)
	PersonTierCandidatesContext(ctx context.Context, afterID int64, limit int, asOf time.Time, cfg persontiers.Config, timezone string, maxDirect int) (store.PersonTierCandidatePage, error)
	SetPersonTierOptOutContext(ctx context.Context, personID int64, lane string, optedOut bool, actor, reason string) error
	ListPersonTierOptOutsContext(ctx context.Context, personID int64) ([]store.PersonTierOptOut, error)
}

var _ PersonTierStore = (*store.Store)(nil)

// ApplyPersonTiersRequest applies one bounded page. Continuations retain both
// the run ID and cursor returned by the preceding page.
type ApplyPersonTiersRequest struct {
	DryRun bool   `json:"dry_run,omitempty"`
	Limit  int    `json:"limit,omitempty" minimum:"1" maximum:"500"`
	Cursor string `json:"cursor,omitempty"`
	RunID  string `json:"run_id,omitempty"`
}

type ResetPersonTiersRequest struct {
	Confirm bool `json:"confirm"`
}
type PutPersonTierOptOutRequest struct {
	Lane     string `json:"lane" enum:"tracking,briefs,enrichment"`
	OptedOut bool   `json:"opted_out"`
	Reason   string `json:"reason,omitempty"`
}
type PersonTierHistoryResponse struct {
	Events []store.PersonTierEvent `json:"events"`
}
type PersonTierOptOutsResponse struct {
	PersonID int64                    `json:"person_id"`
	OptOuts  []store.PersonTierOptOut `json:"opt_outs"`
}

func (s *Server) registerPersonTierRoutes(api huma.API) {
	preview := rawAPIV1Operation("previewPersonTiers", http.MethodGet, "/people-tiers/preview", "Preview automatic people tiers")
	preview.Parameters = []*huma.Param{queryIntegerParam("tier", "Only return this tier (1..3)"), queryBooleanParam("changes_only", "Only return changed decisions"), queryIntegerParam("limit", "Maximum people (default 200, max 500)"), queryStringParam("cursor", "Opaque continuation cursor", false)}
	preview.Responses = jsonResponsesFor[store.PersonTierPage](api)
	addPersonTierErrors(api, preview.Responses)
	registerRawHumaRoute(api, preview, s.handlePreviewPersonTiers)

	apply := rawAPIV1Operation("applyPersonTiers", http.MethodPost, "/people-tiers/apply", "Apply one page of automatic people tiers")
	apply.RequestBody = jsonRequestBodyFor[ApplyPersonTiersRequest](api)
	apply.Responses = jsonResponsesFor[store.PersonTierPage](api)
	addPersonTierErrors(api, apply.Responses)
	registerRawHumaRoute(api, apply, s.handleApplyPersonTiers)

	reset := rawAPIV1Operation("resetPersonTiers", http.MethodPost, "/people-tiers/reset", "Reset automatic people tier grants and state")
	reset.RequestBody = jsonRequestBodyFor[ResetPersonTiersRequest](api)
	reset.Responses = jsonResponsesFor[store.PersonTierResetResult](api)
	addPersonTierErrors(api, reset.Responses)
	registerRawHumaRoute(api, reset, s.handleResetPersonTiers)

	history := rawAPIV1Operation("listPersonTierEvents", http.MethodGet, "/people-tiers/history", "List people tier events")
	history.Parameters = []*huma.Param{queryIntegerParam("person_id", "Filter by a durable person ID"), queryIntegerParam("limit", "Maximum events (default 20, max 200)"), queryIntegerParam("offset", "Events to skip")}
	history.Responses = jsonResponsesFor[PersonTierHistoryResponse](api)
	addPersonTierErrors(api, history.Responses)
	registerRawHumaRoute(api, history, s.handleListPersonTierEvents)

	candidates := rawAPIV1Operation("listPersonTierCandidates", http.MethodGet, "/people-tiers/candidates", "List participants to review for person profiles")
	candidates.Parameters = []*huma.Param{queryIntegerParam("limit", "Maximum participant groups (default 50, max 500)"), queryStringParam("cursor", "Decimal participant group continuation ID", false)}
	candidates.Responses = jsonResponsesFor[store.PersonTierCandidatePage](api)
	addPersonTierErrors(api, candidates.Responses)
	registerRawHumaRoute(api, candidates, s.handleListPersonTierCandidates)

	explain := rawAPIV1Operation("getPersonTier", http.MethodGet, "/people/{id}/tier", "Explain a person's automatic tier")
	addPersonIDParameter(&explain)
	explain.Responses = jsonResponsesFor[store.PersonTierExplanation](api)
	addPersonTierErrors(api, explain.Responses)
	registerRawHumaRoute(api, explain, s.handleGetPersonTier)

	optOut := rawAPIV1Operation("setPersonTierOptOut", http.MethodPut, "/people/{id}/tier/opt-outs", "Replace one people tier automation preference")
	addPersonIDParameter(&optOut)
	optOut.RequestBody = jsonRequestBodyFor[PutPersonTierOptOutRequest](api)
	optOut.Responses = jsonResponsesFor[PersonTierOptOutsResponse](api)
	addPersonTierErrors(api, optOut.Responses)
	registerRawHumaRoute(api, optOut, s.handleSetPersonTierOptOut)
}

func addPersonTierErrors(api huma.API, responses map[string]*huma.Response) {
	addErrorResponses(api, responses, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
}
func (s *Server) personTierStore(w http.ResponseWriter) (PersonTierStore, bool) {
	tiers, ok := s.store.(PersonTierStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_tiers_unavailable", "People tiers are unavailable")
	}
	return tiers, ok
}
func (s *Server) personTierPageRequest(limit int) store.PersonTierPageRequest {
	return store.PersonTierPageRequest{Config: s.cfg.People.Tiers, Timezone: s.cfg.Activity.Timezone, MaxDirect: s.cfg.Activity.MaxDirectCounterparts, AsOf: s.clockNow(), Limit: limit}
}
func personTierQueryLimit(w http.ResponseWriter, r *http.Request, defaultLimit, maxLimit int) (int, bool) {
	limit, present, err := queryInt(r, "limit")
	if err != nil || present && (limit < 1 || limit > maxLimit) {
		writeError(w, http.StatusBadRequest, "bad_request", "limit is outside the allowed range")
		return 0, false
	}
	if !present {
		limit = defaultLimit
	}
	return limit, true
}
func (s *Server) handlePreviewPersonTiers(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	limit, ok := personTierQueryLimit(w, r, 200, 500)
	if !ok {
		return
	}
	req := s.personTierPageRequest(limit)
	tier, present, err := queryInt(r, "tier")
	if err != nil || present && (tier < 1 || tier > 3) {
		writeError(w, http.StatusBadRequest, "bad_request", "tier must be 1, 2 or 3")
		return
	}
	req.Tier = tier
	changes, _, err := queryBool(r, "changes_only")
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.ChangesOnly = changes
	req.Cursor = r.URL.Query().Get("cursor")
	page, err := tiers.PlanPersonTiersContext(r.Context(), req)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) handleApplyPersonTiers(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	var body ApplyPersonTiersRequest
	fields, ok := decodePersonRequestFields(w, r, &body)
	if !ok {
		return
	}
	for key, value := range fields {
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeError(w, http.StatusBadRequest, "bad_request", key+" must not be null")
			return
		}
	}
	limit := body.Limit
	if _, present := fields["limit"]; present {
		if limit < 1 || limit > 500 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be 1..500")
			return
		}
	} else {
		limit = 500
	}
	req := store.PersonTierApplyRequest{PersonTierPageRequest: s.personTierPageRequest(limit), RunID: body.RunID, Actor: personBriefActor, DryRun: body.DryRun}
	req.Cursor = body.Cursor
	page, err := tiers.ApplyPersonTiersPageContext(r.Context(), req)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) handleResetPersonTiers(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	var body ResetPersonTiersRequest
	_, ok = decodePersonRequestFields(w, r, &body)
	if !ok {
		return
	}
	if !body.Confirm {
		writeError(w, http.StatusBadRequest, "bad_request", "confirm must be true")
		return
	}
	result, err := tiers.ResetPersonTiersContext(r.Context(), personBriefActor)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleListPersonTierEvents(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	limit, ok := personTierQueryLimit(w, r, 20, 200)
	if !ok {
		return
	}
	personID, present, err := queryInt64(r, "person_id")
	if err != nil || present && personID < 1 {
		writeError(w, http.StatusBadRequest, "bad_request", "person_id must be positive")
		return
	}
	offset, _, err := queryInt(r, "offset")
	if err != nil || offset < 0 {
		writeError(w, http.StatusBadRequest, "bad_request", "offset must be nonnegative")
		return
	}
	events, err := tiers.PersonTierHistoryContext(r.Context(), personID, limit, offset)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PersonTierHistoryResponse{Events: events})
}
func (s *Server) handleListPersonTierCandidates(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	limit, ok := personTierQueryLimit(w, r, 50, 500)
	if !ok {
		return
	}
	var afterID int64
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		var err error
		afterID, err = strconv.ParseInt(cursor, 10, 64)
		if err != nil || afterID < 1 {
			writeError(w, http.StatusBadRequest, "bad_request", "cursor must be a positive participant ID")
			return
		}
	}
	req := s.personTierPageRequest(limit)
	page, err := tiers.PersonTierCandidatesContext(r.Context(), afterID, limit, req.AsOf, req.Config, req.Timezone, req.MaxDirect)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}
func (s *Server) handleGetPersonTier(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	explanation, err := tiers.ExplainPersonTierContext(r.Context(), personID, s.personTierPageRequest(1))
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, explanation)
}
func (s *Server) handleSetPersonTierOptOut(w http.ResponseWriter, r *http.Request) {
	tiers, ok := s.personTierStore(w)
	if !ok {
		return
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return
	}
	var body PutPersonTierOptOutRequest
	fields, ok := decodePersonRequestFields(w, r, &body)
	if !ok {
		return
	}
	value, present := fields["opted_out"]
	if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		writeError(w, http.StatusBadRequest, "bad_request", "opted_out must be a boolean")
		return
	}
	switch body.Lane {
	case store.PersonTierLaneTracking, store.PersonTierLaneBriefs, store.PersonTierLaneEnrichment:
	default:
		writeError(w, http.StatusBadRequest, "bad_request", "lane must be tracking, briefs or enrichment")
		return
	}
	if err := tiers.SetPersonTierOptOutContext(r.Context(), personID, body.Lane, body.OptedOut, personBriefActor, body.Reason); err != nil {
		s.writePersonTierError(w, err)
		return
	}
	outs, err := tiers.ListPersonTierOptOutsContext(r.Context(), personID)
	if err != nil {
		s.writePersonTierError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, PersonTierOptOutsResponse{PersonID: personID, OptOuts: outs})
}
func (s *Server) writePersonTierError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrInvalidPersonTierRequest), errors.Is(err, store.ErrInvalidActivityRequest):
		writeError(w, http.StatusBadRequest, "invalid_people_tier_request", err.Error())
	case errors.Is(err, store.ErrPersonTierRunSuperseded):
		writeError(w, http.StatusConflict, "people_tier_run_superseded", err.Error())
	case errors.Is(err, store.ErrPersonTierManualGrant), errors.Is(err, store.ErrPersonTierManualRunOpen):
		writeError(w, http.StatusConflict, "people_tier_manual_authority", err.Error())
	default:
		s.writePersonError(w, err)
	}
}
