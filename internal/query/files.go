package query

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const maxFileSearchLimit = 500

// FileMIMEFamily is a stable, presentation-oriented MIME grouping.
type FileMIMEFamily string

const (
	FileMIMEImage    FileMIMEFamily = "image"
	FileMIMEPDF      FileMIMEFamily = "pdf"
	FileMIMEAudio    FileMIMEFamily = "audio"
	FileMIMEVideo    FileMIMEFamily = "video"
	FileMIMEText     FileMIMEFamily = "text"
	FileMIMEDocument FileMIMEFamily = "document"
	FileMIMEArchive  FileMIMEFamily = "archive"
	FileMIMEOther    FileMIMEFamily = "other"
)

var fileMIMEFamilies = map[FileMIMEFamily]struct{}{
	FileMIMEImage: {}, FileMIMEPDF: {}, FileMIMEAudio: {}, FileMIMEVideo: {},
	FileMIMEText: {}, FileMIMEDocument: {}, FileMIMEArchive: {}, FileMIMEOther: {},
}

type FileSearchRequest struct {
	Explore       ExploreRequest   `json:"explore"`
	FilenameQuery string           `json:"filename_query,omitempty"`
	MIMEFamilies  []FileMIMEFamily `json:"mime_families,omitempty"`
	Sort          SortSpec         `json:"sort"`
	Page          PageSpec         `json:"page"`
}

type FileGroupRequest struct {
	Explore       ExploreRequest   `json:"explore"`
	FilenameQuery string           `json:"filename_query,omitempty"`
	MIMEFamilies  []FileMIMEFamily `json:"mime_families,omitempty"`
	Dimension     string           `json:"dimension"`
	Sort          SortSpec         `json:"sort"`
	Page          PageSpec         `json:"page"`
}

type FileRow struct {
	ID                 int64          `json:"id"`
	Key                string         `json:"key"`
	EntryKey           string         `json:"entry_key"`
	MessageID          int64          `json:"message_id"`
	ConversationID     int64          `json:"conversation_id"`
	OccurredAt         time.Time      `json:"occurred_at"`
	SourceID           int64          `json:"source_id"`
	SourceType         string         `json:"source_type"`
	SourceIdentifier   string         `json:"source_identifier"`
	ContainingTitle    string         `json:"containing_title"`
	Filename           string         `json:"filename"`
	MimeType           string         `json:"mime_type"`
	MIMEFamily         FileMIMEFamily `json:"mime_family"`
	Size               int64          `json:"size_bytes"`
	ParticipantIDs     []int64        `json:"participant_ids,omitempty"`
	ParticipantLabels  []string       `json:"participant_labels,omitempty"`
	ParticipantDomains []string       `json:"participant_domains,omitempty"`
}

type FileSearchResponse struct {
	Files            []FileRow        `json:"files"`
	TotalCount       int64            `json:"total_count"`
	CacheRevision    string           `json:"cache_revision"`
	SearchProvenance SearchProvenance `json:"search_provenance"`
}

// SearchFiles projects attachment facts only from the committed analytical cache.
func (e *DuckDBEngine) SearchFiles(ctx context.Context, request FileSearchRequest) (*FileSearchResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if request.Page.Offset < 0 || request.Page.Limit < 0 || request.Page.Limit > maxFileSearchLimit {
		return nil, fmt.Errorf("%w: file page is outside the supported range", ErrInvalidExploreRequest)
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
	}
	order, err := fileSearchOrder(request.Sort)
	if err != nil {
		return nil, err
	}
	if err := validateFileMIMEFamilies(request.MIMEFamilies); err != nil {
		return nil, err
	}
	release, err := e.acquireQuerySlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	// The acquired cache read lock is the authority for both schema refresh and
	// query execution. A long-running server may otherwise retain views bound to
	// the previous attachment schema after an atomic cache publication.
	e.ensureFreshOptionalCols()
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	population, args := filteredFilePopulationSQL(request.Explore, request.FilenameQuery, request.MIMEFamilies)
	limit := request.Page.Limit
	if limit == 0 {
		limit = 100
	}
	args = append(args, limit, request.Page.Offset)
	rows, err := e.db.QueryContext(ctx, fileSearchSQL(population, order), args...)
	if err != nil {
		return nil, fmt.Errorf("search analytical files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &FileSearchResponse{Files: make([]FileRow, 0), CacheRevision: state.Revision(), SearchProvenance: provenance}
	for rows.Next() {
		var row FileRow
		var rawSnippet, participantIDsJSON, participantLabelsJSON, participantDomainsJSON string
		if err := rows.Scan(
			&row.ID, &row.Key, &row.EntryKey, &row.MessageID, &row.ConversationID,
			&row.OccurredAt, &row.SourceID, &row.SourceType, &row.SourceIdentifier,
			&row.ContainingTitle, &rawSnippet, &row.Filename, &row.MimeType, &row.MIMEFamily, &row.Size,
			&participantIDsJSON, &participantLabelsJSON, &participantDomainsJSON, &response.TotalCount,
		); err != nil {
			return nil, fmt.Errorf("scan analytical file: %w", err)
		}
		if err := json.Unmarshal([]byte(participantIDsJSON), &row.ParticipantIDs); err != nil {
			return nil, fmt.Errorf("decode file participant IDs: %w", err)
		}
		if err := json.Unmarshal([]byte(participantLabelsJSON), &row.ParticipantLabels); err != nil {
			return nil, fmt.Errorf("decode file participant labels: %w", err)
		}
		if err := json.Unmarshal([]byte(participantDomainsJSON), &row.ParticipantDomains); err != nil {
			return nil, fmt.Errorf("decode file participant domains: %w", err)
		}
		if row.ContainingTitle == rawSnippet {
			row.ContainingTitle = FlattenSnippet(row.ContainingTitle)
		}
		response.Files = append(response.Files, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical files: %w", err)
	}
	return response, nil
}

// GroupFiles aggregates the same filtered attachment population returned by
// SearchFiles. Counts and estimated bytes therefore describe files, not their
// containing messages.
func (e *DuckDBEngine) GroupFiles(ctx context.Context, request FileGroupRequest) (*ExploreGroupResponse, error) {
	if e.analyticsDir == "" {
		return nil, &CacheUnavailableError{Readiness: CacheAbsent}
	}
	if err := validateExploreAnalysisPage(request.Page, maxFileSearchLimit); err != nil {
		return nil, err
	}
	provenance, err := validateResolvedSearch(request.Explore.Search)
	if err != nil {
		return nil, err
	}
	if err := validateFileMIMEFamilies(request.MIMEFamilies); err != nil {
		return nil, err
	}
	groupExpr, labelExpr, fromSuffix, whereSuffix, err := fileGroupExpressions(request.Dimension)
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
	e.ensureFreshOptionalCols()
	state, err := ReadCacheSyncState(e.analyticsDir)
	if err != nil {
		return nil, fmt.Errorf("read committed cache state: %w", err)
	}
	population, args := filteredFilePopulationSQL(request.Explore, request.FilenameQuery, request.MIMEFamilies)
	limit := request.Page.Limit
	if limit == 0 {
		limit = defaultExploreLimit
	}
	queryText := population + `
, grouped AS (
	SELECT ` + groupExpr + ` AS group_key, ` + labelExpr + ` AS group_label,
		COUNT(*)::BIGINT AS group_count,
		COALESCE(SUM(size), 0)::BIGINT AS estimated_bytes,
		MAX(occurred_at) AS latest_at
	FROM file_population` + fromSuffix + whereSuffix + `
	GROUP BY ` + groupExpr + `
), counted AS (
	SELECT *, COUNT(*) OVER () AS total_count FROM grouped
)
SELECT group_key, group_label, group_count, estimated_bytes, latest_at, total_count
FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit, request.Page.Offset)
	rows, err := e.db.QueryContext(ctx, queryText, args...)
	if err != nil {
		return nil, fmt.Errorf("group analytical files: %w", err)
	}
	defer func() { _ = rows.Close() }()
	response := &ExploreGroupResponse{
		Rows: make([]ExploreGroupRow, 0), CacheRevision: state.Revision(), SearchProvenance: provenance,
	}
	for rows.Next() {
		var row ExploreGroupRow
		if err := rows.Scan(&row.Key, &row.Label, &row.Count, &row.EstimatedBytes, &row.LatestAt, &response.TotalCount); err != nil {
			return nil, fmt.Errorf("scan analytical file group: %w", err)
		}
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytical file groups: %w", err)
	}
	return response, nil
}

func validateFileMIMEFamilies(families []FileMIMEFamily) error {
	for _, family := range families {
		if _, ok := fileMIMEFamilies[family]; !ok {
			return fmt.Errorf("%w: unknown MIME family %q", ErrInvalidExploreRequest, family)
		}
	}
	return nil
}

func filteredFilePopulationSQL(
	explore ExploreRequest,
	filenameQuery string,
	mimeFamilies []FileMIMEFamily,
) (string, []any) {
	conditions, args := buildExploreConditions(explore)
	fileConditions := []string{"true"}
	if query := strings.TrimSpace(filenameQuery); query != "" {
		fileConditions = append(fileConditions, "contains(lower(filename), lower(?))")
		args = append(args, query)
	}
	if len(mimeFamilies) > 0 {
		parts := make([]string, len(mimeFamilies))
		for i, family := range mimeFamilies {
			parts[i] = "?"
			args = append(args, family)
		}
		fileConditions = append(fileConditions, "mime_family IN ("+strings.Join(parts, ",")+")")
	}
	return filePopulationSQL(conditions, strings.Join(fileConditions, " AND ")), args
}

func fileGroupExpressions(dimension string) (key, label, fromSuffix, whereSuffix string, err error) {
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
			" CROSS JOIN UNNEST(file_participant_ids) AS grouped(group_value)" +
				" JOIN participants AS grouped_participant ON grouped_participant.id = group_value", "", nil
	case "domain":
		return "group_value", "group_value", ", UNNEST(file_participant_domains) AS grouped(group_value)",
			" WHERE group_value <> ''", nil
	case messageTypeDimension:
		return messageTypeDimension, messageTypeDimension, "", "", nil
	case "kind":
		return "'file'", "'file'", "", "", nil
	case "year":
		return "strftime(occurred_at, '%Y')", "strftime(occurred_at, '%Y')", "", "", nil
	case timeGranularityMonth:
		return "strftime(occurred_at, '%Y-%m')", "strftime(occurred_at, '%Y-%m')", "", "", nil
	default:
		return "", "", "", "", fmt.Errorf("%w: unknown file group dimension %q", ErrInvalidExploreRequest, dimension)
	}
}

func fileSearchOrder(sort SortSpec) (string, error) {
	if sort.Field == "" {
		sort = SortSpec{Field: "occurred_at", Direction: sortDirectionDesc}
	}
	if sort.Direction != sortDirectionAsc && sort.Direction != sortDirectionDesc {
		return "", fmt.Errorf("%w: invalid file sort direction %q", ErrInvalidExploreRequest, sort.Direction)
	}
	direction := strings.ToUpper(sort.Direction)
	switch sort.Field {
	case "occurred_at":
		return "occurred_at " + direction + ", message_id ASC, attachment_id ASC", nil
	case "filename":
		return "lower(filename) " + direction + ", filename " + direction + ", occurred_at DESC, attachment_id ASC", nil
	case "size":
		return "size " + direction + ", lower(filename) ASC, occurred_at DESC, attachment_id ASC", nil
	default:
		return "", fmt.Errorf("%w: unknown file sort field %q", ErrInvalidExploreRequest, sort.Field)
	}
}

func filePopulationSQL(exploreConditions, fileConditions string) string {
	return `
WITH selected AS (
	SELECT * FROM analytical_entries WHERE ` + exploreConditions + `
), classified AS (
	SELECT
		a.attachment_id, a.message_id, COALESCE(a.size, 0)::BIGINT AS size,
		COALESCE(a.filename, '') AS filename, COALESCE(a.mime_type, '') AS mime_type,
		CASE
			WHEN lower(a.mime_type) LIKE 'image/%' THEN 'image'
			WHEN lower(a.mime_type) = 'application/pdf' THEN 'pdf'
			WHEN lower(a.mime_type) LIKE 'audio/%' THEN 'audio'
			WHEN lower(a.mime_type) LIKE 'video/%' THEN 'video'
			WHEN lower(a.mime_type) LIKE 'text/%' THEN 'text'
			WHEN lower(a.mime_type) IN ('application/msword', 'application/rtf',
				'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
				'application/vnd.oasis.opendocument.text') THEN 'document'
			WHEN lower(a.mime_type) IN ('application/zip', 'application/gzip', 'application/x-tar',
				'application/x-7z-compressed', 'application/x-rar-compressed') THEN 'archive'
			ELSE 'other'
		END AS mime_family,
		s.*
	FROM selected s JOIN attachments a ON a.message_id = s.message_id
), filtered AS (
	SELECT * FROM classified WHERE ` + fileConditions + `
), file_population AS (
	SELECT *,
		list_sort(list_distinct(list_concat(participant_ids, conversation_participant_ids))) AS file_participant_ids,
		list_sort(list_distinct(list_concat(participant_domains, conversation_participant_domains))) AS file_participant_domains
	FROM filtered
)
`
}

func fileSearchSQL(population, order string) string {
	return population + `
, counted AS (
	SELECT *, COUNT(*) OVER () AS total_count FROM file_population
)
SELECT
	attachment_id,
	CASE WHEN lower(message_type) IN (` + TextMessageTypeSQLList + `)
		OR (lower(message_type) IN ('', 'chat', 'text') AND lower(conversation_type) IN ('direct_chat', 'group_chat', 'channel', 'chat'))
		THEN 'source:' || CAST(source_id AS VARCHAR) || ':conversation:' || CAST(conversation_id AS VARCHAR) || ':file:' || CAST(attachment_id AS VARCHAR)
		ELSE 'source:' || CAST(source_id AS VARCHAR) || ':message:' || COALESCE(NULLIF(source_message_id, ''), CAST(message_id AS VARCHAR)) || ':file:' || CAST(attachment_id AS VARCHAR) END,
	CASE WHEN lower(message_type) IN (` + TextMessageTypeSQLList + `)
		OR (lower(message_type) IN ('', 'chat', 'text') AND lower(conversation_type) IN ('direct_chat', 'group_chat', 'channel', 'chat'))
		THEN 'source:' || CAST(source_id AS VARCHAR) || ':conversation:' || CAST(conversation_id AS VARCHAR)
		ELSE 'source:' || CAST(source_id AS VARCHAR) || ':message:' || COALESCE(NULLIF(source_message_id, ''), CAST(message_id AS VARCHAR)) END,
	message_id, conversation_id, occurred_at, source_id, source_type, source_identifier,
	COALESCE(NULLIF(subject, ''), NULLIF(conversation_title, ''), snippet, ''),
	snippet,
	filename, mime_type, mime_family, size,
	CAST(COALESCE(to_json(file_participant_ids), '[]') AS VARCHAR),
	CAST(COALESCE(to_json(list_sort(list_distinct(list_concat(participant_labels, conversation_participant_labels)))), '[]') AS VARCHAR),
	CAST(COALESCE(to_json(file_participant_domains), '[]') AS VARCHAR),
	total_count
FROM counted ORDER BY ` + order + ` LIMIT ? OFFSET ?`
}
