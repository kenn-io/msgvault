package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

type personAgendaScheduleOperations interface {
	UpdateSchedule(ctx context.Context, personID int64, parentID, jobID string, input personagenda.ScheduleUpdateInput) (personagenda.Schedule, error)
}

func (s *Server) handlePersonAgendaScheduleAction(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	operations, ok := s.personAgendaOperations.(personAgendaScheduleOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "schedule_unavailable", "Schedule management is unavailable")
		return
	}
	parentID, jobID := strings.TrimSpace(r.PathValue("ref")), strings.TrimSpace(r.PathValue("job_uid"))
	if parentID == "" || jobID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "Parent and schedule are required")
		return
	}
	var request personagenda.ScheduleUpdateInput
	if !decodePersonAgendaRequest(w, r, &request, "Invalid schedule request") {
		return
	}
	result, err := operations.UpdateSchedule(r.Context(), personID, parentID, jobID, request)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, PersonAgendaScheduleResponse{Schedule: PersonAgendaSchedule(result)})
}

func writeScheduleError(w http.ResponseWriter, err error) {
	switch {
	case taskclient.ErrorStatus(err) == http.StatusForbidden:
		writeError(w, http.StatusForbidden, "schedule_forbidden", "Schedule access is forbidden")
	case errors.Is(err, personagenda.ErrUnsupportedSchedule):
		writeError(w, http.StatusUnprocessableEntity, "unsupported_schedule", "This schedule must be managed in Kata")
	case errors.Is(err, personagenda.ErrInvalidSchedule):
		writeError(w, http.StatusUnprocessableEntity, "invalid_schedule", "This schedule cannot be updated with these settings")
	case errors.Is(err, personagenda.ErrScheduleOutcomeUnknown):
		writeError(w, http.StatusServiceUnavailable, "schedule_outcome_unknown", "The schedule update outcome is unknown. Refresh before retrying.")
	case errors.Is(err, personagenda.ErrRecurringRetryExpired):
		writeError(w, http.StatusConflict, "recurring_retry_expired", "This request is too old to retry safely. Check Kata before starting a new request.")
	case errors.Is(err, taskclient.ErrConflict):
		writeError(w, http.StatusConflict, "schedule_conflict", "The schedule changed. Refresh before trying again.")
	default:
		writePersonAgendaError(w, err)
	}
}
