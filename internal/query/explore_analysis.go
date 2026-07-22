package query

import (
	"context"
	"fmt"
	"strings"
)

const maxExploreFilesLimit = 100

func (e *DuckDBEngine) ExploreGroups(ctx context.Context, request ExploreGroupRequest) (*ExploreGroupResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateExploreAnalysisPage(request.Page, maxExploreLimit); err != nil {
		return nil, err
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
	}
	groupExpr, labelExpr, fromSuffix, whereSuffix, err := exploreGroupExpressions(request.Dimension)
	if err != nil {
		return nil, err
	}
	order, err := exploreGroupOrder(request.Sort)
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
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	queryText := buildExploreLogicalSQL(conditions) + `
), grouped AS (
	SELECT ` + groupExpr + ` AS group_key, ` + labelExpr + ` AS group_label,
		COUNT(*)::BIGINT AS group_count,
		COALESCE(SUM(estimated_bytes), 0)::BIGINT AS estimated_bytes,
		MAX(occurred_at) AS latest_at
	FROM logical_entries` + fromSuffix + whereSuffix + `
	GROUP BY ` + groupExpr + `
), counted AS (
	SELECT *, COUNT(*) OVER () AS total_count FROM grouped
)
SELECT group_key, group_label, group_count, estimated_bytes, latest_at, total_count
FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, request.Page.Offset)
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("group analytical entries: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &ExploreGroupResponse{
		Rows: make([]ExploreGroupRow, 0), CacheRevision: state.Revision(), SearchProvenance: provenance,
	}
	for rows.Next() {
		var row ExploreGroupRow
		if err := rows.Scan(&row.Key, &row.Label, &row.Count, &row.EstimatedBytes, &row.LatestAt, &response.TotalCount); err != nil {
			return nil, fmt.Errorf("scan analytical group: %w", err)
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical groups: %w", err)
	}
	return response, nil
}

func exploreGroupExpressions(dimension string) (key, label, fromSuffix, whereSuffix string, err error) {
	switch dimension {
	case "source":
		return "CAST(source_id AS VARCHAR)", "arg_max(source_identifier, occurred_at)", "", "", nil
	case "participant":
		participantLabel := `COALESCE(
			NULLIF(TRIM(grouped_participant.display_name), ''),
			NULLIF(TRIM(grouped_participant.phone_number), ''),
			NULLIF(TRIM(grouped_participant.email_address), ''),
			CAST(grouped_participant.id AS VARCHAR)
		)`
		return "CAST(group_value AS VARCHAR)", "arg_max(" + participantLabel + ", occurred_at)",
			" CROSS JOIN UNNEST(participant_ids) AS grouped(group_value)" +
				" JOIN participants AS grouped_participant ON grouped_participant.id = group_value", "", nil
	case "domain":
		return "group_value", "group_value", ", UNNEST(participant_domains) AS grouped(group_value)", " WHERE group_value <> ''", nil
	case messageTypeDimension:
		return messageTypeDimension, messageTypeDimension, "", "", nil
	case "kind":
		return "entry_kind", "entry_kind", "", "", nil
	case "year":
		return "strftime(occurred_at, '%Y')", "strftime(occurred_at, '%Y')", "", "", nil
	case timeGranularityMonth:
		return "strftime(occurred_at, '%Y-%m')", "strftime(occurred_at, '%Y-%m')", "", "", nil
	default:
		return "", "", "", "", fmt.Errorf("%w: unknown group dimension %q", ErrInvalidExploreRequest, dimension)
	}
}

func exploreGroupOrder(sort SortSpec) (string, error) {
	if sort.Field == "" {
		sort = SortSpec{Field: "count", Direction: sortDirectionDesc}
	}
	direction, ok := sqlSortDirections[sort.Direction]
	if !ok {
		return "", fmt.Errorf("%w: unknown group sort direction %q", ErrInvalidExploreRequest, sort.Direction)
	}
	var column string
	switch sort.Field {
	case "key":
		column = "group_key"
	case "count":
		column = "group_count"
	case "estimated_bytes":
		column = "estimated_bytes"
	case "latest_at":
		column = "latest_at"
	default:
		return "", fmt.Errorf("%w: unknown group sort field %q", ErrInvalidExploreRequest, sort.Field)
	}
	return column + " " + direction + ", group_key ASC", nil
}

func (e *DuckDBEngine) ExploreSelectionStats(ctx context.Context, request ExploreSelectionRequest) (*ExploreSelectionStats, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
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
	selectionConditions := make([]string, 0, 2)
	if request.IncludedKeys != nil {
		if len(request.IncludedKeys) == 0 {
			selectionConditions = append(selectionConditions, "false")
		} else {
			placeholders := make([]string, len(request.IncludedKeys))
			for i, key := range request.IncludedKeys {
				placeholders[i] = "?"
				args = append(args, key)
			}
			selectionConditions = append(selectionConditions, "entry_key IN ("+strings.Join(placeholders, ",")+")")
		}
	}
	if len(request.ExcludedKeys) > 0 {
		placeholders := make([]string, len(request.ExcludedKeys))
		for i, key := range request.ExcludedKeys {
			placeholders[i] = "?"
			args = append(args, key)
		}
		selectionConditions = append(selectionConditions, "entry_key NOT IN ("+strings.Join(placeholders, ",")+")")
	}
	selection := ""
	if len(selectionConditions) > 0 {
		selection = " WHERE " + strings.Join(selectionConditions, " AND ")
	}
	queryText := buildExploreLogicalSQL(conditions) + `
)
SELECT COUNT(*)::BIGINT, COALESCE(SUM(estimated_bytes), 0)::BIGINT,
	COALESCE(SUM(CASE WHEN deletable THEN 1 ELSE 0 END), 0)::BIGINT,
	COALESCE(SUM(attachment_count), 0)::BIGINT,
	COALESCE(SUM(CASE WHEN attachment_count > 0 THEN 1 ELSE 0 END), 0)::BIGINT,
	COALESCE(SUM(CASE WHEN deletable THEN 1 ELSE 0 END), 0)::BIGINT,
	CASE WHEN COUNT(*) = 1 AND MIN(entry_kind) <> 'conversation'
		THEN MIN(anchor_message_id) ELSE NULL END
FROM logical_entries` + selection
	response := &ExploreSelectionStats{CacheRevision: state.Revision(), SearchProvenance: provenance}
	if err := e.db.QueryRowContext(ctx, queryText, args...).Scan(
		&response.Count, &response.EstimatedBytes, &response.DeletableCount, &response.FileCount,
		&response.ExportableCount, &response.OpenableCount, &response.RawExportMessageID,
	); err != nil {
		return nil, fmt.Errorf("preflight analytical selection: %w", err)
	}
	if request.IncludeDeletableMessageIDs {
		deletableSelection := selection
		if deletableSelection == "" {
			deletableSelection = " WHERE deletable"
		} else {
			deletableSelection += " AND deletable"
		}
		rows, err := e.db.QueryContext(ctx, buildExploreLogicalSQL(conditions)+`)
SELECT anchor_message_id FROM logical_entries`+deletableSelection+`
ORDER BY anchor_message_id`, args...)
		if err != nil {
			return nil, fmt.Errorf("resolve deletable analytical selection: %w", err)
		}
		defer func() { _ = rows.Close() }()
		response.DeletableMessageIDs = make([]int64, 0, response.DeletableCount)
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, fmt.Errorf("scan deletable analytical selection: %w", err)
			}
			response.DeletableMessageIDs = append(response.DeletableMessageIDs, id)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterate deletable analytical selection: %w", err)
		}
	}
	return response, nil
}

func (e *DuckDBEngine) ExploreFiles(ctx context.Context, request ExploreFilesRequest) (*ExploreFilesResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateExploreAnalysisPage(request.Page, maxExploreFilesLimit); err != nil {
		return nil, err
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
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
	limit := request.Page.Limit
	if limit == 0 {
		limit = maxExploreFilesLimit
	}
	queryText := `
WITH selected AS (
	SELECT * FROM analytical_entries WHERE ` + conditions + `
), facts AS (
	SELECT
		` + sqlEntryKeyExpr("s.") + ` AS entry_key,
		s.message_id, s.conversation_id, s.occurred_at, s.source_id, s.source_identifier,
		COALESCE(NULLIF(s.subject, ''), NULLIF(s.conversation_title, ''), s.snippet, '') AS title,
		a.attachment_id, a.filename, a.mime_type, a.size
	FROM selected s JOIN attachments a ON a.message_id = s.message_id
), keyed AS (
	SELECT entry_key || ':file:' || CAST(attachment_id AS VARCHAR) AS file_key,
		*, COUNT(*) OVER () AS total_count
	FROM facts
)
SELECT attachment_id, file_key, entry_key, message_id, conversation_id, occurred_at, source_id,
	source_identifier, title, filename, mime_type, size, total_count
FROM keyed ORDER BY occurred_at DESC, message_id ASC, filename ASC, size ASC, attachment_id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, request.Page.Offset)
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("list analytical files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &ExploreFilesResponse{
		Files: make([]ExploreFileFact, 0), CacheRevision: state.Revision(), SearchProvenance: provenance,
	}
	for rows.Next() {
		var fact ExploreFileFact
		if err := rows.Scan(&fact.ID, &fact.Key, &fact.EntryKey, &fact.MessageID, &fact.ConversationID,
			&fact.OccurredAt, &fact.SourceID, &fact.SourceIdentifier, &fact.Title, &fact.Filename,
			&fact.MimeType, &fact.Size, &response.TotalCount); err != nil {
			return nil, fmt.Errorf("scan analytical file: %w", err)
		}
		response.Files = append(response.Files, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical files: %w", err)
	}
	return response, nil
}

func validateExploreAnalysisPage(page PageSpec, maxLimit int) error {
	if page.Offset < 0 || page.Limit < 0 || page.Limit > maxLimit {
		return fmt.Errorf("%w: page is outside the supported range", ErrInvalidExploreRequest)
	}
	return nil
}

func (e *DuckDBEngine) ExploreMatchCounts(ctx context.Context, request ExploreMatchCountsRequest) (*ExploreMatchCountsResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if len(request.RowKeys) == 0 || len(request.RowKeys) > maxExploreLimit {
		return nil, fmt.Errorf("%w: match-count row keys are outside the supported range", ErrInvalidExploreRequest)
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
	}
	if request.Explore.Search.Mode != SearchFullText {
		return nil, fmt.Errorf("%w: exact match counts require full-text search", ErrInvalidExploreRequest)
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
	placeholders := make([]string, len(request.RowKeys))
	for i, key := range request.RowKeys {
		placeholders[i] = "?"
		args = append(args, key)
	}
	queryText := buildExploreLogicalSQL(conditions) + `
)
SELECT entry_key, message_count FROM logical_entries
WHERE entry_key IN (` + strings.Join(placeholders, ",") + `)`
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("count analytical lexical matches: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &ExploreMatchCountsResponse{
		Counts: make(map[string]int64, len(request.RowKeys)), CacheRevision: state.Revision(), SearchProvenance: provenance,
	}
	for _, key := range request.RowKeys {
		response.Counts[key] = 0
	}
	for rows.Next() {
		var key string
		var count int64
		if err := rows.Scan(&key, &count); err != nil {
			return nil, fmt.Errorf("scan analytical lexical match count: %w", err)
		}
		response.Counts[key] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical lexical match counts: %w", err)
	}
	return response, nil
}
