package query

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/identityindex"
	"go.kenn.io/msgvault/internal/search"
)

func meetingCountActivityCTE(dates string) string {
	return `meeting_activity AS (
	SELECT a.* FROM relationship_counting_activity a
	JOIN selected_meeting_messages s USING (entry_num)
	WHERE a.entry_kind IN ('event', 'meeting') AND a.canonical_id IS NOT NULL AND a.is_direct` + dates + `
)`
}

func (e *DuckDBEngine) meetingScopeCTE(request ExploreRequest) (string, []any, error) {
	unbounded := request
	unbounded.Context.After, unbounded.Context.Before = nil, nil
	unbounded.Context.SourceIDs = nil
	unbounded.Context.AccountScopes = nil
	conditions, args := buildExploreConditions(unbounded)
	conditions, err := e.sentIdentityConditions(conditions)
	if err != nil {
		return "", nil, err
	}
	dates := ""
	if len(request.Context.SourceIDs) > 0 {
		dates += " AND a.source_id IN (SELECT UNNEST(?::BIGINT[]))"
		args = append(args, request.Context.SourceIDs)
	}
	if scopeConditions, scopeArgs := search.AccountScopeConditions(request.Context.AccountScopes, "account_m"); len(scopeConditions) > 0 {
		dates += " AND a.anchor_message_id IN (SELECT account_m.id FROM messages account_m WHERE " + strings.Join(scopeConditions, " AND ") + ")"
		args = append(args, scopeArgs...)
	}
	if request.Context.After != nil {
		dates += " AND a.occurred_at >= CAST(? AS TIMESTAMP)"
		args = append(args, request.Context.After.UTC())
	}
	if request.Context.Before != nil {
		dates += " AND a.occurred_at < CAST(? AS TIMESTAMP)"
		args = append(args, request.Context.Before.UTC())
	}
	return `selected_meeting_messages AS (
	SELECT DISTINCT a.entry_num FROM relationship_counting_activity a WHERE a.message_id IN (SELECT message_id FROM analytical_entries WHERE ` + conditions + `)
), ` + meetingCountActivityCTE(dates), args, nil
}

func identityRequestIsUnfiltered(request ExploreRequest) bool {
	context := request.Context
	return len(context.SourceIDs) == 0 &&
		len(context.AccountScopes) == 0 &&
		context.Identity == nil &&
		len(context.ParticipantIDs) == 0 &&
		len(context.Domains) == 0 &&
		len(context.AdditionalParticipantGroups) == 0 &&
		len(context.AdditionalDomainGroups) == 0 &&
		len(context.MessageTypes) == 0 &&
		len(context.Kinds) == 0 &&
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

// sentIdentityConditions filters before logical grouping so drafts cannot
// become chat anchors or change identity counts. Explore keeps the full set.
func sentIdentityConditions(conditions string) string {
	return conditions + ` AND NOT EXISTS (
		SELECT 1 FROM relationship_unsent_messages a
		WHERE a.message_id = analytical_entries.message_id
	)`
}

// sentIdentityConditions rejects contact reads until the committed exclusion
// set loads successfully. Base-message lookups can keep using their Parquet.
func (e *DuckDBEngine) sentIdentityConditions(conditions string) (string, error) {
	if err := e.relationshipQuerySetsError(); err != nil {
		return "", err
	}
	return sentIdentityConditions(conditions), nil
}

func (e *DuckDBEngine) relationshipQuerySetsError() error {
	e.optColsMu.RLock()
	defer e.optColsMu.RUnlock()
	return e.unsentRefreshErr
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
	conditions, err = e.sentIdentityConditions(conditions)
	if err != nil {
		return "", nil, err
	}
	meetingCTE, meetingArgs, err := e.meetingScopeCTE(request)
	if err != nil {
		return "", nil, err
	}
	args = append(args, meetingArgs...)
	// Numeric entry IDs keep the per-person deduplication hash compact. Message
	// IDs are positive, chat IDs are negated, and meeting IDs lie below int64.
	sql := buildExploreLogicalSQLNoLists(conditions) + `
), ` + meetingCTE + `, meeting_person AS (
	SELECT a.entry_num, a.canonical_id, a.source_type,
	       bool_or(a.is_author) AS is_author, bool_or(a.is_owner) AS is_owner,
	       min(a.occurred_at) AS occurred_at, bool_or(a.is_from_me) AS is_from_me,
	       arg_min(a.message_type, a.message_id) AS message_type,
	       arg_min(a.entry_kind, a.message_id) AS entry_kind,
	       max(a.attachment_count) AS attachment_count
	FROM meeting_activity a
	GROUP BY a.entry_num, a.canonical_id, a.source_type
), message_person AS (
	SELECT a.message_id, a.canonical_id,
	       bool_or(a.is_author) AS is_author,
	       bool_or(a.is_owner) AS is_owner
	FROM relationship_activity_expanded a
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
	JOIN relationship_activity_expanded a ON a.message_id = f.message_id
	JOIN (
		SELECT conversation_id, anchor_message_id FROM logical_entries
		WHERE entry_kind = 'conversation'
	) anchors ON anchors.conversation_id = f.conversation_id
	WHERE f.is_chat
	  AND a.canonical_id IS NOT NULL
	  AND (a.is_direct OR a.is_conversation_member)
	GROUP BY f.conversation_id, a.canonical_id
), person_flags AS (
	SELECT le.anchor_message_id::DECIMAL(38,0) AS entry_num, mp.canonical_id,
	       mp.is_author, mp.is_owner,
	       le.occurred_at, le.is_from_me, le.message_type, le.entry_kind,
	       le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN message_person mp ON mp.message_id = le.anchor_message_id
	WHERE le.entry_kind NOT IN ('conversation', 'event', 'meeting')
	UNION ALL
	SELECT -le.conversation_id::DECIMAL(38,0) AS entry_num, cp.canonical_id,
	       cp.is_author, cp.is_owner,
	       le.occurred_at, le.is_from_me, le.message_type, le.entry_kind,
	       le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN chat_person cp ON cp.conversation_id = le.conversation_id
	WHERE le.entry_kind = 'conversation'
	UNION ALL
	SELECT entry_num, canonical_id, is_author, is_owner,
	       occurred_at, is_from_me, message_type, entry_kind,
	       attachment_count, source_type
	FROM meeting_person
), owner_presence AS (
	SELECT entry_num, bool_or(is_owner) OR EXISTS (
		SELECT 1 FROM relationship_counting_activity owner_edge
		WHERE owner_edge.entry_num = person_flags.entry_num AND owner_edge.is_owner
	) AS with_owner
	FROM person_flags
	GROUP BY entry_num
), logical_people AS (
	SELECT pf.*, op.with_owner
	FROM person_flags pf
	JOIN owner_presence op USING (entry_num)
), file_people AS (
	SELECT canonical_id, sum(attachment_count)::BIGINT AS file_count FROM (
		SELECT DISTINCT a.message_id, a.canonical_id, a.attachment_count
		FROM relationship_activity_expanded a JOIN classified f ON f.message_id=a.message_id
		WHERE a.canonical_id IS NOT NULL AND a.attachment_count > 0 AND a.entry_kind NOT IN ('event','meeting') AND (a.is_direct OR a.is_conversation_member)
		UNION
		SELECT a.message_id, a.canonical_id, a.attachment_count FROM meeting_activity a WHERE a.attachment_count > 0
	) GROUP BY canonical_id
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
	request ExploreRequest, exactDomain string,
) (string, []any, error) {
	var err error
	request, err = e.narrowIdentityFactCandidates(ctx, request)
	if err != nil {
		return "", nil, err
	}
	conditions, args := buildExploreConditions(request)
	conditions, err = e.sentIdentityConditions(conditions)
	if err != nil {
		return "", nil, err
	}
	meetingCTE, meetingArgs, err := e.meetingScopeCTE(request)
	if err != nil {
		return "", nil, err
	}
	args = append(args, meetingArgs...)
	domainCondition, domainArgs := exactDomainPredicates(exactDomain)
	args = append(args, domainArgs...)
	sql := buildExploreLogicalSQLNoLists(conditions) + `
), ` + meetingCTE + `, logical_domains AS (
	SELECT le.anchor_message_id AS entry_num, a.participant_domain AS domain,
	       le.occurred_at, le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN relationship_activity_expanded a ON a.message_id = le.anchor_message_id
	WHERE le.entry_kind NOT IN ('conversation','event','meeting')
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> ''` + domainCondition + `
	UNION
	SELECT -le.conversation_id AS entry_num, a.participant_domain AS domain,
	       le.occurred_at, le.attachment_count, le.source_type
	FROM logical_entries le
	JOIN classified f ON f.conversation_id = le.conversation_id AND f.is_chat
	JOIN relationship_activity_expanded a ON a.message_id = f.message_id
	WHERE le.entry_kind = 'conversation'
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> ''` + domainCondition + `
	UNION
	SELECT a.entry_num, a.participant_domain AS domain, min(a.occurred_at) AS occurred_at,
	       max(a.attachment_count) AS attachment_count, a.source_type
	FROM meeting_activity a WHERE a.participant_domain <> ''` + domainCondition + `
	GROUP BY a.entry_num, a.participant_domain, a.source_type
), logical_person_domains AS (
	SELECT DISTINCT a.participant_domain AS domain, a.canonical_id
	FROM logical_entries le
	JOIN relationship_activity_expanded a ON a.message_id = le.anchor_message_id
	WHERE le.entry_kind NOT IN ('conversation','event','meeting')
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> '' AND a.is_direct` + domainCondition + `
	UNION
	SELECT a.participant_domain AS domain, a.canonical_id
	FROM logical_entries le
	JOIN classified f ON f.conversation_id = le.conversation_id AND f.is_chat
	JOIN relationship_activity_expanded a ON a.message_id = f.message_id
	WHERE le.entry_kind = 'conversation'
	  AND a.canonical_id IS NOT NULL
	  AND a.participant_domain <> ''` + domainCondition + `
	UNION
	SELECT DISTINCT a.participant_domain AS domain, a.canonical_id
	FROM meeting_activity a WHERE a.participant_domain <> '' AND a.is_direct` + domainCondition + `
), file_domains AS (
	SELECT domain, sum(attachment_count)::BIGINT AS file_count FROM (
		SELECT DISTINCT a.message_id, a.participant_domain AS domain, a.attachment_count
		FROM relationship_activity_expanded a JOIN classified f ON f.message_id=a.message_id
		WHERE a.canonical_id IS NOT NULL AND a.attachment_count > 0 AND a.participant_domain <> '' AND a.entry_kind NOT IN ('event','meeting') AND (a.is_direct OR a.is_conversation_member)` + domainCondition + `
		UNION
		SELECT a.message_id, a.participant_domain AS domain, a.attachment_count FROM meeting_activity a WHERE a.attachment_count > 0 AND a.participant_domain <> ''` + domainCondition + `
	) GROUP BY domain
)`
	return sql, args, nil
}

// exactDomainPredicates narrows edge scans without narrowing the message scope.
func exactDomainPredicates(domain string) (string, []any) {
	if domain == "" {
		return "", nil
	}
	return " AND a.participant_domain = ?", []any{domain, domain, domain, domain, domain, domain, domain, domain}
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
	conditions, conditionArgs := buildIdentityFactConditions(request)
	// Every message-level column of f is the same on each of a message's edge
	// rows, and every qualifying message has an edge row matching its first
	// participant or domain group. Matching that group on f directly lets
	// DuckDB push it into the expanded view. With only the EXISTS conditions,
	// f read every edge in the archive, including each expanded chat roster.
	leading, args := identityLeadingEdgeCondition(request)
	args = append(args, conditionArgs...)
	queryText := `
SELECT f.message_id
FROM relationship_counting_activity f
WHERE ` + leading + ` AND ` + conditions + `
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

// identityLeadingEdgeCondition renders the request's first participant or
// domain group as a predicate on the edge alias f.
func identityLeadingEdgeCondition(request ExploreRequest) (string, []any) {
	var parts []string
	var args []any
	switch {
	case len(request.Context.ParticipantIDs) > 0:
		for _, value := range request.Context.ParticipantIDs {
			parts = append(parts, "f.canonical_id = ?")
			args = append(args, value)
		}
	case len(request.Context.Domains) > 0:
		for _, value := range request.Context.Domains {
			parts = append(parts, "lower(f.participant_domain) = ?")
			args = append(args, strings.ToLower(strings.TrimSpace(value)))
		}
	default:
		return "true", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// buildIdentityFactConditions renders the message-level filter for the alias
// f over the expanded relationship activity view.
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
	if scopeConditions, scopeArgs := search.AccountScopeConditions(request.Context.AccountScopes, "account_m"); len(scopeConditions) > 0 {
		conditions = append(conditions, "f.anchor_message_id IN (SELECT account_m.id FROM messages account_m WHERE "+
			strings.Join(scopeConditions, " AND ")+")")
		args = append(args, scopeArgs...)
	}
	if identityCondition, identityArgs := buildIdentityPredicateCondition(request.Context.Identity, "identity_entry.", func(column string) string { return "LOWER(" + column + ")" }, "envelope_address"); identityCondition != "" {
		conditions = append(conditions, `(EXISTS (
			SELECT 1 FROM analytical_entries identity_entry
			WHERE identity_entry.message_id = f.message_id
			  AND `+identityCondition+`
		))`)
		args = append(args, identityArgs...)
	}
	participantPredicate := `(EXISTS (
		SELECT 1
		FROM relationship_activity_expanded edge
		WHERE edge.message_id = f.message_id
		  AND edge.canonical_id = ?
	))`
	domainPredicate := `(EXISTS (
		SELECT 1
		FROM relationship_activity_expanded edge
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
	if len(request.Context.Kinds) > 0 {
		placeholders := make([]string, len(request.Context.Kinds))
		for i, kind := range request.Context.Kinds {
			placeholders[i] = "?"
			args = append(args, string(kind))
		}
		conditions = append(conditions, "(CASE WHEN f.is_chat THEN 'conversation' ELSE f.entry_kind END) IN ("+strings.Join(placeholders, ",")+")")
	}
	if request.Context.After != nil {
		conditions = append(conditions, "(f.entry_kind IN ('event', 'meeting') OR f.occurred_at >= CAST(? AS TIMESTAMP))")
		args = append(args, duckDBDateParam(*request.Context.After))
	}
	if request.Context.Before != nil {
		conditions = append(conditions, "(f.entry_kind IN ('event', 'meeting') OR f.occurred_at < CAST(? AS TIMESTAMP))")
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
			conditions = append(conditions, "f.message_id IN (SELECT UNNEST(?::BIGINT[]))")
			args = append(args, request.Search.CandidateMessageIDs)
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
	order, err := identitySearchOrder(request.Sort, identityDisplayLabelField, "person_id")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.Query) != "" && request.Explore.Search.Mode == SearchNone && (request.Sort.Field == "" || request.Sort.Field == "activity_count") {
		order = "match_rank ASC, current_temperature DESC, last_at DESC, person_id ASC"
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
	} else if identityRequestIsSourceOnly(request.Explore) && len(request.Explore.Context.SourceIDs) == 1 &&
		!e.sourceRollupFastPathDisabled {
		queryText = e.sourceFilteredPeopleSQL(candidates, order, 1)
		args = append(args, candidateArgs...)
		args = append(args, request.Explore.Context.SourceIDs[0])
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
			&row.CurrentRelationshipTemperature,
			&row.PeakRelationshipTemperature,
			&row.PeakRelationshipYear,
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
	searchText = strings.TrimSpace(searchText)
	if searchText == "" {
		return `
			SELECT d.*, d.canonical_id AS person_id, 0::INTEGER AS match_rank
			FROM read_parquet('` + directory + `') d
		`, nil
	}
	return `
		SELECT * FROM (
			SELECT d.*, d.canonical_id AS person_id,
			       list_min(list_transform(
			           list_filter(d.search_values, lambda value: contains(value, lower(?))),
			           lambda value: CASE
			               WHEN value = lower(?) THEN 0
			               WHEN starts_with(value, lower(?)) THEN 1
			               ELSE 2
			           END))::INTEGER AS match_rank
			FROM read_parquet('` + directory + `') d
		) matched
		WHERE match_rank IS NOT NULL
	`, []any{searchText, searchText, searchText}
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
person_stats AS (
	SELECT p.canonical_id, p.source_type,
	       count(*)::BIGINT AS source_count,
	       coalesce(sum(p.attachment_count), 0)::BIGINT AS file_count,
	       min(p.occurred_at)::TIMESTAMP AS first_at,
	       max(p.occurred_at)::TIMESTAMP AS last_at
	FROM person_flags p
	GROUP BY p.canonical_id, p.source_type
), meeting_totals AS (
	SELECT m.canonical_id, count(*) - count(DISTINCT m.entry_num) AS duplicate_count, count(DISTINCT m.entry_num) FILTER (WHERE EXISTS (SELECT 1 FROM relationship_counting_activity o WHERE o.entry_num = m.entry_num AND o.is_owner))::BIGINT AS meeting_count FROM meeting_person m
	GROUP BY m.canonical_id
), person_totals AS (
	SELECT p.canonical_id,
	       (sum(source_count) - coalesce(max(m.duplicate_count), 0))::BIGINT AS activity_count,
	       coalesce(max(m.meeting_count), 0)::BIGINT AS meeting_count,
	       coalesce((SELECT fp.file_count FROM file_people fp WHERE fp.canonical_id=p.canonical_id), 0)::BIGINT AS file_count,
	       min(first_at) AS first_at, max(last_at) AS last_at
	FROM person_stats p LEFT JOIN meeting_totals m USING (canonical_id)
	GROUP BY p.canonical_id
), person_sources AS (
	SELECT canonical_id,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM person_stats
	GROUP BY canonical_id
), population AS (
	SELECT c.canonical_id, c.person_id, c.display_label, c.partial_label,
	       c.member_ids, c.search_values, c.is_owner,
	       c.match_rank,
	       c.current_temperature, c.peak_temperature, c.peak_year,
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
	       c.match_rank,
	       c.current_temperature, c.peak_temperature, c.peak_year,
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
	       p.current_temperature,
	       p.peak_temperature,
	       p.peak_year,
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
			request.Explore, "",
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
	       count(DISTINCT entry_num)::BIGINT AS source_count,
	       coalesce(sum(attachment_count), 0)::BIGINT AS file_count,
	       min(occurred_at)::TIMESTAMP AS first_at,
	       max(occurred_at)::TIMESTAMP AS last_at
	FROM logical_domains
	WHERE ` + domainWhere + `
	GROUP BY GROUPING SETS ((domain, source_type), (domain))
), domain_totals AS (
	SELECT domain, source_count AS activity_count,
	       coalesce((SELECT fd.file_count FROM file_domains fd WHERE fd.domain=domain_stats.domain), 0)::BIGINT AS file_count,
	       first_at, last_at
	FROM domain_stats WHERE source_type IS NULL
), domain_sources AS (
	SELECT domain,
	       list(struct_pack(source_type := source_type, count := source_count)
	            ORDER BY source_type) AS source_counts
	FROM domain_stats
	WHERE source_type IS NOT NULL
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
