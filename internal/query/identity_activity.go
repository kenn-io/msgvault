package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/identityindex"
)

func identityRequestIsUnfiltered(request ExploreRequest) bool {
	context := request.Context
	return len(context.SourceIDs) == 0 &&
		len(context.ParticipantIDs) == 0 &&
		len(context.Domains) == 0 &&
		len(context.AdditionalParticipantGroups) == 0 &&
		len(context.AdditionalDomainGroups) == 0 &&
		len(context.MessageTypes) == 0 &&
		context.After == nil &&
		context.Before == nil &&
		context.Deletion == DeletionAny &&
		request.Search.Mode == SearchNone &&
		request.Search.Query == "" &&
		request.Search.CandidateMessageIDs == nil
}

func (e *DuckDBEngine) identityDatasetPath(dataset string) string {
	return e.parquetPath(dataset)
}

func (e *DuckDBEngine) buildIdentityLogicalSQL(
	request ExploreRequest,
	identityCandidateSQL string,
) (string, []any) {
	conditions, args := buildIdentityFactConditions(request)
	paths := identityindex.ActivityPaths{
		Facts:             e.identityDatasetPath(identityindex.DatasetEntryFacts),
		DirectEdges:       e.identityDatasetPath(identityindex.DatasetDirectEdges),
		ConversationEdges: e.identityDatasetPath(identityindex.DatasetConversationEdges),
		Directory:         e.identityDatasetPath(identityindex.DatasetDirectory),
		Clusters:          e.identityDatasetPath(datasetParticipantClusters),
		Owners:            e.identityDatasetPath(datasetOwnerParticipants),
	}
	sql := identityindex.LogicalActivitySQL(paths, conditions)
	if strings.TrimSpace(identityCandidateSQL) != "" {
		sql += ", identity_candidates AS (" + identityCandidateSQL + ")"
	}
	return e.resolveIdentityPathPlaceholders(sql), args
}

func buildIdentityFactConditions(request ExploreRequest) (string, []any) {
	var conditions []string
	var args []any
	appendIntGroup := func(values []int64, expression string) {
		if len(values) == 0 {
			return
		}
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = expression
			args = append(args, value)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}
	appendIntGroup(request.Context.SourceIDs, "f.source_id = ?")
	participantPredicate := `(EXISTS (
		SELECT 1 FROM read_parquet('` +
		quoteIdentityPathPlaceholder(identityindex.DatasetDirectEdges) + `',
			hive_partitioning=true, union_by_name=true) d
		WHERE d.message_id = f.message_id AND d.participant_id = ?
	) OR EXISTS (
		SELECT 1 FROM read_parquet('` +
		quoteIdentityPathPlaceholder(identityindex.DatasetConversationEdges) + `') c
		WHERE c.conversation_id = f.conversation_id AND c.participant_id = ?
	))`
	domainPredicate := `(EXISTS (
		SELECT 1 FROM read_parquet('` +
		quoteIdentityPathPlaceholder(identityindex.DatasetDirectEdges) + `',
			hive_partitioning=true, union_by_name=true) d
		WHERE d.message_id = f.message_id AND lower(d.participant_domain) = ?
	) OR EXISTS (
		SELECT 1 FROM read_parquet('` +
		quoteIdentityPathPlaceholder(identityindex.DatasetConversationEdges) + `') c
		WHERE c.conversation_id = f.conversation_id AND lower(c.participant_domain) = ?
	))`
	appendIdentityEdgeGroup := func(values []int64) {
		if len(values) == 0 {
			return
		}
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = participantPredicate
			args = append(args, value, value)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}
	appendIdentityDomainGroup := func(values []string) {
		if len(values) == 0 {
			return
		}
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = domainPredicate
			normalized := strings.ToLower(strings.TrimSpace(value))
			args = append(args, normalized, normalized)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}
	appendIdentityEdgeGroup(request.Context.ParticipantIDs)
	appendIdentityDomainGroup(request.Context.Domains)
	for _, group := range request.Context.AdditionalParticipantGroups {
		appendIdentityEdgeGroup(group)
	}
	for _, group := range request.Context.AdditionalDomainGroups {
		appendIdentityDomainGroup(group)
	}

	if condition, conditionArgs := duckDBMessageTypeCondition("f", request.Context.MessageTypes); condition != "" {
		conditions = append(conditions, condition)
		args = append(args, conditionArgs...)
	}
	if request.Context.After != nil {
		conditions = append(conditions, "f.occurred_at >= CAST(? AS TIMESTAMP)")
		args = append(args, duckDBDateParam(*request.Context.After))
	}
	if request.Context.Before != nil {
		conditions = append(conditions, "f.occurred_at < CAST(? AS TIMESTAMP)")
		args = append(args, duckDBDateParam(*request.Context.Before))
	}
	switch request.Context.Deletion {
	case DeletionAny:
	case DeletionActive:
		conditions = append(conditions, "NOT f.deleted_from_source")
	case DeletionDeleted:
		conditions = append(conditions, "f.deleted_from_source")
	}
	if request.Search.CandidateMessageIDs != nil {
		if len(request.Search.CandidateMessageIDs) == 0 {
			conditions = append(conditions, "false")
		} else {
			placeholders := make([]string, len(request.Search.CandidateMessageIDs))
			for index, messageID := range request.Search.CandidateMessageIDs {
				placeholders[index] = "?"
				args = append(args, messageID)
			}
			conditions = append(conditions,
				"f.message_id IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if len(conditions) == 0 {
		return "true", args
	}
	return strings.Join(conditions, " AND "), args
}

// The filter builder is independent of an engine so tests can validate the
// placeholder order. The engine replaces these trusted tokens with absolute
// cache paths immediately before execution.
func quoteIdentityPathPlaceholder(dataset string) string {
	return "{{" + dataset + "}}"
}

func (e *DuckDBEngine) resolveIdentityPathPlaceholders(sql string) string {
	for _, dataset := range []string{
		identityindex.DatasetDirectEdges,
		identityindex.DatasetConversationEdges,
		identityindex.DatasetDirectory,
		identityindex.DatasetRelationships,
		identityindex.DatasetRelationshipFuture,
	} {
		path := strings.ReplaceAll(e.identityDatasetPath(dataset), "'", "''")
		sql = strings.ReplaceAll(sql, quoteIdentityPathPlaceholder(dataset), path)
	}
	if strings.Contains(sql, "{{") {
		panic("unresolved identity dataset path in SQL: " + sql)
	}
	return sql
}

func (e *DuckDBEngine) searchPeople(
	ctx context.Context,
	request PersonSearchRequest,
	exactID *int64,
	clusterMemberIDs []int64,
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
	request.Explore, err = e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}

	candidates, candidateArgs := e.identityPeopleCandidatesSQL(
		request.Query,
		exactID,
		clusterMemberIDs,
	)
	var queryText string
	args := make([]any, 0, len(candidateArgs)+2)
	widenAcrossMembers := len(clusterMemberIDs) > 1
	if identityRequestIsUnfiltered(request.Explore) {
		queryText = e.unfilteredPeopleSQL(candidates, order, widenAcrossMembers)
		args = append(args, candidateArgs...)
	} else {
		logicalSQL, logicalArgs := e.buildIdentityLogicalSQL(request.Explore, candidates)
		queryText = e.filteredPeopleSQL(logicalSQL, order, widenAcrossMembers)
		args = append(args, logicalArgs...)
		args = append(args, candidateArgs...)
	}
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	args = append(args, limit, request.Page.Offset)

	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("search indexed people: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &PersonSearchResponse{
		Rows:             make([]PersonSummary, 0),
		CacheRevision:    state.Revision(),
		SearchProvenance: provenance,
	}
	for rows.Next() {
		var row PersonSummary
		var identifiersJSON, sourceCountsJSON string
		if err := rows.Scan(
			&row.ID,
			&row.DisplayLabel,
			&row.DisplayName,
			&row.PartialLabel,
			&identifiersJSON,
			&row.ActivityCount,
			&row.FileCount,
			&sourceCountsJSON,
			&row.FirstAt,
			&row.LastAt,
			&response.TotalCount,
		); err != nil {
			return nil, fmt.Errorf("scan indexed person: %w", err)
		}
		if err := json.Unmarshal([]byte(identifiersJSON), &row.Identifiers); err != nil {
			return nil, fmt.Errorf("decode indexed person identifiers: %w", err)
		}
		if err := json.Unmarshal([]byte(sourceCountsJSON), &row.SourceCounts); err != nil {
			return nil, fmt.Errorf("decode indexed person source counts: %w", err)
		}
		row.CacheRevision = state.Revision()
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexed people: %w", err)
	}
	return response, nil
}

func (e *DuckDBEngine) identityPeopleCandidatesSQL(
	searchText string,
	exactID *int64,
	clusterMemberIDs []int64,
) (string, []any) {
	directory := quoteIdentitySQLPath(
		e.identityDatasetPath(identityindex.DatasetDirectory),
	)
	var predicates []string
	var args []any
	personID := "d.canonical_id"
	directoryRelation := "read_parquet('" + directory + "') d"
	if exactID != nil {
		personID = fmt.Sprintf("%d::BIGINT", *exactID)
		if len(clusterMemberIDs) > 1 {
			memberIDs := make([]string, len(clusterMemberIDs))
			for index, memberID := range clusterMemberIDs {
				memberIDs[index] = fmt.Sprintf("%d::BIGINT", memberID)
			}
			memberList := "[" + strings.Join(memberIDs, ",") + "]"
			directoryRelation = `(SELECT
				` + personID + ` AS canonical_id,
				arg_min(display_label,
					struct_pack(partial := partial_label, canonical_id := canonical_id))
					AS display_label,
				bool_and(partial_label) AS partial_label,
				list_sort(list_distinct(flatten(list(member_ids)))) AS member_ids,
				list_sort(list_distinct(flatten(list(search_values)))) AS search_values,
				bool_or(is_owner) AS is_owner
			FROM read_parquet('` + directory + `')
			WHERE list_has_any(member_ids, ` + memberList + `)) d`
		} else {
			predicates = append(predicates, "list_contains(d.member_ids, ?)")
			args = append(args, *exactID)
		}
	}
	if searchText = strings.TrimSpace(searchText); searchText != "" {
		predicates = append(predicates, `EXISTS (
			SELECT 1
			FROM unnest(d.search_values) AS searched(value)
			WHERE contains(searched.value, lower(?))
		)`)
		args = append(args, searchText)
	}
	where := "true"
	if len(predicates) > 0 {
		where = strings.Join(predicates, " AND ")
	}
	return fmt.Sprintf(`
		SELECT d.*, %s AS person_id
		FROM %s
		WHERE %s
	`, personID, directoryRelation, where), args
}

func (e *DuckDBEngine) unfilteredPeopleSQL(
	candidates, order string,
	widenAcrossMembers bool,
) string {
	rollups := quoteIdentitySQLPath(
		e.identityDatasetPath(identityindex.DatasetRollups),
	)
	if !widenAcrossMembers {
		// Directory and rollup rows share the same canonical key. Keeping the
		// ordinary list/search path one-to-one avoids grouping 75K rows that
		// carry member and search-value lists merely to recover the same row.
		return `WITH identity_candidates AS (` + candidates + `
), population AS (
	SELECT c.*,
	       r.activity_count,
	       r.file_count,
	       r.first_at,
	       r.last_at,
	       r.source_counts
	FROM identity_candidates c
	JOIN read_parquet('` + rollups + `') r USING (canonical_id)
), counted AS (
	SELECT *, count(*) OVER ()::BIGINT AS total_count
	FROM population
), paged AS (
	SELECT *, row_number() OVER (ORDER BY ` + order + `) AS page_rank
	FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?
)
` + e.peoplePageSelectSQL()
	}
	// Detail callers may explicitly widen an otherwise-unlinked person
	// across several supplied members. That bounded one-row path must merge
	// each member's independent rollup.
	return `WITH identity_candidates AS (` + candidates + `
), people_totals AS (
	SELECT c.*,
	       sum(r.activity_count)::BIGINT AS activity_count,
	       sum(r.file_count)::BIGINT AS file_count,
	       min(r.first_at)::TIMESTAMP AS first_at,
	       max(r.last_at)::TIMESTAMP AS last_at
	FROM identity_candidates c
	JOIN read_parquet('` + rollups + `') r
	  ON list_contains(c.member_ids, r.canonical_id)
	GROUP BY ALL
), people_source_counts AS (
	SELECT c.canonical_id,
	       item.source_type AS source_type,
	       sum(item.count)::BIGINT AS source_count
	FROM identity_candidates c
	JOIN read_parquet('` + rollups + `') r
	  ON list_contains(c.member_ids, r.canonical_id)
	CROSS JOIN unnest(r.source_counts) AS source_item(item)
	GROUP BY c.canonical_id, item.source_type
), people_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM people_source_counts
	GROUP BY canonical_id
), population AS (
	SELECT t.*, s.source_counts
	FROM people_totals t
	JOIN people_sources s USING (canonical_id)
), counted AS (
	SELECT *, count(*) OVER ()::BIGINT AS total_count
	FROM population
), paged AS (
	SELECT *, row_number() OVER (ORDER BY ` + order + `) AS page_rank
	FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?
)
` + e.peoplePageSelectSQL()
}

func (e *DuckDBEngine) filteredPeopleSQL(
	logicalSQL, order string,
	widenAcrossMembers bool,
) string {
	peopleJoin := "p.canonical_id = c.canonical_id"
	if widenAcrossMembers {
		peopleJoin = "list_contains(c.member_ids, p.canonical_id)"
	}
	return logicalSQL + `,
person_totals AS (
	SELECT c.canonical_id,
	       count(*)::BIGINT AS activity_count,
	       coalesce(sum(p.attachment_count), 0)::BIGINT AS file_count,
	       min(p.occurred_at)::TIMESTAMP AS first_at,
	       max(p.occurred_at)::TIMESTAMP AS last_at
	FROM logical_people p
	JOIN identity_candidates c
	  ON ` + peopleJoin + `
	GROUP BY c.canonical_id
), person_source_counts AS (
	SELECT c.canonical_id, p.source_type, count(*)::BIGINT AS source_count
	FROM logical_people p
	JOIN identity_candidates c
	  ON ` + peopleJoin + `
	GROUP BY c.canonical_id, p.source_type
), person_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM person_source_counts
	GROUP BY canonical_id
), population AS (
	SELECT c.*, t.activity_count, t.file_count, s.source_counts,
	       t.first_at, t.last_at
	FROM identity_candidates c
	JOIN person_totals t USING (canonical_id)
	JOIN person_sources s USING (canonical_id)
), counted AS (
	SELECT *, count(*) OVER ()::BIGINT AS total_count
	FROM population
), paged AS (
	SELECT *, row_number() OVER (ORDER BY ` + order + `) AS page_rank
	FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?
)
` + e.peoplePageSelectSQL()
}

func (e *DuckDBEngine) peoplePageSelectSQL() string {
	identifiers := quoteIdentitySQLPath(
		e.identityDatasetPath(datasetParticipantIdentifiers),
	)
	participants := quoteIdentitySQLPath(
		e.identityDatasetPath(datasetParticipants),
	)
	return `
SELECT p.person_id,
       p.display_label,
       coalesce((
	       SELECT raw.display_name
	       FROM read_parquet('` + participants + `') raw
	       WHERE raw.id = p.person_id
       ), '') AS display_name,
       p.partial_label,
       coalesce(CAST((
	       SELECT to_json(list(struct_pack(
		       type := pi.identifier_type,
		       value := pi.identifier_value,
		       display_value := pi.display_value,
		       is_primary := pi.is_primary,
		       provenance := 'participant_identifiers',
		       participant_id := pi.participant_id
	       ) ORDER BY pi.is_primary DESC, pi.identifier_type, pi.identifier_value))
	       FROM read_parquet('` + identifiers + `') pi
	       WHERE list_contains(p.member_ids, pi.participant_id)
       ) AS VARCHAR), '[]') AS identifiers,
       p.activity_count,
       p.file_count,
       coalesce(CAST(to_json(p.source_counts) AS VARCHAR), '[]') AS source_counts,
       p.first_at,
       p.last_at,
       p.total_count
FROM paged p
ORDER BY p.page_rank`
}

func (e *DuckDBEngine) searchDomains(
	ctx context.Context,
	request DomainSearchRequest,
	exactDomain string,
) (*DomainSearchResponse, error) {
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
	request.Explore, err = e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}

	domainWhere, domainArgs := identityDomainWhere(request.Query, exactDomain)
	var queryText string
	var args []any
	if identityRequestIsUnfiltered(request.Explore) {
		queryText = e.unfilteredDomainsSQL(domainWhere, order)
		args = append(args, domainArgs...)
	} else {
		logicalSQL, logicalArgs := e.buildIdentityLogicalSQL(request.Explore, "")
		queryText = filteredDomainsSQL(logicalSQL, domainWhere, order)
		args = append(args, logicalArgs...)
		args = append(args, domainArgs...)
	}
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	args = append(args, limit, request.Page.Offset)

	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("search indexed domains: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &DomainSearchResponse{
		Rows:             make([]DomainSummary, 0),
		CacheRevision:    state.Revision(),
		SearchProvenance: provenance,
	}
	for rows.Next() {
		var row DomainSummary
		var sourceCountsJSON string
		if err := rows.Scan(
			&row.Domain,
			&row.ActivityCount,
			&row.PersonCount,
			&row.FileCount,
			&sourceCountsJSON,
			&row.FirstAt,
			&row.LastAt,
			&response.TotalCount,
		); err != nil {
			return nil, fmt.Errorf("scan indexed domain: %w", err)
		}
		if err := json.Unmarshal([]byte(sourceCountsJSON), &row.SourceCounts); err != nil {
			return nil, fmt.Errorf("decode indexed domain source counts: %w", err)
		}
		row.CacheRevision = state.Revision()
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexed domains: %w", err)
	}
	return response, nil
}

func identityDomainWhere(searchText, exactDomain string) (string, []any) {
	predicates := []string{"domain <> ''"}
	var args []any
	if exactDomain != "" {
		predicates = append(predicates, "domain = ?")
		args = append(args, exactDomain)
	}
	if searchText = strings.TrimSpace(searchText); searchText != "" {
		predicates = append(predicates, "contains(lower(domain), lower(?))")
		args = append(args, searchText)
	}
	return strings.Join(predicates, " AND "), args
}

func (e *DuckDBEngine) unfilteredDomainsSQL(domainWhere, order string) string {
	rollups := quoteIdentitySQLPath(
		e.identityDatasetPath(identityindex.DatasetDomainRollups),
	)
	return `
WITH filtered_domains AS (
	SELECT *
	FROM read_parquet('` + rollups + `')
	WHERE ` + domainWhere + `
), counted AS (
	SELECT *, count(*) OVER ()::BIGINT AS total_count
	FROM filtered_domains
)
SELECT domain, activity_count, person_count, file_count,
       coalesce(CAST(to_json(source_counts) AS VARCHAR), '[]'),
       first_at, last_at, total_count
FROM counted
ORDER BY ` + order + ` LIMIT ? OFFSET ?`
}

func filteredDomainsSQL(logicalSQL, domainWhere, order string) string {
	return logicalSQL + `,
filtered_logical_domains AS (
	SELECT * FROM logical_domains WHERE ` + domainWhere + `
), domain_totals AS (
	SELECT domain,
	       count(*)::BIGINT AS activity_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM filtered_logical_domains
	GROUP BY domain
), domain_source_counts AS (
	SELECT domain, source_type, count(*)::BIGINT AS source_count
	FROM filtered_logical_domains
	GROUP BY domain, source_type
), domain_sources AS (
	SELECT domain,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM domain_source_counts
	GROUP BY domain
), domain_people AS (
	SELECT domain, count(DISTINCT canonical_id)::BIGINT AS person_count
	FROM filtered_logical_domains
	CROSS JOIN unnest(canonical_ids) AS person(canonical_id)
	GROUP BY domain
), population AS (
	SELECT t.domain, t.activity_count,
	       coalesce(p.person_count, 0)::BIGINT AS person_count,
	       t.file_count, s.source_counts, t.first_at, t.last_at
	FROM domain_totals t
	JOIN domain_sources s USING (domain)
	LEFT JOIN domain_people p USING (domain)
), counted AS (
	SELECT *, count(*) OVER ()::BIGINT AS total_count
	FROM population
)
SELECT domain, activity_count, person_count, file_count,
       coalesce(CAST(to_json(source_counts) AS VARCHAR), '[]'),
       first_at, last_at, total_count
FROM counted
ORDER BY ` + order + ` LIMIT ? OFFSET ?`
}

func quoteIdentitySQLPath(value string) string {
	return strings.ReplaceAll(value, "'", "''")
}
