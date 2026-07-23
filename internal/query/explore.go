package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrInvalidExploreRequest = errors.New("invalid exploration request")

const defaultExploreLimit = 100
const maxExploreLimit = 1000

// Explore projects the committed analytical cache into modality-neutral row
// units. It never consults the transactional database.
func (e *DuckDBEngine) Explore(ctx context.Context, request ExploreRequest) (*ExploreResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	if request.Page.Offset < 0 || request.Page.Limit < 0 || request.Page.Limit > maxExploreLimit {
		return nil, fmt.Errorf("%w: page is outside the supported range", ErrInvalidExploreRequest)
	}
	if request.Context.Deletion != DeletionAny &&
		request.Context.Deletion != DeletionActive &&
		request.Context.Deletion != DeletionDeleted {
		return nil, fmt.Errorf("%w: unknown deletion filter %q", ErrInvalidExploreRequest, request.Context.Deletion)
	}
	if len(request.Grouping) > 0 {
		return nil, fmt.Errorf("%w: grouped exploration is not available in the entry-row query", ErrInvalidExploreRequest)
	}
	if request.Presentation != PresentationDefault && request.Presentation != PresentationTable {
		return nil, fmt.Errorf("%w: unsupported presentation %q", ErrInvalidExploreRequest, request.Presentation)
	}
	if len(request.Sort) > 1 || (len(request.Sort) == 1 &&
		(request.Sort[0].Field != "sent_at" || request.Sort[0].Direction != sortDirectionDesc)) {
		return nil, fmt.Errorf("%w: only sent_at descending sort is supported", ErrInvalidExploreRequest)
	}
	searchProvenance, err := validateResolvedSearch(request.Search)
	if err != nil {
		return nil, err
	}

	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	conditions, conditionArgs := buildExploreConditions(request)
	candidateRankExpression, candidateRankArgs := buildExploreCandidateRank(request.Search)
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	countArgs := append(append([]any{}, conditionArgs...), candidateRankArgs...)
	args := append(append([]any{}, countArgs...), limit, request.Page.Offset)
	var queryText string
	if !e.exploreFastPathDisabled && !exploreConditionsTouchParticipantLists(request) {
		queryText = buildExploreFastListingSQL(conditions, candidateRankExpression,
			e.parquetPath(datasetParticipantClusters), e.parquetPath(datasetOwnerParticipants))
		args = append(args, conditionArgs...) // membership rescan
		args = append(args, conditionArgs...) // total-count scan
	} else {
		queryText = buildExploreSQL(conditions, candidateRankExpression,
			e.parquetPath(datasetParticipantClusters), e.parquetPath(datasetOwnerParticipants))
	}

	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("explore analytical entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	response := &ExploreResponse{
		Rows:             make([]EntryRow, 0),
		CacheRevision:    state.Revision(),
		SearchProvenance: searchProvenance,
	}
	for rows.Next() {
		var row EntryRow
		var anchorID, conversationID, strongestMatchedMessageID, counterpartParticipantID sql.NullInt64
		var participantIDsJSON, participantLabelsJSON string
		if err := rows.Scan(
			&row.Key, &row.Kind, &anchorID, &conversationID, &row.OccurredAt,
			&row.SourceID, &row.SourceType, &row.SourceIdentifier,
			&row.MessageType, &row.ConversationType, &row.Title, &row.Preview,
			&participantIDsJSON, &participantLabelsJSON, &strongestMatchedMessageID, &row.MessageCount,
			&row.HasAttachments, &row.AttachmentCount, &row.AttachmentSize,
			&row.DeletedFromSource, &response.TotalCount, &counterpartParticipantID,
		); err != nil {
			return nil, fmt.Errorf("scan analytical entry: %w", err)
		}
		if row.Title == row.Preview {
			row.Title = FlattenSnippet(row.Title)
		}
		row.Preview = FlattenSnippet(row.Preview)
		if anchorID.Valid {
			row.AnchorMessageID = &anchorID.Int64
		}
		if conversationID.Valid {
			row.ConversationID = &conversationID.Int64
		}
		if strongestMatchedMessageID.Valid {
			row.StrongestMatchedMessageID = &strongestMatchedMessageID.Int64
		}
		if counterpartParticipantID.Valid {
			row.CounterpartParticipantID = &counterpartParticipantID.Int64
		}
		if err := json.Unmarshal([]byte(participantIDsJSON), &row.ParticipantIDs); err != nil {
			return nil, fmt.Errorf("decode analytical participant IDs: %w", err)
		}
		if err := json.Unmarshal([]byte(participantLabelsJSON), &row.ParticipantLabels); err != nil {
			return nil, fmt.Errorf("decode analytical participant labels: %w", err)
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical entries: %w", err)
	}
	if len(response.Rows) == 0 && request.Page.Offset > 0 {
		if err := e.db.QueryRowContext(ctx, buildExploreCountSQL(conditions, candidateRankExpression), countArgs...).Scan(&response.TotalCount); err != nil {
			return nil, fmt.Errorf("count analytical entries beyond page: %w", err)
		}
	}
	return response, nil
}

const (
	defaultExploreCoverageBatchSize = 256
	maxExploreCoverageBatchSize     = 512
)

// ExploreCoverage resolves the exact live message-ID population of the
// committed analytical context in one streamed query. It counts the
// population set-wise and invokes visit with bounded, strictly ascending
// batches so callers can intersect the population with a vector index
// without paging the archive. It deliberately does not aggregate chat
// messages into row units: each message is independently eligible for an
// embedding. A visit error aborts the scan and is returned verbatim.
func (e *DuckDBEngine) ExploreCoverage(
	ctx context.Context,
	request ExploreCoverageRequest,
	visit func(messageIDs []int64) error,
) (*ExploreCoverageResult, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if request.Context.Deletion != DeletionAny &&
		request.Context.Deletion != DeletionActive &&
		request.Context.Deletion != DeletionDeleted {
		return nil, fmt.Errorf("%w: unknown deletion filter %q", ErrInvalidExploreRequest, request.Context.Deletion)
	}
	batchSize := request.BatchSize
	if batchSize == 0 {
		batchSize = defaultExploreCoverageBatchSize
	}
	if batchSize < 1 || batchSize > maxExploreCoverageBatchSize {
		return nil, fmt.Errorf("%w: coverage batch size must be between 1 and %d", ErrInvalidExploreRequest, maxExploreCoverageBatchSize)
	}
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	conditions, args := buildExploreConditions(ExploreRequest{Context: request.Context})
	conditions += " AND NOT internally_deleted AND NOT deleted_from_source"
	rows, err := e.db.QueryContext(ctx,
		"SELECT message_id FROM analytical_entries WHERE "+conditions+" ORDER BY message_id", args...)
	if err != nil {
		return nil, fmt.Errorf("resolve analytical coverage context: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := &ExploreCoverageResult{CacheRevision: state.Revision()}
	batch := make([]int64, 0, batchSize)
	var previous int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan analytical coverage message: %w", err)
		}
		if id <= previous {
			return nil, fmt.Errorf("analytical coverage scan is not strictly ordered after message %d", previous)
		}
		previous = id
		result.EligibleCount++
		batch = append(batch, id)
		if len(batch) == batchSize {
			if visit != nil {
				if err := visit(batch); err != nil {
					return nil, err
				}
			}
			batch = batch[:0]
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical coverage messages: %w", err)
	}
	if len(batch) > 0 && visit != nil {
		if err := visit(batch); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func buildExploreCandidateRank(search SearchSpec) (string, []any) {
	if search.Mode != SearchSemantic && search.Mode != SearchHybrid {
		return "NULL::BIGINT", nil
	}
	parts := make([]string, len(search.CandidateMessageIDs))
	args := make([]any, len(search.CandidateMessageIDs))
	for i, messageID := range search.CandidateMessageIDs {
		parts[i] = fmt.Sprintf("WHEN ? THEN %d", i)
		args[i] = messageID
	}
	if len(parts) == 0 {
		return "NULL::BIGINT", nil
	}
	return "CASE message_id " + strings.Join(parts, " ") + " ELSE NULL END", args
}

func validateResolvedSearch(search SearchSpec) (SearchProvenance, error) {
	if search.Mode == SearchNone {
		if search.Query != "" || search.CandidateMessageIDs != nil ||
			search.LexicalIndexRevision != "" || search.VectorGeneration != nil {
			return SearchProvenance{}, fmt.Errorf("%w: no-search mode contains search state", ErrInvalidExploreRequest)
		}
		return SearchProvenance{}, nil
	}
	if search.CandidateMessageIDs == nil {
		return SearchProvenance{}, fmt.Errorf("%w: %s search candidates are unresolved", ErrInvalidExploreRequest, search.Mode)
	}
	switch search.Mode {
	case SearchFullText:
		if search.LexicalIndexRevision == "" {
			return SearchProvenance{}, fmt.Errorf("%w: full-text lexical index revision is unresolved", ErrInvalidExploreRequest)
		}
		if search.VectorGeneration != nil {
			return SearchProvenance{}, fmt.Errorf("%w: full-text search contains vector provenance", ErrInvalidExploreRequest)
		}
		return SearchProvenance{LexicalIndexRevision: search.LexicalIndexRevision}, nil
	case SearchSemantic:
		if search.VectorGeneration == nil {
			return SearchProvenance{}, fmt.Errorf("%w: semantic vector generation is unresolved", ErrInvalidExploreRequest)
		}
		if search.LexicalIndexRevision != "" {
			return SearchProvenance{}, fmt.Errorf("%w: semantic search contains lexical provenance", ErrInvalidExploreRequest)
		}
		return SearchProvenance{VectorGeneration: search.VectorGeneration}, nil
	case SearchHybrid:
		if search.LexicalIndexRevision == "" {
			return SearchProvenance{}, fmt.Errorf("%w: hybrid lexical index revision is unresolved", ErrInvalidExploreRequest)
		}
		if search.VectorGeneration == nil {
			return SearchProvenance{}, fmt.Errorf("%w: hybrid vector generation is unresolved", ErrInvalidExploreRequest)
		}
		return SearchProvenance{
			LexicalIndexRevision: search.LexicalIndexRevision,
			VectorGeneration:     search.VectorGeneration,
		}, nil
	default:
		return SearchProvenance{}, fmt.Errorf("%w: unknown search mode %q", ErrInvalidExploreRequest, search.Mode)
	}
}

func buildExploreConditions(request ExploreRequest) (string, []any) {
	var conditions []string
	var args []any
	appendIntAnyOf := func(values []int64, expression string) {
		if len(values) == 0 {
			return
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = expression
			args = append(args, value)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}
	appendStringAnyOf := func(values []string, expression string) {
		if len(values) == 0 {
			return
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = expression
			args = append(args, value)
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
	}

	appendIntAnyOf(request.Context.SourceIDs, "source_id = ?")
	if len(request.Context.ParticipantIDs) > 0 {
		parts := make([]string, len(request.Context.ParticipantIDs))
		for i := range parts {
			parts[i] = "(sender_id = ? OR list_contains(participant_ids, ?) OR list_contains(conversation_participant_ids, ?))"
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
		for _, value := range request.Context.ParticipantIDs {
			args = append(args, value, value, value)
		}
	}
	if len(request.Context.Domains) > 0 {
		parts := make([]string, len(request.Context.Domains))
		for i := range parts {
			parts[i] = "(lower(sender_domain) = lower(?) OR list_contains(participant_domains, lower(?)) OR list_contains(conversation_participant_domains, lower(?)))"
		}
		conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
		for _, value := range request.Context.Domains {
			args = append(args, value, value, value)
		}
	}
	appendStringAnyOf(request.Context.MessageTypes, "lower(message_type) = lower(?)")
	// CAST(? AS TIMESTAMP) pins the bound time to its UTC wall clock. The Go
	// DuckDB driver binds time.Time as TIMESTAMP WITH TIME ZONE; left uncast,
	// the comparison would coerce the naive-UTC occurred_at column to
	// TIMESTAMPTZ on every row — a per-row ICU session-timezone conversion
	// (see buildRelationshipsSQL for the measured cost of the same hazard).
	if request.Context.After != nil {
		conditions = append(conditions, "occurred_at >= CAST(? AS TIMESTAMP)")
		args = append(args, request.Context.After.UTC())
	}
	if request.Context.Before != nil {
		conditions = append(conditions, "occurred_at < CAST(? AS TIMESTAMP)")
		args = append(args, request.Context.Before.UTC())
	}
	switch request.Context.Deletion {
	case DeletionAny:
	case DeletionActive:
		conditions = append(conditions, "NOT deleted_from_source")
	case DeletionDeleted:
		conditions = append(conditions, "deleted_from_source")
	}
	if request.Search.CandidateMessageIDs != nil {
		if len(request.Search.CandidateMessageIDs) == 0 {
			conditions = append(conditions, "false")
		} else {
			parts := make([]string, len(request.Search.CandidateMessageIDs))
			for i, id := range request.Search.CandidateMessageIDs {
				parts[i] = "?"
				args = append(args, id)
			}
			conditions = append(conditions, "message_id IN ("+strings.Join(parts, ",")+")")
		}
	}
	if len(conditions) == 0 {
		return "true", args
	}
	return strings.Join(conditions, " AND "), args
}

// buildExploreSQL builds the entry-row page query. counterpart_participant_id
// reuses the exact owner-cluster resolution buildRelationshipsSQL uses (see
// its doc comment): owners are unioned across sources (an address confirmed
// as "me" on any account is never "the other side" of an entry, even in a
// different source's archive — see buildRelationshipsSQL on why owner
// identities are person-level) and expanded through participant_clusters so
// an owner's clustered alias is still recognized as the owner, and the
// smallest non-owner participant ID on the entry is returned. If the
// owner_participants dataset has no rows at all, the owner is unknown and
// the column is NULL — never a guess at "the other side" from
// participant_ids[0] alone.
func buildExploreSQL(conditions, candidateRankExpression, clustersGlob, ownersGlob string) string {
	return buildExploreLogicalSQLWithCandidateRank(conditions, candidateRankExpression) + fmt.Sprintf(`
), counted AS (
    SELECT *, COUNT(*) OVER () AS total_count
    FROM logical_entries
), clusters AS (
    SELECT participant_id, canonical_id FROM read_parquet('%s')
), owners AS (
    SELECT DISTINCT participant_id FROM read_parquet('%s')
), canon AS (
    SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
    FROM participants p LEFT JOIN clusters c ON c.participant_id = p.id
), owner_canon AS (
    SELECT DISTINCT cn.canonical_id FROM owners o JOIN canon cn ON cn.participant_id = o.participant_id
), owner_participant_ids AS (
    SELECT DISTINCT cn.participant_id FROM canon cn
    WHERE cn.canonical_id IN (SELECT canonical_id FROM owner_canon)
)
SELECT
    entry_key,
    entry_kind,
    anchor_message_id,
    conversation_id,
    occurred_at,
    source_id,
    source_type,
    source_identifier,
    message_type,
    conversation_type,
    title,
    preview,
    CAST(COALESCE(to_json(participant_ids), '[]') AS VARCHAR) AS participant_ids,
    CAST(COALESCE(to_json(participant_labels), '[]') AS VARCHAR) AS participant_labels,
	strongest_matched_message_id,
    message_count,
    has_attachments,
    attachment_count,
    attachment_size,
    deleted_from_source,
    total_count,
    CASE WHEN NOT EXISTS (SELECT 1 FROM owners) THEN NULL
        ELSE (SELECT MIN(pid) FROM UNNEST(participant_ids) AS u(pid)
              WHERE pid NOT IN (SELECT participant_id FROM owner_participant_ids))
    END AS counterpart_participant_id
FROM counted
ORDER BY occurred_at DESC, source_id ASC, entry_key ASC
LIMIT ? OFFSET ?`, clustersGlob, ownersGlob)
}

func buildExploreCountSQL(conditions, candidateRankExpression string) string {
	return buildExploreLogicalSQLWithCandidateRank(conditions, candidateRankExpression) + `
)
SELECT COUNT(*) FROM logical_entries`
}

// exploreConditionsTouchParticipantLists reports whether buildExploreConditions
// renders predicates over the per-message participant list columns for this
// request. Those predicates force analytical_entries to assemble participant
// lists for the whole archive during filtering, so the two-phase listing fast
// path (which rescans the filtered population) would pay that cost twice; such
// requests keep the single-pass legacy query.
func exploreConditionsTouchParticipantLists(request ExploreRequest) bool {
	return len(request.Context.ParticipantIDs) > 0 || len(request.Context.Domains) > 0
}

// buildExploreFastListingSQL builds the two-phase entry-row page query used
// when exploreConditionsTouchParticipantLists is false. Phase one selects the
// page (ORDER BY + LIMIT/OFFSET) from logical entries WITHOUT participant
// list columns, so the whole-archive per-message list aggregation inside
// analytical_entries is pruned; phase two rebuilds participant_ids and
// participant_labels for the ≤limit page rows only, from the same base tables
// with the same expressions the view uses:
//
//   - a non-conversation entry is one message (its anchor), whose analytical
//     participant list is its recipients plus its sender
//     (message_participant_links in sqlAnalyticalEntries);
//   - a conversation entry aggregates the message-level lists of the chat
//     messages that pass the same filter conditions ("membership" re-applies
//     them), plus the conversation's own participant rows. Per-message
//     conversation-level lists are constant across a group, so the flattened
//     concat in the legacy chat arm reduces to this union.
//
// The total count runs as its own aggregate over the filtered population:
// COUNT(*) OVER () on the page pipeline would materialize every pre-LIMIT
// row, and a scalar subquery on logical_entries would force DuckDB to
// materialize the doubly-referenced CTE with its string columns (both
// measured slower). Bind order: condition args (filtered), candidate-rank
// args (classified), limit, offset (page), condition args again (membership),
// condition args again (total).
//
// Output columns, ordering, and pagination are identical to buildExploreSQL;
// TestExploreListingFastPathMatchesLegacy pins the equivalence.
func buildExploreFastListingSQL(conditions, candidateRankExpression, clustersGlob, ownersGlob string) string {
	return buildExploreFilteredClassifiedCTE(conditions, candidateRankExpression) +
		exploreLogicalEntriesCTE(false) + fmt.Sprintf(`
), page AS (
    SELECT * FROM logical_entries
    ORDER BY occurred_at DESC, source_id ASC, entry_key ASC
    LIMIT ? OFFSET ?
), membership AS (
    SELECT source_id, conversation_id, message_id
    FROM analytical_entries
    WHERE (%s) AND (%s)
), page_messages AS (
    SELECT p.entry_key, p.anchor_message_id AS message_id
    FROM page p WHERE p.entry_kind <> 'conversation'
    UNION ALL
    SELECT p.entry_key, m.message_id
    FROM page p JOIN membership m
      ON m.source_id = p.source_id AND m.conversation_id = p.conversation_id
    WHERE p.entry_kind = 'conversation'
), page_participant_links AS (
    SELECT pm.entry_key, mr.participant_id
    FROM page_messages pm JOIN message_recipients mr ON mr.message_id = pm.message_id
    UNION ALL
    SELECT pm.entry_key, msg.sender_id AS participant_id
    FROM page_messages pm JOIN messages msg ON msg.id = pm.message_id
    WHERE msg.sender_id IS NOT NULL
    UNION ALL
    SELECT p.entry_key, cp.participant_id
    FROM page p JOIN conversation_participants cp ON cp.conversation_id = p.conversation_id
    WHERE p.entry_kind = 'conversation'
), page_participant_facts AS (
    SELECT links.entry_key,
        list_sort(list_distinct(list(links.participant_id))) AS participant_ids,
        list_sort(list_distinct(list(%s))) AS participant_labels
    FROM page_participant_links links
    JOIN participants pt ON pt.id = links.participant_id
    GROUP BY links.entry_key
), total AS (
    SELECT COUNT(*) FILTER (WHERE NOT is_chat)
         + COUNT(DISTINCT (source_id, conversation_id)) FILTER (WHERE is_chat) AS total_count
    FROM (
        SELECT source_id, conversation_id,
            %s AS is_chat
        FROM analytical_entries
        WHERE %s
    )
), clusters AS (
    SELECT participant_id, canonical_id FROM read_parquet('%s')
), owners AS (
    SELECT DISTINCT participant_id FROM read_parquet('%s')
), canon AS (
    SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
    FROM participants p LEFT JOIN clusters c ON c.participant_id = p.id
), owner_canon AS (
    SELECT DISTINCT cn.canonical_id FROM owners o JOIN canon cn ON cn.participant_id = o.participant_id
), owner_participant_ids AS (
    SELECT DISTINCT cn.participant_id FROM canon cn
    WHERE cn.canonical_id IN (SELECT canonical_id FROM owner_canon)
)
SELECT
    p.entry_key,
    p.entry_kind,
    p.anchor_message_id,
    p.conversation_id,
    p.occurred_at,
    p.source_id,
    p.source_type,
    p.source_identifier,
    p.message_type,
    p.conversation_type,
    p.title,
    p.preview,
    CAST(COALESCE(to_json(f.participant_ids), '[]') AS VARCHAR) AS participant_ids,
    CAST(COALESCE(to_json(f.participant_labels), '[]') AS VARCHAR) AS participant_labels,
	p.strongest_matched_message_id,
    p.message_count,
    p.has_attachments,
    p.attachment_count,
    p.attachment_size,
    p.deleted_from_source,
    (SELECT total_count FROM total) AS total_count,
    CASE WHEN NOT EXISTS (SELECT 1 FROM owners) THEN NULL
        ELSE (SELECT MIN(pid) FROM UNNEST(COALESCE(f.participant_ids, []::BIGINT[])) AS u(pid)
              WHERE pid NOT IN (SELECT participant_id FROM owner_participant_ids))
    END AS counterpart_participant_id
FROM page p LEFT JOIN page_participant_facts f ON f.entry_key = p.entry_key
ORDER BY p.occurred_at DESC, p.source_id ASC, p.entry_key ASC`,
		conditions, sqlIsChatPredicate("message_type", "conversation_type"),
		sqlAnalyticalEntriesParticipantLabel("pt"),
		sqlIsChatPredicate("message_type", "conversation_type"), conditions,
		clustersGlob, ownersGlob)
}

func buildExploreLogicalSQL(conditions string) string {
	return buildExploreLogicalSQLWithCandidateRank(conditions, "NULL::BIGINT")
}

// sqlIsChatPredicate renders the shared chat-classification predicate for a
// message row. messageType and conversationType are SQL expressions that
// must never be NULL (analytical_entries and the base views COALESCE them).
// Any query that re-derives chat membership outside the classified CTE
// (e.g. the exact-person fast path in people.go) must use this so
// classifications cannot drift.
func sqlIsChatPredicate(messageType, conversationType string) string {
	return "lower(" + messageType + ") IN (" + TextMessageTypeSQLList + `)
            OR (
                lower(` + messageType + `) IN (` + sqlQuotedList(chatFallbackMessageTypes) + `)
                AND lower(` + conversationType + `) IN (` + sqlQuotedList(chatConversationTypes) + `)
            )`
}

// buildExploreFilteredClassifiedCTE builds the "filtered" and "classified"
// CTEs shared by every query that projects analytical_entries into
// modality-neutral rows: buildExploreLogicalSQLWithCandidateRank's
// per-conversation-lifetime chat grouping, and
// buildRelationshipTimelineSQL's per-local-day chat burst grouping. The
// returned text ends with the "classified" CTE closed, ready for a caller to
// append ", <next_cte> AS (" and read from "classified".
func buildExploreFilteredClassifiedCTE(conditions, candidateRankExpression string) string {
	return `
WITH filtered AS (
    SELECT *
    FROM analytical_entries
    WHERE ` + conditions + `
), classified AS (
    SELECT *,
		` + candidateRankExpression + ` AS candidate_rank,
        ` + sqlIsChatPredicate("message_type", "conversation_type") + ` AS is_chat,
        CASE
            WHEN lower(message_type) = 'email' OR message_type = '' THEN 'email'
            WHEN lower(message_type) = '` + messageTypeCalendar + `' THEN 'event'
            WHEN lower(message_type) IN ('meeting_transcript', 'meeting_note', 'meeting_minutes') THEN 'meeting'
            ELSE 'item'
        END AS entry_kind
    FROM filtered
)`
}

func buildExploreLogicalSQLWithCandidateRank(conditions, candidateRankExpression string) string {
	return buildExploreFilteredClassifiedCTE(conditions, candidateRankExpression) +
		exploreLogicalEntriesCTE(true)
}

// exploreLogicalEntriesCTE renders the logical_entries CTE (appended directly
// after buildExploreFilteredClassifiedCTE, left unclosed for the caller).
// withParticipantLists controls whether the three participant list columns
// are projected. They come from per-message list aggregation over the whole
// archive inside analytical_entries, which dominates listing latency; the
// Explore fast path omits them here and rebuilds them for the ≤limit page
// rows only (see buildExploreFastListingSQL).
func exploreLogicalEntriesCTE(withParticipantLists bool) string {
	messageLists := ""
	conversationLists := ""
	if withParticipantLists {
		messageLists = `
        participant_ids,
        participant_labels,
		list_sort(list_distinct(list_concat(participant_domains, conversation_participant_domains))) AS participant_domains,`
		conversationLists = `
        list_sort(list_distinct(flatten(list(list_concat(participant_ids, conversation_participant_ids))))) AS participant_ids,
        list_sort(list_distinct(flatten(list(list_concat(participant_labels, conversation_participant_labels))))) AS participant_labels,
		list_sort(list_distinct(flatten(list(list_concat(participant_domains, conversation_participant_domains))))) AS participant_domains,`
	}
	return `, logical_entries AS (
    SELECT
        ` + sqlMessageEntryKeyExpr("") + ` AS entry_key,
        entry_kind,
        message_id AS anchor_message_id,
        conversation_id,
        occurred_at,
        source_id,
        source_type,
        source_identifier,
        message_type,
        conversation_type,
        COALESCE(NULLIF(subject, ''), NULLIF(conversation_title, ''), snippet, '') AS title,
        snippet AS preview,` + messageLists + `
		CASE WHEN candidate_rank IS NOT NULL THEN message_id ELSE NULL END AS strongest_matched_message_id,
		1::BIGINT AS message_count,
		(size_estimate + attachment_size)::BIGINT AS estimated_bytes,
		(entry_kind = 'email' AND lower(source_type) = 'gmail' AND NOT deleted_from_source
			AND COALESCE(source_message_id, '') <> '') AS deletable,
		has_attachments,
		is_from_me,
        attachment_count::BIGINT AS attachment_count,
        attachment_size::BIGINT AS attachment_size,
        deleted_from_source
    FROM classified
    WHERE NOT is_chat

    UNION ALL

    SELECT
        ` + sqlConversationEntryKeyExpr("") + ` AS entry_key,
        'conversation' AS entry_kind,
        arg_max(message_id, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS anchor_message_id,
        conversation_id,
        MAX(occurred_at) AS occurred_at,
        source_id,
        arg_max(source_type, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS source_type,
        arg_max(source_identifier, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS source_identifier,
        arg_max(message_type, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS message_type,
        arg_max(conversation_type, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS conversation_type,
        COALESCE(NULLIF(MAX(conversation_title), ''), 'Conversation') AS title,
        arg_max(snippet, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS preview,` + conversationLists + `
		arg_min(message_id, struct_pack(candidate_rank := candidate_rank, message_id := message_id))
			FILTER (WHERE candidate_rank IS NOT NULL) AS strongest_matched_message_id,
		COUNT(*)::BIGINT AS message_count,
		SUM(size_estimate + attachment_size)::BIGINT AS estimated_bytes,
		false AS deletable,
		bool_or(has_attachments) AS has_attachments,
		arg_max(is_from_me, struct_pack(occurred_at := occurred_at, message_id := message_id)) AS is_from_me,
        SUM(attachment_count)::BIGINT AS attachment_count,
        SUM(attachment_size)::BIGINT AS attachment_size,
        bool_or(deleted_from_source) AS deleted_from_source
    FROM classified
    WHERE is_chat
    GROUP BY source_id, conversation_id`
}
