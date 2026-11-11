package query

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/identityindex"
)

// QueryResult holds raw SQL query results in a columnar format.
type QueryResult struct {
	Columns  []string        `json:"columns"`
	Rows     [][]any         `json:"rows"`
	RowCount int             `json:"row_count"`
	Cache    *CacheFreshness `json:"cache,omitempty"`
}

// CacheFreshness describes the committed analytics publication used by a SQL
// result. The engine sets its generation while holding the cache read lock;
// callers may add source-database drift details after the query completes.
type CacheFreshness struct {
	Generation       string    `json:"generation"`
	PublishedAt      time.Time `json:"published_at"`
	StaleReason      string    `json:"stale_reason,omitempty"`
	PendingAdditions int64     `json:"pending_additions,omitempty"`
	Building         bool      `json:"building,omitempty"`
}

// SQLQuerier is implemented by engines that support raw SQL queries.
type SQLQuerier interface {
	QuerySQL(ctx context.Context, sql string) (*QueryResult, error)
}

// probeColumns checks which columns exist in a Parquet file.
// Returns a set of column names present in the schema.
// Missing optional schemas return an empty map (callers supply defaults).
// Context cancellation remains an error so incomplete probes are not cached.
func probeColumns(
	ctx context.Context, db *sql.DB, pathPattern string, hivePartitioning bool,
) (map[string]bool, error) {
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
	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return cols, ctx.Err()
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
		return cols, ctx.Err()
	}
	return cols, ctx.Err()
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
func probeAllOptionalColumns(ctx context.Context, db *sql.DB, analyticsDir string) (map[string]map[string]bool, error) {
	msgGlob := filepath.Join(analyticsDir, datasetMessages, "**", "*.parquet")
	tablePath := func(name string) string {
		return filepath.Join(analyticsDir, name, "*.parquet")
	}
	cols := make(map[string]map[string]bool)
	for _, table := range []string{datasetMessages, datasetParticipants, datasetConversations, "attachments", "sources", "message_recipients"} {
		path := tablePath(table)
		partitioned := table == datasetMessages
		if partitioned {
			path = msgGlob
		}
		var err error
		cols[table], err = probeColumns(ctx, db, path, partitioned)
		if err != nil {
			return nil, err
		}
	}
	return cols, nil
}

// RegisterViews creates DuckDB views over the Parquet files in
// analyticsDir. Each view normalises types and supplies defaults
// for optional columns that may be absent in older cache files.
func RegisterViews(ctx context.Context, db *sql.DB, analyticsDir string) error {
	optCols, err := probeAllOptionalColumns(ctx, db, analyticsDir)
	if err != nil {
		return err
	}
	return RegisterViewsWithColumns(ctx, db, analyticsDir, optCols)
}

// RegisterViewsWithColumns is like RegisterViews but uses pre-computed
// optional column info instead of probing Parquet schemas. Used by
// NewDuckDBEngine which already probed columns during initialisation.
func RegisterViewsWithColumns(ctx context.Context, db *sql.DB, analyticsDir string, optCols map[string]map[string]bool) error {
	if err := createBaseViews(ctx, db, analyticsDir, optCols); err != nil {
		return fmt.Errorf("create base views: %w", err)
	}
	if err := createRelationshipActivityView(ctx, db, analyticsDir); err != nil {
		return err
	}
	return createConvenienceViews(ctx, db)
}

// createBaseViews creates the raw Parquet-backed views using the
// pre-computed optional column map so no additional Parquet probes occur.
func createBaseViews(ctx context.Context, db *sql.DB, analyticsDir string, optCols map[string]map[string]bool) error {
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
					"CAST(archive_only AS BOOLEAN) AS archive_only",
				},
				optionalCols: []optionalCol{
					{name: "account_address", replaceExpr: "CAST(account_address AS VARCHAR) AS account_address", defaultExpr: "NULL::VARCHAR AS account_address"},
					{name: "account_path", replaceExpr: "CAST(account_path AS VARCHAR) AS account_path", defaultExpr: "NULL::VARCHAR AS account_path"},
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
						name:        "owner_participant_id",
						replaceExpr: "TRY_CAST(owner_participant_id AS BIGINT) AS owner_participant_id",
						defaultExpr: "NULL::BIGINT AS owner_participant_id",
					},
					{
						name:        messageTypeDimension,
						replaceExpr: "COALESCE(CAST(message_type AS VARCHAR), '') AS message_type",
						defaultExpr: "'' AS message_type",
					},
					{
						name:        "list_id",
						replaceExpr: "CAST(list_id AS VARCHAR) AS list_id",
						defaultExpr: "NULL::VARCHAR AS list_id",
					},
					{
						name:        "deleted_at",
						replaceExpr: "TRY_CAST(deleted_at AS TIMESTAMP) AS deleted_at",
						defaultExpr: "NULL::TIMESTAMP AS deleted_at",
					},
					{
						name:        "is_from_me",
						replaceExpr: "TRY_CAST(is_from_me AS BOOLEAN) AS is_from_me",
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
				optionalCols: []optionalCol{
					{
						// Resolved recipient address (cache schema v26): the
						// header address when one was recorded, otherwise the
						// participant's current address. NULL only for
						// participants without an email address.
						name:        "email_address",
						replaceExpr: "CAST(email_address AS VARCHAR) AS email_address",
						defaultExpr: "NULL::VARCHAR AS email_address",
					},
					{
						// Header address exactly as recorded (cache schema
						// v26); NULL when none was — chat, calendar, and mail
						// ingested before the store column existed. Identity
						// filters key on its presence (see
						// buildIdentityPredicateCondition); a cache without
						// the column reads as NULL and falls back to
						// participant matching.
						name:        "envelope_address",
						replaceExpr: "CAST(envelope_address AS VARCHAR) AS envelope_address",
						defaultExpr: "NULL::VARCHAR AS envelope_address",
					},
				},
			},
			probe: colsFor("message_recipients"),
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
				optionalCols: []optionalCol{
					{
						name:        "mime_type",
						replaceExpr: "COALESCE(CAST(mime_type AS VARCHAR), '') AS mime_type",
						defaultExpr: "'' AS mime_type",
					},
					{
						name:        "attachment_metadata",
						replaceExpr: "TRY_CAST(attachment_metadata AS VARCHAR) AS attachment_metadata",
						defaultExpr: "NULL::VARCHAR AS attachment_metadata",
					},
				},
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
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("create view %s: %w", d.def.name, err)
		}
	}
	return nil
}

func hasNormalizedRelationshipActivity(analyticsDir string) (bool, error) {
	activityFiles, err := filepath.Glob(filepath.Join(analyticsDir, identityindex.DatasetActivity, "*", "*.parquet"))
	if err != nil {
		return false, fmt.Errorf("find relationship activity: %w", err)
	}
	return len(activityFiles) > 0, nil
}

// The normalized relationship view is used by internal query paths even when
// legacy analytical views are disabled.
func createRelationshipActivityView(ctx context.Context, db *sql.DB, analyticsDir string) error {
	hasActivity, err := hasNormalizedRelationshipActivity(analyticsDir)
	if err != nil {
		return err
	}
	if hasActivity {
		parquet := func(dataset string, hive bool) string {
			glob := filepath.Join(analyticsDir, dataset, "*.parquet")
			options := ""
			if hive {
				glob = filepath.Join(analyticsDir, dataset, "**", "*.parquet")
				options = ", hive_partitioning=true, union_by_name=true"
			}
			return "read_parquet('" + quoteIdentitySQLPath(glob) + "'" + options + ")"
		}
		relation := identityindex.ExpandedActivityRelation(
			parquet(identityindex.DatasetActivity, true),
			parquet(datasetConversationParticipants, false),
			parquet(datasetParticipants, false),
			parquet(datasetParticipantClusters, false),
			parquet(datasetOwnerParticipants, false),
		)
		if _, err := db.ExecContext(ctx, "CREATE OR REPLACE VIEW relationship_activity_expanded AS SELECT * FROM "+relation); err != nil {
			return fmt.Errorf("create relationship activity view: %w", err)
		}
	}
	return nil
}

// refreshRelationshipQuerySets loads unsent and owner event message IDs and owner canonicals once
// per committed cache publication, avoiding sparse-fact scans on each request.
func refreshRelationshipQuerySets(ctx context.Context, db *sql.DB, analyticsDir string) error {
	if analyticsDir == "" {
		return nil
	}
	hasActivity, err := hasNormalizedRelationshipActivity(analyticsDir)
	if err != nil {
		return err
	}
	path := filepath.Join(analyticsDir, identityindex.DatasetActivity, "**", "*.parquet")
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin relationship query set refresh: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, "CREATE OR REPLACE TEMP TABLE relationship_unsent_messages AS SELECT message_id FROM read_parquet('"+quoteIdentitySQLPath(path)+"', hive_partitioning=true, union_by_name=true) WHERE canonical_id IS NULL AND unsent")
	if err != nil {
		return errors.Join(fmt.Errorf("load unsent message IDs: %w", err), tx.Rollback())
	}
	ownerSQL := "SELECT NULL::BIGINT AS canonical_id WHERE false"
	// Match the expanded view: unpartitioned caches have no normalized activity.
	if hasActivity {
		canon := "(" + identityindex.CanonicalParticipantsSQL(
			fmt.Sprintf("read_parquet('%s')", quoteIdentitySQLPath(filepath.Join(analyticsDir, datasetParticipants, "*.parquet"))),
			fmt.Sprintf("read_parquet('%s')", quoteIdentitySQLPath(filepath.Join(analyticsDir, datasetParticipantClusters, "*.parquet"))),
		) + ")"
		owners := fmt.Sprintf("read_parquet('%s')", quoteIdentitySQLPath(filepath.Join(analyticsDir, datasetOwnerParticipants, "*.parquet")))
		// Source-native is_from_me messages can identify an owner absent from owner_participants.
		ownerSQL = fmt.Sprintf(`
  %s
  UNION
  SELECT canonical_id FROM read_parquet('%s', hive_partitioning=true, union_by_name=true)
  WHERE canonical_id IS NOT NULL AND is_owner`,
			identityindex.OwnerCanonicalsSQL(owners, canon), quoteIdentitySQLPath(path))
	}
	_, err = tx.ExecContext(ctx, "CREATE OR REPLACE TEMP TABLE relationship_owner_canonicals AS "+ownerSQL)
	if err != nil {
		return errors.Join(fmt.Errorf("load relationship owner canonicals: %w", err), tx.Rollback())
	}
	eventSQL := "SELECT NULL::BIGINT AS message_id WHERE false"
	if hasActivity {
		// Sparse activity marks is_owner only on direct rows with a confirmed owner canonical or the is_from_me owner sender, so roster expansion adds no owner-attended events.
		eventSQL = "SELECT message_id FROM read_parquet('" + quoteIdentitySQLPath(path) + "', hive_partitioning=true, union_by_name=true) WHERE entry_kind IN ('event', 'meeting') AND is_owner"
	}
	if _, err := tx.ExecContext(ctx, "CREATE OR REPLACE TEMP TABLE relationship_owner_event_messages AS "+eventSQL); err != nil {
		return errors.Join(fmt.Errorf("load owner event message IDs: %w", err), tx.Rollback())
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit relationship query set refresh: %w", err)
	}
	return nil
}

// createConvenienceViews builds higher-level views on top of the
// base Parquet views. Each view joins or aggregates the base views
// to provide ready-to-query datasets.
func createConvenienceViews(ctx context.Context, db *sql.DB) error {
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
		if _, err := db.ExecContext(ctx, v.sql); err != nil {
			return fmt.Errorf("create view %s: %w", v.name, err)
		}
	}
	return nil
}

// sqlAnalyticalEntriesParticipantLabel renders the canonical participant
// label used by every analytical participant list: display name, then phone
// number, then email address, then numeric X ID, then empty string. alias is the participants
// table alias. Any query that rebuilds participant labels outside the
// analytical_entries view (the explore/files page-enrichment fast paths)
// must use this so labels cannot drift from the view's.
func sqlAnalyticalEntriesParticipantLabel(alias string) string {
	return "COALESCE(NULLIF(" + alias + ".display_name, ''), NULLIF(" + alias + ".phone_number, ''), NULLIF(" + alias + ".email_address, ''), " + xUserIDExpr(alias, "") + ", '')"
}

// buildNarrowAnalyticalEntriesCTE renders the scalar part of
// analytical_entries without its archive-wide participant-list aggregates.
// Listing fast paths page or count this relation before enriching only the
// returned rows with participant facts.
func buildNarrowAnalyticalEntriesCTE(name string) string {
	return buildScalarAnalyticalEntriesCTE(name, true)
}

// buildNarrowFileEntriesCTE omits the per-message attachment summary because
// Files joins the individual attachment rows and never consumes those totals.
func buildNarrowFileEntriesCTE(name string) string {
	return buildScalarAnalyticalEntriesCTE(name, false)
}

func buildScalarAnalyticalEntriesCTE(name string, includeAttachmentSummary bool) string {
	return buildScalarAnalyticalEntriesCTEWithSource(name, includeAttachmentSummary, "s.account_email", "m.archive_only")
}

func buildScalarAnalyticalEntriesCTEWithSource(name string, includeAttachmentSummary bool, sourceIdentifier, archiveOnly string) string {
	attachmentColumns := `
		CAST(0 AS BIGINT) AS attachment_count,
		CAST(0 AS BIGINT) AS attachment_size`
	attachmentJoin := ""
	if includeAttachmentSummary {
		attachmentColumns = `
		COALESCE(att.attachment_count, 0) AS attachment_count,
		COALESCE(att.attachment_size, 0) AS attachment_size`
		attachmentJoin = `
	LEFT JOIN (
		SELECT message_id, COUNT(*) AS attachment_count,
			COALESCE(SUM(size), 0) AS attachment_size
		FROM attachments GROUP BY message_id
	) att ON att.message_id = m.id`
	}
	return name + ` AS NOT MATERIALIZED (
	SELECT
		m.id AS message_id,
		m.source_id,
		COALESCE(s.source_type, '') AS source_type,
		COALESCE(` + sourceIdentifier + `, '') AS source_identifier,
		m.source_message_id,
		m.conversation_id,
		COALESCE(c.source_conversation_id, '') AS source_conversation_id,
		COALESCE(c.conversation_type, '') AS conversation_type,
		COALESCE(c.title, '') AS conversation_title,
		COALESCE(m.message_type, '') AS message_type,
		m.list_id,
		m.sender_id,
		COALESCE(` + identityindex.RelayAddressSubquery("m.id", "message_recipients") + `, NULLIF(sender.email_address, ''), NULLIF(sender.phone_number, ''), ` + xUserIDExpr("sender", "") + `, '') AS sender_identifier,
		` + sqlAnalyticalEntriesParticipantLabel("sender") + ` AS sender_display,
		COALESCE(NULLIF(` + identityindex.SenderEnvelopeDomainExpr(identityindex.RelayAddressSubquery("m.id", "message_recipients")) + `, ''), sender.domain, '') AS sender_domain,
		m.sent_at AS occurred_at,
		COALESCE(m.subject, '') AS subject,
		COALESCE(m.snippet, '') AS snippet,
		m.is_from_me,
		m.size_estimate,
		m.deleted_at IS NOT NULL AS internally_deleted,
		` + archiveOnly + ` AS archive_only,
		m.deleted_from_source_at IS NOT NULL AS deleted_from_source,
		COALESCE(m.has_attachments, false) AS has_attachments,` + attachmentColumns + `
	FROM messages m
	JOIN sources s ON s.id = m.source_id
	LEFT JOIN conversations c ON c.id = m.conversation_id
	LEFT JOIN participants sender ON sender.id = m.sender_id` + attachmentJoin + `
)`
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
    m.list_id,
    m.sender_id,
    COALESCE(` + identityindex.RelayAddressSubquery("m.id", "message_recipients") + `, NULLIF(sender.email_address, ''), NULLIF(sender.phone_number, ''), ` + xUserIDExpr("sender", "") + `, '') AS sender_identifier,
    ` + sqlAnalyticalEntriesParticipantLabel("sender") + ` AS sender_display,
    COALESCE(NULLIF(` + identityindex.SenderEnvelopeDomainExpr(identityindex.RelayAddressSubquery("m.id", "message_recipients")) + `, ''), sender.domain, '') AS sender_domain,
    m.sent_at AS occurred_at,
    COALESCE(m.subject, '') AS subject,
    COALESCE(m.snippet, '') AS snippet,
    m.is_from_me,
	m.size_estimate,
	m.deleted_at IS NOT NULL AS internally_deleted,
	m.archive_only,
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
var sqlVMessages = `
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
` + sqlMessageSenderJoins + `
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
var sqlVSenders = `
CREATE OR REPLACE VIEW v_senders AS
SELECT
    ` + identityindex.SenderAddressExpr("mr", "p") + ` AS from_email,
    FIRST(CASE WHEN mr.is_relayed THEN mr.email_address ELSE COALESCE(
        NULLIF(TRIM(mr.display_name), ''),
        NULLIF(TRIM(p.display_name), ''),
        p.email_address
    ) END) AS from_name,
    ` + identityindex.SenderDomainExpr("mr", "p") + ` AS from_domain,
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
GROUP BY ` + identityindex.SenderAddressExpr("mr", "p") + `, ` + identityindex.SenderDomainExpr("mr", "p") + `
`

// sqlVDomains: per-domain aggregates.
var sqlVDomains = `
CREATE OR REPLACE VIEW v_domains AS
SELECT
    ` + identityindex.SenderDomainExpr("mr", "p") + ` AS domain,
    COUNT(*) AS message_count,
    SUM(m.size_estimate) AS total_size,
    COUNT(DISTINCT ` + identityindex.SenderAddressExpr("mr", "p") + `) AS sender_count
FROM message_recipients mr
JOIN participants p ON p.id = mr.participant_id
JOIN messages m ON m.id = mr.message_id
WHERE mr.recipient_type = 'from'
GROUP BY ` + identityindex.SenderDomainExpr("mr", "p") + `
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

// sqlMessageSenderJoins resolves a message's From recipient before its direct
// sender. Separate fragments let page queries bound each input before aggregation.
var sqlMessageSenderJoins = sqlMessageSenderJoinsStart + sqlMessageSenderJoinsMiddle + sqlMessageSenderJoinsEnd

var sqlMessageSenderJoinsStart = `LEFT JOIN (
    SELECT
        mr.message_id,
        FIRST(` + identityindex.SenderAddressExpr("mr", "p") + `) AS from_email,
        FIRST(
            ` + recipientNameExpr("mr", "p") + `
        ) AS from_name,
        FIRST(` + identityindex.SenderDomainExpr("mr", "p") + `) AS from_domain,
        FIRST(COALESCE(p.phone_number, '')) AS from_phone
    FROM message_recipients mr
    JOIN participants p ON p.id = mr.participant_id
    WHERE mr.recipient_type = 'from'`

var sqlMessageSenderJoinsMiddle = `
    GROUP BY mr.message_id
) ms ON ms.message_id = m.id
LEFT JOIN (
    SELECT
        msg.id AS message_id,
        COALESCE(p.email_address, '') AS from_email,
        ` + directSenderNameExpr("p", "") + ` AS from_name,
        COALESCE(p.domain, '') AS from_domain,
        COALESCE(p.phone_number, '') AS from_phone
    FROM messages msg
    JOIN participants p ON p.id = msg.sender_id
    WHERE msg.sender_id IS NOT NULL`

const sqlMessageSenderJoinsEnd = `
) ds ON ds.message_id = m.id AND ms.message_id IS NULL`

// sqlPageSenderLabels reuses v_messages' sender relation with bounded inputs.
// Only the page label falls back from an unnamed direct sender to its identifier.
func sqlPageSenderLabels(messageIDs string) string {
	return `SELECT m.id AS sender_message_id,
		COALESCE(NULLIF(TRIM(COALESCE(ms.from_name, ds.from_name)), ''),
			NULLIF(COALESCE(ms.from_phone, ds.from_phone), ''), ms.from_email, ds.from_email, '') AS sender_label
	FROM messages m
	` + sqlMessageSenderJoinsStart + " AND mr.message_id IN (" + messageIDs + ")" +
		sqlMessageSenderJoinsMiddle + " AND msg.id IN (" + messageIDs + ")" + sqlMessageSenderJoinsEnd + `
	WHERE m.id IN (` + messageIDs + `)`
}
