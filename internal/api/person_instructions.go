package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

// PersonInstructionStore is the owner-only instruction ledger capability.
type PersonInstructionStore interface {
	AddPersonInstructionContext(ctx context.Context, input store.PersonInstructionInput) (*store.PersonInstruction, error)
	GetPersonInstructionContext(ctx context.Context, id int64) (*store.PersonInstruction, error)
	ListPersonInstructionsContext(ctx context.Context, personID int64, filter store.PersonInstructionFilter) ([]store.PersonInstruction, error)
	ListPersonInstructionReviewContext(ctx context.Context, limit int) ([]store.PersonInstruction, error)
	DecidePersonInstructionContext(ctx context.Context, decision store.PersonInstructionDecision) (*store.PersonInstructionDecisionResult, error)
}

var _ PersonInstructionStore = (*store.Store)(nil)

type PersonInstructionsResponse struct {
	Instructions []store.PersonInstruction `json:"instructions"`
}
type AddPersonInstructionRequest struct {
	Kind           personfacts.InstructionKind `json:"kind"`
	Text           string                      `json:"text"`
	Value          string                      `json:"value,omitempty"`
	ContactPointID *int64                      `json:"contact_point_id,omitzero"`
	ValidUntil     *time.Time                  `json:"valid_until,omitzero"`
}
type DecidePersonInstructionRequest struct {
	Action           personfacts.InstructionAction `json:"action"`
	ExpectedRevision int64                         `json:"expected_revision"`
	Reason           string                        `json:"reason,omitempty"`
	Edit             *store.PersonInstructionEdit  `json:"edit,omitzero"`
}

func (s *Server) registerPersonInstructionRoutes(api huma.API) {
	list := rawAPIV1Operation("listPersonInstructions", http.MethodGet, "/people/{id}/instructions", "List a person's standing instructions")
	addPersonIDParameter(&list)
	list.Parameters = append(list.Parameters, queryStringParam("status", "Comma-separated lifecycle statuses", false), queryBooleanParam("include_expired", "Include expired instructions"))
	list.Responses = jsonResponsesFor[PersonInstructionsResponse](api)
	addErrorResponses(api, list.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, list, s.handleListPersonInstructions)
	add := rawAPIV1Operation("addPersonInstruction", http.MethodPost, "/people/{id}/instructions", "Add an owner-entered standing instruction")
	addPersonIDParameter(&add)
	add.RequestBody = jsonRequestBodyFor[AddPersonInstructionRequest](api)
	add.Responses = jsonResponsesFor[store.PersonInstruction](api, http.StatusCreated)
	addErrorResponses(api, add.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, add, s.handleAddPersonInstruction)
	get := rawAPIV1Operation("getPersonInstruction", http.MethodGet, "/person-instructions/{id}", "Read a standing instruction")
	addPersonIDParameter(&get)
	get.Responses = jsonResponsesFor[store.PersonInstruction](api)
	addErrorResponses(api, get.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, get, s.handleGetPersonInstruction)
	review := rawAPIV1Operation("listPersonInstructionReview", http.MethodGet, "/person-instructions/review", "List standing instructions needing owner review")
	review.Parameters = append(review.Parameters, queryIntegerParam("limit", "Maximum rows (default 50, max 200)"))
	review.Responses = jsonResponsesFor[PersonInstructionsResponse](api)
	addErrorResponses(api, review.Responses, http.StatusBadRequest, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, review, s.handleListPersonInstructionReview)
	decide := rawAPIV1Operation("decidePersonInstruction", http.MethodPost, "/person-instructions/{id}/decisions", "Record an owner instruction decision")
	addPersonIDParameter(&decide)
	decide.RequestBody = jsonRequestBodyFor[DecidePersonInstructionRequest](api)
	decide.Responses = jsonResponsesFor[store.PersonInstructionDecisionResult](api)
	addErrorResponses(api, decide.Responses, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, decide, s.handleDecidePersonInstruction)
}
func (s *Server) personInstructionStore(w http.ResponseWriter) (PersonInstructionStore, bool) {
	w.Header().Set("Cache-Control", "no-store")
	ledger, ok := s.store.(PersonInstructionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "person_instructions_unavailable", "Standing instructions are unavailable")
	}
	return ledger, ok
}
func (s *Server) writePersonInstructionError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound), errors.Is(err, store.ErrInstructionNotFound):
		writeError(w, http.StatusNotFound, "instruction_not_found", "Person or instruction not found")
	case errors.Is(err, store.ErrInstructionInvalid):
		writeError(w, http.StatusBadRequest, "instruction_invalid", err.Error())
	case errors.Is(err, store.ErrInstructionDuplicate), errors.Is(err, store.ErrInstructionRevisionConflict), errors.Is(err, store.ErrInstructionTransition):
		writeError(w, http.StatusConflict, "instruction_conflict", err.Error())
	default:
		s.logger.Error("standing instruction operation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "instruction_failed", "Standing instruction operation failed")
	}
}
func (s *Server) handleListPersonInstructions(w http.ResponseWriter, r *http.Request) {
	ledger, ok := s.personInstructionStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	include, _, err := queryBool(r, "include_expired")
	if err != nil {
		writeError(w, http.StatusBadRequest, "instruction_invalid", err.Error())
		return
	}
	filter := store.PersonInstructionFilter{IncludeExpired: include}
	if status := r.URL.Query().Get("status"); status != "" {
		for v := range strings.SplitSeq(status, ",") {
			filter.Statuses = append(filter.Statuses, personfacts.InstructionStatus(strings.TrimSpace(v)))
		}
	}
	rows, err := ledger.ListPersonInstructionsContext(r.Context(), id, filter)
	if err != nil {
		s.writePersonInstructionError(w, err)
		return
	}
	if rows == nil {
		rows = []store.PersonInstruction{}
	}
	writeJSON(w, http.StatusOK, PersonInstructionsResponse{Instructions: rows})
}
func (s *Server) handleAddPersonInstruction(w http.ResponseWriter, r *http.Request) {
	ledger, ok := s.personInstructionStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	var request AddPersonInstructionRequest
	if _, ok := decodePersonRequestFields(w, r, &request); !ok {
		return
	}
	row, err := ledger.AddPersonInstructionContext(r.Context(), store.PersonInstructionInput{PersonID: id, Kind: request.Kind, Text: request.Text, ValueText: request.Value, ContactPointID: request.ContactPointID, ValidUntil: request.ValidUntil, Actor: "owner"})
	if err != nil {
		s.writePersonInstructionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, row)
}
func (s *Server) handleGetPersonInstruction(w http.ResponseWriter, r *http.Request) {
	ledger, ok := s.personInstructionStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	row, err := ledger.GetPersonInstructionContext(r.Context(), id)
	if err != nil {
		s.writePersonInstructionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, row)
}
func (s *Server) handleListPersonInstructionReview(w http.ResponseWriter, r *http.Request) {
	ledger, ok := s.personInstructionStore(w)
	if !ok {
		return
	}
	limit, present, err := queryInt(r, "limit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "instruction_invalid", err.Error())
		return
	}
	if !present {
		limit = 50
	}
	rows, err := ledger.ListPersonInstructionReviewContext(r.Context(), limit)
	if err != nil {
		s.writePersonInstructionError(w, err)
		return
	}
	if rows == nil {
		rows = []store.PersonInstruction{}
	}
	writeJSON(w, http.StatusOK, PersonInstructionsResponse{Instructions: rows})
}
func (s *Server) handleDecidePersonInstruction(w http.ResponseWriter, r *http.Request) {
	ledger, ok := s.personInstructionStore(w)
	if !ok {
		return
	}
	id, ok := personProfileID(w, r)
	if !ok {
		return
	}
	var request DecidePersonInstructionRequest
	if _, ok := decodePersonRequestFields(w, r, &request); !ok {
		return
	}
	result, err := ledger.DecidePersonInstructionContext(r.Context(), store.PersonInstructionDecision{InstructionID: id, ExpectedRevision: request.ExpectedRevision, Action: request.Action, Reason: request.Reason, Edit: request.Edit, Actor: "owner"})
	if err != nil {
		s.writePersonInstructionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
