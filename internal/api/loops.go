package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/jobctx"
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
	Kind               string           `json:"kind" enum:"reply_owed,awaiting_reply" doc:"reply_owed waits on you; awaiting_reply waits on them"`
	AnchorMessageID    int64            `json:"anchor_message_id" doc:"The message that started the wait. Done and snooze use it as the loop ID."`
	QuestionMessageIDs []int64          `json:"question_message_ids" doc:"The questions this loop covers, oldest first. Pass them to done or snooze so a question that appeared later stays open."`
	ConversationID     int64            `json:"conversation_id"`
	SourceID           int64            `json:"source_id"`
	Channel            string           `json:"channel" enum:"email,chat"`
	Counterparty       LoopCounterparty `json:"counterparty"`
	Summary            string           `json:"summary" doc:"The email subject, or the chat message text"`
	OwedSinceAt        time.Time        `json:"owed_since_at"`
	LastActivityAt     time.Time        `json:"last_activity_at"`
}

// LoopListResponse lists open loops oldest first.
type LoopListResponse struct {
	Items              []LoopItem `json:"items"`
	Truncated          bool       `json:"truncated" doc:"True when more loops are open than limit returned"`
	CoverageIncomplete bool       `json:"coverage_incomplete" doc:"True when more history remains after checking 1,000 uncached messages; another request reuses saved evidence to continue"`
	// SkippedUnparseable counts stored originals the list could not parse. A reply in one of them may have settled a loop that still lists.
	SkippedUnparseable int `json:"skipped_unparseable" doc:"Stored originals that failed to parse and were passed over; a reply in one may have settled a listed loop"`
}

// LoopDecisionResponse is the result of a done, snooze, or reopen action.
type LoopDecisionResponse struct {
	AnchorMessageID int64      `json:"anchor_message_id"`
	State           string     `json:"state" enum:"open,done,snoozed"`
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
		{"reopenLoop", "/loops/{message_id}/reopen", "Clear a loop decision", s.handleLoopReopen, false},
	} {
		op := rawAPIV1Operation(action.id, http.MethodPost, action.path, action.summary)
		op.Errors = []int{400, 401, 403, 404, 413, 415, 500, 503}
		if action.body {
			op.RequestBody = jsonRequestBodyFor[SnoozeLoopRequest](api)
		}
		op.Responses = jsonResponsesFor[LoopDecisionResponse](api)
		addActivityErrorResponses(api, op.Responses, op.Errors...)
		op.Responses["503"].Description = "Open loops unavailable, or loop_scan_incomplete: retry to continue checking retained evidence"
		op.Responses["503"].Headers = map[string]*huma.Param{"Retry-After": {Description: "Seconds before retrying an incomplete evidence scan", Schema: &huma.Schema{Type: huma.TypeString}}}
		id := pathNamedIntegerParam("message_id", "ID of the message that anchors the loop")
		minimum := float64(1)
		id.Schema.Minimum = &minimum
		op.Parameters = []*huma.Param{id}
		if action.id != "reopenLoop" {
			op.Parameters = append(op.Parameters, queryIntegerArrayParam("question_message_ids",
				"The list item's question_message_ids; only these are marked. Omit to cover every question visible now."))
		}
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
	evidence := &gatedLoopEvidence{Store: loopStore, gate: s.operationGate}
	result, err := loops.List(r.Context(), evidence, opts, s.clockNow().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to list open loops")
		return
	}
	response := loopListResponse(result)
	for _, l := range result.Loops {
		item := LoopItem{
			Kind: l.Kind, AnchorMessageID: l.Anchor.ID, QuestionMessageIDs: loopQuestionIDs(l),
			ConversationID: l.ConversationID, SourceID: l.SourceID,
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
	if result.CoverageIncomplete {
		s.startLoopEvidence(loopStore, opts.PersonID)
	}
}

// startLoopEvidence continues an incomplete request under the server lifetime.
func (s *Server) startLoopEvidence(st loops.Store, personID int64) {
	s.importMu.Lock()
	if s.importsClosed {
		s.importMu.Unlock()
		return
	}
	if s.loopEvidenceRunning {
		if !slices.Contains(s.loopEvidencePending, personID) {
			s.loopEvidencePending = append(s.loopEvidencePending, personID)
		}
		s.importMu.Unlock()
		return
	}
	finishIdle, ok := s.idleTracker.BeginMaintenanceWorkContext(s.importContext)
	if !ok {
		s.importMu.Unlock()
		return
	}
	s.loopEvidenceRunning = true
	s.loopEvidencePending = append(s.loopEvidencePending, personID)
	s.importWG.Add(1)
	s.importMu.Unlock()
	go func() {
		defer s.importWG.Done()
		defer finishIdle()
		s.fillLoopEvidence(st)
	}()
}

func (s *Server) fillLoopEvidence(st loops.Store) {
	for {
		s.importMu.Lock()
		personID := s.loopEvidencePending[0]
		s.importMu.Unlock()
		ctx, cancel := context.WithTimeout(jobctx.WithProgress(s.importContext), 2*time.Minute)
		_, err := loops.Warm(ctx, &gatedLoopEvidence{Store: st, gate: s.operationGate, background: true}, personID, s.clockNow().UTC())
		expired := errors.Is(ctx.Err(), context.DeadlineExceeded)
		progress := jobctx.HasProgress(ctx)
		if expired && !progress {
			s.logger.Warn("fill loop evidence stopped without progress")
		} else if err != nil && ctx.Err() == nil && !errors.Is(err, errMutationGateBusy) {
			s.logger.Error("fill loop evidence", "error", err)
		}
		cancel()
		s.importMu.Lock()
		if !s.importsClosed && expired && progress {
			s.importMu.Unlock()
			continue
		}
		s.loopEvidencePending = s.loopEvidencePending[1:]
		if s.importsClosed || errors.Is(err, errMutationGateBusy) || len(s.loopEvidencePending) == 0 {
			s.loopEvidenceRunning = false
			s.loopEvidencePending = nil
			s.importMu.Unlock()
			return
		}
		s.importMu.Unlock()
	}
}

func loopListResponse(result loops.Result) LoopListResponse {
	return LoopListResponse{
		Items: make([]LoopItem, 0, len(result.Loops)), Truncated: result.Truncated,
		CoverageIncomplete: result.CoverageIncomplete, SkippedUnparseable: result.SkippedUnparseable,
	}
}

func (s *Server) handleLoopDone(w http.ResponseWriter, r *http.Request) {
	s.applyLoopDecision(w, r, true, func(loopStore loops.Store, id int64, questions []int64) (store.LoopDecision, error) {
		return loops.Done(r.Context(), loopStore, id, questions, s.clockNow().UTC())
	}, nil)
}

func (s *Server) handleLoopSnooze(w http.ResponseWriter, r *http.Request) {
	var request SnoozeLoopRequest
	s.applyLoopDecision(w, r, true, func(loopStore loops.Store, id int64, questions []int64) (store.LoopDecision, error) {
		now := s.clockNow().UTC()
		if !request.Until.After(now) {
			return store.LoopDecision{}, errSnoozeNotFuture
		}
		return loops.Snooze(r.Context(), loopStore, id, questions, request.Until, now)
	}, &request)
}

func (s *Server) handleLoopReopen(w http.ResponseWriter, r *http.Request) {
	s.applyLoopDecision(w, r, false, func(loopStore loops.Store, id int64, _ []int64) (store.LoopDecision, error) {
		return loops.Reopen(r.Context(), loopStore, id)
	}, nil)
}

var errSnoozeNotFuture = errors.New("until must be in the future")

func (s *Server) applyLoopDecision(
	w http.ResponseWriter, r *http.Request, takesQuestions bool,
	apply func(loops.Store, int64, []int64) (store.LoopDecision, error), body any,
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
	if _, err := url.ParseQuery(r.URL.RawQuery); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "query string could not be parsed")
		return
	}
	var questions []int64
	if takesQuestions {
		questions, _, err = queryInt64s(r, "question_message_ids")
		if err == nil && slices.ContainsFunc(questions, func(q int64) bool { return q < 1 }) {
			err = store.ErrInvalidActivityRequest
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "question_message_ids must be positive integers")
			return
		}
		// The anchor is the oldest listed question; leaving it out would
		// report a decision on a question that stays open.
		if len(questions) > 0 && !slices.Contains(questions, id) {
			writeError(w, http.StatusBadRequest, "invalid_request", "question_message_ids must include message_id")
			return
		}
	}
	if body != nil && !decodeEntityRequest(w, r, body, "loop") {
		return
	}
	decision, err := apply(loopStore, id, questions)
	switch {
	case errors.Is(err, errSnoozeNotFuture):
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, loops.ErrEvidenceIncomplete):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "loop_scan_incomplete", "More history remains. Retry this action to continue checking.")
	case errors.Is(err, loops.ErrNotOpen):
		writeError(w, http.StatusNotFound, "loop_not_found", "That message does not anchor an open loop")
	case errors.Is(err, loops.ErrDecisionNotFound):
		writeError(w, http.StatusNotFound, "loop_decision_not_found", "That message has no done or snooze decision")
	case err != nil:
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update the loop")
	default:
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, LoopDecisionResponse{
			AnchorMessageID: decision.AnchorMessageID, State: decision.State, SnoozedUntil: decision.SnoozedUntil,
		})
	}
}

func loopQuestionIDs(l loops.Loop) []int64 {
	ids := make([]int64, len(l.Questions))
	for i, q := range l.Questions {
		ids[i] = q.ID
	}
	return ids
}

// loopEvidenceGateWait bounds how long one evidence write waits for the
// operation gate before the request stops saving evidence.
const loopEvidenceGateWait = 50 * time.Millisecond

// gatedLoopEvidence lets a list read the archive without holding the operation
// gate for the whole scan. Evidence is a rebuildable cache, so each write takes
// the gate briefly. Once the gate is busy, as during a backup freeze or a sync,
// the request keeps reading saved evidence but saves no more.
type gatedLoopEvidence struct {
	loops.Store

	gate       OperationGate
	readOnly   bool
	background bool
}

// begin takes the gate for one write, or marks the request read-only.
func (g *gatedLoopEvidence) begin(ctx context.Context) (func(), bool) {
	if g.readOnly {
		return nil, false
	}
	if g.gate == nil {
		return func() {}, true
	}
	if g.background {
		return g.gate.BeginWorkContext(ctx)
	}
	waitCtx, cancel := context.WithTimeout(ctx, loopEvidenceGateWait)
	defer cancel()
	release, ok := g.gate.BeginWorkContext(waitCtx)
	if !ok {
		g.readOnly = true
	}
	return release, ok
}

func (g *gatedLoopEvidence) LoopMessageEvidenceContext(ctx context.Context, id int64, version int) (store.LoopMessageEvidence, string, bool, error) {
	release, ok := g.begin(ctx)
	if !ok {
		if g.background {
			return store.LoopMessageEvidence{}, "", false, errMutationGateBusy
		}
		e, hit, err := g.CachedLoopMessageEvidenceContext(ctx, id, version)
		return e, "", hit, err
	}
	defer release()
	return g.Store.LoopMessageEvidenceContext(ctx, id, version)
}

func (g *gatedLoopEvidence) SaveLoopMessageEvidenceContext(ctx context.Context, id int64, version int, token string, e store.LoopMessageEvidence) error {
	if token == "" {
		return nil
	}
	release, ok := g.begin(ctx)
	if !ok {
		if g.background {
			return errMutationGateBusy
		}
		return nil
	}
	defer release()
	return g.Store.SaveLoopMessageEvidenceContext(ctx, id, version, token, e)
}
