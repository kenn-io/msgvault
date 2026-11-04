package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/contactcadence"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

// RelationshipsHTTPRequest scopes and pages a relationship ranking request.
// Filters follow the same predicate shape as /explore; there is no text
// query because ranking is over reciprocity signals, not lexical/semantic
// search candidates.
type RelationshipsHTTPRequest struct {
	Filters       []ExploreFilter `json:"filters,omitempty"`
	ShowAll       bool            `json:"show_all,omitzero"`
	Cursor        string          `json:"cursor,omitempty"`
	Limit         int             `json:"limit,omitzero" minimum:"0" maximum:"500"`
	Cadence       string          `json:"cadence,omitempty" enum:"overdue,due,no_list,unknown"`
	DueWithinDays *int            `json:"due_within_days,omitempty" minimum:"0" maximum:"3660"`
}

// RelationshipsHTTPResponse echoes both revisions a page was computed
// against so clients can detect archive or identity drift across pages.
type RelationshipsHTTPResponse struct {
	Rows             []query.RelationshipRow `json:"rows"`
	TotalCount       int64                   `json:"total_count"`
	CacheRevision    string                  `json:"cache_revision"`
	IdentityRevision int64                   `json:"identity_revision"`
	NextCursor       string                  `json:"next_cursor,omitempty"`
}

type RelationshipCalendarHTTPRequest struct {
	Year     int    `json:"year" minimum:"1970"`
	Timezone string `json:"timezone,omitempty"`
}

type RelationshipCalendarHTTPResponse struct {
	*query.RelationshipCalendarResponse

	ParticipantID int64 `json:"participant_id"`
}

func (s *Server) registerRelationshipRoutes(api huma.API) {
	registerExploreRoute[RelationshipsHTTPRequest, RelationshipsHTTPResponse](
		api, "listRelationships", "/relationships", "Rank counterparts by reciprocity-weighted interaction", s.handleRelationships,
	)
	registerExploreRoute[RelationshipTimelineHTTPRequest, RelationshipTimelineHTTPResponse](
		api, "getRelationshipTimeline", "/relationships/{id}/timeline",
		"Get one counterpart's interaction timeline, with chat grouped into local-day bursts", s.handleRelationshipTimeline,
	)
	calendar := rawAPIV1Operation("getRelationshipCalendar", http.MethodPost,
		"/relationships/{id}/calendar",
		"Get one counterpart's timezone-aware relationship calendar")
	calendar.Tags = []string{"Exploration"}
	calendar.RequestBody = jsonRequestBodyFor[RelationshipCalendarHTTPRequest](api)
	calendar.Responses = jsonResponsesFor[RelationshipCalendarHTTPResponse](api)
	addErrorResponses(api, calendar.Responses, http.StatusBadRequest,
		http.StatusNotFound, http.StatusServiceUnavailable)
	calendar.Responses[httpStatusKey(http.StatusServiceUnavailable)] = exploreUnavailableResponseFor(api)
	registerRawHumaRoute(api, calendar, s.handleRelationshipCalendar)
}

func (s *Server) handleRelationshipCalendar(w http.ResponseWriter, r *http.Request) {
	participantID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || participantID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_participant_id",
			"participant ID must be a positive integer")
		return
	}
	var request RelationshipCalendarHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	engine := s.queryEngineForContext(r.Context())
	resolver, ok := engine.(query.RelationshipCanonicalResolver)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}
	analyzer, ok := engine.(query.RelationshipCalendarAnalyzer)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}
	canonicalID, err := resolver.ResolveCanonicalParticipant(r.Context(), participantID)
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}
	result, err := analyzer.RelationshipCalendar(r.Context(), query.RelationshipCalendarRequest{
		CanonicalID: canonicalID, Year: request.Year, Timezone: request.Timezone,
	})
	if err != nil {
		switch {
		case errors.Is(err, query.ErrInvalidRelationshipYear):
			writeError(w, http.StatusBadRequest, "invalid_year", err.Error())
		case errors.Is(err, query.ErrInvalidRelationshipTimezone):
			writeError(w, http.StatusBadRequest, "invalid_timezone", err.Error())
		case errors.Is(err, query.ErrRelationshipPersonNotFound):
			writeError(w, http.StatusNotFound, "participant_not_found", "Participant cluster not found")
		default:
			s.writeExploreError(r.Context(), w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, RelationshipCalendarHTTPResponse{
		ParticipantID: participantID, RelationshipCalendarResponse: result,
	})
}

func (s *Server) handleRelationships(w http.ResponseWriter, r *http.Request) {
	var request RelationshipsHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	request.Cadence = strings.ToLower(strings.TrimSpace(request.Cadence))
	if err := validateRelationshipCadence(request.Cadence, request.DueWithinDays); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_cadence", err.Error())
		return
	}
	canonicalizeRelationshipFilters(request.Filters)
	analyticalContext, err := exploreContext(request.Filters)
	if err != nil {
		s.writeExploreFilterError(w, err, "invalid_filter")
		return
	}
	if err := s.resolveExploreIdentityContext(r.Context(), request.Filters, &analyticalContext); err != nil {
		s.writeExploreFilterError(w, err, "invalid_filter")
		return
	}
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreMaxLimit))
		return
	}
	canonical := request
	canonical.Cursor = ""
	requestHash := hashCanonicalValue(canonical, false)
	offset, ok := s.parseExploreCursor(w, request.Cursor, requestHash)
	if !ok {
		return
	}
	var cursor exploreCursor
	cadenceEvaluatedAt := s.clockNow().UTC()
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
		pinned, err := time.Parse(time.RFC3339Nano, cursor.CadenceEvaluatedAt)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "relationship cursor has no valid cadence evaluation time")
			return
		}
		cadenceEvaluatedAt = pinned.UTC()
	}
	decayDate, ok := s.relationshipDecayDate(w, cursor)
	if !ok {
		return
	}

	analyzer, ok := s.queryEngineForContext(r.Context()).(query.RelationshipAnalyzer)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}
	if request.Cadence != "" {
		if _, ok := s.store.(relationshipDirectoryStore); !ok {
			writeError(w, http.StatusServiceUnavailable, "persons_unavailable", "Person profiles are unavailable")
			return
		}
	}
	var result *query.RelationshipsResponse
	var cadenceFingerprint string
	if request.Cadence == "" {
		result, err = analyzer.Relationships(r.Context(), query.RelationshipsRequest{
			Context: analyticalContext, ShowAll: request.ShowAll, Limit: request.Limit, Offset: offset, Now: decayDate,
		})
		if err == nil {
			err = s.hydrateRelationshipRows(r.Context(), result.Rows, cadenceEvaluatedAt)
		}
	} else {
		result, cadenceFingerprint, err = s.relationshipsWithCadenceFilter(
			r.Context(), analyzer, analyticalContext, request, decayDate, offset, cadenceEvaluatedAt,
		)
	}
	if err != nil {
		if directoryErr, ok := errors.AsType[*relationshipDirectoryError](err); ok {
			s.writeDirectoryPeopleError(w, directoryErr.err)
		} else {
			s.writeExploreError(r.Context(), w, err)
		}
		return
	}
	if request.Cursor != "" {
		// Check identity drift before the archive-revision comparison:
		// CacheSyncState.Revision() folds IdentityRevision into its hash, so
		// any identity-only change (a link/unlink/merge) also changes
		// CacheRevision. Checking archive revision first would make
		// identity_revision_changed unreachable — every identity drift
		// would surface as archive_revision_changed instead.
		if cursor.IdentityRevision != result.IdentityRevision {
			writeError(w, http.StatusConflict, "identity_revision_changed", "Identity clusters changed; restart pagination")
			return
		}
		if cursor.Revision != result.CacheRevision {
			writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
			return
		}
		if request.Cadence != "" && cursor.CadenceFingerprint != cadenceFingerprint {
			writeError(w, http.StatusConflict, "cadence_revision_changed", "Keep-in-touch cadence changed; restart pagination")
			return
		}
	}
	response := RelationshipsHTTPResponse{
		Rows: result.Rows, TotalCount: result.TotalCount,
		CacheRevision: result.CacheRevision, IdentityRevision: result.IdentityRevision,
	}
	if next := offset + len(result.Rows); next < int(result.TotalCount) {
		nextCursor := exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision, IdentityRevision: result.IdentityRevision,
			DecayDate: decayDate.Format(time.DateOnly), CadenceEvaluatedAt: cadenceEvaluatedAt.Format(time.RFC3339Nano),
		}
		if request.Cadence != "" {
			nextCursor.CadenceFingerprint = cadenceFingerprint
		}
		response.NextCursor = s.encodeExploreCursor(nextCursor)
	}
	writeJSON(w, http.StatusOK, response)
}

// relationshipDirectoryStore is deliberately optional so analytical test
// doubles and old embedders retain the original ranking contract. Production
// *store.Store implements it through the keeper Directory projection.
type relationshipDirectoryStore interface {
	DirectoryPeopleForParticipantsContext(
		ctx context.Context, participantIDs []int64,
	) (map[int64]store.DirectoryPersonSummary, error)
}

type relationshipDirectoryStoreAt interface {
	DirectoryPeopleForParticipantsAtContext(
		ctx context.Context, participantIDs []int64, evaluatedAt time.Time,
	) (map[int64]store.DirectoryPersonSummary, error)
}

type relationshipDirectoryError struct{ err error }

func (e *relationshipDirectoryError) Error() string {
	return fmt.Sprintf("hydrate relationship directory context: %v", e.err)
}

func (e *relationshipDirectoryError) Unwrap() error { return e.err }

func (s *Server) hydrateRelationshipRows(ctx context.Context, rows []query.RelationshipRow, evaluatedAt time.Time) error {
	lookup, ok := s.store.(relationshipDirectoryStore)
	if !ok || len(rows) == 0 {
		return nil
	}
	participantIDs := make([]int64, 0)
	for _, row := range rows {
		participantIDs = append(participantIDs, row.MemberIDs...)
		if len(row.MemberIDs) == 0 {
			participantIDs = append(participantIDs, row.CanonicalID)
		}
	}
	var people map[int64]store.DirectoryPersonSummary
	var err error
	if lookupAt, ok := s.store.(relationshipDirectoryStoreAt); ok {
		people, err = lookupAt.DirectoryPeopleForParticipantsAtContext(ctx, participantIDs, evaluatedAt)
	} else {
		people, err = lookup.DirectoryPeopleForParticipantsContext(ctx, participantIDs)
	}
	if err != nil {
		return &relationshipDirectoryError{err: err}
	}
	for index := range rows {
		members := rows[index].MemberIDs
		if len(members) == 0 {
			members = []int64{rows[index].CanonicalID}
		}
		var matchedParticipantID, matchedPersonID int64
		for _, participantID := range members {
			person, ok := people[participantID]
			if !ok {
				continue
			}
			if matchedPersonID != 0 && matchedPersonID != person.ID {
				return &relationshipDirectoryError{err: fmt.Errorf(
					"relationship cluster %d is bound to multiple directory people",
					rows[index].CanonicalID,
				)}
			}
			matchedPersonID = person.ID
			matchedParticipantID = participantID
		}
		if matchedParticipantID != 0 {
			person := people[matchedParticipantID]
			personID := person.ID
			rows[index].PersonID = &personID
			rows[index].Cadence = &query.RelationshipCadence{
				FrequencyDays: person.Cadence.FrequencyDays,
				List:          person.Cadence.List,
				DueAt:         person.Cadence.DueAt,
				Status:        person.Cadence.Status,
				DaysOverdue:   person.Cadence.DaysOverdue,
				Stale:         person.Cadence.Stale,
			}
			rows[index].LastContactChannel = person.LastContactChannel
		}
	}
	return nil
}

func validateRelationshipCadence(cadence string, dueWithinDays *int) error {
	switch cadence {
	case "", "overdue", "due", "no_list", "unknown":
	default:
		return errors.New("cadence must be overdue, due, no_list, or unknown")
	}
	if dueWithinDays == nil {
		return nil
	}
	if cadence != "due" || *dueWithinDays < 0 || *dueWithinDays > 3660 {
		return errors.New("due_within_days requires cadence=due and an integer from 0 to 3660")
	}
	return nil
}

func relationshipRowMatchesCadence(row query.RelationshipRow, cadence string, dueWithinDays *int, now time.Time) bool {
	if cadence == "" {
		return true
	}
	if row.Cadence == nil {
		return false
	}
	schedule := row.Cadence
	switch cadence {
	case "overdue":
		return schedule.Status == store.CadenceOverdue
	case "due":
		if schedule.DueAt == nil {
			return false
		}
		horizon := 0
		if dueWithinDays != nil {
			horizon = *dueWithinDays
		}
		return contactcadence.DueWithinUTCDateHorizon(*schedule.DueAt, now, horizon)
	case "no_list":
		return schedule.FrequencyDays == nil
	case "unknown":
		return schedule.FrequencyDays != nil && schedule.DueAt == nil
	default:
		return false
	}
}

// relationshipsWithCadenceFilter gets the complete ranking in one analyzer
// call because cadence belongs to the durable-person Store. It hydrates and
// filters the stable ranking before applying the requested page boundary.
func (s *Server) relationshipsWithCadenceFilter(
	ctx context.Context,
	analyzer query.RelationshipAnalyzer,
	analyticalContext query.Context,
	request RelationshipsHTTPRequest,
	decayDate time.Time,
	offset int,
	evaluatedAt time.Time,
) (*query.RelationshipsResponse, string, error) {
	ranking, err := analyzer.Relationships(ctx, query.RelationshipsRequest{
		Context: analyticalContext, ShowAll: request.ShowAll, AllRows: true, Now: decayDate,
	})
	if err != nil {
		return nil, "", err
	}
	if int64(len(ranking.Rows)) != ranking.TotalCount {
		return nil, "", fmt.Errorf("relationship analyzer returned %d of %d ranked rows", len(ranking.Rows), ranking.TotalCount)
	}
	all := ranking.Rows
	if err := s.hydrateRelationshipRows(ctx, all, evaluatedAt); err != nil {
		return nil, "", err
	}
	cadenceFingerprint := relationshipCadenceFingerprint(all)
	filtered := make([]query.RelationshipRow, 0, len(all))
	for _, row := range all {
		if relationshipRowMatchesCadence(row, request.Cadence, request.DueWithinDays, evaluatedAt) {
			filtered = append(filtered, row)
		}
	}
	if offset > len(filtered) {
		offset = len(filtered)
	}
	end := min(offset+request.Limit, len(filtered))
	ranking.Rows = append([]query.RelationshipRow(nil), filtered[offset:end]...)
	ranking.TotalCount = int64(len(filtered))
	return ranking, cadenceFingerprint, nil
}

func relationshipCadenceFingerprint(rows []query.RelationshipRow) string {
	type cadenceSnapshot struct {
		CanonicalID        int64                      `json:"canonical_id"`
		PersonID           *int64                     `json:"person_id,omitempty"`
		Cadence            *query.RelationshipCadence `json:"cadence,omitempty"`
		LastContactChannel string                     `json:"last_contact_channel,omitempty"`
	}
	snapshot := make([]cadenceSnapshot, len(rows))
	for index, row := range rows {
		snapshot[index] = cadenceSnapshot{
			CanonicalID: row.CanonicalID, PersonID: row.PersonID,
			Cadence: row.Cadence, LastContactChannel: row.LastContactChannel,
		}
	}
	return hashCanonicalValue(snapshot, false)
}

// relationshipDecayDate returns the UTC decay date every page of one
// relationship listing must rank with: midnight UTC of the current date for a
// first page, or the date pinned in the cursor for subsequent pages, so
// pagination crossing UTC midnight cannot re-rank rows mid-listing. The
// indexed ranking depends only on the UTC date of the timestamp, so midnight
// is equivalent to any instant on the same date. Cursors minted before the
// field existed carry no date and fall back to the current date — the prior
// behavior. Cursors are HMAC-signed, so the bounds check below is
// defense-in-depth against server bugs, not against tampering: dates before
// the Unix epoch or more than a day ahead of the clock are rejected.
func (s *Server) relationshipDecayDate(w http.ResponseWriter, cursor exploreCursor) (time.Time, bool) {
	now := s.clockNow().UTC()
	if cursor.DecayDate == "" {
		return now.Truncate(24 * time.Hour), true
	}
	pinned, err := time.Parse(time.DateOnly, cursor.DecayDate)
	if err != nil || pinned.Before(time.Unix(0, 0).UTC()) || pinned.After(now.AddDate(0, 0, 1)) {
		writeError(w, http.StatusBadRequest, "invalid_cursor", "cursor decay date is invalid")
		return time.Time{}, false
	}
	return pinned, true
}

// RelationshipTimelineHTTPRequest scopes and pages one counterpart's
// interaction timeline. Timezone is an IANA name used to bucket chat
// messages into local-day bursts; "" means UTC. The counterpart is fixed by
// the {id} path segment, which accepts any member of that identity's
// cluster; a "participant" filter dimension in Filters further restricts
// entries within that cluster rather than replacing the cluster scope (see
// query.RelationshipTimeline).
type RelationshipTimelineHTTPRequest struct {
	Timezone string          `json:"timezone,omitempty"`
	Filters  []ExploreFilter `json:"filters,omitempty"`
	Cursor   string          `json:"cursor,omitempty"`
	Limit    int             `json:"limit,omitzero" minimum:"0" maximum:"500"`
}

// RelationshipTimelineHTTPResponse echoes the canonical cluster ID the
// {id} path segment resolved to, plus both revisions the page was computed
// against so clients can detect archive or identity drift across pages.
type RelationshipTimelineHTTPResponse struct {
	CanonicalID      int64               `json:"canonical_id"`
	Rows             []query.TimelineRow `json:"rows"`
	TotalCount       int64               `json:"total_count"`
	CacheRevision    string              `json:"cache_revision"`
	IdentityRevision int64               `json:"identity_revision"`
	NextCursor       string              `json:"next_cursor,omitempty"`
}

func (s *Server) handleRelationshipTimeline(w http.ResponseWriter, r *http.Request) {
	participantID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || participantID < 1 {
		writeError(w, http.StatusBadRequest, "invalid_participant_id", "participant ID must be a positive integer")
		return
	}
	var request RelationshipTimelineHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	canonicalizeRelationshipFilters(request.Filters)
	analyticalContext, err := exploreContext(request.Filters)
	if err != nil {
		s.writeExploreFilterError(w, err, "invalid_filter")
		return
	}
	if err := s.resolveExploreIdentityContext(r.Context(), request.Filters, &analyticalContext); err != nil {
		s.writeExploreFilterError(w, err, "invalid_filter")
		return
	}
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreMaxLimit))
		return
	}

	analyzer, ok := s.queryEngineForContext(r.Context()).(query.RelationshipAnalyzer)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}
	canonicalID, err := analyzer.ResolveCanonicalParticipant(r.Context(), participantID)
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}

	canonical := request
	canonical.Cursor = ""
	requestHash := hashCanonicalValue(canonical, false)

	offset := 0
	if request.Cursor != "" {
		cursor, err := s.decodeExploreCursor(request.Cursor)
		if err != nil || cursor.Offset < 0 || cursor.Request != requestHash ||
			cursor.Timezone != request.Timezone || cursor.CanonicalID != canonicalID {
			writeError(w, http.StatusConflict, "cursor_invalidated", "The timeline context changed; restart pagination")
			return
		}
		offset = cursor.Offset
	}

	result, err := analyzer.RelationshipTimeline(r.Context(), query.RelationshipTimelineRequest{
		CanonicalID: canonicalID, Timezone: request.Timezone, Context: analyticalContext,
		Limit: request.Limit, Offset: offset,
	})
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}
	if request.Cursor != "" {
		cursor, _ := s.decodeExploreCursor(request.Cursor)
		if cursor.Revision != result.CacheRevision || cursor.IdentityRevision != result.IdentityRevision {
			writeError(w, http.StatusConflict, "cursor_invalidated", "The timeline context changed; restart pagination")
			return
		}
	}
	response := RelationshipTimelineHTTPResponse{
		CanonicalID: canonicalID, Rows: result.Rows, TotalCount: result.TotalCount,
		CacheRevision: result.CacheRevision, IdentityRevision: result.IdentityRevision,
	}
	if next := offset + len(result.Rows); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision, IdentityRevision: result.IdentityRevision,
			Timezone: request.Timezone, CanonicalID: canonicalID,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func canonicalizeRelationshipFilters(filters []ExploreFilter) {
	canonicalizeExploreFilters(filters)
}
