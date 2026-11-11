package api

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
)

// CommitmentQuote cites the exact runes within an archived original body.
type CommitmentQuote struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
}

type LoopCommitment struct {
	ID                 string           `json:"id"`
	PersonID           int64            `json:"person_id"`
	ClaimID            int64            `json:"claim_id"`
	Direction          string           `json:"direction" enum:"owed_by,owed_to"`
	Summary            string           `json:"summary"`
	Promise            CommitmentQuote  `json:"promise"`
	PromisedAt         time.Time        `json:"promised_at"`
	ExpiresAt          time.Time        `json:"expires_at"`
	State              string           `json:"state" enum:"open,done,snoozed,fulfilled"`
	SnoozedUntil       *time.Time       `json:"snoozed_until,omitempty"`
	DueOn              *string          `json:"due_on,omitempty" format:"date"`
	ProposedFulfilment *CommitmentQuote `json:"proposed_fulfilment,omitempty"`
}

type CommitmentDecisionRequest struct {
	State string     `json:"state" enum:"open,done,snoozed"`
	Until *time.Time `json:"until,omitempty"`
}

type CommitmentDecisionResponse struct {
	ID           string     `json:"id"`
	State        string     `json:"state" enum:"open,done,snoozed"`
	SnoozedUntil *time.Time `json:"snoozed_until,omitempty"`
}

func loopCommitments(commitments []store.PersonCommitment) []LoopCommitment {
	var response []LoopCommitment
	for _, c := range commitments {
		if c.ClaimID == nil {
			continue
		}
		item := LoopCommitment{
			ID: loops.FormatCommitmentID(c.ID), PersonID: c.PersonID, ClaimID: *c.ClaimID,
			Direction: c.Direction, Summary: c.Summary, PromisedAt: c.PromisedAt, DueOn: c.DueOn,
			ExpiresAt: c.ExpiresAt, State: c.State, SnoozedUntil: c.SnoozedUntil,
			Promise: CommitmentQuote{MessageID: c.AnchorMessageID, Text: c.Quote, Start: c.Start, End: c.End},
		}
		if f := c.Fulfilment; f != nil {
			item.ProposedFulfilment = &CommitmentQuote{MessageID: f.MessageID, Text: f.Quote, Start: f.Start, End: f.End}
		}
		response = append(response, item)
	}
	return response
}

func (s *Server) registerCommitmentDecision(api huma.API) {
	op := rawAPIV1Operation("setCommitmentDecision", http.MethodPost, "/loops/commitments/{commitment_id}/decision", "Set an owner decision on a cited promise")
	op.Errors = []int{400, 401, 403, 404, 413, 415, 500, 503}
	op.Parameters = []*huma.Param{pathStringParam("commitment_id", "The commitment ID, for example c12")}
	op.RequestBody = jsonRequestBodyFor[CommitmentDecisionRequest](api)
	op.Responses = jsonResponsesFor[CommitmentDecisionResponse](api)
	addActivityErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, s.handleCommitmentDecision)
}

func (s *Server) handleCommitmentDecision(w http.ResponseWriter, r *http.Request) {
	decider, ok := s.store.(loops.CommitmentDecider)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "loops_unavailable", "Commitment decisions are unavailable")
		return
	}
	id, err := loops.ParseCommitmentID(r.PathValue("commitment_id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var request CommitmentDecisionRequest
	if !decodeEntityRequest(w, r, &request, "commitment decision") {
		return
	}
	decision, err := decider.DecidePersonCommitmentContext(r.Context(), id, request.State, request.Until, s.clockNow().UTC())
	switch {
	case errors.Is(err, store.ErrInvalidActivityRequest):
		writeError(w, http.StatusBadRequest, "invalid_request", "Use open, done, or snoozed with a future until")
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "commitment_not_found", "Commitment not found")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update the commitment")
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, CommitmentDecisionResponse{
			ID: loops.FormatCommitmentID(decision.ID), State: decision.State, SnoozedUntil: decision.SnoozedUntil,
		})
	}
}
