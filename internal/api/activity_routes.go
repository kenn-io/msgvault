package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/store"
)

const (
	activityDateField        = "date"
	maxDailyNoteRequestBytes = 1 << 20
)

// ActivityStore is the optional dated-activity capability used by the person
// and calendar route families.
type ActivityStore interface {
	ContactStateContext(
		ctx context.Context, personID int64, now time.Time,
	) (store.ContactState, error)
	PersonDaysContext(
		ctx context.Context, request store.PersonDaysRequest,
	) (*store.PersonDaysPage, error)
	PersonDayContext(
		ctx context.Context, request store.PersonDayRequest,
	) (*store.PersonDayPage, error)
	DayContext(
		ctx context.Context, request store.DayRequest,
	) (*store.DayPage, error)
	ListDailyNoteEntriesContext(
		ctx context.Context, localDate string, limit, offset int,
	) ([]store.DailyNoteEntry, error)
	CreateDailyNoteEntryContext(
		ctx context.Context, input store.DailyNoteEntryInput,
	) (*store.DailyNoteEntry, error)
	DeleteDailyNoteEntryContext(ctx context.Context, id int64) error
}

type DailyNoteEntriesResponse struct {
	Entries []store.DailyNoteEntry `json:"entries"`
}

type CreateDailyNoteEntryRequest struct {
	Body      string  `json:"body"`
	Author    string  `json:"author,omitempty"`
	PersonIDs []int64 `json:"person_ids,omitempty"`
}

func (s *Server) registerActivityRoutes(api huma.API) {
	operations := []struct {
		id, method, path, summary string
		handler                   http.HandlerFunc
		response                  func(huma.API) map[string]*huma.Response
		errors                    []int
	}{
		{
			"getPersonContactState", http.MethodGet, "/persons/{id}/contact-state",
			"Get computed contact state for a person", s.handlePersonContactState,
			func(api huma.API) map[string]*huma.Response {
				return jsonResponsesFor[store.ContactState](api)
			},
			[]int{400, 401, 403, 404, 500, 503},
		},
		{
			"listPersonActivityDays", http.MethodGet, "/persons/{id}/days",
			"List calendar days intersecting a person", s.handlePersonDays,
			func(api huma.API) map[string]*huma.Response {
				return jsonResponsesFor[store.PersonDaysPage](api)
			},
			[]int{400, 401, 403, 404, 500, 503},
		},
		{
			"getPersonActivityDay", http.MethodGet, "/persons/{id}/days/{date}",
			"Get one person's activity and notes for a day", s.handlePersonDay,
			func(api huma.API) map[string]*huma.Response {
				return jsonResponsesFor[store.PersonDayPage](api)
			},
			[]int{400, 401, 403, 404, 500, 503},
		},
		{
			"getActivityDay", http.MethodGet, "/days/{date}",
			"Get people, activity, and notes for a day", s.handleDay,
			func(api huma.API) map[string]*huma.Response {
				return jsonResponsesFor[store.DayPage](api)
			},
			[]int{400, 401, 403, 500, 503},
		},
		{
			"listDayEntries", http.MethodGet, "/days/{date}/entries",
			"List authored entries for a day", s.handleDayEntries,
			func(api huma.API) map[string]*huma.Response {
				return jsonResponsesFor[DailyNoteEntriesResponse](api)
			},
			[]int{400, 401, 403, 500, 503},
		},
	}
	for _, route := range operations {
		op := rawAPIV1Operation(route.id, route.method, route.path, route.summary)
		op.Errors = route.errors
		op.Responses = route.response(api)
		addActivityErrorResponses(api, op.Responses, route.errors...)
		op.Parameters = activityRouteParameters(route.id)
		registerRawHumaRoute(api, op, route.handler)
	}

	create := rawAPIV1Operation(
		"createDayEntry", http.MethodPost, "/days/{date}/entries",
		"Create an authored entry for a day")
	create.Errors = []int{400, 401, 403, 404, 413, 415, 500, 503}
	create.RequestBody = jsonRequestBodyFor[CreateDailyNoteEntryRequest](api)
	create.Responses = jsonResponsesFor[store.DailyNoteEntry](api, http.StatusCreated)
	addActivityErrorResponses(api, create.Responses, create.Errors...)
	create.Parameters = activityRouteParameters(create.OperationID)
	registerRawHumaRoute(api, create, s.handleCreateDayEntry)

	remove := rawAPIV1Operation(
		"deleteDayEntry", http.MethodDelete, "/days/entries/{id}",
		"Delete an authored daily entry")
	remove.Errors = []int{400, 401, 403, 404, 500, 503}
	remove.Responses = rawHumaResponses(http.StatusNoContent)
	addActivityErrorResponses(api, remove.Responses, remove.Errors...)
	remove.Parameters = activityRouteParameters(remove.OperationID)
	registerRawHumaRoute(api, remove, s.handleDeleteDayEntry)
}

func addActivityErrorResponses(
	api huma.API, responses map[string]*huma.Response, statuses ...int,
) {
	for _, status := range statuses {
		responses[httpStatusKey(status)] = errorResponseFor(api)
	}
}

func activityRouteParameters(operationID string) []*huma.Param {
	id := activityPathIDParam()
	date := activityDateParam(activityDateField, "Exact local calendar date")
	limit := activityLimitParam("limit", "Maximum primary rows to return")
	offset := activityOffsetParam("offset", "Zero-based primary row offset")
	entryLimit := activityLimitParam(
		"entry_limit", "Maximum authored entries to return independently")
	entryOffset := activityOffsetParam(
		"entry_offset", "Zero-based authored-entry offset")
	switch operationID {
	case "getPersonContactState":
		return []*huma.Param{id}
	case "listPersonActivityDays":
		return []*huma.Param{
			id,
			activityDateParam("from", "Inclusive first local calendar date"),
			activityDateParam("to", "Inclusive last local calendar date"),
			limit, offset,
		}
	case "getPersonActivityDay":
		return []*huma.Param{id, date, limit, offset, entryLimit, entryOffset}
	case "getActivityDay":
		return []*huma.Param{
			date, limit, offset, entryLimit, entryOffset,
			activityLimitParam(
				"activity_limit_per_person",
				"Maximum activity references previewed for each person"),
		}
	case "listDayEntries":
		return []*huma.Param{date, limit, offset}
	case "createDayEntry":
		return []*huma.Param{date}
	case "deleteDayEntry":
		return []*huma.Param{id}
	default:
		return nil
	}
}

func activityPathIDParam() *huma.Param {
	parameter := pathIntegerParam("Positive durable identifier")
	minimum := float64(1)
	parameter.Schema.Minimum = &minimum
	return parameter
}

func activityDateParam(name, description string) *huma.Param {
	location := "query"
	required := false
	if name == activityDateField {
		location = "path"
		required = true
	}
	parameter := param(name, location, huma.TypeString, description, required)
	parameter.Schema.Pattern = `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`
	return parameter
}

func activityLimitParam(name, description string) *huma.Param {
	parameter := queryIntegerParam(name, description)
	minimum := float64(1)
	maximum := float64(store.ActivityMaxLimit)
	parameter.Schema.Minimum = &minimum
	parameter.Schema.Maximum = &maximum
	parameter.Schema.Default = store.ActivityDefaultLimit
	return parameter
}

func activityOffsetParam(name, description string) *huma.Param {
	parameter := queryIntegerParam(name, description)
	minimum := float64(0)
	parameter.Schema.Minimum = &minimum
	parameter.Schema.Default = 0
	return parameter
}

func (s *Server) activityStore(w http.ResponseWriter) (ActivityStore, bool) {
	activityStore, ok := s.store.(ActivityStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "service_unavailable",
			"dated activity is unavailable")
	}
	return activityStore, ok
}

func positivePathID(r *http.Request) (int64, error) {
	value, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || value <= 0 {
		return 0, store.ErrInvalidActivityRequest
	}
	return value, nil
}

func boundedActivityQuery(
	r *http.Request, name string, defaultValue, minimum, maximum int,
) (int, error) {
	values, supplied := r.URL.Query()[name]
	if !supplied {
		return defaultValue, nil
	}
	if len(values) != 1 || values[0] == "" {
		return 0, store.ErrInvalidActivityRequest
	}
	value, err := strconv.Atoi(values[0])
	if err != nil || value < minimum || value > maximum {
		return 0, store.ErrInvalidActivityRequest
	}
	return value, nil
}

func activityDate(value string) (string, error) {
	if !store.IsValidLocalDate(value) {
		return "", store.ErrInvalidActivityRequest
	}
	return value, nil
}

func optionalActivityDateQuery(r *http.Request, name string) (string, error) {
	values, supplied := r.URL.Query()[name]
	if !supplied {
		return "", nil
	}
	if len(values) != 1 {
		return "", store.ErrInvalidActivityRequest
	}
	return activityDate(values[0])
}

func (s *Server) handlePersonContactState(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	id, err := positivePathID(r)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	state, err := activityStore.ContactStateContext(r.Context(), id, s.clockNow())
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func (s *Server) handlePersonDays(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	id, err := positivePathID(r)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	limit, err := boundedActivityQuery(
		r, "limit", store.ActivityDefaultLimit, 1, store.ActivityMaxLimit)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	offset, err := boundedActivityQuery(r, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	from, err := optionalActivityDateQuery(r, "from")
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	to, err := optionalActivityDateQuery(r, "to")
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	if from != "" && to != "" && from > to {
		s.writeActivityError(w, store.ErrInvalidActivityRequest)
		return
	}
	page, err := activityStore.PersonDaysContext(r.Context(), store.PersonDaysRequest{
		PersonID: id, From: from, To: to, Limit: limit, Offset: offset,
	})
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handlePersonDay(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	id, err := positivePathID(r)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	date, err := activityDate(r.PathValue(activityDateField))
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	limit, offset, entryLimit, entryOffset, err := activityDualPages(r)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	page, err := activityStore.PersonDayContext(r.Context(), store.PersonDayRequest{
		PersonID: id, LocalDate: date, Limit: limit, Offset: offset,
		EntryLimit: entryLimit, EntryOffset: entryOffset,
	})
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func activityDualPages(r *http.Request) (int, int, int, int, error) {
	limit, err := boundedActivityQuery(
		r, "limit", store.ActivityDefaultLimit, 1, store.ActivityMaxLimit)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	offset, err := boundedActivityQuery(r, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	entryLimit, err := boundedActivityQuery(
		r, "entry_limit", store.ActivityDefaultLimit, 1, store.ActivityMaxLimit)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	entryOffset, err := boundedActivityQuery(
		r, "entry_offset", 0, 0, int(^uint(0)>>1))
	return limit, offset, entryLimit, entryOffset, err
}

func (s *Server) handleDay(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	date, err := activityDate(r.PathValue(activityDateField))
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	limit, offset, entryLimit, entryOffset, err := activityDualPages(r)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	preview, err := boundedActivityQuery(
		r, "activity_limit_per_person", store.ActivityDefaultLimit,
		1, store.ActivityMaxLimit)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	page, err := activityStore.DayContext(r.Context(), store.DayRequest{
		LocalDate: date, Limit: limit, Offset: offset,
		EntryLimit: entryLimit, EntryOffset: entryOffset,
		ActivityLimitPerPerson: preview,
	})
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleDayEntries(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	date, err := activityDate(r.PathValue(activityDateField))
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	limit, err := boundedActivityQuery(
		r, "limit", store.ActivityDefaultLimit, 1, store.ActivityMaxLimit)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	offset, err := boundedActivityQuery(r, "offset", 0, 0, int(^uint(0)>>1))
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	entries, err := activityStore.ListDailyNoteEntriesContext(
		r.Context(), date, limit, offset)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	if entries == nil {
		entries = []store.DailyNoteEntry{}
	}
	writeJSON(w, http.StatusOK, DailyNoteEntriesResponse{Entries: entries})
}

func (s *Server) handleCreateDayEntry(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	date, err := activityDate(r.PathValue(activityDateField))
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	if r.ContentLength > maxDailyNoteRequestBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"daily entry request is too large")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDailyNoteRequestBytes))
	if err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"daily entry request is too large")
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", "invalid daily entry request")
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var request *CreateDailyNoteEntryRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid daily entry request")
		return
	}
	if request == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid daily entry request")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request",
			"daily entry request must contain one JSON object")
		return
	}
	for _, personID := range request.PersonIDs {
		if personID <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request",
				"person_ids must contain only positive IDs")
			return
		}
	}
	entry, err := activityStore.CreateDailyNoteEntryContext(
		r.Context(), store.DailyNoteEntryInput{
			LocalDate: date, Body: request.Body, Author: request.Author,
			PersonIDs: request.PersonIDs,
		})
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, entry)
}

func (s *Server) handleDeleteDayEntry(w http.ResponseWriter, r *http.Request) {
	activityStore, ok := s.activityStore(w)
	if !ok {
		return
	}
	id, err := positivePathID(r)
	if err != nil {
		s.writeActivityError(w, err)
		return
	}
	if err := activityStore.DeleteDailyNoteEntryContext(r.Context(), id); err != nil {
		s.writeActivityError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeActivityError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrPersonNotFound),
		errors.Is(err, store.ErrDailyNoteEntryNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrInvalidActivityRequest),
		errors.Is(err, store.ErrDailyNoteDateRequired),
		errors.Is(err, store.ErrDailyNoteBodyRequired):
		writeError(w, http.StatusBadRequest, "bad_request", "invalid activity request")
	default:
		writeError(w, http.StatusInternalServerError, "internal_error",
			"dated activity request failed")
	}
}
