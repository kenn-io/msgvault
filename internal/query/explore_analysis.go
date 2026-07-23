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
	spec, err := exploreGroupExpressions(request.Dimension, e.parquetPath(datasetParticipantClusters))
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
	explore, err := e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}
	conditions, args := buildExploreConditions(explore)
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	queryText := buildExploreLogicalSQL(conditions) + spec.cte + `
), grouped AS (
	SELECT ` + spec.key + ` AS group_key, ` + spec.label + ` AS group_label,
		COUNT(*)::BIGINT AS group_count,
		COALESCE(SUM(estimated_bytes), 0)::BIGINT AS estimated_bytes,
		MAX(occurred_at) AS latest_at
	FROM ` + spec.source + spec.fromSuffix + spec.whereSuffix + `
	GROUP BY ` + spec.groupBy + `
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

// groupExpressions describes how one grouping dimension renders into the
// shared grouped-aggregation SQL built by ExploreGroups and GroupFiles.
type groupExpressions struct {
	// key is the projected group_key expression; groupBy is the GROUP BY
	// expression key is functionally dependent on (identical for most
	// dimensions; the raw canonical ID for "participant" so the label's
	// scalar subquery can reference it).
	key     string
	label   string
	groupBy string
	// cte is an extra CTE chunk interposed between the population CTEs and
	// the grouped aggregate ("participant" canonicalization; empty
	// otherwise). ExploreGroups chunks open by closing the still-open
	// logical_entries CTE and are left unclosed (personEntriesCTE style);
	// GroupFiles chunks are closed ", name AS (...)" additions after the
	// closed file_population CTE.
	cte string
	// source is the FROM target of the grouped aggregate; fromSuffix and
	// whereSuffix extend its FROM/WHERE clauses (the "domain" UNNEST
	// fan-out and its empty-value guard).
	source      string
	fromSuffix  string
	whereSuffix string
}

// sqlClustersCanonCTE renders the shared clusters/canon CTE pair mapping
// every participant to its identity-cluster canonical ID (itself when
// unlinked), resolved through the committed participant_clusters dataset —
// the same resolution buildExploreSQL, buildRelationshipsSQL, and
// personEntriesCTE inline. Returned closed, without surrounding commas.
func sqlClustersCanonCTE(clustersGlob string) string {
	return fmt.Sprintf(`clusters AS (
	SELECT participant_id, canonical_id FROM read_parquet('%s')
), canon AS (
	SELECT p.id AS participant_id, COALESCE(c.canonical_id, p.id) AS canonical_id
	FROM participants p LEFT JOIN clusters c ON c.participant_id = p.id
)`, clustersGlob)
}

// sqlCanonicalPersonGroupLabelExpr renders a canonical "People" group row's
// label: the shared cluster label policy (see person_label.go) evaluated for
// the canonical participant bound as person_id, with cluster members
// resolved through the canon CTE the caller emits (sqlClustersCanonCTE) — so
// a merged row is labeled by the cluster's best display name, never by
// whichever alias happened to appear latest. person_id must be the GROUP BY
// key of the surrounding aggregate.
func sqlCanonicalPersonGroupLabelExpr() string {
	return "(SELECT " + sqlPersonDisplayLabelExpr(sqlClusterBestNameExpr(
		"pbn.id IN (SELECT cnl.participant_id FROM canon cnl WHERE cnl.canonical_id = p2.id)"), "p2") +
		" FROM participants p2 WHERE p2.id = person_id)"
}

// exploreGroupExpressions maps a grouping dimension onto the grouped
// aggregate ExploreGroups builds over logical_entries. The "participant"
// dimension groups by canonical identity-cluster IDs: raw participant_ids
// members are resolved through canon before grouping, and the DISTINCT
// mirrors personEntriesCTE/buildRelationshipsSQL — an entry listing several
// aliases of one person collapses to a single (entry, canonical) row, so the
// entry is never double-counted (entry_key is projected only to carry
// per-entry uniqueness through that DISTINCT).
func exploreGroupExpressions(dimension, clustersGlob string) (groupExpressions, error) {
	simple := func(key string) groupExpressions {
		return groupExpressions{key: key, label: key, groupBy: key, source: "logical_entries"}
	}
	switch dimension {
	case "source":
		return groupExpressions{
			key: "CAST(source_id AS VARCHAR)", label: "arg_max(source_identifier, occurred_at)",
			groupBy: "CAST(source_id AS VARCHAR)", source: "logical_entries",
		}, nil
	case "participant":
		return groupExpressions{
			key: "CAST(person_id AS VARCHAR)", label: sqlCanonicalPersonGroupLabelExpr(), groupBy: "person_id",
			cte: `
), ` + sqlClustersCanonCTE(clustersGlob) + `, participant_entries AS (
	SELECT DISTINCT le.entry_key, cn.canonical_id AS person_id, le.occurred_at, le.estimated_bytes
	FROM logical_entries le
	CROSS JOIN UNNEST(le.participant_ids) AS unnested(participant_id)
	JOIN canon cn ON cn.participant_id = unnested.participant_id`,
			source: "participant_entries",
		}, nil
	case "domain":
		spec := simple("group_value")
		spec.fromSuffix = ", UNNEST(participant_domains) AS grouped(group_value)"
		spec.whereSuffix = " WHERE group_value <> ''"
		return spec, nil
	case messageTypeDimension:
		return simple(messageTypeDimension), nil
	case "kind":
		return simple("entry_kind"), nil
	case "year":
		return simple("strftime(occurred_at, '%Y')"), nil
	case timeGranularityMonth:
		return simple("strftime(occurred_at, '%Y-%m')"), nil
	default:
		return groupExpressions{}, fmt.Errorf("%w: unknown group dimension %q", ErrInvalidExploreRequest, dimension)
	}
}

// expandParticipantFilterClusters widens a request's participant filter
// across linked identity clusters, resolved through the committed
// participant_clusters dataset — the same identity model "People" group
// rows, person search rows, and the ranked relationships list key on
// (canonical = smallest member ID). A participant group row's key is a
// canonical cluster ID, so drilling into it must also match entries and
// files recorded under any linked alias; conversely, filtering by a
// non-canonical member widens to its whole cluster, matching what the
// cluster-aware person routes report for the same identity. Unlinked (or
// unknown) IDs pass through unchanged, so archives without identity links
// keep their exact pre-cluster filter semantics.
func (e *DuckDBEngine) expandParticipantFilterClusters(ctx context.Context, explore ExploreRequest) (ExploreRequest, error) {
	ids := explore.Context.ParticipantIDs
	if len(ids) == 0 {
		return explore, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "(CAST(? AS BIGINT))"
		args[i] = id
	}
	queryText := fmt.Sprintf(`
WITH clusters AS (
	SELECT participant_id, canonical_id FROM read_parquet('%s')
), requested AS (
	SELECT v.id, COALESCE(c.canonical_id, v.id) AS canonical_id
	FROM (VALUES %s) AS v(id)
	LEFT JOIN clusters c ON c.participant_id = v.id
)
SELECT DISTINCT member_id FROM (
	SELECT id AS member_id FROM requested
	UNION ALL
	SELECT canonical_id AS member_id FROM requested
	UNION ALL
	SELECT c.participant_id AS member_id FROM clusters c
	WHERE c.canonical_id IN (SELECT canonical_id FROM requested)
)
ORDER BY member_id`, e.parquetPath(datasetParticipantClusters), strings.Join(placeholders, ","))
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return explore, fmt.Errorf("expand participant filter clusters: %w", err)
	}
	defer func() { _ = rows.Close() }()
	members := make([]int64, 0, len(ids))
	for rows.Next() {
		var member int64
		if err := rows.Scan(&member); err != nil {
			return explore, fmt.Errorf("scan participant cluster member: %w", err)
		}
		members = append(members, member)
	}
	if err := rows.Err(); err != nil {
		return explore, fmt.Errorf("iterate participant cluster members: %w", err)
	}
	explore.Context.ParticipantIDs = members
	return explore, nil
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
	explore, err := e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}
	conditions, args := buildExploreConditions(explore)
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
	explore, err := e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}
	conditions, args := buildExploreConditions(explore)
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
	explore, err := e.expandParticipantFilterClusters(ctx, request.Explore)
	if err != nil {
		return nil, err
	}
	conditions, args := buildExploreConditions(explore)
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
