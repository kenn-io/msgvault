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

func identityRequestIsSourceOnly(request ExploreRequest) bool {
	if len(request.Context.SourceIDs) == 0 {
		return false
	}
	request.Context.SourceIDs = nil
	return identityRequestIsUnfiltered(request)
}

// identityActivityPath returns the relationship_activity glob escaped for
// direct embedding in trusted SQL text.
func (e *DuckDBEngine) identityActivityPath() string {
	return quoteIdentitySQLPath(e.parquetPath(identityindex.DatasetActivity))
}

func (e *DuckDBEngine) buildIdentityLogicalSQL(
	ctx context.Context,
	request ExploreRequest,
	identityCandidateSQL string,
) (string, []any, error) {
	var err error
	request, err = e.narrowIdentityFactCandidates(ctx, request)
	if err != nil {
		return "", nil, err
	}
	conditions, args := buildIdentityFactConditions(request, e.identityActivityPath())
	sql := identityindex.LogicalActivitySQL(
		e.parquetPath(identityindex.DatasetActivity),
		conditions,
	)
	if strings.TrimSpace(identityCandidateSQL) != "" {
		sql += ", identity_candidates AS (" + identityCandidateSQL + ")"
	}
	return sql, args, nil
}

func identityRequestHasEdgeFilters(request ExploreRequest) bool {
	return len(request.Context.ParticipantIDs) > 0 ||
		len(request.Context.Domains) > 0 ||
		len(request.Context.AdditionalParticipantGroups) > 0 ||
		len(request.Context.AdditionalDomainGroups) > 0
}

func (e *DuckDBEngine) narrowIdentityFactCandidates(
	ctx context.Context,
	request ExploreRequest,
) (ExploreRequest, error) {
	if e.identityCandidateFastPathDisabled ||
		!identityRequestHasEdgeFilters(request) {
		return request, nil
	}
	facts := e.identityActivityPath()
	conditions, args := buildIdentityFactConditions(request, facts)
	queryText := `
SELECT f.message_id
FROM read_parquet('` + facts + `', hive_partitioning=true, union_by_name=true) f
WHERE ` + conditions + `
GROUP BY f.message_id
LIMIT ?`
	args = append(args, MaxExploreCandidateMessageIDs+1)

	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return request, fmt.Errorf("narrow identity fact candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	messageIDs := make([]int64, 0)
	for rows.Next() {
		var messageID int64
		if err := rows.Scan(&messageID); err != nil {
			return request, fmt.Errorf("scan identity fact candidate: %w", err)
		}
		messageIDs = append(messageIDs, messageID)
	}
	if err := rows.Err(); err != nil {
		return request, fmt.Errorf("iterate identity fact candidates: %w", err)
	}
	if len(messageIDs) > MaxExploreCandidateMessageIDs {
		return request, nil
	}

	request.Context.ParticipantIDs = nil
	request.Context.Domains = nil
	request.Context.AdditionalParticipantGroups = nil
	request.Context.AdditionalDomainGroups = nil
	request.Search.CandidateMessageIDs = messageIDs
	return request, nil
}

// buildIdentityFactConditions renders the message-level filter for the alias
// f over relationship_activity. activityPath is the SQL-escaped activity glob
// used by the participant/domain edge semi-join predicates.
func buildIdentityFactConditions(request ExploreRequest, activityPath string) (string, []any) {
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
		SELECT 1
		FROM read_parquet('` + activityPath + `',
			hive_partitioning=true, union_by_name=true) edge
		WHERE edge.message_id = f.message_id
		  AND edge.canonical_id = ?
	))`
	domainPredicate := `(EXISTS (
		SELECT 1
		FROM read_parquet('` + activityPath + `',
			hive_partitioning=true, union_by_name=true) edge
		WHERE edge.message_id = f.message_id
		  AND lower(edge.participant_domain) = ?
	))`
	appendIdentityEdgeGroup := func(values []int64) {
		if len(values) == 0 {
			return
		}
		parts := make([]string, len(values))
		for index, value := range values {
			parts[index] = participantPredicate
			args = append(args, value)
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
			args = append(args, normalized)
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

func (e *DuckDBEngine) searchPeople(
	ctx context.Context,
	request PersonSearchRequest,
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

	candidates, candidateArgs := e.identityPeopleCandidatesSQL(request.Query)
	var queryText string
	args := make([]any, 0, len(candidateArgs)+2)
	if identityRequestIsUnfiltered(request.Explore) {
		queryText = e.unfilteredPeopleSQL(candidates, order)
		args = append(args, candidateArgs...)
	} else if identityRequestIsSourceOnly(request.Explore) &&
		!e.sourceRollupFastPathDisabled {
		queryText = e.sourceFilteredPeopleSQL(
			candidates,
			order,
			len(request.Explore.Context.SourceIDs),
		)
		args = append(args, candidateArgs...)
		for _, sourceID := range request.Explore.Context.SourceIDs {
			args = append(args, sourceID)
		}
	} else {
		logicalSQL, logicalArgs, logicalErr := e.buildIdentityLogicalSQL(
			ctx,
			request.Explore,
			candidates,
		)
		if logicalErr != nil {
			return nil, logicalErr
		}
		queryText = e.filteredPeopleSQL(logicalSQL, order)
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
) (string, []any) {
	directory := quoteIdentitySQLPath(
		e.parquetPath(identityindex.DatasetPeople),
	)
	var predicates []string
	var args []any
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
	return `
		SELECT d.*, d.canonical_id AS person_id
		FROM read_parquet('` + directory + `') d
		WHERE ` + where + `
	`, args
}

func (e *DuckDBEngine) unfilteredPeopleSQL(
	candidates, order string,
) string {
	// relationship_people already contains both search metadata and compact
	// totals, so the default path is a single narrow Parquet scan.
	return `WITH identity_candidates AS (` + candidates + `
), counted AS (
	SELECT *, count(*) OVER ()::BIGINT AS total_count
	FROM identity_candidates
), paged AS (
	SELECT *, row_number() OVER (ORDER BY ` + order + `) AS page_rank
	FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?
)
` + e.peoplePageSelectSQL()
}

func (e *DuckDBEngine) filteredPeopleSQL(
	logicalSQL, order string,
) string {
	return logicalSQL + `,
person_totals AS (
	SELECT c.canonical_id,
	       count(*)::BIGINT AS activity_count,
	       coalesce(sum(p.attachment_count), 0)::BIGINT AS file_count,
	       min(p.occurred_at)::TIMESTAMP AS first_at,
	       max(p.occurred_at)::TIMESTAMP AS last_at
	FROM logical_people p
	JOIN identity_candidates c
	  ON p.canonical_id = c.canonical_id
	GROUP BY c.canonical_id
), person_source_counts AS (
	SELECT c.canonical_id, p.source_type, count(*)::BIGINT AS source_count
	FROM logical_people p
	JOIN identity_candidates c
	  ON p.canonical_id = c.canonical_id
	GROUP BY c.canonical_id, p.source_type
), person_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM person_source_counts
	GROUP BY canonical_id
), population AS (
	SELECT c.canonical_id, c.person_id, c.display_label, c.partial_label,
	       c.member_ids, c.search_values, c.is_owner,
	       t.activity_count, t.file_count, s.source_counts,
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

func (e *DuckDBEngine) sourceFilteredPeopleSQL(
	candidates, order string,
	sourceCount int,
) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", sourceCount), ",")
	return `WITH identity_candidates AS (` + candidates + `
), selected_source_rollups AS (
	SELECT c.canonical_id,
	       item.source_type,
	       item.activity_count,
	       item.file_count,
	       item.first_at,
	       item.last_at
	FROM identity_candidates c
	CROSS JOIN unnest(c.source_rollups) AS source_rollup(item)
	WHERE item.source_id IN (` + placeholders + `)
), person_totals AS (
	SELECT canonical_id,
	       sum(activity_count)::BIGINT AS activity_count,
	       sum(file_count)::BIGINT AS file_count,
	       min(first_at)::TIMESTAMP AS first_at,
	       max(last_at)::TIMESTAMP AS last_at
	FROM selected_source_rollups
	GROUP BY canonical_id
), person_source_counts AS (
	SELECT canonical_id, source_type,
	       sum(activity_count)::BIGINT AS source_count
	FROM selected_source_rollups
	GROUP BY canonical_id, source_type
), person_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM person_source_counts
	GROUP BY canonical_id
), population AS (
	SELECT c.canonical_id, c.person_id, c.display_label, c.partial_label,
	       c.member_ids, c.search_values, c.is_owner,
	       t.activity_count, t.file_count, s.source_counts,
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
		e.parquetPath(datasetParticipantIdentifiers),
	)
	participants := quoteIdentitySQLPath(
		e.parquetPath(datasetParticipants),
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

	domainWhere, domainArgs := identityDomainWhere(request.Query)
	var queryText string
	var args []any
	if identityRequestIsUnfiltered(request.Explore) {
		queryText = e.unfilteredDomainsSQL(domainWhere, order)
		args = append(args, domainArgs...)
	} else {
		logicalSQL, logicalArgs, logicalErr := e.buildIdentityLogicalSQL(
			ctx,
			request.Explore,
			"",
		)
		if logicalErr != nil {
			return nil, logicalErr
		}
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

func identityDomainWhere(searchText string) (string, []any) {
	predicates := []string{"domain <> ''"}
	var args []any
	if searchText = strings.TrimSpace(searchText); searchText != "" {
		predicates = append(predicates, "contains(lower(domain), lower(?))")
		args = append(args, searchText)
	}
	return strings.Join(predicates, " AND "), args
}

func (e *DuckDBEngine) unfilteredDomainsSQL(domainWhere, order string) string {
	rollups := quoteIdentitySQLPath(
		e.parquetPath(identityindex.DatasetDomains),
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
