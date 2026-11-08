package api

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/personagenda"
	"go.kenn.io/msgvault/internal/taskclient"
)

type personAgendaRecurringOperations interface {
	CreateRecurring(ctx context.Context, personID int64, key string, input personagenda.RecurringCreateInput) (personagenda.RecurringCreateResult, error)
}

func (s *Server) handleCreatePersonAgendaRecurring(w http.ResponseWriter, r *http.Request) {
	personID, ok := personProfileID(w, r)
	if !ok || !s.requirePersonAgenda(w) {
		return
	}
	operations, ok := s.personAgendaOperations.(personAgendaRecurringOperations)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "schedule_unavailable", "Recurring creation is unavailable")
		return
	}
	key, ok := personOperationIdempotencyKey(w, r)
	if !ok {
		return
	}
	var request personagenda.RecurringCreateInput
	if !decodePersonAgendaRequest(w, r, &request, "Invalid recurring task request") {
		return
	}
	result, err := operations.CreateRecurring(r.Context(), personID, key, request)
	if err != nil && result.Parent.UID == "" {
		if errors.Is(err, personagenda.ErrRecurringProjectChanged) {
			writeError(w, http.StatusConflict, "recurring_project_changed", "The Kata project changed. Check the original project before starting another request")
			return
		}
		if errors.Is(err, personagenda.ErrInvalidRecurringKey) {
			writeError(w, http.StatusUnprocessableEntity, "invalid_idempotency_key", "Use an RFC3339 timestamp followed by / and a UUID; the timestamp must not be more than five minutes ahead of the server")
			return
		}
		if errors.Is(err, taskclient.ErrIncompatible) {
			writeError(w, http.StatusUnprocessableEntity, "cron_unsupported", "Kata does not support recurring schedules")
			return
		}
		writeScheduleError(w, err)
		return
	}
	response := PersonAgendaRecurringCreateResponse{Parent: personAgendaItem(result.Parent), Status: "created"}
	status := http.StatusCreated
	if result.Schedule != nil {
		schedule := PersonAgendaSchedule(*result.Schedule)
		response.Schedule = &schedule
	}
	if err != nil {
		status = http.StatusAccepted
		response.Status = "schedule_not_created"
		if errors.Is(err, personagenda.ErrScheduleOutcomeUnknown) {
			response.Status = "outcome_unknown"
		} else if errors.Is(err, taskclient.ErrConflict) {
			response.Status = "conflict"
		} else if taskclient.ErrorStatus(err) == http.StatusForbidden {
			response.Status = "forbidden"
		} else if errors.Is(err, taskclient.ErrAuthenticationRequired) {
			response.Status = "authentication_required"
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, response)
}
