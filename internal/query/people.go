package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxPeopleSearchLimit = 500

// PersonIdentifier is explicit stored identity evidence. Provenance names the
// canonical read model; it does not imply that two values are interchangeable.
// ParticipantID names which raw participant this evidence was stored against
// — for a linked cluster's person detail, identifiers span every member, so
// this is the only field that tells the caller which chip belongs to which
// member (see PersonSummary.Cluster).
type PersonIdentifier struct {
	Type          string `json:"type"`
	Value         string `json:"value"`
	DisplayValue  string `json:"display_value,omitempty"`
	IsPrimary     bool   `json:"is_primary"`
	Provenance    string `json:"provenance"`
	ParticipantID int64  `json:"participant_id"`
}

// PersonClusterEdge is one participant_links edge within a person's cluster,
// as recorded by the store (the query layer has no edge data of its own —
// see internal/store/participant_links.go's LinkEdge).
type PersonClusterEdge struct {
	ParticipantA int64 `json:"participant_a"`
	ParticipantB int64 `json:"participant_b"`
}

// PersonCluster describes the identity-link cluster a person detail belongs
// to. CanonicalID is the cluster's smallest member ID, matching the store's
// link-graph convention (see ParticipantClusters/rebuildClusterAsStar in
// internal/store/participant_links.go). Populated only when the requested
// participant is linked to at least one other participant; a nil Cluster on
// PersonSummary means the participant is unlinked.
type PersonCluster struct {
	CanonicalID int64               `json:"canonical_id"`
	MemberIDs   []int64             `json:"member_ids"`
	Edges       []PersonClusterEdge `json:"edges"`
}

type SourceCount struct {
	SourceType string `json:"source_type"`
	Count      int64  `json:"count"`
}

type PersonSearchRequest struct {
	Explore ExploreRequest `json:"explore"`
	Query   string         `json:"query,omitempty"`
	Sort    SortSpec       `json:"sort"`
	Page    PageSpec       `json:"page"`
}

type PersonSummary struct {
	ID            int64              `json:"id"`
	DisplayLabel  string             `json:"display_label"`
	DisplayName   string             `json:"display_name,omitempty"`
	PartialLabel  bool               `json:"partial_label"`
	Identifiers   []PersonIdentifier `json:"identifiers"`
	ActivityCount int64              `json:"activity_count"`
	FileCount     int64              `json:"file_count"`
	SourceCounts  []SourceCount      `json:"source_counts"`
	FirstAt       time.Time          `json:"first_at"`
	LastAt        time.Time          `json:"last_at"`
	CacheRevision string             `json:"cache_revision"`
	Cluster       *PersonCluster     `json:"cluster,omitempty"`
}

type PersonSearchResponse struct {
	Rows             []PersonSummary  `json:"rows"`
	TotalCount       int64            `json:"total_count"`
	CacheRevision    string           `json:"cache_revision"`
	SearchProvenance SearchProvenance `json:"search_provenance"`
}

type DomainSearchRequest struct {
	Explore ExploreRequest `json:"explore"`
	Query   string         `json:"query,omitempty"`
	Sort    SortSpec       `json:"sort"`
	Page    PageSpec       `json:"page"`
}

type DomainSummary struct {
	Domain        string        `json:"domain"`
	ActivityCount int64         `json:"activity_count"`
	PersonCount   int64         `json:"person_count"`
	FileCount     int64         `json:"file_count"`
	SourceCounts  []SourceCount `json:"source_counts"`
	FirstAt       time.Time     `json:"first_at"`
	LastAt        time.Time     `json:"last_at"`
	CacheRevision string        `json:"cache_revision"`
}

type DomainSearchResponse struct {
	Rows             []DomainSummary  `json:"rows"`
	TotalCount       int64            `json:"total_count"`
	CacheRevision    string           `json:"cache_revision"`
	SearchProvenance SearchProvenance `json:"search_provenance"`
}

func (e *DuckDBEngine) SearchPeople(ctx context.Context, request PersonSearchRequest) (*PersonSearchResponse, error) {
	return e.searchPeople(ctx, request, nil, nil)
}

// GetPerson returns one participant's analytical summary. clusterMemberIDs,
// when it has two or more entries, widens the returned Identifiers to span
// every listed participant (each tagged with its own ParticipantID) instead
// of just id — the caller (the person-detail HTTP handler) resolves cluster
// membership from the store and passes it in, keeping this query layer free
// of a store dependency. A nil/single-element list leaves Identifiers scoped
// to id alone, matching the pre-cluster-aware behavior.
func (e *DuckDBEngine) GetPerson(ctx context.Context, id int64, analyticalContext Context, clusterMemberIDs []int64) (*PersonSummary, error) {
	if id < 1 {
		return nil, fmt.Errorf("%w: person ID must be positive", ErrInvalidExploreRequest)
	}
	result, err := e.searchPeople(ctx, PersonSearchRequest{Explore: ExploreRequest{Context: analyticalContext}, Page: PageSpec{Limit: 1}}, &id, clusterMemberIDs)
	if err != nil || len(result.Rows) == 0 {
		return nil, err
	}
	return &result.Rows[0], nil
}

// GetPersonSummary returns contextual metrics for one durable identity. Unlike
// GetPerson, its population is the exact resolved canonical Explore predicate.
func (e *DuckDBEngine) GetPersonSummary(ctx context.Context, id int64, explore ExploreRequest) (*PersonSearchResponse, error) {
	if id < 1 {
		return nil, fmt.Errorf("%w: person ID must be positive", ErrInvalidExploreRequest)
	}
	return e.searchPeople(ctx, PersonSearchRequest{Explore: explore, Page: PageSpec{Limit: 1}}, &id, nil)
}

func (e *DuckDBEngine) searchPeople(
	ctx context.Context, request PersonSearchRequest, exactID *int64, identifierScopeIDs []int64,
) (*PersonSearchResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateIdentityRequest(request.Explore.Context, request.Page); err != nil {
		return nil, err
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
	}
	order, err := identitySearchOrder(request.Sort, "display_label", "person_id")
	if err != nil {
		return nil, err
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	conditions, args := buildExploreConditions(request.Explore)
	entriesCTE, entryArgs := personEntriesCTE(exactID, conditions)
	args = append(args, entryArgs...)
	personWhere := []string{"true"}
	if exactID != nil {
		personWhere = append(personWhere, "person_id = ?")
		args = append(args, *exactID)
	}
	if searchText := strings.TrimSpace(request.Query); searchText != "" {
		personWhere = append(personWhere, `(contains(lower(display_name), lower(?))
			OR contains(lower(email_address), lower(?)) OR contains(lower(phone_number), lower(?))
			OR EXISTS (SELECT 1 FROM participant_identifiers pi
				WHERE pi.participant_id = person_id AND (contains(lower(pi.identifier_value), lower(?))
					OR contains(lower(pi.display_value), lower(?)))))`)
		args = append(args, searchText, searchText, searchText, searchText, searchText)
	}
	// identifierFilter scopes the per-row identifiers subquery below. Absent
	// a caller-supplied cluster (identifierScopeIDs has fewer than two
	// entries — the common listing/search case), it stays scoped to the
	// row's own person_id. GetPerson passes every cluster member so a
	// linked participant's identifiers span the whole cluster instead of
	// just the requested ID.
	identifierFilter := "pi.participant_id = counted.person_id"
	if len(identifierScopeIDs) > 1 {
		placeholders := make([]string, len(identifierScopeIDs))
		for i, memberID := range identifierScopeIDs {
			placeholders[i] = "?"
			args = append(args, memberID)
		}
		identifierFilter = "pi.participant_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	args = append(args, limit, request.Page.Offset)
	queryText := buildExploreLogicalSQL(conditions) + entriesCTE + `
), person_population AS (
	SELECT p.id AS person_id, COALESCE(p.display_name, '') AS display_name,
		COALESCE(p.email_address, '') AS email_address, COALESCE(p.phone_number, '') AS phone_number,
		COALESCE(NULLIF(TRIM(p.display_name), ''), NULLIF(TRIM(p.phone_number), ''),
			NULLIF(TRIM(p.email_address), ''),
			(SELECT COALESCE(NULLIF(TRIM(pi.display_value), ''), pi.identifier_value)
			 FROM participant_identifiers pi WHERE pi.participant_id = p.id
			 ORDER BY pi.is_primary DESC, pi.identifier_type, pi.identifier_value LIMIT 1),
			'Unknown person #' || CAST(p.id AS VARCHAR)) AS display_label,
		COUNT(*)::BIGINT AS activity_count, COALESCE(SUM(pe.attachment_count), 0)::BIGINT AS file_count,
		MIN(pe.occurred_at) AS first_at, MAX(pe.occurred_at) AS last_at
	FROM person_entries pe JOIN participants p ON p.id = pe.person_id
	GROUP BY p.id, p.display_name, p.email_address, p.phone_number
), filtered_people AS (
	SELECT * FROM person_population WHERE ` + strings.Join(personWhere, " AND ") + `
), counted AS (
	SELECT *, COUNT(*) OVER () AS total_count FROM filtered_people
)
SELECT person_id, display_label, display_name,
	(TRIM(display_name) = '') AS partial_label,
	COALESCE(CAST((SELECT to_json(list(struct_pack(
		type := pi.identifier_type, value := pi.identifier_value, display_value := pi.display_value,
		is_primary := pi.is_primary, provenance := 'participant_identifiers', participant_id := pi.participant_id)
		ORDER BY pi.is_primary DESC, pi.identifier_type, pi.identifier_value))
		FROM participant_identifiers pi WHERE ` + identifierFilter + `) AS VARCHAR), '[]'),
	activity_count, file_count,
	COALESCE(CAST((SELECT to_json(list(struct_pack(source_type := source_type, count := source_count)
		ORDER BY source_type)) FROM (SELECT source_type, COUNT(*)::BIGINT AS source_count
		FROM person_entries pe WHERE pe.person_id = counted.person_id GROUP BY source_type)) AS VARCHAR), '[]'),
	first_at, last_at, total_count
FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("search analytical people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &PersonSearchResponse{Rows: make([]PersonSummary, 0), CacheRevision: state.Revision(), SearchProvenance: provenance}
	for rows.Next() {
		var row PersonSummary
		var identifiersJSON, sourceCountsJSON string
		if err := rows.Scan(&row.ID, &row.DisplayLabel, &row.DisplayName, &row.PartialLabel,
			&identifiersJSON, &row.ActivityCount, &row.FileCount, &sourceCountsJSON,
			&row.FirstAt, &row.LastAt, &response.TotalCount); err != nil {
			return nil, fmt.Errorf("scan analytical person: %w", err)
		}
		if err := json.Unmarshal([]byte(identifiersJSON), &row.Identifiers); err != nil {
			return nil, fmt.Errorf("decode person identifiers: %w", err)
		}
		if err := json.Unmarshal([]byte(sourceCountsJSON), &row.SourceCounts); err != nil {
			return nil, fmt.Errorf("decode person source counts: %w", err)
		}
		row.CacheRevision = state.Revision()
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical people: %w", err)
	}
	return response, nil
}

// personEntriesCTE returns the person_entries CTE (appended directly after
// buildExploreLogicalSQL, so it opens by closing the logical_entries CTE)
// plus the parameter values it binds. person_entries always projects exactly
// the columns the aggregates in searchPeople consume: it is referenced more
// than once, so DuckDB materializes it, and carrying logical_entries.* would
// materialize every wide list/text column per row.
//
// Three shapes, cheapest applicable first:
//
//  1. Exact-ID lookup with no analytical conditions (the person-detail
//     endpoint): membership is resolved with semi-joins against the raw
//     link tables, never touching logical_entries.participant_ids —
//     unnesting that column forces the analytical_entries view to assemble
//     per-message participant lists for the entire archive, which dominated
//     person-detail latency. Equivalence with the fan-out shape: a non-chat
//     entry is one message whose participants are its recipients plus its
//     sender (the view's message_participant_links); a conversation entry's
//     participants are the message-level participants of the group's chat
//     messages plus the conversation's own participant rows. conversation_id
//     is globally unique and NOT NULL, so matching it alone reproduces the
//     (source_id, conversation_id) grouping exactly.
//  2. Exact-ID lookup with conditions: the fan-out filtered to one person
//     before aggregation. The semi-join shape cannot be used because a
//     conversation entry's participant list must only reflect messages
//     inside the filtered context.
//  3. Listing/search: the full fan-out across every participant.
func personEntriesCTE(exactID *int64, conditions string) (string, []any) {
	const fanOut = `
), person_entries AS (
	SELECT grouped.person_id, occurred_at, attachment_count, source_type
	FROM logical_entries CROSS JOIN UNNEST(participant_ids) AS grouped(person_id)`
	if exactID == nil {
		return fanOut, nil
	}
	if conditions != "true" {
		return fanOut + `
	WHERE grouped.person_id = ?`, []any{*exactID}
	}
	return `
), person_message_ids AS (
	SELECT mr.message_id FROM message_recipients mr WHERE mr.participant_id = ?
	UNION
	SELECT m.id AS message_id FROM messages m WHERE m.sender_id = ?
), person_chat_conversation_ids AS (
	SELECT cp.conversation_id FROM conversation_participants cp WHERE cp.participant_id = ?
	UNION
	SELECT m.conversation_id
	FROM person_message_ids pm
	JOIN messages m ON m.id = pm.message_id
	LEFT JOIN conversations c ON c.id = m.conversation_id
	WHERE ` + sqlIsChatPredicate("m.message_type", "COALESCE(c.conversation_type, '')") + `
), person_entries AS (
	SELECT ?::BIGINT AS person_id, occurred_at, attachment_count, source_type
	FROM logical_entries le
	WHERE (le.entry_kind <> 'conversation'
	       AND le.anchor_message_id IN (SELECT message_id FROM person_message_ids))
	   OR (le.entry_kind = 'conversation'
	       AND le.conversation_id IN (SELECT conversation_id FROM person_chat_conversation_ids))`,
		[]any{*exactID, *exactID, *exactID, *exactID}
}

func (e *DuckDBEngine) SearchDomains(ctx context.Context, request DomainSearchRequest) (*DomainSearchResponse, error) {
	return e.searchDomains(ctx, request, "")
}

func (e *DuckDBEngine) GetDomain(ctx context.Context, domain string, analyticalContext Context) (*DomainSummary, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, fmt.Errorf("%w: domain is required", ErrInvalidExploreRequest)
	}
	result, err := e.searchDomains(ctx, DomainSearchRequest{Explore: ExploreRequest{Context: analyticalContext}, Page: PageSpec{Limit: 1}}, domain)
	if err != nil || len(result.Rows) == 0 {
		return nil, err
	}
	return &result.Rows[0], nil
}

// GetDomainSummary returns contextual metrics for one exact normalized domain.
func (e *DuckDBEngine) GetDomainSummary(ctx context.Context, domain string, explore ExploreRequest) (*DomainSearchResponse, error) {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if domain == "" {
		return nil, fmt.Errorf("%w: domain is required", ErrInvalidExploreRequest)
	}
	return e.searchDomains(ctx, DomainSearchRequest{Explore: explore, Page: PageSpec{Limit: 1}}, domain)
}

func (e *DuckDBEngine) searchDomains(ctx context.Context, request DomainSearchRequest, exactDomain string) (*DomainSearchResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateIdentityRequest(request.Explore.Context, request.Page); err != nil {
		return nil, err
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
	}
	order, err := identitySearchOrder(request.Sort, "domain", "domain")
	if err != nil {
		return nil, err
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	conditions, args := buildExploreConditions(request.Explore)
	domainWhere := []string{"domain <> ''"}
	if exactDomain != "" {
		domainWhere = append(domainWhere, "domain = ?")
		args = append(args, exactDomain)
	}
	if searchText := strings.TrimSpace(request.Query); searchText != "" {
		domainWhere = append(domainWhere, "contains(lower(domain), lower(?))")
		args = append(args, searchText)
	}
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	args = append(args, limit, request.Page.Offset)
	queryText := buildExploreLogicalSQL(conditions) + `
), domain_entries AS (
	SELECT lower(grouped.domain) AS domain, logical_entries.*
	FROM logical_entries CROSS JOIN UNNEST(participant_domains) AS grouped(domain)
), domain_population AS (
	SELECT domain, COUNT(*)::BIGINT AS activity_count,
		COALESCE(SUM(attachment_count), 0)::BIGINT AS file_count,
		MIN(occurred_at) AS first_at, MAX(occurred_at) AS last_at
	FROM domain_entries GROUP BY domain
), filtered_domains AS (
	SELECT * FROM domain_population WHERE ` + strings.Join(domainWhere, " AND ") + `
), counted AS (
	SELECT *, COUNT(*) OVER () AS total_count FROM filtered_domains
)
SELECT domain, activity_count,
	(SELECT COUNT(DISTINCT person_id)::BIGINT FROM (
		SELECT UNNEST(participant_ids) AS person_id FROM domain_entries de WHERE de.domain = counted.domain
	) people JOIN participants p ON p.id = people.person_id AND lower(COALESCE(p.domain, '')) = counted.domain) AS person_count,
	file_count,
	COALESCE(CAST((SELECT to_json(list(struct_pack(source_type := source_type, count := source_count)
		ORDER BY source_type)) FROM (SELECT source_type, COUNT(*)::BIGINT AS source_count
		FROM domain_entries de WHERE de.domain = counted.domain GROUP BY source_type)) AS VARCHAR), '[]'),
	first_at, last_at, total_count
FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("search analytical domains: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &DomainSearchResponse{Rows: make([]DomainSummary, 0), CacheRevision: state.Revision(), SearchProvenance: provenance}
	for rows.Next() {
		var row DomainSummary
		var sourceCountsJSON string
		if err := rows.Scan(&row.Domain, &row.ActivityCount, &row.PersonCount, &row.FileCount,
			&sourceCountsJSON, &row.FirstAt, &row.LastAt, &response.TotalCount); err != nil {
			return nil, fmt.Errorf("scan analytical domain: %w", err)
		}
		if err := json.Unmarshal([]byte(sourceCountsJSON), &row.SourceCounts); err != nil {
			return nil, fmt.Errorf("decode domain source counts: %w", err)
		}
		row.CacheRevision = state.Revision()
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical domains: %w", err)
	}
	return response, nil
}

func validateIdentityRequest(analyticalContext Context, page PageSpec) error {
	if page.Offset < 0 || page.Limit < 0 || page.Limit > maxPeopleSearchLimit {
		return fmt.Errorf("%w: identity page is outside the supported range", ErrInvalidExploreRequest)
	}
	if analyticalContext.Deletion != DeletionAny && analyticalContext.Deletion != DeletionActive && analyticalContext.Deletion != DeletionDeleted {
		return fmt.Errorf("%w: unknown deletion filter %q", ErrInvalidExploreRequest, analyticalContext.Deletion)
	}
	return nil
}

func identitySearchOrder(sort SortSpec, labelField, tieField string) (string, error) {
	if sort.Field == "" {
		sort = SortSpec{Field: "activity_count", Direction: sortDirectionDesc}
	}
	if sort.Direction != sortDirectionAsc && sort.Direction != sortDirectionDesc {
		return "", fmt.Errorf("%w: unknown identity sort direction %q", ErrInvalidExploreRequest, sort.Direction)
	}
	var column string
	switch sort.Field {
	case "activity_count":
		column = "activity_count"
	case "latest_at":
		column = "last_at"
	case "display_label":
		column = labelField
	default:
		return "", fmt.Errorf("%w: unknown identity sort field %q", ErrInvalidExploreRequest, sort.Field)
	}
	return column + " " + strings.ToUpper(sort.Direction) + ", " + tieField + " ASC", nil
}
