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
// when it has two or more entries, widens the whole summary to span every
// listed participant: Identifiers carry each member's own ParticipantID, and
// the row metrics (ActivityCount, FileCount, SourceCounts, FirstAt/LastAt)
// aggregate activity owned by any member — matching what the cluster-aware
// relationship timeline and files search report for the same identity. The
// caller (the person-detail HTTP handler) resolves cluster membership from
// the store and passes it in, keeping this query layer free of a store
// dependency. A nil/single-element list leaves everything scoped to id
// alone, matching the pre-cluster-aware behavior.
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
// clusterMemberIDs widens the summary across a linked identity cluster exactly
// as GetPerson documents: metrics aggregate activity owned by any member, the
// label follows the shared cluster best-name policy, and identifiers span every
// member — so a predicate matching only alias-owned activity still reports it
// instead of returning an empty (not-found) summary.
func (e *DuckDBEngine) GetPersonSummary(ctx context.Context, id int64, explore ExploreRequest, clusterMemberIDs []int64) (*PersonSearchResponse, error) {
	if id < 1 {
		return nil, fmt.Errorf("%w: person ID must be positive", ErrInvalidExploreRequest)
	}
	return e.searchPeople(ctx, PersonSearchRequest{Explore: explore, Page: PageSpec{Limit: 1}}, &id, clusterMemberIDs)
}

func (e *DuckDBEngine) searchPeople(
	ctx context.Context, request PersonSearchRequest, exactID *int64, clusterMemberIDs []int64,
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
	entriesCTE, entryArgs := personEntriesCTE(exactID, clusterMemberIDs, conditions, e.parquetPath(datasetParticipantClusters))
	args = append(args, entryArgs...)
	// bestNameExpr is the shared cluster label policy (see person_label.go).
	// Listing/search rows are canonical identities, so the label evaluates
	// every member of the row's cluster (resolved through the canon CTE that
	// personEntriesCTE emits), exactly as the ranked relationships list does.
	// Exact-ID lookups have no canon CTE: with a caller-supplied cluster the
	// members bind as placeholders here, before the personWhere and
	// identifier args, matching their position inside person_population;
	// otherwise the row participant's own name applies.
	bestNameExpr := "NULLIF(TRIM(p.display_name), '')"
	switch {
	case exactID == nil:
		bestNameExpr = sqlClusterBestNameExpr("pbn.id IN (SELECT cnl.participant_id FROM canon cnl WHERE cnl.canonical_id = p.id)")
	case len(clusterMemberIDs) > 1:
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(clusterMemberIDs)), ",")
		bestNameExpr = sqlClusterBestNameExpr("pbn.id IN (" + placeholders + ")")
		for _, memberID := range clusterMemberIDs {
			args = append(args, memberID)
		}
	}
	personWhere := []string{"true"}
	if exactID != nil {
		personWhere = append(personWhere, "person_id = ?")
		args = append(args, *exactID)
	}
	if searchText := strings.TrimSpace(request.Query); searchText != "" {
		// Listing/search rows key on the canonical identity, so a term must
		// match when it matches ANY cluster member's name, address, phone, or
		// stored identifier evidence — otherwise a linked alias would be
		// unfindable by the identifier it was linked under.
		matchExpr := `(EXISTS (SELECT 1 FROM canon cq JOIN participants pq ON pq.id = cq.participant_id
			WHERE cq.canonical_id = person_id
				AND (contains(lower(COALESCE(pq.display_name, '')), lower(?))
					OR contains(lower(COALESCE(pq.email_address, '')), lower(?))
					OR contains(lower(COALESCE(pq.phone_number, '')), lower(?))))
			OR EXISTS (SELECT 1 FROM participant_identifiers pi
				JOIN canon cq ON cq.participant_id = pi.participant_id
				WHERE cq.canonical_id = person_id AND (contains(lower(pi.identifier_value), lower(?))
					OR contains(lower(pi.display_value), lower(?)))))`
		if exactID != nil {
			matchExpr = `(contains(lower(display_name), lower(?))
			OR contains(lower(email_address), lower(?)) OR contains(lower(phone_number), lower(?))
			OR EXISTS (SELECT 1 FROM participant_identifiers pi
				WHERE pi.participant_id = person_id AND (contains(lower(pi.identifier_value), lower(?))
					OR contains(lower(pi.display_value), lower(?)))))`
		}
		personWhere = append(personWhere, matchExpr)
		args = append(args, searchText, searchText, searchText, searchText, searchText)
	}
	// identifierFilter scopes the per-row identifiers subquery below. For
	// listing/search rows the canonical identity's identifiers span its whole
	// cluster (via canon), matching what the person-detail endpoint returns
	// for the same row. GetPerson passes every cluster member explicitly so
	// a linked participant's identifiers span the whole cluster; an exact-ID
	// lookup without members stays scoped to the row's own person_id.
	identifierFilter := "pi.participant_id = counted.person_id"
	switch {
	case exactID == nil:
		identifierFilter = "pi.participant_id IN (SELECT cni.participant_id FROM canon cni WHERE cni.canonical_id = counted.person_id)"
	case len(clusterMemberIDs) > 1:
		placeholders := make([]string, len(clusterMemberIDs))
		for i, memberID := range clusterMemberIDs {
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
		` + bestNameExpr + ` AS best_display_name,
		` + sqlPersonIdentifierFallbackExpr("p") + ` AS fallback_label,
		COUNT(*)::BIGINT AS activity_count, COALESCE(SUM(pe.attachment_count), 0)::BIGINT AS file_count,
		MIN(pe.occurred_at) AS first_at, MAX(pe.occurred_at) AS last_at
	FROM person_entries pe JOIN participants p ON p.id = pe.person_id
	GROUP BY p.id, p.display_name, p.email_address, p.phone_number
), filtered_people AS (
	SELECT *, COALESCE(best_display_name, fallback_label) AS display_label,
		(best_display_name IS NULL) AS partial_label
	FROM person_population WHERE ` + strings.Join(personWhere, " AND ") + `
), counted AS (
	SELECT *, COUNT(*) OVER () AS total_count FROM filtered_people
)
SELECT person_id, display_label, display_name,
	partial_label,
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
// For exact-ID lookups, memberIDs (the identity cluster GetPerson resolved;
// empty means the requested ID alone) widens membership so an entry owned by
// any linked alias counts once toward the requested person's metrics — every
// matched entry projects exactID as its person_id, so the aggregates and the
// participants join downstream still key on the requested row.
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
//  2. Exact-ID lookup with conditions: logical entries containing any member
//     in participant_ids, filtered inside the analytical context. The
//     semi-join shape cannot be used because a conversation entry's
//     participant list must only reflect messages inside the filtered
//     context.
//  3. Listing/search: the full fan-out across every participant, resolved to
//     canonical identities through the committed participant_clusters
//     dataset (clustersGlob) so a linked identity aggregates as ONE row
//     keyed by its canonical ID — matching the ranked relationships list
//     instead of splitting cluster members into partial rows. The DISTINCT
//     mirrors the relationships ranking: an entry listing several raw
//     members of one cluster (e.g. cc'ing a contact's work and personal
//     addresses) collapses to a single (entry, canonical) row, so
//     aggregation never double-counts the entry. entry_key is projected
//     only to carry per-entry uniqueness through that DISTINCT.
func personEntriesCTE(exactID *int64, memberIDs []int64, conditions, clustersGlob string) (string, []any) {
	if exactID == nil {
		return fmt.Sprintf(`
), clusters AS (
	SELECT participant_id, canonical_id FROM read_parquet('%s')
), canon AS (
	SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
	FROM participants p LEFT JOIN clusters c ON c.participant_id = p.id
), person_entries AS (
	SELECT DISTINCT le.entry_key, cn.canonical_id AS person_id, le.occurred_at, le.attachment_count, le.source_type
	FROM logical_entries le
	CROSS JOIN UNNEST(le.participant_ids) AS grouped(person_id)
	JOIN canon cn ON cn.participant_id = grouped.person_id`, clustersGlob), nil
	}
	if len(memberIDs) == 0 {
		memberIDs = []int64{*exactID}
	}
	if conditions != "true" {
		contains := make([]string, len(memberIDs))
		args := make([]any, 0, len(memberIDs)+1)
		args = append(args, *exactID)
		for i, memberID := range memberIDs {
			contains[i] = "list_contains(participant_ids, ?)"
			args = append(args, memberID)
		}
		return `
), person_entries AS (
	SELECT ?::BIGINT AS person_id, occurred_at, attachment_count, source_type
	FROM logical_entries
	WHERE (` + strings.Join(contains, " OR ") + `)`, args
	}
	memberList := "(" + strings.TrimSuffix(strings.Repeat("?,", len(memberIDs)), ",") + ")"
	args := make([]any, 0, len(memberIDs)*3+1)
	for range 3 {
		for _, memberID := range memberIDs {
			args = append(args, memberID)
		}
	}
	args = append(args, *exactID)
	return `
), person_message_ids AS (
	SELECT mr.message_id FROM message_recipients mr WHERE mr.participant_id IN ` + memberList + `
	UNION
	SELECT m.id AS message_id FROM messages m WHERE m.sender_id IN ` + memberList + `
), person_chat_conversation_ids AS (
	SELECT cp.conversation_id FROM conversation_participants cp WHERE cp.participant_id IN ` + memberList + `
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
		args
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
	// person_count counts identities with at least one address on the domain:
	// participants are filtered to the domain by their OWN address first, then
	// resolved to canonical cluster IDs (canon, sqlClustersCanonCTE) so linked
	// aliases on the same domain count once — matching the cluster-aware People
	// and Relationships views. Canonicalizing after the domain filter keeps a
	// cluster whose canonical member lives on another domain counted here.
	queryText := buildExploreLogicalSQL(conditions) + `
), ` + sqlClustersCanonCTE(e.parquetPath(datasetParticipantClusters)) + `, domain_entries AS (
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
	(SELECT COUNT(DISTINCT cn.canonical_id)::BIGINT FROM (
		SELECT UNNEST(participant_ids) AS person_id FROM domain_entries de WHERE de.domain = counted.domain
	) people JOIN participants p ON p.id = people.person_id AND lower(COALESCE(p.domain, '')) = counted.domain
	JOIN canon cn ON cn.participant_id = people.person_id) AS person_count,
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
	direction, ok := sqlSortDirections[sort.Direction]
	if !ok {
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
	return column + " " + direction + ", " + tieField + " ASC", nil
}
