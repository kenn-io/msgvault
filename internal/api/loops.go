package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
)

// LoopCounterparty is who a loop waits on or is owed to.
type LoopCounterparty struct {
	ParticipantID int64  `json:"participant_id"`
	PersonID      *int64 `json:"person_id,omitempty"`
	DisplayName   string `json:"display_name"`
	Address       string `json:"address"`
}

// LoopItem is one conversation turn that still waits on a reply.
type LoopItem struct {
	Kind            string           `json:"kind" enum:"reply_owed,awaiting_reply" doc:"reply_owed waits on you; awaiting_reply waits on them"`
	AnchorMessageID int64            `json:"anchor_message_id" doc:"The message that started the wait. Done and snooze use it as the loop ID."`
	ConversationID  int64            `json:"conversation_id"`
	SourceID        int64            `json:"source_id"`
	Channel         string           `json:"channel" enum:"email,chat"`
	Counterparty    LoopCounterparty `json:"counterparty"`
	Summary         string           `json:"summary" doc:"The email subject, or the chat message text"`
	OwedSinceAt     time.Time        `json:"owed_since_at"`
	LastActivityAt  time.Time        `json:"last_activity_at"`
}

// LoopListResponse lists open loops oldest first.
type LoopListResponse struct {
	Items              []LoopItem `json:"items"`
	Truncated          bool       `json:"truncated" doc:"True when more loops are open than limit returned"`
	CoverageIncomplete bool       `json:"coverage_incomplete" doc:"True when more history remains after checking 1,000 uncached messages; another request reuses saved evidence to continue"`
	// SkippedUnparseable counts stored originals the list could not parse. A reply in one of them may have settled a loop that still lists.
	SkippedUnparseable int `json:"skipped_unparseable" doc:"Stored originals that failed to parse and were passed over; a reply in one may have settled a listed loop"`
}

// LoopDecisionResponse is the mark stored by done or snooze.
type LoopDecisionResponse struct {
	AnchorMessageID int64      `json:"anchor_message_id"`
	State           string     `json:"state" enum:"done,snoozed"`
	SnoozedUntil    *time.Time `json:"snoozed_until,omitempty"`
}

// SnoozeLoopRequest hides a loop until a future instant.
type SnoozeLoopRequest struct {
	Until time.Time `json:"until"`
}

func (s *Server) registerLoopRoutes(api huma.API) {
	list := rawAPIV1Operation("listLoops", http.MethodGet, "/loops", "List conversations waiting on a reply")
	list.Errors = []int{400, 401, 403, 500, 503}
	list.Responses = jsonResponsesFor[LoopListResponse](api)
	addActivityErrorResponses(api, list.Responses, list.Errors...)
	list.Parameters = loopListParameters()
	registerRawHumaRoute(api, list, s.handleListLoops)

	for _, action := range []struct {
		id, path, summary string
		handler           http.HandlerFunc
		body              bool
	}{
		{"markLoopDone", "/loops/{message_id}/done", "Mark a loop done", s.handleLoopDone, false},
		{"snoozeLoop", "/loops/{message_id}/snooze", "Hide a loop until a later time", s.handleLoopSnooze, true},
	} {
		op := rawAPIV1Operation(action.id, http.MethodPost, action.path, action.summary)
		op.Errors = []int{400, 401, 403, 404, 413, 415, 500, 503}
		if action.body {
			op.RequestBody = jsonRequestBodyFor[SnoozeLoopRequest](api)
		}
		op.Responses = jsonResponsesFor[LoopDecisionResponse](api)
		addActivityErrorResponses(api, op.Responses, op.Errors...)
		id := pathNamedIntegerParam("message_id", "ID of the message that anchors the loop")
		minimum := float64(1)
		id.Schema.Minimum = &minimum
		op.Parameters = []*huma.Param{id}
		registerRawHumaRoute(api, op, action.handler)
	}
}

func loopListParameters() []*huma.Param {
	bounded := func(name, doc string, def, maximum int) *huma.Param {
		p := queryIntegerParam(name, doc)
		lo, hi := float64(1), float64(maximum)
		p.Schema.Minimum, p.Schema.Maximum, p.Schema.Default = &lo, &hi, def
		return p
	}
	return []*huma.Param{
		queryIntegerParam("person_id", "Only loops with this person"),
		bounded("limit", "Maximum loops to return", loops.DefaultLimit, loops.MaxLimit),
	}
}

func (s *Server) loopStore(w http.ResponseWriter) (loops.Store, bool) {
	loopStore, ok := s.store.(loops.Store)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "loops_unavailable", "Open loops are unavailable")
	}
	return loopStore, ok
}

func (s *Server) handleListLoops(w http.ResponseWriter, r *http.Request) {
	loopStore, ok := s.loopStore(w)
	if !ok {
		return
	}
	opts := loops.Options{}
	// r.URL.Query drops a parameter it cannot decode, which would silently
	// widen the result instead of refusing the filter.
	_, err := url.ParseQuery(r.URL.RawQuery)
	if err == nil {
		opts.Limit, err = boundedActivityQuery(r, "limit", loops.DefaultLimit, 1, loops.MaxLimit)
	}
	if err == nil {
		if values, supplied := r.URL.Query()["person_id"]; supplied {
			if opts.PersonID, err = strconv.ParseInt(values[0], 10, 64); err != nil || len(values) != 1 || opts.PersonID < 1 {
				err = store.ErrInvalidActivityRequest
			}
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "person_id and limit must be valid")
		return
	}
	result, err := loops.List(r.Context(), loopStore, opts, s.clockNow().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list open loops")
		return
	}
	response := loopListResponse(result)
	for _, l := range result.Loops {
		item := LoopItem{
			Kind: l.Kind, AnchorMessageID: l.Anchor.ID, ConversationID: l.ConversationID, SourceID: l.SourceID,
			Channel: string(l.Channel), Summary: l.Summary,
			OwedSinceAt: l.OwedSince.UTC(), LastActivityAt: l.LastActivity.UTC(),
			Counterparty: LoopCounterparty{
				ParticipantID: l.Counterparty.ParticipantID, DisplayName: l.Counterparty.DisplayName, Address: l.Counterparty.Address,
			},
		}
		if l.Counterparty.PersonID != 0 {
			item.Counterparty.PersonID = &l.Counterparty.PersonID
		}
		response.Items = append(response.Items, item)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}

func loopListResponse(result loops.Result) LoopListResponse {
	return LoopListResponse{
		Items: make([]LoopItem, 0, len(result.Loops)), Truncated: result.Truncated,
		CoverageIncomplete: result.CoverageIncomplete, SkippedUnparseable: result.SkippedUnparseable,
	}
}

func (s *Server) handleLoopDone(w http.ResponseWriter, r *http.Request) {
	s.applyLoopDecision(w, r, func(loopStore loops.Store, id int64) (store.LoopDecision, error) {
		return loops.Done(r.Context(), loopStore, id, s.clockNow().UTC())
	}, nil)
}

func (s *Server) handleLoopSnooze(w http.ResponseWriter, r *http.Request) {
	var request SnoozeLoopRequest
	s.applyLoopDecision(w, r, func(loopStore loops.Store, id int64) (store.LoopDecision, error) {
		now := s.clockNow().UTC()
		if !request.Until.After(now) {
			return store.LoopDecision{}, errSnoozeNotFuture
		}
		return loops.Snooze(r.Context(), loopStore, id, request.Until, now)
	}, &request)
}

var errSnoozeNotFuture = errors.New("until must be in the future")

func (s *Server) applyLoopDecision(
	w http.ResponseWriter, r *http.Request,
	apply func(loops.Store, int64) (store.LoopDecision, error), body any,
) {
	loopStore, ok := s.loopStore(w)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("message_id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_request", "message_id must be a positive integer")
		return
	}
	if body != nil && !decodeEntityRequest(w, r, body, "loop") {
		return
	}
	decision, err := apply(loopStore, id)
	switch {
	case errors.Is(err, errSnoozeNotFuture):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, loops.ErrNotOpen):
		writeError(w, http.StatusNotFound, "loop_not_found", "That message does not anchor an open loop")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update the loop")
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, LoopDecisionResponse{
			AnchorMessageID: decision.AnchorMessageID, State: decision.State, SnoozedUntil: decision.SnoozedUntil,
		})
	}
}
