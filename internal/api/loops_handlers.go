package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"go.kenn.io/msgvault/internal/loops"
	"go.kenn.io/msgvault/internal/store"
)

func (s *Server) loopHandler(operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loopCapabilitiesEnabled() {
			s.writeLoopError(w, loops.ErrDisabled)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		var result any
		var err error
		status := http.StatusOK
		switch operation {
		case "listOpenLoops", "listPersonLoops":
			var options loops.ListOptions
			options, err = parseLoopList(r, operation == "listPersonLoops")
			if err == nil {
				result, err = s.loopService.List(r.Context(), options)
			}
		case "getWaitingLoops":
			var options loops.WaitingOptions
			options, err = parseLoopWaiting(r)
			if err == nil {
				result, err = s.loopService.Waiting(r.Context(), options)
			}
		case "getLoopsStatus":
			if s.loopState == nil {
				err = loops.ErrDisabled
				break
			}
			var state store.LoopDerivationState
			state, err = s.loopState(r.Context())
			if err == nil {
				var response loops.StatusResponse
				response.State = state
				response.Freshness, err = s.loopService.Freshness(r.Context(), s.clockNow().UTC(), nil)
				result = response
			}
		case "runLoops", "rebuildLoops":
			if s.loopRunner == nil {
				err = loops.ErrDisabled
				break
			}
			var response loops.RunResponse
			response.Disposition, err = s.loopRunner.Start(operation == "rebuildLoops")
			result = response
		case "listLoopSuppressions":
			var response loops.SuppressionListResponse
			response.Items, err = s.loopService.ListSuppressions(r.Context())
			result = response
		case "createLoopSuppression":
			var request loops.SuppressionRequest
			if _, ok := decodeEntityRequestFields(w, r, &request, "loop"); !ok {
				return
			}
			result, err = s.loopService.Suppress(r.Context(), request)
			if !request.DryRun {
				status = http.StatusCreated
			}
		default:
			var id int64
			id, err = loopPathID(r)
			if err != nil {
				break
			}
			switch operation {
			case "getOpenLoop":
				result, err = s.loopService.Get(r.Context(), id)
			case "closeOpenLoop":
				var request CloseOpenLoopRequest
				if _, ok := decodeEntityRequestFields(w, r, &request, "loop"); !ok {
					return
				}
				result, err = s.loopService.Close(r.Context(), id, request.Note)
			case "snoozeOpenLoop":
				var request SnoozeOpenLoopRequest
				if _, ok := decodeEntityRequestFields(w, r, &request, "loop"); !ok {
					return
				}
				result, err = s.loopService.Snooze(r.Context(), id, request.Until)
			case "unsnoozeOpenLoop":
				result, err = s.loopService.Unsnooze(r.Context(), id)
			case "deleteLoopSuppression":
				err = s.loopService.DeleteSuppression(r.Context(), id)
				status = http.StatusNoContent
			}
		}
		if err != nil {
			s.writeLoopError(w, err)
			return
		}
		if status == http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		writeJSON(w, status, result)
	}
}

func loopPathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		return 0, store.ErrInvalidLoop
	}
	return id, nil
}

func loopQuery(r *http.Request, key string) (string, error) {
	values, ok := r.URL.Query()[key]
	if !ok {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("%w: repeated %s", store.ErrInvalidLoop, key)
	}
	return values[0], nil
}

func loopBoolQuery(r *http.Request, key string) (bool, error) {
	value, err := loopQuery(r, key)
	if err != nil {
		return false, err
	}
	if _, supplied := r.URL.Query()[key]; !supplied {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%w: invalid %s", store.ErrInvalidLoop, key)
	}
	return parsed, nil
}

func loopPositiveQuery(r *http.Request, key string) (int64, error) {
	value, err := loopQuery(r, key)
	if err != nil {
		return 0, err
	}
	if _, supplied := r.URL.Query()[key]; !supplied {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("%w: invalid %s", store.ErrInvalidLoop, key)
	}
	return parsed, nil
}

func parseLoopList(r *http.Request, person bool) (loops.ListOptions, error) {
	var out loops.ListOptions
	var err error
	out.Limit, err = boundedActivityQuery(r, "limit", 50, 1, 200)
	if err != nil {
		return out, err
	}
	if person {
		out.PersonID, err = loopPathID(r)
		return out, err
	}
	out.Kind, err = loopQuery(r, "kind")
	if err != nil {
		return out, err
	}
	out.Status, err = loopQuery(r, "status")
	if err != nil {
		return out, err
	}
	out.Cursor, err = loopQuery(r, "cursor")
	if err != nil {
		return out, err
	}
	out.PersonID, err = loopPositiveQuery(r, "person_id")
	if err != nil {
		return out, err
	}
	out.SourceID, err = loopPositiveQuery(r, "source_id")
	if err != nil {
		return out, err
	}
	out.IncludeSnoozed, err = loopBoolQuery(r, "include_snoozed")
	return out, err
}

func parseLoopWaiting(r *http.Request) (loops.WaitingOptions, error) {
	var out loops.WaitingOptions
	var err error
	out.Top, err = boundedActivityQuery(r, "top", 10, 1, 50)
	if err != nil {
		return out, err
	}
	out.AllowStale, err = loopBoolQuery(r, "allow_stale")
	if err != nil {
		return out, err
	}
	value, err := loopQuery(r, "as_of")
	if err != nil {
		return out, err
	}
	if _, supplied := r.URL.Query()["as_of"]; supplied {
		out.AsOf, err = time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return out, fmt.Errorf("%w: invalid as_of", store.ErrInvalidLoop)
		}
	}
	return out, nil
}

func (s *Server) writeLoopError(w http.ResponseWriter, err error) {
	if stale, ok := errors.AsType[*loops.StaleError](err); ok {
		writeJSON(w, http.StatusConflict, stale.Response)
		return
	}
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, loops.ErrDisabled):
		writeError(w, http.StatusServiceUnavailable, "loops_disabled", "Open loops are unavailable")
	case errors.Is(err, store.ErrLoopNotFound):
		writeError(w, http.StatusNotFound, "loop_not_found", "Open loop or suppression not found")
	case errors.Is(err, store.ErrLoopAlreadyClosed):
		writeError(w, http.StatusConflict, "loop_already_closed", "Open loop is already closed")
	case errors.Is(err, store.ErrInvalidLoop), errors.Is(err, store.ErrInvalidActivityRequest):
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid open-loop request")
	default:
		s.logger.Error("open-loop request failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "Open-loop request failed")
	}
}
