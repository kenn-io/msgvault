package api

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/store"
)

type MeetingDecisionStore interface {
	DecideMeetingLinkContext(context.Context, store.MeetingLinkRef, store.MeetingLinkRef, string) (store.MeetingLinkDecisionResult, error)
}

type MeetingDecisionRequest struct {
	Left     string `json:"left"`
	Right    string `json:"right"`
	Decision string `json:"decision" enum:"link,unlink,clear"`
}

func (s *Server) handleMeetingDecision(w http.ResponseWriter, r *http.Request) {
	if !s.apiRequestAuthorized(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Owner authorization is required")
		return
	}
	var request MeetingDecisionRequest
	if _, ok := decodeMeetingRequest(w, r, &request); !ok {
		return
	}
	left, err := parseMeetingLinkRef(request.Left)
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	right, err := parseMeetingLinkRef(request.Right)
	if err != nil {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	if request.Decision != "link" && request.Decision != "unlink" && request.Decision != "clear" || left == right {
		writeError(w, 400, "invalid_argument", "Choose two different references and link, unlink, or clear")
		return
	}
	backend, ok := s.store.(MeetingDecisionStore)
	if !ok {
		writeError(w, 503, "meetings_unavailable", "Meeting decisions are unavailable")
		return
	}
	done, err := s.beginMutation("meeting link decision")(r.Context())
	if err != nil {
		writeOperationGateBusy(w, r, s.operationGate)
		return
	}
	defer done()
	result, err := backend.DecideMeetingLinkContext(r.Context(), left, right, request.Decision)
	if errors.Is(err, store.ErrMeetingLinkSameCopy) {
		writeError(w, 400, "invalid_argument", err.Error())
		return
	}
	if errors.Is(err, store.ErrMeetingLinkConflict) {
		writeError(w, 409, "meeting_link_conflict", err.Error())
		return
	}
	if err != nil {
		s.writeMeetingLinkError(w, err)
		return
	}
	writeJSON(w, 200, result)
}
