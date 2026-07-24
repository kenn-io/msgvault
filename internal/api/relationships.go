package api

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
)

// RelationshipsHTTPRequest scopes and pages a relationship ranking request.
// Filters follow the same predicate shape as /explore; there is no text
// query because ranking is over reciprocity signals, not lexical/semantic
// search candidates.
type RelationshipsHTTPRequest struct {
	Filters []ExploreFilter `json:"filters,omitempty"`
	ShowAll bool            `json:"show_all,omitempty"`
	Cursor  string          `json:"cursor,omitempty"`
	Limit   int             `json:"limit,omitempty" minimum:"0" maximum:"500"`
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

func (s *Server) registerRelationshipRoutes(api huma.API) {
	registerExploreRoute[RelationshipsHTTPRequest, RelationshipsHTTPResponse](
		api, "listRelationships", "/relationships", "Rank counterparts by reciprocity-weighted interaction", s.handleRelationships,
	)
	registerExploreRoute[RelationshipTimelineHTTPRequest, RelationshipTimelineHTTPResponse](
		api, "getRelationshipTimeline", "/relationships/{id}/timeline",
		"Get one counterpart's interaction timeline, with chat grouped into local-day bursts", s.handleRelationshipTimeline,
	)
}

func (s *Server) handleRelationships(w http.ResponseWriter, r *http.Request) {
	var request RelationshipsHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return
	}
	canonicalizeRelationshipFilters(request.Filters)
	analyticalContext, err := exploreContext(request.Filters)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
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
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
	}
	decayDate, ok := s.relationshipDecayDate(w, cursor)
	if !ok {
		return
	}

	analyzer, ok := s.engine.(query.RelationshipAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.Relationships(r.Context(), query.RelationshipsRequest{
		Context: analyticalContext, ShowAll: request.ShowAll, Limit: request.Limit, Offset: offset, Now: decayDate,
	})
	if err != nil {
		s.writeExploreError(w, err)
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
	}
	response := RelationshipsHTTPResponse{
		Rows: result.Rows, TotalCount: result.TotalCount,
		CacheRevision: result.CacheRevision, IdentityRevision: result.IdentityRevision,
	}
	if next := offset + len(result.Rows); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{
			Offset: next, Request: requestHash, Revision: result.CacheRevision, IdentityRevision: result.IdentityRevision,
			DecayDate: decayDate.Format(time.DateOnly),
		})
	}
	writeJSON(w, http.StatusOK, response)
}

// relationshipDecayDate returns the UTC decay date every page of one
// relationship listing must rank with: midnight UTC of the current date for a
// first page, or the date pinned in the cursor for subsequent pages, so
// pagination crossing UTC midnight cannot re-rank rows mid-listing. Decay and
// the ranking memo key depend only on the UTC date of the timestamp (see
// buildRelationshipsSQL and relationshipsMemoKey in internal/query), so
// midnight is equivalent to any instant on the same date, and pinned pages
// share the first page's memoized candidate list. Cursors minted before the
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
	Limit    int             `json:"limit,omitempty" minimum:"0" maximum:"500"`
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
		writeError(w, http.StatusBadRequest, "invalid_filter", err.Error())
		return
	}
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreMaxLimit))
		return
	}

	analyzer, ok := s.engine.(query.RelationshipAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	canonicalID, err := analyzer.ResolveCanonicalParticipant(r.Context(), participantID)
	if err != nil {
		s.writeExploreError(w, err)
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
		s.writeExploreError(w, err)
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
	for i := range filters {
		filters[i].Dimension = strings.ToLower(strings.TrimSpace(filters[i].Dimension))
		for j := range filters[i].Values {
			filters[i].Values[j] = strings.TrimSpace(filters[i].Values[j])
		}
		slices.Sort(filters[i].Values)
		filters[i].Values = slices.Compact(filters[i].Values)
	}
	slices.SortFunc(filters, func(a, b ExploreFilter) int { return strings.Compare(a.Dimension, b.Dimension) })
}
