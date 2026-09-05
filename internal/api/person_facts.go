package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

// PersonFactStore is the bounded diagnostic and direct-pin capability used by
// the person fact API. It deliberately exposes no review workflow.
type PersonFactStore interface {
	BuildPersonFactCatalogContext(ctx context.Context, includeSensitive bool) (personfacts.Catalog, error)
	GetPersonContext(ctx context.Context, personID int64) (*store.Person, error)
	ListPersonFactEvidenceContext(
		ctx context.Context, personID int64, filter personfacts.EvidenceFilter,
	) ([]personfacts.Evidence, error)
	ListPersonFactEvidenceStatusEventsContext(
		ctx context.Context, personID int64, filter personfacts.EvidenceStatusFilter,
	) ([]personfacts.EvidenceStatusEvent, error)
	ListPersonFactClaimsContext(
		ctx context.Context, personID int64, filter personfacts.ClaimFilter,
	) ([]personfacts.Claim, error)
	ListPersonFactDecisionsContext(
		ctx context.Context, personID int64, filter personfacts.DecisionFilter,
	) ([]personfacts.Decision, error)
	ListPersonFactPinsContext(ctx context.Context, personID int64) ([]personfacts.PinState, error)
	SetPersonFactPinContext(
		ctx context.Context, personID int64, target personfacts.TargetRef, pinned bool, actor string,
	) (*personfacts.PinWrite, error)
}

var _ PersonFactStore = (*store.Store)(nil)

const personFactPathParameterLocation = "path"

type PersonFactEvidenceStatusEvent struct {
	ID            int64                            `json:"id"`
	GenerationID  int64                            `json:"generation_id"`
	EvidenceKey   string                           `json:"evidence_key"`
	SourceVersion string                           `json:"source_version" nullable:"true"`
	Supported     bool                             `json:"supported"`
	Reason        personfacts.EvidenceStatusReason `json:"reason"`
	CreatedAt     time.Time                        `json:"created_at"`
}

type PersonFactEvidence struct {
	ID              int64                           `json:"id"`
	EvidenceKey     string                          `json:"evidence_key"`
	SourceClass     personfacts.EvidenceSourceClass `json:"source_class"`
	Directness      personfacts.EvidenceDirectness  `json:"directness"`
	Authority       personfacts.EvidenceAuthority   `json:"authority"`
	SourceRef       string                          `json:"source_ref" nullable:"true"`
	SourceURL       string                          `json:"source_url" nullable:"true"`
	SubjectPersonID *int64                          `json:"subject_person_id,omitempty" nullable:"true"`
	SubjectRef      string                          `json:"subject_ref" nullable:"true"`
	SpanStart       *int64                          `json:"span_start,omitempty" nullable:"true"`
	SpanEnd         *int64                          `json:"span_end,omitempty" nullable:"true"`
	Excerpt         string                          `json:"excerpt" nullable:"true"`
	ContentSHA256   string                          `json:"content_sha256" nullable:"true"`
	SourceVersion   string                          `json:"source_version" nullable:"true"`
	EventTime       time.Time                       `json:"event_time"`
	RecordedTime    time.Time                       `json:"recorded_time"`
	IdentityScore   int                             `json:"identity_score"`
	Supported       bool                            `json:"supported"`
	LatestStatus    *PersonFactEvidenceStatusEvent  `json:"latest_status,omitempty"`
	CreatedAt       time.Time                       `json:"created_at"`
}

type PersonFactClaim struct {
	ID                 int64                        `json:"id"`
	GenerationID       int64                        `json:"generation_id"`
	ClaimKey           string                       `json:"claim_key"`
	ProgramID          string                       `json:"program_id"`
	ProgramVersion     string                       `json:"program_version"`
	ProgramFingerprint string                       `json:"program_fingerprint"`
	Target             personfacts.TargetRef        `json:"target"`
	Relation           personfacts.ClaimRelation    `json:"relation"`
	SubmittedValue     string                       `json:"submitted_value"`
	NormalizedValue    *string                      `json:"normalized_value,omitempty" nullable:"true"`
	ValueFingerprint   *string                      `json:"value_fingerprint,omitempty" nullable:"true"`
	EvidenceIDs        []int64                      `json:"evidence_ids"`
	ValidFrom          *time.Time                   `json:"valid_from,omitempty" nullable:"true"`
	ValidUntil         *time.Time                   `json:"valid_until,omitempty" nullable:"true"`
	Origin             personfacts.ClaimOrigin      `json:"origin"`
	Confidence         personfacts.ConfidenceInputs `json:"confidence"`
	CreatedAt          time.Time                    `json:"created_at"`
}

type PersonFactDecision struct {
	ID                int64                      `json:"id"`
	ResolutionID      int64                      `json:"resolution_id"`
	DecisionKey       string                     `json:"decision_key"`
	ClaimKey          string                     `json:"claim_key"`
	Action            personfacts.DecisionAction `json:"action"`
	Reason            personfacts.DecisionReason `json:"reason"`
	Score             PersonFactScoreBreakdown   `json:"score"`
	CompetingClaimKey string                     `json:"competing_claim_key,omitempty"`
	Projection        *personfacts.ProjectionRef `json:"projection,omitempty"`
	CreatedAt         time.Time                  `json:"created_at"`
}

type PersonFactScoreBreakdown struct {
	SourceClass   int `json:"source_class"`
	Directness    int `json:"directness"`
	Authority     int `json:"authority"`
	Confidence    int `json:"confidence"`
	Freshness     int `json:"freshness"`
	Corroboration int `json:"corroboration"`
	Total         int `json:"total"`
}

type PersonFactResolutionResult struct {
	ID               int64                       `json:"id"`
	Target           personfacts.TargetRef       `json:"target"`
	ResolverVersion  string                      `json:"resolver_version"`
	InputFingerprint string                      `json:"input_fingerprint"`
	ResolvedAt       time.Time                   `json:"resolved_at"`
	Decisions        []PersonFactDecision        `json:"decisions"`
	Projections      []personfacts.ProjectionRef `json:"projections"`
}

type PersonFactPinWrite struct {
	State       personfacts.PinState         `json:"state"`
	Resolutions []PersonFactResolutionResult `json:"resolutions"`
	Projections []personfacts.ProjectionRef  `json:"projections"`
}

type PersonFactEvidenceResponse struct {
	Evidence []PersonFactEvidence `json:"evidence"`
}

type PersonFactEvidenceStatusEventsResponse struct {
	Events []PersonFactEvidenceStatusEvent `json:"events"`
}

type PersonFactClaimsResponse struct {
	Claims []PersonFactClaim `json:"claims"`
}

type PersonFactDecisionsResponse struct {
	Decisions []PersonFactDecision `json:"decisions"`
}

type PersonFactPinsResponse struct {
	Pins []personfacts.PinState `json:"pins"`
}

type SetPersonFactPinRequest struct {
	Pinned bool `json:"pinned"`
}

func (s *Server) registerPersonFactRoutes(api huma.API) {
	catalog := rawAPIV1Operation("listPersonFactTargets", http.MethodGet,
		"/person-fact-targets", "List eligible automatic person fact targets")
	catalog.Parameters = append(catalog.Parameters,
		queryBooleanParam("include_sensitive", "Include sensitive targets"))
	catalog.Responses = jsonResponsesFor[personfacts.Catalog](api)
	addErrorResponses(api, catalog.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, catalog, s.handleListPersonFactTargets)

	evidence := personFactHistoryOperation("listPersonFactEvidence", "/people/{id}/fact-evidence",
		"List immutable person fact evidence")
	evidence.Responses = jsonResponsesFor[PersonFactEvidenceResponse](api)
	addErrorResponses(api, evidence.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, evidence, s.handleListPersonFactEvidence)

	status := rawAPIV1Operation("listPersonFactEvidenceStatusEvents", http.MethodGet,
		"/people/{id}/fact-evidence-status-events", "List person fact evidence status history")
	addPersonIDParameter(&status)
	status.Parameters = append(status.Parameters,
		queryStringParam("evidence_key", "Restrict to one immutable evidence key", false),
		queryBooleanParam("supported", "Restrict to supported or unsupported events"),
		queryIntegerParam(limitParam, "Maximum events to return (default 50, max 200)"),
		queryIntegerParam("offset", "Zero-based event offset"))
	status.Responses = jsonResponsesFor[PersonFactEvidenceStatusEventsResponse](api)
	addErrorResponses(api, status.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, status, s.handleListPersonFactEvidenceStatusEvents)

	claims := personFactHistoryOperation("listPersonFactClaims", "/people/{id}/fact-claims",
		"List immutable person fact claims")
	claims.Responses = jsonResponsesFor[PersonFactClaimsResponse](api)
	addErrorResponses(api, claims.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, claims, s.handleListPersonFactClaims)

	decisions := personFactHistoryOperation("listPersonFactDecisions", "/people/{id}/fact-decisions",
		"List immutable person fact decisions")
	decisions.Responses = jsonResponsesFor[PersonFactDecisionsResponse](api)
	addErrorResponses(api, decisions.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, decisions, s.handleListPersonFactDecisions)

	pins := rawAPIV1Operation("listPersonFactPins", http.MethodGet,
		"/people/{id}/fact-pins", "List effective person fact pins")
	addPersonIDParameter(&pins)
	pins.Responses = jsonResponsesFor[PersonFactPinsResponse](api)
	addErrorResponses(api, pins.Responses, http.StatusBadRequest, http.StatusNotFound,
		http.StatusServiceUnavailable)
	registerRawHumaRoute(api, pins, s.handleListPersonFactPins)

	setPin := rawAPIV1Operation("setPersonFactPin", http.MethodPut,
		"/people/{id}/fact-pins/{kind}/{key}", "Replace an effective person fact pin")
	addPersonIDParameter(&setPin)
	setPin.Parameters = append(setPin.Parameters,
		&huma.Param{Name: "kind", In: personFactPathParameterLocation, Required: true,
			Description: "Closed person fact target kind",
			Schema: &huma.Schema{Type: huma.TypeString, Enum: []any{
				string(personfacts.TargetAttribute), string(personfacts.TargetEmployment),
			}}},
		&huma.Param{Name: "key", In: personFactPathParameterLocation, Required: true,
			Description: "Exact person fact target key",
			Schema:      &huma.Schema{Type: huma.TypeString}},
	)
	setPin.RequestBody = jsonRequestBodyFor[SetPersonFactPinRequest](api)
	setPin.Responses = jsonResponsesFor[PersonFactPinWrite](api)
	addErrorResponses(api, setPin.Responses, http.StatusBadRequest, http.StatusConflict,
		http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, setPin, s.handleSetPersonFactPin)
}

func personFactHistoryOperation(operationID, path, summary string) huma.Operation {
	operation := rawAPIV1Operation(operationID, http.MethodGet, path, summary)
	addPersonIDParameter(&operation)
	operation.Parameters = append(operation.Parameters,
		queryStringParam("target", "Exact target as kind:key:sha256:<64 lowercase hex characters>", false),
		queryIntegerParam(limitParam, "Maximum rows to return (default 50, max 200)"),
		queryIntegerParam("offset", "Zero-based row offset"))
	return operation
}

func (s *Server) handleListPersonFactTargets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, ok := s.personFactStore(w)
	if !ok {
		return
	}
	includeSensitive, _, err := queryBool(r, "include_sensitive")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	catalog, err := facts.BuildPersonFactCatalogContext(r.Context(), includeSensitive)
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	if catalog.Targets == nil {
		catalog.Targets = []personfacts.TargetDescriptor{}
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (s *Server) handleListPersonFactEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, personID, ok := s.personFactRequestStore(w, r)
	if !ok {
		return
	}
	filter, ok := personFactTargetPageFilter(w, r)
	if !ok {
		return
	}
	rows, err := facts.ListPersonFactEvidenceContext(r.Context(), personID,
		personfacts.EvidenceFilter{Target: filter.target, Limit: filter.limit, Offset: filter.offset})
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	response := PersonFactEvidenceResponse{Evidence: make([]PersonFactEvidence, len(rows))}
	for index := range rows {
		response.Evidence[index] = personFactEvidenceDTO(rows[index])
	}
	writePersonFactDiagnostic(w, response)
}
func (s *Server) handleListPersonFactEvidenceStatusEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, personID, ok := s.personFactRequestStore(w, r)
	if !ok {
		return
	}
	page, ok := personFactPageFilter(w, r)
	if !ok {
		return
	}
	supported, present, err := queryBool(r, "supported")
	if err != nil {
		s.rejectBadParam(w, err)
		return
	}
	var supportedFilter *bool
	if present {
		supportedFilter = &supported
	}
	rows, err := facts.ListPersonFactEvidenceStatusEventsContext(r.Context(), personID,
		personfacts.EvidenceStatusFilter{
			EvidenceKey: strings.TrimSpace(r.URL.Query().Get("evidence_key")),
			Supported:   supportedFilter, Limit: page.limit, Offset: page.offset,
		})
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	response := PersonFactEvidenceStatusEventsResponse{
		Events: make([]PersonFactEvidenceStatusEvent, len(rows)),
	}
	for index := range rows {
		response.Events[index] = personFactEvidenceStatusEventDTO(rows[index])
	}
	writePersonFactDiagnostic(w, response)
}

func (s *Server) handleListPersonFactClaims(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, personID, ok := s.personFactRequestStore(w, r)
	if !ok {
		return
	}
	filter, ok := personFactTargetPageFilter(w, r)
	if !ok {
		return
	}
	rows, err := facts.ListPersonFactClaimsContext(r.Context(), personID,
		personfacts.ClaimFilter{Target: filter.target, Limit: filter.limit, Offset: filter.offset})
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	response := PersonFactClaimsResponse{Claims: make([]PersonFactClaim, len(rows))}
	for index := range rows {
		response.Claims[index] = personFactClaimDTO(rows[index])
	}
	writePersonFactDiagnostic(w, response)
}

func (s *Server) handleListPersonFactDecisions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, personID, ok := s.personFactRequestStore(w, r)
	if !ok {
		return
	}
	filter, ok := personFactTargetPageFilter(w, r)
	if !ok {
		return
	}
	rows, err := facts.ListPersonFactDecisionsContext(r.Context(), personID,
		personfacts.DecisionFilter{Target: filter.target, Limit: filter.limit, Offset: filter.offset})
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	response := PersonFactDecisionsResponse{Decisions: make([]PersonFactDecision, len(rows))}
	for index := range rows {
		response.Decisions[index] = personFactDecisionDTO(rows[index])
	}
	writePersonFactDiagnostic(w, response)
}

func (s *Server) handleListPersonFactPins(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, personID, ok := s.personFactRequestStore(w, r)
	if !ok {
		return
	}
	rows, err := facts.ListPersonFactPinsContext(r.Context(), personID)
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	if rows == nil {
		rows = []personfacts.PinState{}
	}
	writePersonFactDiagnostic(w, PersonFactPinsResponse{Pins: rows})
}

func (s *Server) handleSetPersonFactPin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	facts, personID, ok := s.personFactRequestStore(w, r)
	if !ok {
		return
	}
	kind, err := parsePersonFactTargetKind(r.PathValue("kind"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target_kind", err.Error())
		return
	}
	// net/http has already unescaped each wildcard path value exactly once.
	// Calling url.PathUnescape here would incorrectly decode literal %xx data.
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeError(w, http.StatusBadRequest, "invalid_target_key", "Person fact target key must not be empty")
		return
	}
	var request SetPersonFactPinRequest
	fields, ok := decodePersonRequestFields(w, r, &request)
	if !ok {
		return
	}
	if raw, present := fields["pinned"]; !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		writeError(w, http.StatusBadRequest, "bad_request", "pinned must be a boolean")
		return
	}
	catalog, err := facts.BuildPersonFactCatalogContext(r.Context(), true)
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	target, found := personFactCatalogTarget(catalog, kind, key)
	if !found {
		writeError(w, http.StatusBadRequest, "invalid_fact_target", "Person fact target is not active")
		return
	}
	write, err := facts.SetPersonFactPinContext(r.Context(), personID,
		personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision},
		request.Pinned, "api")
	if err != nil {
		s.writePersonFactError(w, err)
		return
	}
	if write == nil {
		s.logger.Error("person fact pin returned no result")
		writeError(w, http.StatusInternalServerError, "person_fact_failed", "Person fact operation failed")
		return
	}
	writePersonFactDiagnostic(w, personFactPinWriteDTO(write))
}

type personFactPageQuery struct {
	target *personfacts.TargetRef
	limit  int
	offset int
}

func personFactTargetPageFilter(w http.ResponseWriter, r *http.Request) (personFactPageQuery, bool) {
	filter, ok := personFactPageFilter(w, r)
	if !ok {
		return filter, false
	}
	rawTarget := r.URL.Query().Get("target")
	if rawTarget == "" {
		return filter, true
	}
	target, err := personfacts.DecodeTargetRef(rawTarget)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_target", err.Error())
		return filter, false
	}
	filter.target = &target
	return filter, true
}

func personFactPageFilter(w http.ResponseWriter, r *http.Request) (personFactPageQuery, bool) {
	filter := personFactPageQuery{}
	limit, present, err := queryInt(r, limitParam)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_limit", err.Error())
		return filter, false
	}
	if present && (limit < 1 || limit > 200) {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
		return filter, false
	}
	if present {
		filter.limit = limit
	}
	offset, present, err := queryInt(r, "offset")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_offset", err.Error())
		return filter, false
	}
	if present && offset < 0 {
		writeError(w, http.StatusBadRequest, "invalid_offset", "offset must not be negative")
		return filter, false
	}
	if present {
		filter.offset = offset
	}
	return filter, true
}

func parsePersonFactTargetKind(raw string) (personfacts.TargetKind, error) {
	kind := personfacts.TargetKind(strings.TrimSpace(raw))
	switch kind {
	case personfacts.TargetAttribute, personfacts.TargetEmployment:
		return kind, nil
	default:
		return "", fmt.Errorf("unknown person fact target kind %q", raw)
	}
}

func personFactCatalogTarget(
	catalog personfacts.Catalog, kind personfacts.TargetKind, key string,
) (personfacts.TargetDescriptor, bool) {
	for _, target := range catalog.Targets {
		if target.Kind == kind && target.Key == key {
			return target, true
		}
	}
	return personfacts.TargetDescriptor{}, false
}

func (s *Server) personFactStore(w http.ResponseWriter) (PersonFactStore, bool) {
	facts, ok := s.store.(PersonFactStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "person_facts_unavailable",
			"Person fact diagnostics are unavailable")
	}
	return facts, ok
}

func (s *Server) personFactRequestStore(
	w http.ResponseWriter, r *http.Request,
) (PersonFactStore, int64, bool) {
	facts, ok := s.personFactStore(w)
	if !ok {
		return nil, 0, false
	}
	personID, ok := personProfileID(w, r)
	if !ok {
		return nil, 0, false
	}
	if _, err := facts.GetPersonContext(r.Context(), personID); err != nil {
		s.writePersonFactError(w, err)
		return nil, 0, false
	}
	return facts, personID, true
}

func (s *Server) writePersonFactError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "person_profile_not_found", "Person profile not found")
	case errors.Is(err, store.ErrPersonFactKeyCollision):
		writeError(w, http.StatusConflict, "person_fact_conflict", "Person fact history conflicts with existing data")
	case errors.Is(err, store.ErrPersonFactPersonNotTracked),
		strings.Contains(err.Error(), "target descriptor is stale"),
		strings.Contains(err.Error(), "target") && strings.Contains(err.Error(), "is not active"):
		writeError(w, http.StatusConflict, "person_fact_conflict", err.Error())
	default:
		s.logger.Error("person fact operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "person_fact_failed", "Person fact operation failed")
	}
}

func writePersonFactDiagnostic(w http.ResponseWriter, response any) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func personFactEvidenceStatusEventDTO(
	event personfacts.EvidenceStatusEvent,
) PersonFactEvidenceStatusEvent {
	return PersonFactEvidenceStatusEvent{
		ID: event.ID, GenerationID: event.GenerationID, EvidenceKey: event.EvidenceKey,
		SourceVersion: event.SourceVersion, Supported: event.Supported,
		Reason: event.Reason, CreatedAt: event.CreatedAt,
	}
}

func personFactEvidenceDTO(evidence personfacts.Evidence) PersonFactEvidence {
	result := PersonFactEvidence{
		ID: evidence.ID, EvidenceKey: evidence.Key,
		SourceClass: evidence.Input.SourceClass, Directness: evidence.Input.Directness,
		Authority: evidence.Input.Authority, SourceRef: evidence.Input.SourceRef,
		SourceURL: evidence.Input.SourceURL, SubjectPersonID: evidence.Input.SubjectPersonID,
		SubjectRef: evidence.Input.SubjectRef, SpanStart: evidence.Input.SpanStart,
		SpanEnd: evidence.Input.SpanEnd, Excerpt: evidence.Input.Excerpt,
		ContentSHA256: evidence.Input.ContentSHA256, SourceVersion: evidence.Input.SourceVersion,
		EventTime: evidence.Input.EventTime, RecordedTime: evidence.Input.RecordedTime,
		IdentityScore: evidence.Input.IdentityScore, Supported: evidence.Supported,
		CreatedAt: evidence.CreatedAt,
	}
	if evidence.LatestStatus != nil {
		status := personFactEvidenceStatusEventDTO(*evidence.LatestStatus)
		result.LatestStatus = &status
	}
	return result
}

func personFactClaimDTO(claim personfacts.Claim) PersonFactClaim {
	result := PersonFactClaim{
		ID: claim.ID, GenerationID: claim.GenerationID, ClaimKey: claim.ClaimKey,
		ProgramID: claim.Generation.ProgramID, ProgramVersion: claim.Generation.ProgramVersion,
		ProgramFingerprint: claim.Generation.ProgramFingerprint, Target: claim.Target,
		Relation: claim.Relation, SubmittedValue: string(claim.SubmittedValue),
		EvidenceIDs: append([]int64(nil), claim.EvidenceIDs...), ValidFrom: claim.ValidFrom,
		ValidUntil: claim.ValidUntil, Origin: claim.Origin, Confidence: claim.Confidence,
		CreatedAt: claim.CreatedAt,
	}
	if result.EvidenceIDs == nil {
		result.EvidenceIDs = []int64{}
	}
	if claim.Normalized != nil {
		normalized := string(claim.Normalized.JSON)
		fingerprint := claim.Normalized.Fingerprint
		result.NormalizedValue = &normalized
		result.ValueFingerprint = &fingerprint
	}
	return result
}

func personFactDecisionDTO(decision personfacts.Decision) PersonFactDecision {
	return PersonFactDecision{
		ID: decision.ID, ResolutionID: decision.ResolutionID,
		DecisionKey: decision.DecisionKey, ClaimKey: decision.ClaimKey,
		Action: decision.Action, Reason: decision.Reason, Score: PersonFactScoreBreakdown{
			SourceClass: decision.Score.SourceClass, Directness: decision.Score.Directness,
			Authority: decision.Score.Authority, Confidence: decision.Score.Confidence,
			Freshness: decision.Score.Freshness, Corroboration: decision.Score.Corroboration,
			Total: decision.Score.Total,
		},
		CompetingClaimKey: decision.CompetingClaimKey, Projection: decision.Projection,
		CreatedAt: decision.CreatedAt,
	}
}

func personFactPinWriteDTO(write *personfacts.PinWrite) PersonFactPinWrite {
	response := PersonFactPinWrite{
		State:       write.State,
		Resolutions: make([]PersonFactResolutionResult, len(write.Resolutions)),
		Projections: append([]personfacts.ProjectionRef(nil), write.Projections...),
	}
	if response.Projections == nil {
		response.Projections = []personfacts.ProjectionRef{}
	}
	for index, resolution := range write.Resolutions {
		item := PersonFactResolutionResult{
			ID: resolution.ID, Target: resolution.Target,
			ResolverVersion:  resolution.ResolverVersion,
			InputFingerprint: resolution.InputFingerprint,
			ResolvedAt:       resolution.ResolvedAt,
			Decisions:        make([]PersonFactDecision, len(resolution.Decisions)),
			Projections:      append([]personfacts.ProjectionRef(nil), resolution.Projections...),
		}
		if item.Projections == nil {
			item.Projections = []personfacts.ProjectionRef{}
		}
		for decisionIndex, decision := range resolution.Decisions {
			item.Decisions[decisionIndex] = personFactDecisionDTO(decision)
		}
		response.Resolutions[index] = item
	}
	return response
}
