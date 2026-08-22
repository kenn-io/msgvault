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
		context.Identity == nil &&
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

// buildIdentityLogicalSQL renders the context-filtered logical-entry
// population with per-(entry, canonical) relationship facts as a closed
// logical_people CTE: entry facts come from analytical_entries (one row per
// message, list columns never referenced) and membership from
// relationship_activity edges. Its predecessor re-derived logical units from
// the activity dataset at query time (the former identityindex logical-activity SQL),
// whose per-message GROUP BY dedup over the whole edge set exceeds the
// interactive memory budget on production archives under broad filters.
//
// logical_people carries one row per (entry, canonical) with the flags the
// filtered relationships ranking consumes — is_author (anchor authorship for
// chat entries, matching the index builder), is_owner, with_owner — plus the
// entry facts the filtered people search aggregates. Conditions render
// through buildExploreConditions, so every Explore dimension filters these
// searches identically to Explore itself.
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
	conditions, args := buildExploreConditions(request)
	activityScan := `read_parquet('` + e.identityActivityPath() + `',
		hive_partitioning=true, union_by_name=true)`
	// entry_num is a numeric logical-entry key: the anchor message ID for
	// message entries, the (globally unique, NOT NULL) conversation ID
	// negated for chat conversation entries so the two spaces cannot
	// collide. The per-(entry, person) dedup hash below holds millions of
	// groups under a broad filter; a 16-byte numeric pair keeps it inside
	// the interactive memory budget where the VARCHAR entry_key did not.
	// The dedup aggregates run over purely numeric (id, canonical) pairs
	// with two boolean flags BEFORE the entry-payload join, so their hash
	// tables never hold entry payload for millions of groups — the shape
	// that previously sat on the interactive memory limit and failed
	// intermittently. person_flags then attaches payload with one
	// streaming join per branch.
	sql := buildExploreLogicalSQLNoLists(conditions) + `
), message_person AS (
	SELECT a.message_id, a.canonical_id,
	       bool_or(a.is_author) AS is_author,
	       bool_or(a.is_owner) AS is_owner
	FROM ` + activityScan + ` a
	JOIN (
		SELECT DISTINCT anchor_message_id FROM logical_entries
		WHERE entry_kind <> 'conversation'
	) m ON m.anchor_message_id = a.message_id
	WHERE a.canonical_id IS NOT NULL
	  AND a.is_direct
	GROUP BY a.message_id, a.canonical_id
), chat_person AS (
	SELECT f.conversation_id, a.canonical_id,
	       bool_or(a.is_author AND a.message_id = anchors.anchor_message_id) AS is_author,
	       bool_or(a.is_owner) AS is_owner
	FROM classified f
	JOIN ` + activityScan + ` a ON a.message_id = f.message_id
	JOIN (
		SELECT conversation_id, anchor_message_id FROM logical_entries
		WHERE entry_kind = 'conversation'
	) anchors ON anchors.conversation_id = f.conversation_id
	WHERE f.is_chat
	  AND a.canonical_id IS NOT NULL
	  AND (a.is_direct OR a.is_conversation_member)
	GROUP BY f.conversation_id, a.canonical_id
), person_flags AS (
	SELECT le.anchor_message_id AS entry_num, mp.canonical_id,
	       mp.is_author, mp.is_owner,
	       le.occurred_at, le.is_from_me, le.message_type, le.entry_kind,
	       le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN message_person mp ON mp.message_id = le.anchor_message_id
	WHERE le.entry_kind <> 'conversation'
	UNION ALL
	SELECT -le.conversation_id AS entry_num, cp.canonical_id,
	       cp.is_author, cp.is_owner,
	       le.occurred_at, le.is_from_me, le.message_type, le.entry_kind,
	       le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN chat_person cp ON cp.conversation_id = le.conversation_id
	WHERE le.entry_kind = 'conversation'
), owner_presence AS (
	SELECT entry_num, bool_or(is_owner) AS with_owner
	FROM person_flags
	GROUP BY entry_num
), logical_people AS (
	SELECT pf.*, op.with_owner
	FROM person_flags pf
	JOIN owner_presence op USING (entry_num)
)`
	if strings.TrimSpace(identityCandidateSQL) != "" {
		sql += ", identity_candidates AS (" + identityCandidateSQL + ")"
	}
	return sql, args, nil
}

// buildIdentityDomainLogicalSQL is the domain-search counterpart of
// buildIdentityLogicalSQL: logical_domains carries one row per
// (entry, domain) with the entry facts domain totals aggregate, and
// logical_person_domains the (entry, domain, canonical) pairs person_count
// dedups over — direct edges only for message entries, matching the
// relationship_domains dataset's person-domain pairing, while the domain
// rows themselves include conversation-roster domains exactly as the
// aggregated participant_domains lists did.
func (e *DuckDBEngine) buildIdentityDomainLogicalSQL(
	ctx context.Context,
	request ExploreRequest,
) (string, []any, error) {
	var err error
	request, err = e.narrowIdentityFactCandidates(ctx, request)
	if err != nil {
		return "", nil, err
	}
	conditions, args := buildExploreConditions(request)
	activityGlob := e.identityActivityPath()
	activityScan := `read_parquet('` + activityGlob + `',
		hive_partitioning=true, union_by_name=true)`
	// entry_num mirrors buildIdentityLogicalSQL: the per-(entry, domain)
	// dedup hash stays numeric-keyed so a broad filter fits the interactive
	// memory budget.
	sql := buildExploreLogicalSQLNoLists(conditions) + `
), logical_domains AS (
	SELECT le.anchor_message_id AS entry_num, a.participant_domain AS domain,
	       le.occurred_at, le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN ` + activityScan + ` a ON a.message_id = le.anchor_message_id
	WHERE le.entry_kind <> 'conversation'
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> ''
	UNION
	SELECT -le.conversation_id AS entry_num, a.participant_domain AS domain,
	       le.occurred_at, le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN classified f ON f.conversation_id = le.conversation_id AND f.is_chat
	JOIN ` + activityScan + ` a ON a.message_id = f.message_id
	WHERE le.entry_kind = 'conversation'
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> ''
), logical_person_domains AS (
	SELECT DISTINCT a.participant_domain AS domain, a.canonical_id
	FROM logical_entries le
	JOIN ` + activityScan + ` a ON a.message_id = le.anchor_message_id
	WHERE le.entry_kind <> 'conversation'
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> '' AND a.is_direct
	UNION
	SELECT a.participant_domain AS domain, a.canonical_id
	FROM logical_entries le
	JOIN classified f ON f.conversation_id = le.conversation_id AND f.is_chat
	JOIN ` + activityScan + ` a ON a.message_id = f.message_id
	WHERE le.entry_kind = 'conversation'
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> ''
)`
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
	if identityCondition, identityArgs := buildIdentityPredicateCondition(request.Context.Identity, "identity_entry."); identityCondition != "" {
		conditions = append(conditions, `(EXISTS (
			SELECT 1 FROM analytical_entries identity_entry
			WHERE identity_entry.message_id = f.message_id
			  AND `+identityCondition+`
		))`)
		args = append(args, identityArgs...)
	}
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
			&row.MeetingCount,
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
	// person_stats is the ONLY reference to the per-(entry, person) rows,
	// and it reads person_flags rather than logical_people: a second
	// reference would make DuckDB materialize the full row set — hundreds
	// of MB on a production archive under a broad filter — and the
	// owner_presence join inside logical_people serves only the filtered
	// relationships ranking, so consuming person_flags lets the optimizer
	// drop it entirely here. Totals and per-source counts both reduce from
	// the compact (person, source_type) stats, and the candidate
	// restriction happens in the population join below rather than inside
	// the aggregate.
	return logicalSQL + `,
person_stats AS (
	SELECT p.canonical_id, p.source_type,
	       count(*)::BIGINT AS source_count,
	       count(*) FILTER (WHERE p.message_type = 'meeting_transcript')::BIGINT AS meeting_count,
	       coalesce(sum(p.attachment_count), 0)::BIGINT AS file_count,
	       min(p.occurred_at)::TIMESTAMP AS first_at,
	       max(p.occurred_at)::TIMESTAMP AS last_at
	FROM person_flags p
	GROUP BY p.canonical_id, p.source_type
), person_totals AS (
	SELECT canonical_id,
	       sum(source_count)::BIGINT AS activity_count,
	       sum(meeting_count)::BIGINT AS meeting_count,
	       sum(file_count)::BIGINT AS file_count,
	       min(first_at)::TIMESTAMP AS first_at,
	       max(last_at)::TIMESTAMP AS last_at
	FROM person_stats
	GROUP BY canonical_id
), person_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM person_stats
	GROUP BY canonical_id
), population AS (
	SELECT c.canonical_id, c.person_id, c.display_label, c.partial_label,
	       c.member_ids, c.search_values, c.is_owner,
	       t.activity_count, t.meeting_count, t.file_count, s.source_counts,
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
	       item.meeting_count,
	       item.file_count,
	       item.first_at,
	       item.last_at
	FROM identity_candidates c
	CROSS JOIN unnest(c.source_rollups) AS source_rollup(item)
	WHERE item.source_id IN (` + placeholders + `)
), person_totals AS (
	SELECT canonical_id,
	       sum(activity_count)::BIGINT AS activity_count,
	       sum(meeting_count)::BIGINT AS meeting_count,
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
	       t.activity_count, t.meeting_count, t.file_count, s.source_counts,
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
	       p.meeting_count,
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
		logicalSQL, logicalArgs, logicalErr := e.buildIdentityDomainLogicalSQL(
			ctx,
			request.Explore,
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
	// domain_stats is the ONLY reference to the per-(entry, domain) rows:
	// a second reference would materialize them all (see filteredPeopleSQL).
	// Totals and per-source counts reduce from the compact
	// (domain, source_type) stats; person_count dedups the separate
	// person-domain pair edges against the surviving domains.
	return logicalSQL + `,
domain_stats AS (
	SELECT domain, source_type,
	       count(*)::BIGINT AS source_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM logical_domains
	WHERE ` + domainWhere + `
	GROUP BY domain, source_type
), domain_totals AS (
	SELECT domain,
	       sum(source_count)::BIGINT AS activity_count,
	       sum(file_count)::BIGINT AS file_count,
	       min(first_at)::TIMESTAMP AS first_at,
	       max(last_at)::TIMESTAMP AS last_at
	FROM domain_stats
	GROUP BY domain
), domain_sources AS (
	SELECT domain,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM domain_stats
	GROUP BY domain
), domain_people AS (
	SELECT domain, count(DISTINCT canonical_id)::BIGINT AS person_count
	FROM logical_person_domains
	WHERE domain IN (SELECT DISTINCT domain FROM domain_stats)
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
