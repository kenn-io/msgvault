package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/meetingcontent"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

const (
	meetingRequestMaxBytes = 1 << 20
	meetingContextMaxIDs   = 100
	meetingExploreMaxIDs   = query.MaxExploreCandidateMessageIDs
	meetingContextMaxBytes = 1 << 20
	meetingContextMinBytes = 4096
	meetingContextDefault  = 131072
)

// MeetingStore is the feature-local persistence capability behind the three
// meeting intelligence routes.
type MeetingStore interface {
	GetMeetingContextContext(
		ctx context.Context, ids []int64, options meetingcontent.PacketOptions,
	) (*meetingcontent.PacketResult, error)
	ListMeetingActionsContext(
		ctx context.Context, request store.MeetingActionsQuery,
	) (*meetingcontent.ActionsPage, error)
	GetMeetingMetricsContext(
		ctx context.Context, scope store.MeetingQueryScope,
	) (*meetingcontent.Metrics, error)
}

var _ MeetingStore = (*store.Store)(nil)

type MeetingScopeRequest struct {
	MessageIDs     *[]int64   `json:"message_ids,omitempty" maxItems:"100"`
	SourceIDs      []int64    `json:"source_ids,omitempty" maxItems:"100"`
	ParticipantIDs []int64    `json:"participant_ids,omitempty" maxItems:"100"`
	PersonID       int64      `json:"person_id,omitempty" minimum:"1" maximum:"9007199254740991"`
	ParticipantID  int64      `json:"participant_id,omitempty" minimum:"1" maximum:"9007199254740991"`
	Domains        []string   `json:"domains,omitempty" maxItems:"100"`
	After          *time.Time `json:"after,omitempty"`
	Before         *time.Time `json:"before,omitempty"`
	Deletion       string     `json:"deletion,omitempty" enum:"any,active,deleted"`
}

type MeetingExploreScope struct {
	Predicate           ExploreHTTPRequest     `json:"predicate"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type MeetingContextRequest struct {
	MessageIDs        *[]int64              `json:"message_ids,omitempty" maxItems:"100"`
	Selection         *ExploreSelection     `json:"selection,omitempty"`
	Format            meetingcontent.Format `json:"format,omitempty" enum:"json,markdown"`
	IncludeTranscript bool                  `json:"include_transcript,omitempty"`
	MaxBytes          int                   `json:"max_bytes,omitempty" minimum:"4096" maximum:"1048576"`
}

type MeetingActionsRequest struct {
	Scope         *MeetingScopeRequest  `json:"scope,omitempty"`
	Explore       *MeetingExploreScope  `json:"explore,omitempty"`
	AssigneeEmail string                `json:"assignee_email,omitempty"`
	Status        meetingcontent.Status `json:"status,omitempty" enum:"pending,completed,cancelled,unknown"`
	Query         string                `json:"query,omitempty" maxLength:"256"`
	Limit         int                   `json:"limit,omitempty" minimum:"1" maximum:"200"`
	Cursor        string                `json:"cursor,omitempty"`
}

type MeetingMetricsRequest struct {
	Scope   *MeetingScopeRequest `json:"scope,omitempty"`
	Explore *MeetingExploreScope `json:"explore,omitempty"`
}

func (s *Server) registerMeetingRoutes(api huma.API) {
	registerMeetingRoute[MeetingContextRequest, meetingcontent.PacketResult](
		api, "getMeetingContext", "/meetings/context", "Render bounded archived meeting context", s.handleMeetingContext,
	)
	registerMeetingRoute[MeetingActionsRequest, meetingcontent.ActionsPage](
		api, "listMeetingActionItems", "/meetings/actions", "List archived meeting action items", s.handleMeetingActions,
	)
	registerMeetingRoute[MeetingMetricsRequest, meetingcontent.Metrics](
		api, "getMeetingMetrics", "/meetings/metrics", "Get archived meeting duration metrics", s.handleMeetingMetrics,
	)
}

func registerMeetingRoute[Request any, Response any](
	api huma.API,
	operationID, path, summary string,
	handler http.HandlerFunc,
) {
	op := rawAPIV1Operation(operationID, http.MethodPost, path, summary)
	op.Tags = []string{"Meetings"}
	op.RequestBody = jsonRequestBodyFor[Request](api)
	op.Responses = jsonResponsesFor[Response](api)
	op.Errors = []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusRequestEntityTooLarge,
		http.StatusUnsupportedMediaType,
		http.StatusInternalServerError,
		http.StatusServiceUnavailable,
	}
	addErrorResponses(api, op.Responses, op.Errors...)
	registerRawHumaRoute(api, op, handler)
}

func (s *Server) handleMeetingContext(w http.ResponseWriter, r *http.Request) {
	var request MeetingContextRequest
	fields, ok := decodeMeetingRequest(w, r, &request)
	if !ok {
		return
	}
	messageIDsPresent := fieldPresent(fields, "message_ids")
	selectionFields, selectionPresent, ok := meetingObjectField(w, fields, "selection")
	if !ok {
		return
	}
	if selectionPresent && !requireMeetingObjectFields(
		w, "selection", selectionFields, "mode", "predicate", "cache_revision", "search_provenance",
	) {
		return
	}
	if messageIDsPresent == selectionPresent {
		writeError(w, http.StatusBadRequest, "invalid_meeting_selection",
			"Exactly one of message_ids or selection is required")
		return
	}

	format := request.Format
	if _, present := fields["format"]; present {
		if format != meetingcontent.FormatJSON && format != meetingcontent.FormatMarkdown {
			writeError(w, http.StatusBadRequest, "invalid_meeting_format", "format must be json or markdown")
			return
		}
	} else {
		format = meetingcontent.FormatJSON
	}
	maxBytes := request.MaxBytes
	if _, present := fields["max_bytes"]; present {
		if maxBytes < meetingContextMinBytes || maxBytes > meetingContextMaxBytes {
			writeError(w, http.StatusBadRequest, "invalid_meeting_budget",
				"max_bytes must be between 4096 and 1048576")
			return
		}
	} else {
		maxBytes = meetingContextDefault
	}

	var ids []int64
	if messageIDsPresent {
		if !validateOptionalMeetingIDs(w, "message_ids", request.MessageIDs, fields, false) {
			return
		}
		ids = normalizedPublicMeetingIDs(*request.MessageIDs)
	} else {
		if request.Selection == nil {
			writeError(w, http.StatusBadRequest, "invalid_meeting_selection", "selection must be an object")
			return
		}
		resolved, resolvedOK := s.resolveMeetingExploreScope(
			r.Context(), w, MeetingExploreScope{}, request.Selection, meetingContextMaxIDs, true,
		)
		if !resolvedOK {
			return
		}
		ids = *resolved.Scope.MessageIDs
	}
	meetingStore, ok := s.store.(MeetingStore)
	if !ok || meetingStore == nil {
		writeError(w, http.StatusServiceUnavailable, "meetings_unavailable",
			"Meeting intelligence is unavailable")
		return
	}
	result, err := meetingStore.GetMeetingContextContext(r.Context(), ids, meetingcontent.PacketOptions{
		Format: format, IncludeTranscript: request.IncludeTranscript, MaxBytes: maxBytes,
	})
	if err != nil {
		if selectionPresent && (errors.Is(err, store.ErrMeetingNotFound) || errors.Is(err, store.ErrNotMeeting)) {
			writeError(w, http.StatusConflict, "meeting_scope_changed",
				"The selected meeting population changed; resolve the scope again")
			return
		}
		s.writeMeetingError(w, err)
		return
	}
	if result == nil {
		s.writeMeetingError(w, errors.New("meeting context returned no result"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMeetingActions(w http.ResponseWriter, r *http.Request) {
	var request MeetingActionsRequest
	fields, ok := decodeMeetingRequest(w, r, &request)
	if !ok {
		return
	}
	if !validateMeetingActionsRequest(w, request, fields) {
		return
	}
	resolved, ok := s.resolveMeetingRequestScope(r.Context(), w, fields, request.Scope, request.Explore)
	if !ok {
		return
	}
	meetingStore, ok := s.store.(MeetingStore)
	if !ok || meetingStore == nil {
		writeError(w, http.StatusServiceUnavailable, "meetings_unavailable",
			"Meeting intelligence is unavailable")
		return
	}
	result, err := meetingStore.ListMeetingActionsContext(r.Context(), store.MeetingActionsQuery{
		Scope: resolved.Scope, AssigneeEmail: request.AssigneeEmail, Status: request.Status,
		Query: request.Query, Limit: request.Limit, Cursor: request.Cursor,
	})
	if err != nil {
		s.writeMeetingError(w, err)
		return
	}
	if result == nil {
		s.writeMeetingError(w, errors.New("meeting actions returned no result"))
		return
	}
	result.Scope = resolved.Provenance
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleMeetingMetrics(w http.ResponseWriter, r *http.Request) {
	var request MeetingMetricsRequest
	fields, ok := decodeMeetingRequest(w, r, &request)
	if !ok {
		return
	}
	resolved, ok := s.resolveMeetingRequestScope(r.Context(), w, fields, request.Scope, request.Explore)
	if !ok {
		return
	}
	meetingStore, ok := s.store.(MeetingStore)
	if !ok || meetingStore == nil {
		writeError(w, http.StatusServiceUnavailable, "meetings_unavailable",
			"Meeting intelligence is unavailable")
		return
	}
	result, err := meetingStore.GetMeetingMetricsContext(r.Context(), resolved.Scope)
	if err != nil {
		s.writeMeetingError(w, err)
		return
	}
	if result == nil {
		s.writeMeetingError(w, errors.New("meeting metrics returned no result"))
		return
	}
	result.Scope = resolved.Provenance
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) resolveMeetingRequestScope(
	ctx context.Context,
	w http.ResponseWriter,
	fields map[string]json.RawMessage,
	direct *MeetingScopeRequest,
	explore *MeetingExploreScope,
) (meetingScopeResolution, bool) {
	directFields, directPresent, ok := meetingObjectField(w, fields, "scope")
	if !ok {
		return meetingScopeResolution{}, false
	}
	exploreFields, explorePresent, ok := meetingObjectField(w, fields, "explore")
	if !ok {
		return meetingScopeResolution{}, false
	}
	if directPresent && explorePresent {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope",
			"scope and explore are mutually exclusive")
		return meetingScopeResolution{}, false
	}
	if explorePresent {
		if explore == nil {
			writeError(w, http.StatusBadRequest, "invalid_meeting_scope", "explore must be an object")
			return meetingScopeResolution{}, false
		}
		if !requireMeetingObjectFields(
			w, "explore", exploreFields, "predicate", "cache_revision", "search_provenance",
		) {
			return meetingScopeResolution{}, false
		}
		return s.resolveMeetingExploreScope(ctx, w, *explore, nil, meetingExploreMaxIDs, false)
	}
	if directPresent && direct == nil {
		writeError(w, http.StatusBadRequest, "invalid_meeting_scope", "scope must be an object")
		return meetingScopeResolution{}, false
	}
	return s.resolveDirectMeetingScope(ctx, w, direct, directFields)
}

func validateMeetingActionsRequest(
	w http.ResponseWriter, request MeetingActionsRequest, fields map[string]json.RawMessage,
) bool {
	if _, present := fields["status"]; present {
		switch request.Status {
		case meetingcontent.StatusPending, meetingcontent.StatusCompleted,
			meetingcontent.StatusCancelled, meetingcontent.StatusUnknown:
		default:
			writeError(w, http.StatusBadRequest, "invalid_meeting_status",
				"status must be pending, completed, cancelled, or unknown")
			return false
		}
	}
	if utf8.RuneCountInString(request.Query) > 256 {
		writeError(w, http.StatusBadRequest, "invalid_meeting_query", "query must not exceed 256 characters")
		return false
	}
	if _, present := fields["limit"]; present && (request.Limit < 1 || request.Limit > 200) {
		writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be between 1 and 200")
		return false
	}
	return true
}

func decodeMeetingRequest(
	w http.ResponseWriter, r *http.Request, destination any,
) (map[string]json.RawMessage, bool) {
	if !hasJSONContentType(r) {
		writeError(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Content-Type must be application/json")
		return nil, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, meetingRequestMaxBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid meeting request JSON")
		return nil, false
	}
	if len(data) > meetingRequestMaxBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"Meeting request exceeds 1 MiB")
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return nil, false
	}
	if meetingJSONContainsNull(fields) {
		writeError(w, http.StatusBadRequest, "bad_request", "Meeting request fields must not be null")
		return nil, false
	}
	if meetingJSONHasNoncanonicalField(fields, reflect.TypeOf(destination)) {
		writeError(w, http.StatusBadRequest, "bad_request", "Meeting request field names must use canonical casing")
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid meeting request JSON")
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad_request", "Request body must contain one JSON object")
		return nil, false
	}
	return fields, true
}

func meetingJSONHasNoncanonicalField(fields map[string]json.RawMessage, destinationType reflect.Type) bool {
	destinationType = meetingJSONValueType(destinationType)
	if destinationType == nil || destinationType.Kind() != reflect.Struct {
		return false
	}
	fieldTypes := make(map[string]reflect.Type, destinationType.NumField())
	for field := range destinationType.Fields() {
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fieldTypes[name] = field.Type
	}
	for name, raw := range fields {
		fieldType, exact := fieldTypes[name]
		if !exact {
			for canonicalName := range fieldTypes {
				if strings.EqualFold(name, canonicalName) {
					return true
				}
			}
			continue
		}
		if meetingJSONValueHasNoncanonicalField(raw, fieldType) {
			return true
		}
	}
	return false
}

func meetingJSONValueHasNoncanonicalField(raw json.RawMessage, valueType reflect.Type) bool {
	valueType = meetingJSONValueType(valueType)
	if valueType == nil {
		return false
	}
	switch {
	case valueType.Kind() == reflect.Struct:
		var fields map[string]json.RawMessage
		return json.Unmarshal(raw, &fields) == nil &&
			meetingJSONHasNoncanonicalField(fields, valueType)
	case valueType.Kind() == reflect.Array || valueType.Kind() == reflect.Slice:
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil {
			return false
		}
		for _, value := range values {
			if meetingJSONValueHasNoncanonicalField(value, valueType.Elem()) {
				return true
			}
		}
	}
	return false
}

func meetingJSONValueType(valueType reflect.Type) reflect.Type {
	for valueType != nil && valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	return valueType
}

func meetingJSONContainsNull(value any) bool {
	switch current := value.(type) {
	case nil:
		return true
	case map[string]json.RawMessage:
		for _, raw := range current {
			var nested any
			if err := json.Unmarshal(raw, &nested); err != nil || meetingJSONContainsNull(nested) {
				return true
			}
		}
	case map[string]any:
		for _, nested := range current {
			if meetingJSONContainsNull(nested) {
				return true
			}
		}
	case []any:
		return slices.ContainsFunc(current, meetingJSONContainsNull)
	}
	return false
}

func meetingObjectField(
	w http.ResponseWriter, fields map[string]json.RawMessage, name string,
) (map[string]json.RawMessage, bool, bool) {
	raw, present := fields[name]
	if !present {
		return nil, false, true
	}
	if isJSONNull(raw) {
		writeError(w, http.StatusBadRequest, "bad_request", name+" must be an object")
		return nil, true, false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil || nested == nil {
		writeError(w, http.StatusBadRequest, "bad_request", name+" must be an object")
		return nil, true, false
	}
	return nested, true, true
}

func requireMeetingObjectFields(
	w http.ResponseWriter, objectName string, fields map[string]json.RawMessage, required ...string,
) bool {
	for _, name := range required {
		if !fieldPresent(fields, name) {
			writeError(w, http.StatusBadRequest, "bad_request", objectName+"."+name+" is required")
			return false
		}
	}
	return true
}

func (s *Server) writeMeetingError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	switch {
	case errors.Is(err, store.ErrMeetingNotFound):
		writeError(w, http.StatusNotFound, "meeting_not_found", "Meeting not found")
	case errors.Is(err, store.ErrNotMeeting):
		writeError(w, http.StatusBadRequest, "not_a_meeting", "Selected message is not a meeting")
	case errors.Is(err, store.ErrMeetingInvalidCursor):
		writeError(w, http.StatusBadRequest, "invalid_cursor", "Meeting action cursor is invalid")
	case errors.Is(err, store.ErrMeetingProjectionUnavailable):
		writeError(w, http.StatusServiceUnavailable, "meeting_projection_unavailable",
			"Meeting evidence projection is unavailable")
	case errors.Is(err, store.ErrMeetingScopeChanged):
		writeError(w, http.StatusConflict, "meeting_scope_changed",
			"The selected meeting population changed; resolve the scope again")
	case errors.Is(err, meetingcontent.ErrBudgetTooSmall):
		writeError(w, http.StatusBadRequest, "context_budget_too_small",
			"The meeting context budget is too small for the required envelope")
	default:
		if s.logger != nil {
			s.logger.Error("meeting intelligence failed", "error", err)
		}
		writeError(w, http.StatusInternalServerError, "internal_error", "Meeting intelligence failed")
	}
}
