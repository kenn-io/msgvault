package api

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
)

// MeetingRecordsBackend composes archive evidence inside the admitted sources.
type MeetingRecordsBackend interface {
	GetMeetingRecordContext(ctx context.Context, scope store.SourceScope, ref string) (store.MeetingRecord, error)
	ListPersonMeetingsContext(ctx context.Context, scope store.SourceScope, query store.PersonMeetingsQuery) ([]store.MeetingRecordSummary, string, error)
	SuggestMeetingLinksContext(ctx context.Context, scope store.SourceScope, ref string, limit int) ([]store.MeetingSuggestion, error)
	LinkMeetingContext(ctx context.Context, ref string, messageID int64) error
	UnlinkMeetingContext(ctx context.Context, ref string, messageID int64) error
}

type PersonMeetingsResponse struct {
	Items      []store.MeetingRecordSummary `json:"items"`
	NextCursor string                       `json:"next_cursor,omitempty"`
}

type MeetingLinkRequest struct {
	MessageID int64 `json:"message_id" minimum:"1" maximum:"9007199254740991"`
}

func meetingRecordsLimitParam(maximum, fallback int) *huma.Param {
	param := queryIntegerParam("limit", "Maximum returned items")
	low, high := float64(1), float64(maximum)
	param.Schema.Minimum = &low
	param.Schema.Maximum = &high
	param.Schema.Default = fallback
	return param
}

func (s *Server) registerMeetingRecordsRoutes(api huma.API) {
	refParam := func() *huma.Param {
		return &huma.Param{Name: "ref", In: "path", Required: true, Schema: &huma.Schema{Type: "string"}, Description: "evt:<id>[@<original-key>] or msg:<id>"}
	}
	get := rawAPIV1Operation("getMeeting", http.MethodGet, "/meetings/{ref}", "Read a meeting record")
	get.Tags = []string{"Meetings"}
	get.Parameters = []*huma.Param{refParam()}
	get.Responses = jsonResponsesFor[store.MeetingRecord](api)
	get.Errors = []int{400, 401, 403, 404, 422, 503, 500}
	addErrorResponses(api, get.Responses, get.Errors...)
	registerRawHumaRoute(api, get, s.handleGetMeeting)
	list := rawAPIV1Operation("listPersonMeetings", http.MethodGet, "/people/{ref}/meetings", "List a person's archived meetings")
	list.Tags = get.Tags
	personRef := refParam()
	personRef.Description = "Numeric person ID, person:<id>, or participant:<id>"
	list.Parameters = []*huma.Param{personRef, queryStringParam("from", "Inclusive RFC3339 start; default 365 days before to", false), queryStringParam("to", "Exclusive RFC3339 end; default now", false), meetingRecordsLimitParam(500, 100), queryStringParam("cursor", "Opaque continuation cursor from the same query", false)}
	list.Responses = jsonResponsesFor[PersonMeetingsResponse](api)
	list.Errors = []int{400, 401, 403, 404, 409, 422, 503, 500}
	addErrorResponses(api, list.Responses, list.Errors...)
	registerRawHumaRoute(api, list, s.handleListPersonMeetings)
	suggest := rawAPIV1Operation("suggestMeetingLinks", http.MethodGet, "/meetings/{ref}/suggestions", "Suggest archive meeting links")
	suggest.Tags = get.Tags
	suggest.Parameters = []*huma.Param{refParam(), meetingRecordsLimitParam(100, 10)}
	suggest.Responses = jsonResponsesFor[[]store.MeetingSuggestion](api)
	suggest.Errors = get.Errors
	addErrorResponses(api, suggest.Responses, suggest.Errors...)
	registerRawHumaRoute(api, suggest, s.handleSuggestMeetingLinks)
	link := rawAPIV1Operation("linkMeeting", http.MethodPost, "/meetings/{ref}/links", "Link an archived note to an occurrence (owner only)")
	link.Tags = get.Tags
	link.Parameters = []*huma.Param{refParam()}
	link.RequestBody = jsonRequestBodyFor[MeetingLinkRequest](api)
	link.Responses = jsonResponsesFor[store.MeetingRecord](api)
	link.Errors = get.Errors
	addErrorResponses(api, link.Responses, link.Errors...)
	registerRawHumaRoute(api, link, s.handleLinkMeeting)
	unlink := rawAPIV1Operation("unlinkMeeting", http.MethodDelete, "/meetings/{ref}/links/{message_id}", "Split an archived note from an occurrence (owner only)")
	unlink.Tags = get.Tags
	unlink.Parameters = []*huma.Param{refParam(), {Name: "message_id", In: "path", Required: true, Schema: &huma.Schema{Type: "integer", Format: "int64"}}}
	unlink.Responses = jsonResponsesFor[store.MeetingRecord](api)
	unlink.Errors = get.Errors
	addErrorResponses(api, unlink.Responses, unlink.Errors...)
	registerRawHumaRoute(api, unlink, s.handleUnlinkMeeting)
}

func (s *Server) meetingRecordsBackend(w http.ResponseWriter) (MeetingRecordsBackend, bool) {
	backend, ok := s.store.(MeetingRecordsBackend)
	if !ok {
		writeError(w, 503, "meeting_records_unavailable", "Meeting records are unavailable")
	}
	return backend, ok
}
func (s *Server) writeMeetingRecordError(w http.ResponseWriter, err error) {
	if s.writeIfContextError(w, err) {
		return
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, store.ErrNotFound) {
		writeError(w, 404, "not_found", "Meeting not found")
		return
	}
	s.writeCalendarEventsError(w, err)
}
func (s *Server) handleGetMeeting(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if err := store.ValidateMeetingRecordRef(ref); err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	backend, ok := s.meetingRecordsBackend(w)
	if !ok {
		return
	}
	record, err := backend.GetMeetingRecordContext(r.Context(), s.calendarEventsScope(r), ref)
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	writeJSON(w, 200, record)
}

// T5 accepts syntactic refs but reports an unqualified series as a plain error.
// T4's occurrence validator supplies the public invalid-argument contract.
func (s *Server) validateMeetingOccurrence(w http.ResponseWriter, r *http.Request, backend MeetingRecordsBackend, ref string, scope store.SourceScope) bool {
	var err error
	if events, ok := s.store.(CalendarEventsBackend); ok {
		_, err = events.GetCalendarOccurrenceContext(r.Context(), scope, ref)
	} else {
		_, err = backend.GetMeetingRecordContext(r.Context(), scope, ref)
	}
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return false
	}
	return true
}
func (s *Server) handleSuggestMeetingLinks(w http.ResponseWriter, r *http.Request) {
	ref := r.PathValue("ref")
	if err := store.ValidateMeetingRecordRef(ref); err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	limit, present, err := queryInt(r, "limit")
	if err != nil || (present && (limit < 1 || limit > 100)) {
		writeError(w, 400, "invalid_argument", "limit must be between 1 and 100")
		return
	}
	if !present {
		limit = 10
	}
	backend, ok := s.meetingRecordsBackend(w)
	if !ok {
		return
	}
	if strings.HasPrefix(ref, "evt:") && !s.validateMeetingOccurrence(w, r, backend, ref, s.calendarEventsScope(r)) {
		return
	}
	items, err := backend.SuggestMeetingLinksContext(r.Context(), s.calendarEventsScope(r), ref, limit)
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	writeJSON(w, 200, items)
}
func (s *Server) handleLinkMeeting(w http.ResponseWriter, r *http.Request) {
	s.handleMeetingLinkWrite(w, r, false)
}
func (s *Server) handleUnlinkMeeting(w http.ResponseWriter, r *http.Request) {
	s.handleMeetingLinkWrite(w, r, true)
}
func (s *Server) handleMeetingLinkWrite(w http.ResponseWriter, r *http.Request, unlink bool) {
	if !s.ownerAccessAuthorized(r) {
		writeError(w, 403, "permission_denied", "Meeting link changes require owner access")
		return
	}
	ref := r.PathValue("ref")
	kind, _, _, err := store.ParseMeetingRef(ref)
	if err != nil || kind != "evt" || store.ValidateMeetingRecordRef(ref) != nil {
		writeError(w, 400, "invalid_argument", "ref must identify a calendar occurrence")
		return
	}
	var id int64
	if unlink {
		id, err = strconv.ParseInt(r.PathValue("message_id"), 10, 64)
	} else {
		var body MeetingLinkRequest
		data, readErr := io.ReadAll(io.LimitReader(r.Body, 65537))
		err = readErr
		if err == nil {
			if len(data) > 65536 {
				err = store.ErrInvalidArgument
			} else {
				err = json.Unmarshal(data, &body)
			}
		}
		id = body.MessageID
	}
	if err != nil || id < 1 || id > maxPublicMeetingID {
		writeError(w, 400, "invalid_argument", "message_id must be a positive JavaScript-safe integer")
		return
	}
	backend, ok := s.meetingRecordsBackend(w)
	if !ok {
		return
	}
	if !s.validateMeetingOccurrence(w, r, backend, ref, store.OwnerScope()) {
		return
	}
	if unlink {
		err = backend.UnlinkMeetingContext(r.Context(), ref, id)
	} else {
		err = backend.LinkMeetingContext(r.Context(), ref, id)
	}
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	record, err := backend.GetMeetingRecordContext(store.WithoutReadSnapshotContext(r.Context()), store.OwnerScope(), ref)
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	writeJSON(w, 200, record)
}

func (s *Server) handleListPersonMeetings(w http.ResponseWriter, r *http.Request) {
	kind, id, err := store.ParsePersonMeetingRef(r.PathValue("ref"))
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	q := store.PersonMeetingsQuery{Cursor: r.URL.Query().Get("cursor")}
	for _, field := range []struct {
		name   string
		target *time.Time
	}{{"from", &q.From}, {"to", &q.To}} {
		if value := r.URL.Query().Get(field.name); value != "" {
			*field.target, err = time.Parse(time.RFC3339, value)
			if err != nil {
				writeError(w, 400, "invalid_argument", field.name+" must be an RFC3339 timestamp")
				return
			}
		}
	}
	limit, present, err := queryInt(r, "limit")
	if err != nil || (present && (limit < 1 || limit > 500)) {
		writeError(w, 400, "invalid_argument", "limit must be between 1 and 500")
		return
	}
	q.Limit = limit
	backend, ok := s.meetingRecordsBackend(w)
	if !ok {
		return
	}
	people, ok := s.store.(meetingPersonSnapshotBackend)
	if !ok {
		writeError(w, 503, "person_scope_unavailable", "Person scope resolution is unavailable")
		return
	}
	ctx, release, err := people.BeginReadSnapshotContext(r.Context())
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	defer release()
	reference := resolver.Reference{Kind: resolver.ReferenceKind(kind), ID: id}
	resolved, err := resolver.Resolve(ctx, meetingSnapshotPersonResolver{backend: people, ctx: ctx}, reference, nil)
	personID := resolved.PersonID
	if errors.Is(err, resolver.ErrEmptyPopulation) && kind == "person" {
		person, getErr := people.GetMeetingPersonContext(ctx, id)
		err = getErr
		if err == nil {
			personID = person.ID
		}
	}
	if err != nil {
		s.writePersonScopeError(w, reference, err, "meeting history")
		return
	}
	q.ParticipantIDs = resolved.Scope.ParticipantIDs
	q.Emails, err = people.MeetingParticipantEmailsContext(ctx, q.ParticipantIDs)
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	if personID > 0 {
		points, err := people.ListMeetingPersonContactPointsContext(ctx, personID)
		if err != nil {
			s.writeMeetingRecordError(w, err)
			return
		}
		for _, point := range points {
			if point.AddressKind == store.ContactAddressEmail {
				q.Emails = append(q.Emails, point.OriginalValue)
			}
		}
	}
	items, cursor, err := backend.ListPersonMeetingsContext(ctx, s.calendarEventsScope(r), q)
	if err != nil {
		s.writeMeetingRecordError(w, err)
		return
	}
	writeJSON(w, 200, PersonMeetingsResponse{Items: items, NextCursor: cursor})
}
