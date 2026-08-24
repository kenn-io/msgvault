package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

type PersonCompletionStore interface {
	CompletePersonProfilesContext(
		ctx context.Context, request store.PersonCompletionQuery,
	) ([]store.PersonCompletion, error)
}

type ParticipantCompletionHTTPRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty" minimum:"0" maximum:"20"`
}

type ParticipantCompletionHTTPRow struct {
	ParticipantID int64                      `json:"participant_id"`
	DisplayLabel  string                     `json:"display_label"`
	Kind          query.PeopleCompletionKind `json:"kind" enum:"name,phone,email,username,impp,organization,title,role"`
	Value         string                     `json:"value"`
	Source        string                     `json:"source"`
}

type ParticipantCompletionHTTPResponse struct {
	Rows          []ParticipantCompletionHTTPRow `json:"rows"`
	CacheRevision string                         `json:"cache_revision"`
}

func (s *Server) registerParticipantCompletionRoute(api huma.API) {
	op := rawAPIV1Operation(
		"completeParticipants", http.MethodPost, "/participants/completions",
		"Complete observed people by typed contact primitives",
	)
	op.Description = "Returns a bounded, typed completion set from the committed observed-person index and current curated profile primitives. The private query stays in the JSON body."
	op.RequestBody = jsonRequestBodyFor[ParticipantCompletionHTTPRequest](api)
	op.Responses = jsonResponsesFor[ParticipantCompletionHTTPResponse](api)
	addErrorResponses(api, op.Responses, http.StatusBadRequest,
		http.StatusInternalServerError, http.StatusServiceUnavailable)
	registerRawHumaRoute(api, op, s.handleCompleteParticipants)
}

func (s *Server) handleCompleteParticipants(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var request ParticipantCompletionHTTPRequest
	if !decodeEntityRequest(w, r, &request, "people completion") {
		return
	}
	queryText := strings.TrimSpace(request.Query)
	if queryText == "" {
		writeError(w, http.StatusBadRequest, "invalid_completion_query",
			"Completion query must not be blank")
		return
	}
	if utf8.RuneCountInString(queryText) > query.MaxPeopleCompletionQueryRunes {
		writeError(w, http.StatusBadRequest, "invalid_completion_query",
			fmt.Sprintf("Completion query must not exceed %d characters",
				query.MaxPeopleCompletionQueryRunes))
		return
	}
	limit := request.Limit
	if limit == 0 {
		limit = query.DefaultPeopleCompletionLimit
	}
	if limit < 1 || limit > query.MaxPeopleCompletionLimit {
		writeError(w, http.StatusBadRequest, "invalid_completion_limit",
			fmt.Sprintf("Completion limit must be between 1 and %d",
				query.MaxPeopleCompletionLimit))
		return
	}

	completer, ok := s.queryEngineForContext(r.Context()).(query.PeopleCompleter)
	if !ok {
		s.writeExploreUnavailable(r.Context(), w, query.CacheAbsent)
		return
	}
	profiles, ok := s.store.(PersonCompletionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "people_completion_unavailable",
			"Curated people completion is unavailable")
		return
	}
	observed, err := completer.CompletePeople(r.Context(), query.PeopleCompletionRequest{
		Query: queryText, Limit: query.MaxPeopleCompletionLimit,
	})
	if err != nil {
		s.writeExploreError(r.Context(), w, err)
		return
	}
	curated, err := profiles.CompletePersonProfilesContext(r.Context(), store.PersonCompletionQuery{
		Query: queryText, Limit: store.MaxPersonCompletionLimit,
	})
	if err != nil {
		if s.writeIfContextError(w, err) {
			return
		}
		s.logger.Error("complete curated people failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Could not complete curated people")
		return
	}
	if observed == nil {
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Observed people completion returned no response")
		return
	}

	merged, err := s.mergeParticipantCompletions(queryText, observed.Rows, curated)
	if err != nil {
		s.logger.Error("merge people completions failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error",
			"Could not merge people completions")
		return
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	response := ParticipantCompletionHTTPResponse{
		Rows:          make([]ParticipantCompletionHTTPRow, len(merged)),
		CacheRevision: observed.CacheRevision,
	}
	for i, row := range merged {
		response.Rows[i] = ParticipantCompletionHTTPRow{
			ParticipantID: row.ParticipantID, DisplayLabel: row.DisplayLabel,
			Kind: row.Kind, Value: row.Value, Source: row.Source,
		}
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) mergeParticipantCompletions(
	queryText string, observed []query.PeopleCompletion, curated []store.PersonCompletion,
) ([]query.PeopleCompletion, error) {
	canonical := make(map[int64]int64)
	canonicalID := func(id int64) int64 {
		if cached, ok := canonical[id]; ok {
			return cached
		}
		resolved := id
		if members := s.clusterMemberIDs(id); len(members) > 0 {
			resolved = members[0]
			for _, member := range members {
				canonical[member] = resolved
			}
		}
		canonical[id] = resolved
		return resolved
	}
	type completionKey struct {
		participantID int64
		kind          query.PeopleCompletionKind
		matchValue    string
	}
	byKey := make(map[completionKey]query.PeopleCompletion,
		len(observed)+len(curated))
	for _, row := range observed {
		if row.ParticipantID <= 0 || !row.Kind.Valid() {
			return nil, fmt.Errorf("invalid observed completion target=%d kind=%q",
				row.ParticipantID, row.Kind)
		}
		row.ParticipantID = canonicalID(row.ParticipantID)
		row.MatchValue = query.NormalizePeopleCompletionText(row.MatchValue)
		if _, matches := query.PeopleCompletionMatchRank(
			row.Kind, queryText, row.MatchValue); !matches {
			continue
		}
		key := completionKey{row.ParticipantID, row.Kind, row.MatchValue}
		if current, exists := byKey[key]; !exists || preferCompletion(row, current) {
			byKey[key] = row
		}
	}
	for _, profile := range curated {
		kind := query.PeopleCompletionKind(profile.Kind)
		if profile.ParticipantID <= 0 || !kind.Valid() {
			return nil, fmt.Errorf("invalid curated completion target=%d kind=%q",
				profile.ParticipantID, profile.Kind)
		}
		row := query.PeopleCompletion{
			ParticipantID: canonicalID(profile.ParticipantID),
			DisplayLabel:  profile.DisplayLabel, Kind: kind, Value: profile.Value,
			MatchValue: query.NormalizePeopleCompletionText(profile.MatchValue),
			Source:     profile.Source,
		}
		if _, matches := query.PeopleCompletionMatchRank(
			row.Kind, queryText, row.MatchValue); !matches {
			continue
		}
		key := completionKey{row.ParticipantID, row.Kind, row.MatchValue}
		if current, exists := byKey[key]; exists {
			current.DisplayLabel = row.DisplayLabel
			current.Value = row.Value
			current.Source = row.Source
			byKey[key] = current
		} else {
			byKey[key] = row
		}
	}
	rows := make([]query.PeopleCompletion, 0, len(byKey))
	for _, row := range byKey {
		rows = append(rows, row)
	}
	query.SortPeopleCompletions(rows, queryText)
	return rows, nil
}

func preferCompletion(candidate, current query.PeopleCompletion) bool {
	return slices.CompareFunc(
		[]string{query.NormalizePeopleCompletionText(candidate.DisplayLabel),
			query.NormalizePeopleCompletionText(candidate.Value), candidate.Source},
		[]string{query.NormalizePeopleCompletionText(current.DisplayLabel),
			query.NormalizePeopleCompletionText(current.Value), current.Source},
		cmp.Compare[string],
	) < 0
}
