package api

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/query"
)

type IdentitySearchSort struct {
	Field     string `json:"field" enum:"activity_count,latest_at,display_label"`
	Direction string `json:"direction" enum:"asc,desc"`
}

type IdentitySearchHTTPRequest struct {
	Predicate     ExploreHTTPRequest `json:"predicate"`
	IdentityQuery string             `json:"identity_query,omitempty"`
	Sort          IdentitySearchSort `json:"sort"`
	Cursor        string             `json:"cursor,omitempty"`
	Limit         int                `json:"limit,omitempty" minimum:"0" maximum:"500"`
}

type PersonSearchHTTPResponse struct {
	Rows                []query.PersonSummary  `json:"rows"`
	TotalCount          int64                  `json:"total_count"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	NextCursor          string                 `json:"next_cursor,omitempty"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type DomainSearchHTTPResponse struct {
	Rows                []query.DomainSummary  `json:"rows"`
	TotalCount          int64                  `json:"total_count"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	NextCursor          string                 `json:"next_cursor,omitempty"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type PersonContextSummaryHTTPResponse struct {
	Summary             query.PersonSummary    `json:"summary"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type DomainContextSummaryHTTPResponse struct {
	Summary             query.DomainSummary    `json:"summary"`
	CacheRevision       string                 `json:"cache_revision"`
	SearchProvenance    query.SearchProvenance `json:"search_provenance"`
	CandidateSnapshotID string                 `json:"candidate_snapshot_id,omitempty"`
}

type identitySearchPrepared struct {
	predicate   explorePrepared
	sort        query.SortSpec
	offset      int
	requestHash string
	cursor      exploreCursor
	search      query.SearchSpec
	snapshotID  string
}

var domainFactPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)

func (s *Server) registerPeopleRoutes(api huma.API) {
	registerExploreRoute[IdentitySearchHTTPRequest, PersonSearchHTTPResponse](
		api, "searchPeople", "/people/search", "Search analytical people", s.handleSearchPeople,
	)
	registerAPIV1RawHumaJSONRoute[query.PersonSummary](
		api, "getPerson", http.MethodGet, "/people/{id}", "Get one analytical person", s.handleGetPerson,
	)
	registerExploreRoute[ExploreHTTPRequest, PersonContextSummaryHTTPResponse](
		api, "getPersonContextSummary", "/people/{id}/summary", "Get one person's contextual analytical summary", s.handlePersonContextSummary,
	)
	registerExploreRoute[ExploreHTTPRequest, ExploreHTTPResponse](
		api, "getPersonTimeline", "/people/{id}/timeline", "Get one person's canonical activity timeline", s.handlePersonTimeline,
	)
	registerExploreRoute[IdentitySearchHTTPRequest, DomainSearchHTTPResponse](
		api, "searchDomains", "/domains/search", "Search analytical domains", s.handleSearchDomains,
	)
	registerAPIV1RawHumaJSONRoute[query.DomainSummary](
		api, "getDomain", http.MethodGet, "/domains/{domain}", "Get one analytical domain", s.handleGetDomain,
	)
	registerExploreRoute[ExploreHTTPRequest, DomainContextSummaryHTTPResponse](
		api, "getDomainContextSummary", "/domains/{domain}/summary", "Get one domain's contextual analytical summary", s.handleDomainContextSummary,
	)
	registerExploreRoute[ExploreHTTPRequest, ExploreHTTPResponse](
		api, "getDomainTimeline", "/domains/{domain}/timeline", "Get one domain's canonical activity timeline", s.handleDomainTimeline,
	)
}

func (s *Server) handleSearchPeople(w http.ResponseWriter, r *http.Request) {
	var request IdentitySearchHTTPRequest
	prepared, ok := s.prepareIdentitySearch(w, r, &request)
	if !ok {
		return
	}
	analyzer, ok := s.engine.(query.PeopleAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.SearchPeople(r.Context(), query.PersonSearchRequest{
		Explore: query.ExploreRequest{Context: prepared.predicate.query.Context, Search: prepared.search},
		Query:   strings.TrimSpace(request.IdentityQuery), Sort: prepared.sort,
		Page: query.PageSpec{Limit: request.Limit, Offset: prepared.offset},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if request.Cursor != "" && prepared.cursor.Revision != result.CacheRevision {
		writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
		return
	}
	response := PersonSearchHTTPResponse{Rows: result.Rows, TotalCount: result.TotalCount, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: prepared.snapshotID}
	if next := prepared.offset + len(result.Rows); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{Offset: next, Request: prepared.requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(prepared.search), Snapshot: prepared.snapshotID})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleSearchDomains(w http.ResponseWriter, r *http.Request) {
	var request IdentitySearchHTTPRequest
	prepared, ok := s.prepareIdentitySearch(w, r, &request)
	if !ok {
		return
	}
	analyzer, ok := s.engine.(query.PeopleAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.SearchDomains(r.Context(), query.DomainSearchRequest{
		Explore: query.ExploreRequest{Context: prepared.predicate.query.Context, Search: prepared.search},
		Query:   strings.TrimSpace(request.IdentityQuery), Sort: prepared.sort,
		Page: query.PageSpec{Limit: request.Limit, Offset: prepared.offset},
	})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if request.Cursor != "" && prepared.cursor.Revision != result.CacheRevision {
		writeError(w, http.StatusConflict, "archive_revision_changed", "The committed analytical cache changed; restart pagination")
		return
	}
	response := DomainSearchHTTPResponse{Rows: result.Rows, TotalCount: result.TotalCount, CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: prepared.snapshotID}
	if next := prepared.offset + len(result.Rows); next < int(result.TotalCount) {
		response.NextCursor = s.encodeExploreCursor(exploreCursor{Offset: next, Request: prepared.requestHash, Revision: result.CacheRevision,
			SearchRevision: exploreResolvedSearchRevision(prepared.search), Snapshot: prepared.snapshotID})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) prepareIdentitySearch(w http.ResponseWriter, r *http.Request, request *IdentitySearchHTTPRequest) (identitySearchPrepared, bool) {
	if !decodeExploreJSON(w, r, request) {
		return identitySearchPrepared{}, false
	}
	if len(request.Predicate.Grouping) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_identity_predicate", "identity search does not accept grouping")
		return identitySearchPrepared{}, false
	}
	request.Predicate.Cursor = ""
	prepared, err := prepareExplorePredicate(request.Predicate)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_identity_predicate", err.Error())
		return identitySearchPrepared{}, false
	}
	if request.Limit == 0 {
		request.Limit = exploreDefaultLimit
	}
	if request.Limit < 1 || request.Limit > exploreMaxLimit {
		writeError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", exploreMaxLimit))
		return identitySearchPrepared{}, false
	}
	request.IdentityQuery = strings.TrimSpace(request.IdentityQuery)
	request.Sort.Field = strings.ToLower(strings.TrimSpace(request.Sort.Field))
	request.Sort.Direction = strings.ToLower(strings.TrimSpace(request.Sort.Direction))
	if request.Sort.Field == "" {
		request.Sort = IdentitySearchSort{Field: "activity_count", Direction: apiSortDirectionDesc}
	}
	if !slices.Contains([]string{"activity_count", "latest_at", "display_label"}, request.Sort.Field) ||
		!slices.Contains([]string{"asc", apiSortDirectionDesc}, request.Sort.Direction) {
		writeError(w, http.StatusBadRequest, "invalid_sort", "unknown identity sort field or direction")
		return identitySearchPrepared{}, false
	}
	request.Predicate = prepared.request
	canonical := *request
	canonical.Cursor = ""
	requestHash := hashCanonicalValue(canonical, false)
	offset, ok := s.parseExploreCursor(w, request.Cursor, requestHash)
	if !ok {
		return identitySearchPrepared{}, false
	}
	var cursor exploreCursor
	searchRequest := prepared.request
	if request.Cursor != "" {
		cursor, _ = s.decodeExploreCursor(request.Cursor)
		if searchRequest.SearchMode == exploreSearchModeSemantic || searchRequest.SearchMode == exploreSearchModeHybrid {
			if cursor.Snapshot == "" {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "semantic cursor is missing its candidate snapshot")
				return identitySearchPrepared{}, false
			}
			searchRequest.CandidateSnapshotID = cursor.Snapshot
		}
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, searchRequest)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return identitySearchPrepared{}, false
	}
	if request.Cursor != "" && cursor.SearchRevision != exploreResolvedSearchRevision(searchSpec) {
		writeError(w, http.StatusConflict, "search_revision_changed", "The resolved search index revision changed; restart pagination")
		return identitySearchPrepared{}, false
	}
	return identitySearchPrepared{predicate: prepared, sort: query.SortSpec{Field: request.Sort.Field, Direction: request.Sort.Direction},
		offset: offset, requestHash: requestHash, cursor: cursor, search: searchSpec, snapshotID: snapshotID}, true
}

func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request) {
	id, ok := positivePersonPathID(w, r)
	if !ok {
		return
	}
	analyzer, ok := s.engine.(query.PeopleAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	members := s.clusterMemberIDs(id)
	person, err := analyzer.GetPerson(r.Context(), id, query.Context{}, members)
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if person == nil {
		writeError(w, http.StatusNotFound, "person_not_found", "Person not found")
		return
	}
	s.attachPersonCluster(person, id, members)
	writeJSON(w, http.StatusOK, person)
}

// clusterMemberIDs returns id's sorted cluster member IDs, or nil if the
// store has no ClusterLookupStore capability, the lookup fails, or id is
// unlinked (fewer than two members) — in every one of those cases the
// caller (person detail, person-scoped files search) stays scoped to id
// alone, matching pre-cluster-aware behavior. Errors are swallowed here
// rather than failing the request: an unavailable cluster lookup must
// degrade to unlinked, not break the whole endpoint.
func (s *Server) clusterMemberIDs(id int64) []int64 {
	lookup, ok := s.store.(ClusterLookupStore)
	if !ok {
		return nil
	}
	members, err := lookup.ClusterMembers(id)
	if err != nil || len(members) < 2 {
		return nil
	}
	return members
}

// attachPersonCluster sets person.Cluster from the store's edge graph when
// id is linked (members has ≥2 entries, as returned by clusterMemberIDs).
// Canonical is members[0]: ClusterMembers returns members sorted ascending,
// and the store's link-graph convention roots every cluster at its smallest
// member ID (see ParticipantClusters/rebuildClusterAsStar in
// internal/store/participant_links.go).
func (s *Server) attachPersonCluster(person *query.PersonSummary, id int64, members []int64) {
	if len(members) < 2 {
		return
	}
	lookup, ok := s.store.(ClusterLookupStore)
	if !ok {
		return
	}
	edges, err := lookup.ClusterEdges(id)
	if err != nil {
		s.logger.Error("cluster edges lookup failed", "error", err, "participant_id", id)
		return
	}
	clusterEdges := make([]query.PersonClusterEdge, 0, len(edges))
	for _, e := range edges {
		clusterEdges = append(clusterEdges, query.PersonClusterEdge{ParticipantA: e.A, ParticipantB: e.B})
	}
	person.Cluster = &query.PersonCluster{CanonicalID: members[0], MemberIDs: members, Edges: clusterEdges}
}

func (s *Server) handlePersonContextSummary(w http.ResponseWriter, r *http.Request) {
	id, ok := positivePersonPathID(w, r)
	if !ok {
		return
	}
	explore, snapshotID, ok := s.prepareIdentitySummary(w, r)
	if !ok {
		return
	}
	analyzer, ok := s.engine.(query.PeopleAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.GetPersonSummary(r.Context(), id, explore)
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if result == nil || len(result.Rows) == 0 {
		writeError(w, http.StatusNotFound, "person_not_found", "Person not found in the active analytical context")
		return
	}
	writeJSON(w, http.StatusOK, PersonContextSummaryHTTPResponse{Summary: result.Rows[0], CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: snapshotID})
}

func (s *Server) handleGetDomain(w http.ResponseWriter, r *http.Request) {
	domain, ok := domainPathFactSuffix(w, r, "")
	if !ok {
		return
	}
	analyzer, ok := s.engine.(query.PeopleAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.GetDomain(r.Context(), domain, query.Context{})
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if result == nil {
		writeError(w, http.StatusNotFound, "domain_not_found", "Domain not found")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleDomainContextSummary(w http.ResponseWriter, r *http.Request) {
	domain, ok := domainPathFactSuffix(w, r, "/summary")
	if !ok {
		return
	}
	explore, snapshotID, ok := s.prepareIdentitySummary(w, r)
	if !ok {
		return
	}
	analyzer, ok := s.engine.(query.PeopleAnalyzer)
	if !ok {
		writeExploreUnavailable(w, query.CacheAbsent)
		return
	}
	result, err := analyzer.GetDomainSummary(r.Context(), domain, explore)
	if err != nil {
		s.writeExploreError(w, err)
		return
	}
	if result == nil || len(result.Rows) == 0 {
		writeError(w, http.StatusNotFound, "domain_not_found", "Domain not found in the active analytical context")
		return
	}
	writeJSON(w, http.StatusOK, DomainContextSummaryHTTPResponse{Summary: result.Rows[0], CacheRevision: result.CacheRevision,
		SearchProvenance: result.SearchProvenance, CandidateSnapshotID: snapshotID})
}

func (s *Server) prepareIdentitySummary(w http.ResponseWriter, r *http.Request) (query.ExploreRequest, string, bool) {
	var request ExploreHTTPRequest
	if !decodeExploreJSON(w, r, &request) {
		return query.ExploreRequest{}, "", false
	}
	if len(request.Grouping) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_identity_predicate", "identity summary does not accept grouping")
		return query.ExploreRequest{}, "", false
	}
	prepared, err := prepareExplorePredicate(request)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_identity_predicate", err.Error())
		return query.ExploreRequest{}, "", false
	}
	searchSpec, snapshotID, ok := s.resolveExploreSearch(r.Context(), w, prepared.request)
	if !ok || !requireCompleteCandidatePool(w, searchSpec) {
		return query.ExploreRequest{}, "", false
	}
	return query.ExploreRequest{Context: prepared.query.Context, Search: searchSpec}, snapshotID, true
}

func (s *Server) handlePersonTimeline(w http.ResponseWriter, r *http.Request) {
	id, ok := positivePersonPathID(w, r)
	if !ok {
		return
	}
	s.forwardIdentityTimeline(w, r, ExploreFilter{Dimension: "participant", Values: []string{strconv.FormatInt(id, 10)}})
}

func (s *Server) handleDomainTimeline(w http.ResponseWriter, r *http.Request) {
	domain, ok := domainPathFact(w, r, true)
	if !ok {
		return
	}
	s.forwardIdentityTimeline(w, r, ExploreFilter{Dimension: "domain", Values: []string{domain}})
}

func (s *Server) forwardIdentityTimeline(w http.ResponseWriter, r *http.Request, exact ExploreFilter) {
	s.handleExploreWithScope(w, r, &exact)
}

func positivePersonPathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := strings.TrimPrefix(r.URL.Path, "/api/v1/people/")
	raw = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(raw, "/files/search"), "/timeline"), "/summary")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "invalid_person_id", "person ID must be a positive integer")
		return 0, false
	}
	return id, true
}

func domainPathFact(w http.ResponseWriter, r *http.Request, timeline bool) (string, bool) {
	suffix := ""
	if timeline {
		suffix = "/timeline"
	}
	return domainPathFactSuffix(w, r, suffix)
}

func domainPathFactSuffix(w http.ResponseWriter, r *http.Request, suffix string) (string, bool) {
	raw := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/domains/"), suffix)
	domain := strings.ToLower(strings.TrimSpace(raw))
	if len(domain) == 0 || len(domain) > 253 || !domainFactPattern.MatchString(domain) {
		writeError(w, http.StatusBadRequest, "invalid_domain", "domain must be an exact normalized domain fact")
		return "", false
	}
	return domain, true
}
