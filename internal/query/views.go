package query

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// QueryResult holds raw SQL query results in a columnar format.
type QueryResult struct {
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	RowCount int      `json:"row_count"`
}

// SQLQuerier is implemented by engines that support raw SQL queries.
type SQLQuerier interface {
	QuerySQL(ctx context.Context, sql string) (*QueryResult, error)
}

// probeColumns checks which columns exist in a Parquet file.
// Returns a set of column names present in the schema.
// On any error, returns an empty map (callers supply defaults).
func probeColumns(
	db *sql.DB, pathPattern string, hivePartitioning bool,
) map[string]bool {
	cols := make(map[string]bool)
	hiveOpt := ""
	if hivePartitioning {
		hiveOpt = ", hive_partitioning=true, union_by_name=true"
	}
	escaped := strings.ReplaceAll(pathPattern, "'", "''")
	q := fmt.Sprintf(
		"DESCRIBE SELECT * FROM read_parquet('%s'%s)",
		escaped, hiveOpt,
	)
	rows, err := db.Query(q)
	if err != nil {
		return cols
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, colType, isNull, key, dflt, extra sql.NullString
		if err := rows.Scan(
			&name, &colType, &isNull, &key, &dflt, &extra,
		); err != nil {
			continue
		}
		if name.Valid {
			cols[name.String] = true
		}
	}
	if rows.Err() != nil {
		return cols
	}
	return cols
}

// viewDef holds the parameters needed to create one DuckDB view
// over a Parquet table.
type viewDef struct {
	name             string
	pathPattern      string
	hivePartitioning bool
	replaceCols      []string
	optionalCols     []optionalCol
}

// optionalCol defines a column that may or may not exist in the
// Parquet schema. If present, replaceExpr is added to the REPLACE
// clause; if absent, defaultExpr is appended as an extra SELECT column.
type optionalCol struct {
	name        string
	replaceExpr string
	defaultExpr string
}

// buildViewSQL generates the CREATE OR REPLACE VIEW statement for
// a single view definition, using the probed column set to decide
// how to handle optional columns.
func buildViewSQL(def viewDef, probedCols map[string]bool) string {
	replace := make([]string, 0, len(def.replaceCols)+len(def.optionalCols))
	replace = append(replace, def.replaceCols...)

	var extra []string
	for _, oc := range def.optionalCols {
		if probedCols[oc.name] {
			replace = append(replace, oc.replaceExpr)
		} else {
			extra = append(extra, oc.defaultExpr)
		}
	}

	hiveOpt := ""
	if def.hivePartitioning {
		hiveOpt = ", hive_partitioning=true, union_by_name=true"
	}
	escaped := strings.ReplaceAll(def.pathPattern, "'", "''")

	selectClause := fmt.Sprintf(
		"SELECT * REPLACE (%s)",
		strings.Join(replace, ", "),
	)
	if len(extra) > 0 {
		selectClause += ", " + strings.Join(extra, ", ")
	}

	return fmt.Sprintf(
		"CREATE OR REPLACE VIEW %s AS %s FROM read_parquet('%s'%s)",
		def.name, selectClause, escaped, hiveOpt,
	)
}

// probeAllOptionalColumns probes Parquet schemas for all tables that
// have optional columns, returning a map of table name -> column set.
// Used by both RegisterViews and RegisterViewsWithColumns.
func probeAllOptionalColumns(db *sql.DB, analyticsDir string) map[string]map[string]bool {
	msgGlob := filepath.Join(analyticsDir, datasetMessages, "**", "*.parquet")
	tablePath := func(name string) string {
		return filepath.Join(analyticsDir, name, "*.parquet")
	}
	return map[string]map[string]bool{
		datasetMessages:      probeColumns(db, msgGlob, true),
		datasetParticipants:  probeColumns(db, tablePath(datasetParticipants), false),
		datasetConversations: probeColumns(db, tablePath(datasetConversations), false),
		"attachments":        probeColumns(db, tablePath("attachments"), false),
		"sources":            probeColumns(db, tablePath("sources"), false),
	}
}

// RegisterViews creates DuckDB views over the Parquet files in
// analyticsDir. Each view normalises types and supplies defaults
// for optional columns that may be absent in older cache files.
func RegisterViews(db *sql.DB, analyticsDir string) error {
	optCols := probeAllOptionalColumns(db, analyticsDir)
	return RegisterViewsWithColumns(db, analyticsDir, optCols)
}

// RegisterViewsWithColumns is like RegisterViews but uses pre-computed
// optional column info instead of probing Parquet schemas. Used by
// NewDuckDBEngine which already probed columns during initialisation.
func RegisterViewsWithColumns(db *sql.DB, analyticsDir string, optCols map[string]map[string]bool) error {
	if err := createBaseViews(db, analyticsDir, optCols); err != nil {
		return fmt.Errorf("create base views: %w", err)
	}
	return createConvenienceViews(db)
}

// createBaseViews creates the raw Parquet-backed views using the
// pre-computed optional column map so no additional Parquet probes occur.
func createBaseViews(db *sql.DB, analyticsDir string, optCols map[string]map[string]bool) error {
	msgGlob := filepath.Join(
		analyticsDir, datasetMessages, "**", "*.parquet",
	)
	tablePath := func(name string) string {
		return filepath.Join(analyticsDir, name, "*.parquet")
	}

	colsFor := func(table string) map[string]bool {
		if cols, ok := optCols[table]; ok {
			return cols
		}
		return map[string]bool{}
	}

	defs := []struct {
		def   viewDef
		probe map[string]bool
	}{
		{
			def: viewDef{
				name:             datasetMessages,
				pathPattern:      msgGlob,
				hivePartitioning: true,
				replaceCols: []string{
					"CAST(id AS BIGINT) AS id",
					"CAST(source_id AS BIGINT) AS source_id",
					"CAST(source_message_id AS VARCHAR) AS source_message_id",
					"CAST(conversation_id AS BIGINT) AS conversation_id",
					"CAST(subject AS VARCHAR) AS subject",
					"CAST(snippet AS VARCHAR) AS snippet",
					"CAST(size_estimate AS BIGINT) AS size_estimate",
					"COALESCE(TRY_CAST(has_attachments AS BOOLEAN), false) AS has_attachments",
				},
				optionalCols: []optionalCol{
					{
						name:        "attachment_count",
						replaceExpr: "COALESCE(TRY_CAST(attachment_count AS INTEGER), 0) AS attachment_count",
						defaultExpr: "0 AS attachment_count",
					},
					{
						name:        "sender_id",
						replaceExpr: "TRY_CAST(sender_id AS BIGINT) AS sender_id",
						defaultExpr: "NULL::BIGINT AS sender_id",
					},
					{
						name:        messageTypeDimension,
						replaceExpr: "COALESCE(CAST(message_type AS VARCHAR), '') AS message_type",
						defaultExpr: "'' AS message_type",
					},
					{
						name:        "deleted_at",
						replaceExpr: "TRY_CAST(deleted_at AS TIMESTAMP) AS deleted_at",
						defaultExpr: "NULL::TIMESTAMP AS deleted_at",
					},
					{
						name:        "is_from_me",
						replaceExpr: "COALESCE(TRY_CAST(is_from_me AS BOOLEAN), false) AS is_from_me",
						defaultExpr: "false AS is_from_me",
					},
				},
			},
			probe: colsFor(datasetMessages),
		},
		{
			def: viewDef{
				name:        datasetParticipants,
				pathPattern: tablePath(datasetParticipants),
				replaceCols: []string{
					"CAST(id AS BIGINT) AS id",
					"CAST(email_address AS VARCHAR) AS email_address",
					"CAST(domain AS VARCHAR) AS domain",
					"CAST(display_name AS VARCHAR) AS display_name",
				},
				optionalCols: []optionalCol{
					{
						name:        "phone_number",
						replaceExpr: "COALESCE(CAST(phone_number AS VARCHAR), '') AS phone_number",
						defaultExpr: "'' AS phone_number",
					},
				},
			},
			probe: colsFor(datasetParticipants),
		},
		{
			def: viewDef{
				name:        "message_recipients",
				pathPattern: tablePath("message_recipients"),
				replaceCols: []string{
					"CAST(message_id AS BIGINT) AS message_id",
					"CAST(participant_id AS BIGINT) AS participant_id",
					"CAST(recipient_type AS VARCHAR) AS recipient_type",
					"CAST(display_name AS VARCHAR) AS display_name",
				},
			},
		},
		{
			def: viewDef{
				name:        "labels",
				pathPattern: tablePath("labels"),
				replaceCols: []string{
					"CAST(id AS BIGINT) AS id",
					"CAST(name AS VARCHAR) AS name",
				},
			},
		},
		{
			def: viewDef{
				name:        "message_labels",
				pathPattern: tablePath("message_labels"),
				replaceCols: []string{
					"CAST(message_id AS BIGINT) AS message_id",
					"CAST(label_id AS BIGINT) AS label_id",
				},
			},
		},
		{
			def: viewDef{
				name:        "attachments",
				pathPattern: tablePath("attachments"),
				replaceCols: []string{
					"CAST(message_id AS BIGINT) AS message_id",
					"CAST(size AS BIGINT) AS size",
					"CAST(filename AS VARCHAR) AS filename",
				},
				optionalCols: []optionalCol{{
					name:        "mime_type",
					replaceExpr: "COALESCE(CAST(mime_type AS VARCHAR), '') AS mime_type",
					defaultExpr: "'' AS mime_type",
				}},
			},
			probe: colsFor("attachments"),
		},
		{
			def: viewDef{
				name:        datasetConversations,
				pathPattern: tablePath(datasetConversations),
				replaceCols: []string{
					"CAST(id AS BIGINT) AS id",
					"CAST(source_conversation_id AS VARCHAR) AS source_conversation_id",
				},
				optionalCols: []optionalCol{
					{
						name:        "title",
						replaceExpr: "COALESCE(CAST(title AS VARCHAR), '') AS title",
						defaultExpr: "'' AS title",
					},
					{
						name:        "conversation_type",
						replaceExpr: "COALESCE(CAST(conversation_type AS VARCHAR), 'email') AS conversation_type",
						defaultExpr: "'email' AS conversation_type",
					},
				},
			},
			probe: colsFor(datasetConversations),
		},
		{
			def: viewDef{
				name:        datasetConversationParticipants,
				pathPattern: tablePath(datasetConversationParticipants),
				replaceCols: []string{
					"CAST(conversation_id AS BIGINT) AS conversation_id",
					"CAST(participant_id AS BIGINT) AS participant_id",
				},
			},
		},
		{
			def: viewDef{
				name:        datasetParticipantIdentifiers,
				pathPattern: tablePath(datasetParticipantIdentifiers),
				replaceCols: []string{
					"CAST(participant_id AS BIGINT) AS participant_id",
					"CAST(identifier_type AS VARCHAR) AS identifier_type",
					"CAST(identifier_value AS VARCHAR) AS identifier_value",
					"COALESCE(CAST(display_value AS VARCHAR), '') AS display_value",
					"COALESCE(TRY_CAST(is_primary AS BOOLEAN), false) AS is_primary",
				},
			},
		},
		{
			def: viewDef{
				name:        "sources",
				pathPattern: tablePath("sources"),
				replaceCols: []string{
					"CAST(id AS BIGINT) AS id",
				},
				optionalCols: []optionalCol{
					{
						name:        "source_type",
						replaceExpr: "COALESCE(CAST(source_type AS VARCHAR), 'gmail') AS source_type",
						defaultExpr: "'gmail' AS source_type",
					},
				},
			},
			probe: colsFor("sources"),
		},
	}

	for _, d := range defs {
		probe := d.probe
		if probe == nil {
			probe = map[string]bool{}
		}
		stmt := buildViewSQL(d.def, probe)
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("create view %s: %w", d.def.name, err)
		}
	}
	return nil
}

// createConvenienceViews builds higher-level views on top of the
// base Parquet views. Each view joins or aggregates the base views
// to provide ready-to-query datasets.
func createConvenienceViews(db *sql.DB) error {
	views := []struct {
		name string
		sql  string
	}{
		{"v_messages", sqlVMessages},
		{"v_senders", sqlVSenders},
		{"v_domains", sqlVDomains},
		{"v_labels", sqlVLabels},
		{"v_threads", sqlVThreads},
		{"analytical_entries", sqlAnalyticalEntries},
	}
	for _, v := range views {
		if _, err := db.Exec(v.sql); err != nil {
			return fmt.Errorf("create view %s: %w", v.name, err)
		}
	}
	return nil
}

// sqlAnalyticalEntriesParticipantLabel renders the canonical participant
// label used by every analytical participant list: display name, then phone
// number, then email address, then empty string. alias is the participants
// table alias. Any query that rebuilds participant labels outside the
// analytical_entries view (the explore/files page-enrichment fast paths)
// must use this so labels cannot drift from the view's.
func sqlAnalyticalEntriesParticipantLabel(alias string) string {
	return "COALESCE(NULLIF(" + alias + ".display_name, ''), NULLIF(" + alias + ".phone_number, ''), " + alias + ".email_address, '')"
}

// sqlAnalyticalEntries normalizes every durable message-shaped source row.
// Logical row-unit selection deliberately remains in Explore so Context can be
// applied before chat conversations are aggregated.
var sqlAnalyticalEntries = `
CREATE OR REPLACE VIEW analytical_entries AS
WITH message_participant_links AS (
    SELECT message_id, participant_id
    FROM message_recipients
    UNION ALL
    SELECT id AS message_id, sender_id AS participant_id
    FROM messages
    WHERE sender_id IS NOT NULL
), message_participant_facts AS (
    SELECT
        link.message_id,
        list_sort(list_distinct(list(link.participant_id))) AS participant_ids,
        list_sort(list_distinct(list(` + sqlAnalyticalEntriesParticipantLabel("p") + `))) AS participant_labels,
        list_sort(list_distinct(list(COALESCE(p.domain, '')))) AS participant_domains
    FROM message_participant_links link
    JOIN participants p ON p.id = link.participant_id
    GROUP BY link.message_id
), conversation_participant_facts AS (
    SELECT
        cp.conversation_id,
        list_sort(list_distinct(list(cp.participant_id))) AS participant_ids,
        list_sort(list_distinct(list(` + sqlAnalyticalEntriesParticipantLabel("p") + `))) AS participant_labels,
        list_sort(list_distinct(list(COALESCE(p.domain, '')))) AS participant_domains
    FROM conversation_participants cp
    JOIN participants p ON p.id = cp.participant_id
    GROUP BY cp.conversation_id
)
SELECT
    m.id AS message_id,
    m.source_id,
    COALESCE(s.source_type, '') AS source_type,
    COALESCE(s.account_email, '') AS source_identifier,
    m.source_message_id,
    m.conversation_id,
    COALESCE(c.source_conversation_id, '') AS source_conversation_id,
    COALESCE(c.conversation_type, '') AS conversation_type,
    COALESCE(c.title, '') AS conversation_title,
    COALESCE(m.message_type, '') AS message_type,
    m.sender_id,
    COALESCE(NULLIF(sender.email_address, ''), NULLIF(sender.phone_number, ''), '') AS sender_identifier,
    COALESCE(NULLIF(sender.display_name, ''), NULLIF(sender.phone_number, ''), sender.email_address, '') AS sender_display,
    COALESCE(sender.domain, '') AS sender_domain,
    m.sent_at AS occurred_at,
    COALESCE(m.subject, '') AS subject,
    COALESCE(m.snippet, '') AS snippet,
    m.is_from_me,
	m.size_estimate,
	m.deleted_at IS NOT NULL AS internally_deleted,
    m.deleted_from_source_at IS NOT NULL AS deleted_from_source,
    COALESCE(m.has_attachments, false) AS has_attachments,
    COALESCE(att.attachment_count, 0) AS attachment_count,
    COALESCE(att.attachment_size, 0) AS attachment_size,
    COALESCE(recip.participant_ids, []::BIGINT[]) AS participant_ids,
    COALESCE(recip.participant_labels, []::VARCHAR[]) AS participant_labels,
    COALESCE(recip.participant_domains, []::VARCHAR[]) AS participant_domains,
    COALESCE(conv_part.participant_ids, []::BIGINT[]) AS conversation_participant_ids,
    COALESCE(conv_part.participant_labels, []::VARCHAR[]) AS conversation_participant_labels,
    COALESCE(conv_part.participant_domains, []::VARCHAR[]) AS conversation_participant_domains
FROM messages m
JOIN sources s ON s.id = m.source_id
LEFT JOIN conversations c ON c.id = m.conversation_id
LEFT JOIN participants sender ON sender.id = m.sender_id
LEFT JOIN message_participant_facts recip ON recip.message_id = m.id
LEFT JOIN conversation_participant_facts conv_part ON conv_part.conversation_id = m.conversation_id
LEFT JOIN (
    SELECT message_id, COUNT(*) AS attachment_count, COALESCE(SUM(size), 0) AS attachment_size
    FROM attachments
    GROUP BY message_id
) att ON att.message_id = m.id
`

// sqlVMessages: messages with sender resolved via dual-path
// (message_recipients for email, messages.sender_id for chat)
// and labels as sorted JSON array.
const sqlVMessages = `
CREATE OR REPLACE VIEW v_messages AS
SELECT
    m.id,
    m.source_id,
    m.source_message_id,
    m.conversation_id,
    m.subject,
    m.snippet,
    m.sent_at,
    m.size_estimate,
    m.has_attachments,
    m.attachment_count,
    m.message_type,
    m.year,
    m.month,
    COALESCE(ms.from_email, ds.from_email, '') AS from_email,
    COALESCE(ms.from_name, ds.from_name, '') AS from_name,
    COALESCE(ms.from_domain, ds.from_domain, '') AS from_domain,
    COALESCE(ms.from_phone, ds.from_phone, '') AS from_phone,
    CAST(
        COALESCE(to_json(ml_agg.labels), '[]') AS VARCHAR
    ) AS labels,
    m.deleted_from_source_at
FROM messages m
LEFT JOIN (
    SELECT
        mr.message_id,
        FIRST(p.email_address) AS from_email,
        FIRST(
            COALESCE(NULLIF(TRIM(mr.display_name), ''), NULLIF(TRIM(p.display_name), ''), NULLIF(p.phone_number, ''), p.email_address, '')
        ) AS from_name,
        FIRST(p.domain) AS from_domain,
        FIRST(COALESCE(p.phone_number, '')) AS from_phone
    FROM message_recipients mr
    JOIN participants p ON p.id = mr.participant_id
    WHERE mr.recipient_type = 'from'
    GROUP BY mr.message_id
) ms ON ms.message_id = m.id
LEFT JOIN (
    SELECT
        msg.id AS message_id,
        COALESCE(p.email_address, '') AS from_email,
        COALESCE(p.display_name, '') AS from_name,
        COALESCE(p.domain, '') AS from_domain,
        COALESCE(p.phone_number, '') AS from_phone
    FROM messages msg
    JOIN participants p ON p.id = msg.sender_id
    WHERE msg.sender_id IS NOT NULL
) ds ON ds.message_id = m.id AND ms.message_id IS NULL
LEFT JOIN (
    SELECT
        ml.message_id,
        list(l.name ORDER BY l.name) AS labels
    FROM message_labels ml
    JOIN labels l ON l.id = ml.label_id
    GROUP BY ml.message_id
) ml_agg ON ml_agg.message_id = m.id
`

// sqlVSenders: per-sender aggregates.
const sqlVSenders = `
CREATE OR REPLACE VIEW v_senders AS
SELECT
    p.email_address AS from_email,
    COALESCE(
        NULLIF(TRIM(FIRST(mr.display_name)), ''),
        NULLIF(TRIM(FIRST(p.display_name)), ''),
        p.email_address
    ) AS from_name,
    p.domain AS from_domain,
    COUNT(*) AS message_count,
    SUM(m.size_estimate) AS total_size,
    COALESCE(SUM(att.attachment_size), 0) AS attachment_size,
    COALESCE(SUM(att.attachment_count), 0) AS attachment_count,
    MIN(m.sent_at) AS first_message_at,
    MAX(m.sent_at) AS last_message_at
FROM message_recipients mr
JOIN participants p ON p.id = mr.participant_id
JOIN messages m ON m.id = mr.message_id
LEFT JOIN (
    SELECT
        message_id,
        SUM(size) AS attachment_size,
        COUNT(*) AS attachment_count
    FROM attachments
    GROUP BY message_id
) att ON att.message_id = m.id
WHERE mr.recipient_type = 'from'
GROUP BY p.email_address, p.domain
`

// sqlVDomains: per-domain aggregates.
const sqlVDomains = `
CREATE OR REPLACE VIEW v_domains AS
SELECT
    p.domain,
    COUNT(*) AS message_count,
    SUM(m.size_estimate) AS total_size,
    COUNT(DISTINCT p.email_address) AS sender_count
FROM message_recipients mr
JOIN participants p ON p.id = mr.participant_id
JOIN messages m ON m.id = mr.message_id
WHERE mr.recipient_type = 'from'
GROUP BY p.domain
`

// sqlVLabels: label name with message count and total size.
const sqlVLabels = `
CREATE OR REPLACE VIEW v_labels AS
SELECT
    l.name,
    COUNT(*) AS message_count,
    SUM(m.size_estimate) AS total_size
FROM message_labels ml
JOIN labels l ON l.id = ml.label_id
JOIN messages m ON m.id = ml.message_id
GROUP BY l.name
`

// sqlVThreads: per-conversation aggregates with participant
// emails as a JSON array.
const sqlVThreads = `
CREATE OR REPLACE VIEW v_threads AS
SELECT
    c.id AS conversation_id,
    c.source_conversation_id,
    c.title AS conversation_title,
    c.conversation_type,
    COUNT(DISTINCT m.id) AS message_count,
    MIN(m.sent_at) AS first_message_at,
    MAX(m.sent_at) AS last_message_at,
    CAST(
        COALESCE(
            to_json(list(DISTINCT p.email_address)),
            '[]'
        ) AS VARCHAR
    ) AS participant_emails
FROM conversations c
JOIN messages m ON m.conversation_id = c.id
LEFT JOIN message_recipients mr ON mr.message_id = m.id
LEFT JOIN participants p ON p.id = mr.participant_id
GROUP BY c.id, c.source_conversation_id, c.title, c.conversation_type
`
