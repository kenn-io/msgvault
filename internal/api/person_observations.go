package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
	"go.kenn.io/msgvault/internal/store"
)

type PersonObservationStore interface {
	GetPersonContext(ctx context.Context, id int64) (*store.Person, error)
	ListPersonObservations(ctx context.Context, personID int64, filter personobs.Filter) (personobs.Page, error)
	GetPersonObservation(ctx context.Context, id int64) (personobs.Detail, error)
	AddOwnerObservation(ctx context.Context, input personobs.OwnerInput) (personobs.Observation, error)
	DecidePersonObservation(ctx context.Context, decision personobs.Decision) (personobs.Observation, error)
	ClearObservationSuppression(ctx context.Context, personID int64, key, actor string) error
	ListObservationSuppressions(ctx context.Context, personID int64) ([]store.ObservationSuppression, error)
}

var _ PersonObservationStore = (*store.Store)(nil)

type PersonObservation struct {
	ID                 int64                          `json:"id"`
	PersonID           int64                          `json:"person_id"`
	Sequence           int64                          `json:"sequence"`
	GenerationID       *int64                         `json:"generation_id,omitzero" nullable:"false"`
	Level              personobs.Level                `json:"level"`
	Origin             personobs.Origin               `json:"origin"`
	Kind               personobs.Kind                 `json:"kind"`
	Content            string                         `json:"content"`
	ContentKey         string                         `json:"content_key"`
	SubjectRef         string                         `json:"subject_ref" nullable:"true"`
	SubjectPersonID    *int64                         `json:"subject_person_id,omitzero" nullable:"false"`
	Directness         personfacts.EvidenceDirectness `json:"directness"`
	Sensitive          bool                           `json:"sensitive"`
	ConfidenceBP       int                            `json:"confidence_bp"`
	ValidFrom          *time.Time                     `json:"valid_from" nullable:"true"`
	ValidUntil         *time.Time                     `json:"valid_until" nullable:"true"`
	OccursOn           *personfacts.PartialDateValue  `json:"occurs_on,omitzero" nullable:"false"`
	Status             personobs.Status               `json:"status"`
	StatusReason       personobs.StatusReason         `json:"status_reason" nullable:"true"`
	SupersededByID     *int64                         `json:"superseded_by_id,omitzero" nullable:"false"`
	OwnerState         personobs.OwnerState           `json:"owner_state" nullable:"true"`
	Pinned             bool                           `json:"pinned"`
	SupportFlag        personobs.SupportFlag          `json:"support_flag" nullable:"true"`
	Stability          personobs.Stability            `json:"stability"`
	StabilitySource    personobs.StabilitySource      `json:"stability_source"`
	TargetKind         string                         `json:"target_kind" nullable:"true"`
	TargetKey          string                         `json:"target_key" nullable:"true"`
	Categories         []personobs.Category           `json:"categories"`
	MentionCount       int                            `json:"mention_count"`
	SupportedCount     int                            `json:"supported_count"`
	FirstSeenAt        *time.Time                     `json:"first_seen_at" nullable:"true"`
	LastSeenAt         *time.Time                     `json:"last_seen_at" nullable:"true"`
	PremiseChangedAt   *time.Time                     `json:"premise_changed_at" nullable:"true"`
	ProgramFingerprint string                         `json:"program_fingerprint" nullable:"true"`
	CreatedAt          time.Time                      `json:"created_at"`
	UpdatedAt          time.Time                      `json:"updated_at"`
}

func personObservationDTO(o personobs.Observation) PersonObservation {
	result := PersonObservation{ID: o.ID, PersonID: o.PersonID, Sequence: o.Sequence, GenerationID: o.GenerationID, Level: o.Level, Origin: o.Origin, Kind: o.Kind, Content: o.Content, ContentKey: o.ContentKey, SubjectRef: o.SubjectRef, SubjectPersonID: o.SubjectPersonID, Directness: o.Directness, Sensitive: o.Sensitive, ConfidenceBP: o.ConfidenceBP, ValidFrom: o.ValidFrom, ValidUntil: o.ValidUntil, OccursOn: o.OccursOn, Status: o.Status, StatusReason: o.StatusReason, SupersededByID: o.SupersededByID, OwnerState: o.OwnerState, Pinned: o.Pinned, SupportFlag: o.SupportFlag, Stability: o.Stability, StabilitySource: o.StabilitySource, TargetKind: o.TargetKind, TargetKey: o.TargetKey, Categories: o.Categories, MentionCount: o.MentionCount, SupportedCount: o.SupportedCount, FirstSeenAt: o.FirstSeenAt, LastSeenAt: o.LastSeenAt, PremiseChangedAt: o.PremiseChangedAt, ProgramFingerprint: o.ProgramFingerprint, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt}
	if result.Categories == nil {
		result.Categories = []personobs.Category{}
	}
	return result
}

type PersonObservationLeaf struct {
	MessageID        int64     `json:"message_id"`
	ConversationID   int64     `json:"conversation_id"`
	ConversationKind string    `json:"conversation_kind"`
	AuthoredByPerson bool      `json:"authored_by_person"`
	AuthorIsOwner    bool      `json:"author_is_owner"`
	EventTime        time.Time `json:"event_time"`
}
type PersonObservationCitation struct {
	EvidenceID  int64                     `json:"evidence_id"`
	QuoteStart  int                       `json:"quote_start"`
	QuoteEnd    int                       `json:"quote_end"`
	QuoteSHA256 string                    `json:"quote_sha256"`
	Exclusion   personobs.ExclusionReason `json:"exclusion" nullable:"true"`
	Leaf        PersonObservationLeaf     `json:"leaf"`
}
type PersonObservationPremise struct {
	ID   int64                 `json:"id"`
	Role personobs.PremiseRole `json:"role"`
}
type PersonObservationEvent struct {
	ID            int64     `json:"id"`
	ObservationID int64     `json:"observation_id"`
	Event         string    `json:"event"`
	Actor         string    `json:"actor"`
	Detail        string    `json:"detail" nullable:"true"`
	CreatedAt     time.Time `json:"created_at"`
}
type PersonObservationDetail struct {
	PersonObservation

	Evidence   []PersonObservationCitation `json:"evidence"`
	Premises   []PersonObservationPremise  `json:"premises"`
	Dependents []int64                     `json:"dependents"`
	Events     []PersonObservationEvent    `json:"events"`
}
type PersonObservationsResponse struct {
	Observations []PersonObservation `json:"observations"`
	Total        int                 `json:"total"`
}
type AddOwnerObservationRequest struct {
	Content    string                        `json:"content" minLength:"1" maxLength:"500"`
	SubjectRef string                        `json:"subject_ref,omitempty" maxLength:"64"`
	Kind       personobs.Kind                `json:"kind,omitempty" enum:"state,event,plan,preference,relationship,pattern,other"`
	Categories []personobs.Category          `json:"categories,omitempty"`
	Stability  personobs.Stability           `json:"stability,omitempty" enum:"fixed,durable,volatile,episodic"`
	ValidFrom  *time.Time                    `json:"valid_from,omitempty" nullable:"true"`
	ValidUntil *time.Time                    `json:"valid_until,omitempty" nullable:"true"`
	OccursOn   *personfacts.PartialDateValue `json:"occurs_on,omitzero" nullable:"false"`
}
type DecidePersonObservationRequest struct {
	Action            personobs.DecisionAction `json:"action" enum:"confirm,unsure,clear,reject,pin,unpin,end,forget"`
	ExpectedUpdatedAt time.Time                `json:"expected_updated_at"`
	EndAt             *time.Time               `json:"end_at,omitempty" nullable:"true"`
	Reason            string                   `json:"reason,omitempty" enum:"wrong,not_useful,outdated,owner-rejected"`
}
type PersonObservationSuppression struct {
	ID         int64     `json:"id"`
	PersonID   int64     `json:"person_id"`
	ContentKey string    `json:"content_key"`
	Reason     string    `json:"reason"`
	Actor      string    `json:"actor"`
	RejectedAt time.Time `json:"rejected_at"`
	CreatedAt  time.Time `json:"created_at"`
}
type ObservationSuppressionsResponse struct {
	Suppressions []PersonObservationSuppression `json:"suppressions"`
}

func (s *Server) registerPersonObservationRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersonObservations", http.MethodGet, "/people/{id}/observations", "List person observations")
	addPersonIDParameter(&list)
	for _, name := range []string{"level", "status", "kind", "as_of"} {
		list.Parameters = append(list.Parameters, queryStringParam(name, "Filter observations by "+name, false))
	}
	list.Parameters = append(list.Parameters, queryBooleanParam("include_sensitive", "Include sensitive statements"), queryIntegerParam("limit", "Maximum rows, default 100, maximum 500"), queryIntegerParam("offset", "Zero-based offset"))
	list.Responses = jsonResponsesFor[PersonObservationsResponse](api)
	addErrorResponses(api, list.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonObservations)
	show := rawAPIV1Operation("getPersonObservation", http.MethodGet, "/observations/{id}", "Get an observation with support and history")
	show.Parameters = append(show.Parameters, &huma.Param{Name: "id", In: "path", Required: true, Description: "Observation ID", Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64}}, queryBooleanParam("include_sensitive", "Include sensitive statements"))
	show.Responses = jsonResponsesFor[PersonObservationDetail](api)
	addErrorResponses(api, show.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, show, s.handleGetPersonObservation)
	add := rawAPIV1Operation("addOwnerObservation", http.MethodPost, "/people/{id}/observations", "Record an owner-authored person observation")
	addPersonIDParameter(&add)
	add.RequestBody = jsonRequestBodyFor[AddOwnerObservationRequest](api)
	add.Responses = jsonResponsesFor[PersonObservation](api, http.StatusCreated)
	addErrorResponses(api, add.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, add, s.handleAddOwnerObservation)
	decide := rawAPIV1Operation("decidePersonObservation", http.MethodPost, "/observations/{id}/decisions", "Curate a person observation")
	decide.Parameters = append(decide.Parameters, &huma.Param{Name: "id", In: "path", Required: true, Description: "Observation ID", Schema: &huma.Schema{Type: huma.TypeInteger, Format: formatInt64}})
	decide.RequestBody = jsonRequestBodyFor[DecidePersonObservationRequest](api)
	decide.Responses = jsonResponsesFor[PersonObservation](api)
	addErrorResponses(api, decide.Responses, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, decide, s.handleDecidePersonObservation)
	suppressions := rawAPIV1Operation("listObservationSuppressions", http.MethodGet, "/people/{id}/observation-suppressions", "List active hash-only observation suppressions")
	addPersonIDParameter(&suppressions)
	suppressions.Responses = jsonResponsesFor[ObservationSuppressionsResponse](api)
	addErrorResponses(api, suppressions.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, suppressions, s.handleListObservationSuppressions)
	clearOp := rawAPIV1Operation("clearObservationSuppression", http.MethodDelete, "/people/{id}/observation-suppressions/{key}", "Clear an observation suppression")
	addPersonIDParameter(&clearOp)
	clearOp.Parameters = append(clearOp.Parameters, &huma.Param{Name: "key", In: "path", Required: true, Schema: &huma.Schema{Type: huma.TypeString, Pattern: "^[0-9a-f]{64}$"}})
	clearOp.Responses = map[string]*huma.Response{"204": {Description: "Suppression cleared"}}
	addErrorResponses(api, clearOp.Responses, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, clearOp, s.handleClearObservationSuppression)
}
func (s *Server) personObservationRequest(w http.ResponseWriter, r *http.Request, person bool) (PersonObservationStore, int64, bool) {
	w.Header().Set("Cache-Control", "no-store")
	observations, ok := s.store.(PersonObservationStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "person_observations_unavailable", "Person observations are unavailable")
		return nil, 0, false
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return nil, 0, false
	}
	if person {
		if _, err := observations.GetPersonContext(r.Context(), id); err != nil {
			s.writeObservationError(w, err)
			return nil, 0, false
		}
	}
	return observations, id, true
}
func observationIncludeSensitive(w http.ResponseWriter, r *http.Request) (bool, bool) {
	value := r.URL.Query().Get("include_sensitive")
	if value == "" {
		return false, true
	}
	include, err := strconv.ParseBool(value)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "include_sensitive must be a boolean")
		return false, false
	}
	return include, true
}
func (s *Server) handleListPersonObservations(w http.ResponseWriter, r *http.Request) {
	observations, id, ok := s.personObservationRequest(w, r, true)
	if !ok {
		return
	}
	f := personobs.Filter{}
	f.IncludeSensitive, ok = observationIncludeSensitive(w, r)
	if !ok {
		return
	}
	query := r.URL.Query()
	for name, dst := range map[string]*int{"limit": &f.Limit, "offset": &f.Offset} {
		if raw := query.Get(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || (name == "limit" && (n == 0 || n > 500)) {
				writeError(w, http.StatusBadRequest, "bad_request", name+" is out of range")
				return
			}
			*dst = n
		}
	}
	if raw := query.Get("as_of"); raw != "" {
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "as_of must be an RFC3339 timestamp")
			return
		}
		f.AsOf = &at
	}
	if raw := query.Get("level"); raw != "" {
		for value := range strings.SplitSeq(raw, ",") {
			level := personobs.Level(value)
			if !slices.Contains([]personobs.Level{personobs.LevelExplicit, personobs.LevelDeductive, personobs.LevelInductive, personobs.LevelContradiction}, level) {
				writeError(w, http.StatusBadRequest, "bad_request", "Invalid level")
				return
			}
			f.Levels = append(f.Levels, level)
		}
	}
	if raw := query.Get("status"); raw != "" {
		for value := range strings.SplitSeq(raw, ",") {
			status := personobs.Status(value)
			if !slices.Contains([]personobs.Status{personobs.StatusActive, personobs.StatusUnsupported, personobs.StatusSuperseded, personobs.StatusRejected}, status) {
				writeError(w, http.StatusBadRequest, "bad_request", "Invalid status")
				return
			}
			f.Statuses = append(f.Statuses, status)
		}
	}
	if raw := query.Get("kind"); raw != "" {
		for value := range strings.SplitSeq(raw, ",") {
			kind := personobs.Kind(value)
			if !slices.Contains([]personobs.Kind{personobs.KindState, personobs.KindEvent, personobs.KindPlan, personobs.KindPreference, personobs.KindRelationship, personobs.KindPattern, personobs.KindOther}, kind) {
				writeError(w, http.StatusBadRequest, "bad_request", "Invalid kind")
				return
			}
			f.Kinds = append(f.Kinds, kind)
		}
	}
	page, err := observations.ListPersonObservations(r.Context(), id, f)
	if err != nil {
		s.writeObservationError(w, err)
		return
	}
	response := PersonObservationsResponse{Observations: make([]PersonObservation, len(page.Items)), Total: page.Total}
	for i, o := range page.Items {
		response.Observations[i] = personObservationDTO(o)
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) handleGetPersonObservation(w http.ResponseWriter, r *http.Request) {
	observations, id, ok := s.personObservationRequest(w, r, false)
	if !ok {
		return
	}
	include, ok := observationIncludeSensitive(w, r)
	if !ok {
		return
	}
	detail, err := observations.GetPersonObservation(r.Context(), id)
	if err != nil {
		s.writeObservationError(w, err)
		return
	}
	if detail.Sensitive && !include {
		writeError(w, http.StatusNotFound, "observation_not_found", "Observation not found")
		return
	}
	result := PersonObservationDetail{PersonObservation: personObservationDTO(detail.Observation), Evidence: []PersonObservationCitation{}, Premises: []PersonObservationPremise{}, Dependents: append([]int64{}, detail.Dependents...), Events: []PersonObservationEvent{}}
	for _, c := range detail.Evidence {
		leaf := c.Leaf
		result.Evidence = append(result.Evidence, PersonObservationCitation{c.EvidenceID, c.QuoteStart, c.QuoteEnd, c.QuoteSHA256, c.Exclusion, PersonObservationLeaf{leaf.MessageID, leaf.ConversationID, leaf.ConversationKind, leaf.AuthoredByPerson, leaf.AuthorIsOwner, leaf.EventTime}})
	}
	for _, p := range detail.Premises {
		result.Premises = append(result.Premises, PersonObservationPremise{p.ID, p.Role})
	}
	for _, e := range detail.Events {
		result.Events = append(result.Events, PersonObservationEvent{e.ID, e.ObservationID, e.Event, e.Actor, e.Detail, e.CreatedAt})
	}
	writeJSON(w, http.StatusOK, result)
}
func (s *Server) handleAddOwnerObservation(w http.ResponseWriter, r *http.Request) {
	observations, id, ok := s.personObservationRequest(w, r, true)
	if !ok {
		return
	}
	var body AddOwnerObservationRequest
	if _, ok := decodePersonRequestFields(w, r, &body); !ok {
		return
	}
	row, err := observations.AddOwnerObservation(r.Context(), personobs.OwnerInput{PersonID: id, Content: body.Content, SubjectRef: body.SubjectRef, Kind: body.Kind, Categories: body.Categories, Stability: body.Stability, ValidFrom: body.ValidFrom, ValidUntil: body.ValidUntil, OccursOn: body.OccursOn, Actor: "api"})
	if err != nil {
		s.writeObservationError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, personObservationDTO(row))
}
func (s *Server) handleDecidePersonObservation(w http.ResponseWriter, r *http.Request) {
	observations, id, ok := s.personObservationRequest(w, r, false)
	if !ok {
		return
	}
	var body DecidePersonObservationRequest
	if _, ok := decodePersonRequestFields(w, r, &body); !ok {
		return
	}
	if body.ExpectedUpdatedAt.IsZero() {
		writeError(w, http.StatusBadRequest, "bad_request", "expected_updated_at is required")
		return
	}
	row, err := observations.DecidePersonObservation(r.Context(), personobs.Decision{ObservationID: id, Action: body.Action, ExpectedUpdatedAt: body.ExpectedUpdatedAt, EndAt: body.EndAt, Reason: body.Reason, Actor: "api"})
	if err != nil {
		s.writeObservationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, personObservationDTO(row))
}
func (s *Server) handleListObservationSuppressions(w http.ResponseWriter, r *http.Request) {
	observations, id, ok := s.personObservationRequest(w, r, true)
	if !ok {
		return
	}
	rows, err := observations.ListObservationSuppressions(r.Context(), id)
	if err != nil {
		s.writeObservationError(w, err)
		return
	}
	response := ObservationSuppressionsResponse{Suppressions: make([]PersonObservationSuppression, len(rows))}
	for i, row := range rows {
		response.Suppressions[i] = PersonObservationSuppression{row.ID, row.PersonID, row.ContentKey, row.Reason, row.Actor, row.RejectedAt, row.CreatedAt}
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) handleClearObservationSuppression(w http.ResponseWriter, r *http.Request) {
	observations, id, ok := s.personObservationRequest(w, r, true)
	if !ok {
		return
	}
	if err := observations.ClearObservationSuppression(r.Context(), id, r.PathValue("key"), "api"); err != nil {
		s.writeObservationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) writeObservationError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	var validation *personobs.ValidationError
	switch {
	case errors.As(err, &validation):
		writeError(w, http.StatusBadRequest, "invalid_observation", validation.Error())
	case errors.Is(err, store.ErrObservationConflict):
		writeError(w, http.StatusConflict, "observation_conflict", err.Error())
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, store.ErrPersonNotFound):
		writeError(w, http.StatusNotFound, "observation_not_found", "Observation or person not found")
	default:
		s.logger.Error("observation operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "observation_failed", "Observation operation failed")
	}
}
