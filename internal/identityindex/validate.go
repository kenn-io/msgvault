package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// ValidationOptions describes the complete post-publication population and
// the staged datasets that must contain a schema-bearing shard.
type ValidationOptions struct {
	OutputRoot             string
	RequiredOutputDatasets []string
	Activity               ActivityPaths
	Messages               string
	Participants           string
	Conversations          string
	AnchorDate             time.Time
}

// Validate rejects malformed or internally inconsistent identity indexes
// before the cache marker makes them visible to readers.
func Validate(
	ctx context.Context,
	db sqlExecutor,
	opts ValidationOptions,
) error {
	for _, dataset := range opts.RequiredOutputDatasets {
		if err := requireParquetDataset(opts.OutputRoot, dataset); err != nil {
			return fmt.Errorf("validate %s: %w", dataset, err)
		}
	}

	facts := activityRelation(opts.Activity.Facts, true)
	directEdges := activityRelation(opts.Activity.DirectEdges, false)
	conversationEdges := activityRelation(opts.Activity.ConversationEdges, false)
	directory := activityRelation(opts.Activity.Directory, false)
	clusters := activityRelation(opts.Activity.Clusters, false)
	owners := activityRelation(opts.Activity.Owners, false)
	participants := activityRelation(opts.Participants, false)
	conversations := activityRelation(opts.Conversations, false)
	rollups := activityRelation(
		filepath.Join(opts.OutputRoot, DatasetRollups, "*.parquet"),
		false,
	)
	relationships := activityRelation(
		filepath.Join(opts.OutputRoot, DatasetRelationships, "*.parquet"),
		false,
	)
	domainRollups := activityRelation(
		filepath.Join(opts.OutputRoot, DatasetDomainRollups, "*.parquet"),
		false,
	)
	daily := activityRelation(
		filepath.Join(opts.OutputRoot, DatasetRelationshipDaily, "*.parquet"),
		false,
	)
	messagesPath := opts.Messages
	if strings.TrimSpace(messagesPath) == "" {
		messagesPath = parquetDatasetGlob(opts.OutputRoot, "messages")
	}
	messages := activityRelation(messagesPath, true)

	relations := map[string]string{
		DatasetEntryFacts:        facts,
		DatasetDirectEdges:       directEdges,
		DatasetConversationEdges: conversationEdges,
		DatasetDirectory:         directory,
		DatasetRollups:           rollups,
		DatasetDomainRollups:     domainRollups,
		DatasetRelationships:     relationships,
		DatasetRelationshipDaily: daily,
	}
	for _, dataset := range opts.RequiredOutputDatasets {
		if err := validateDatasetSchema(ctx, db, dataset, relations[dataset]); err != nil {
			return err
		}
	}

	checks := []struct {
		dataset   string
		invariant string
		query     string
	}{
		{
			DatasetEntryFacts,
			"duplicate message IDs",
			`SELECT count(*) FROM (
				SELECT message_id FROM ` + facts + `
				GROUP BY message_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetEntryFacts,
			"cached messages have no fact",
			`SELECT count(*) FROM ` + messages + ` m
			 LEFT JOIN ` + facts + ` f
			   ON f.message_id = try_cast(m.id AS BIGINT)
			 WHERE f.message_id IS NULL`,
		},
		{
			DatasetEntryFacts,
			"facts have no cached message",
			`SELECT count(*) FROM ` + facts + ` f
			 LEFT JOIN ` + messages + ` m
			   ON try_cast(m.id AS BIGINT) = f.message_id
			 WHERE m.id IS NULL`,
		},
		{
			DatasetDirectEdges,
			"duplicate message/participant pairs",
			`SELECT count(*) FROM (
				SELECT message_id, participant_id FROM ` + directEdges + `
				GROUP BY message_id, participant_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetConversationEdges,
			"duplicate conversation/participant pairs",
			`SELECT count(*) FROM (
				SELECT conversation_id, participant_id FROM ` + conversationEdges + `
				GROUP BY conversation_id, participant_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetDirectory,
			"duplicate canonical IDs",
			`SELECT count(*) FROM (
				SELECT canonical_id FROM ` + directory + `
				GROUP BY canonical_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetRollups,
			"duplicate canonical IDs",
			`SELECT count(*) FROM (
				SELECT canonical_id FROM ` + rollups + `
				GROUP BY canonical_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetRollups,
			"source rollups do not decompose identity totals",
			`SELECT count(*)
			 FROM ` + rollups + ` r
			 WHERE len(r.source_rollups) = 0
			    OR (SELECT sum(item.activity_count)::BIGINT
			        FROM unnest(r.source_rollups) AS source(item))
			       <> r.activity_count
			    OR (SELECT sum(item.file_count)::BIGINT
			        FROM unnest(r.source_rollups) AS source(item))
			       <> r.file_count
			    OR (SELECT min(item.first_at)::TIMESTAMP
			        FROM unnest(r.source_rollups) AS source(item))
			       <> r.first_at
			    OR (SELECT max(item.last_at)::TIMESTAMP
			        FROM unnest(r.source_rollups) AS source(item))
			       <> r.last_at
			    OR (SELECT sum(item.count)::BIGINT
			        FROM unnest(r.source_counts) AS source(item))
			       <> r.activity_count`,
		},
		{
			DatasetRelationships,
			"duplicate canonical IDs",
			`SELECT count(*) FROM (
				SELECT canonical_id FROM ` + relationships + `
				GROUP BY canonical_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetRelationshipDaily,
			"duplicate canonical/date pairs",
			`SELECT count(*) FROM (
				SELECT canonical_id, event_date FROM ` + daily + `
				GROUP BY canonical_id, event_date HAVING count(*) > 1
			)`,
		},
		{
			DatasetDomainRollups,
			"duplicate domain keys",
			`SELECT count(*) FROM (
				SELECT domain FROM ` + domainRollups + `
				GROUP BY domain HAVING count(*) > 1
			)`,
		},
		{
			DatasetDirectEdges,
			"edges without a fact",
			`SELECT count(*) FROM ` + directEdges + ` d
			 LEFT JOIN ` + facts + ` f USING (message_id)
			 WHERE f.message_id IS NULL`,
		},
		{
			DatasetDirectEdges,
			"edges without a participant",
			`SELECT count(*) FROM ` + directEdges + ` d
			 LEFT JOIN ` + participants + ` p ON p.id = d.participant_id
			 WHERE p.id IS NULL`,
		},
		{
			DatasetConversationEdges,
			"edges without a conversation",
			`SELECT count(*) FROM ` + conversationEdges + ` d
			 LEFT JOIN ` + conversations + ` c ON c.id = d.conversation_id
			 WHERE c.id IS NULL`,
		},
		{
			DatasetConversationEdges,
			"edges without a participant",
			`SELECT count(*) FROM ` + conversationEdges + ` d
			 LEFT JOIN ` + participants + ` p ON p.id = d.participant_id
			 WHERE p.id IS NULL`,
		},
		{
			DatasetRollups,
			"canonical IDs absent from the directory",
			`SELECT count(*) FROM ` + rollups + ` r
			 LEFT JOIN ` + directory + ` d USING (canonical_id)
			 WHERE d.canonical_id IS NULL`,
		},
		{
			DatasetRelationships,
			"canonical IDs absent from the directory",
			`SELECT count(*) FROM ` + relationships + ` r
			 LEFT JOIN ` + directory + ` d USING (canonical_id)
			 WHERE d.canonical_id IS NULL`,
		},
		{
			DatasetRelationshipDaily,
			"canonical IDs absent from the directory",
			`SELECT count(*) FROM ` + daily + ` r
			 LEFT JOIN ` + directory + ` d USING (canonical_id)
			 WHERE d.canonical_id IS NULL`,
		},
		{
			DatasetRelationshipDaily,
			"daily identities have no relationship rollup",
			`SELECT count(*) FROM (
				SELECT DISTINCT f.canonical_id FROM ` + daily + ` f
				LEFT JOIN ` + relationships + ` r USING (canonical_id)
				WHERE r.canonical_id IS NULL
			)`,
		},
		{
			DatasetRelationships,
			"owner canonical IDs are present",
			ownerRelationshipCountSQL(relationships, clusters, owners),
		},
		{
			DatasetRelationshipDaily,
			"owner canonical IDs are present",
			ownerRelationshipCountSQL(daily, clusters, owners),
		},
		{
			DatasetRelationships,
			"rows have a different anchor date",
			`SELECT count(*) FROM ` + relationships +
				` WHERE anchor_date <> DATE '` +
				quoteSQLString(opts.AnchorDate.UTC().Format(time.DateOnly)) + `'`,
		},
		{
			DatasetRelationships,
			"non-empty rows have null last_at",
			`SELECT count(*) FROM ` + relationships + ` WHERE last_at IS NULL`,
		},
		{
			DatasetRelationshipDaily,
			"non-empty rows have null last_at",
			`SELECT count(*) FROM ` + daily + ` WHERE last_at IS NULL`,
		},
		{
			DatasetRelationshipDaily,
			"rows violate raw count decomposition",
			`SELECT count(*) FROM ` + daily + `
			 WHERE sent_count <> sent_units
			    OR meeting_count <> meeting_units`,
		},
		{
			DatasetRelationshipDaily,
			"rows have an event-date/last-at mismatch",
			`SELECT count(*) FROM ` + daily + `
			 WHERE last_at::DATE <> event_date`,
		},
		{
			DatasetRelationships,
			"daily signals do not decompose relationship totals",
			`WITH daily_totals AS (
				SELECT canonical_id,
				       sum(sent_count)::BIGINT AS sent_count,
				       sum(meeting_count)::BIGINT AS meeting_count,
				       bit_or(modality_mask)::UTINYINT AS modality_mask,
				       max(last_at)::TIMESTAMP AS last_at
				FROM ` + daily + `
				GROUP BY canonical_id
			)
			SELECT count(*)
			FROM ` + relationships + ` r
			LEFT JOIN daily_totals d USING (canonical_id)
			WHERE d.canonical_id IS NULL
			   OR d.sent_count <> r.sent_count
			   OR d.meeting_count <> r.meeting_count
			   OR d.modality_mask <> r.modality_mask
			   OR d.last_at <> r.last_at`,
		},
	}
	for _, check := range checks {
		var count int64
		if err := db.QueryRowContext(ctx, check.query).Scan(&count); err != nil {
			return fmt.Errorf("validate %s %s: %w", check.dataset, check.invariant, err)
		}
		if count != 0 {
			return fmt.Errorf("validate %s: %d %s", check.dataset, count, check.invariant)
		}
	}
	return nil
}

type schemaColumn struct {
	name string
	typ  string
}

const duckDBTypeBigInt = "BIGINT"

var datasetSchemas = map[string][]schemaColumn{
	DatasetEntryFacts: {
		{"message_id", duckDBTypeBigInt},
		{"conversation_id", duckDBTypeBigInt},
		{"source_id", duckDBTypeBigInt},
		{"source_type", "VARCHAR"},
		{"occurred_at", "TIMESTAMP"},
		{"message_type", "VARCHAR"},
		{"conversation_type", "VARCHAR"},
		{"entry_kind", "VARCHAR"},
		{"is_chat", "BOOLEAN"},
		{"is_from_me", "BOOLEAN"},
		{"has_attachments", "BOOLEAN"},
		{"attachment_count", "INTEGER"},
		{"deleted_from_source", "BOOLEAN"},
		{"occurred_year", duckDBTypeBigInt},
	},
	DatasetDirectEdges: {
		{"message_id", duckDBTypeBigInt},
		{"occurred_year", duckDBTypeBigInt},
		{"participant_id", duckDBTypeBigInt},
		{"participant_domain", "VARCHAR"},
		{"is_sender", "BOOLEAN"},
		{"is_author", "BOOLEAN"},
	},
	DatasetConversationEdges: {
		{"conversation_id", duckDBTypeBigInt},
		{"participant_id", duckDBTypeBigInt},
		{"participant_domain", "VARCHAR"},
	},
	DatasetDirectory: {
		{"canonical_id", duckDBTypeBigInt},
		{"display_label", "VARCHAR"},
		{"partial_label", "BOOLEAN"},
		{"member_ids", "BIGINT[]"},
		{"search_values", "VARCHAR[]"},
		{"is_owner", "BOOLEAN"},
	},
	DatasetRollups: {
		{"canonical_id", duckDBTypeBigInt},
		{"activity_count", duckDBTypeBigInt},
		{"file_count", duckDBTypeBigInt},
		{"first_at", "TIMESTAMP"},
		{"last_at", "TIMESTAMP"},
		{"source_counts", "STRUCT(source_type VARCHAR, count BIGINT)[]"},
		{
			"source_rollups",
			"STRUCT(source_id BIGINT, source_type VARCHAR, activity_count BIGINT, file_count BIGINT, first_at TIMESTAMP, last_at TIMESTAMP)[]",
		},
	},
	DatasetDomainRollups: {
		{"domain", "VARCHAR"},
		{"activity_count", duckDBTypeBigInt},
		{"person_count", duckDBTypeBigInt},
		{"file_count", duckDBTypeBigInt},
		{"first_at", "TIMESTAMP"},
		{"last_at", "TIMESTAMP"},
		{"source_counts", "STRUCT(source_type VARCHAR, count BIGINT)[]"},
	},
	DatasetRelationships: {
		{"canonical_id", duckDBTypeBigInt},
		{"anchor_date", "DATE"},
		{"sent_decayed", "DOUBLE"},
		{"received_decayed", "DOUBLE"},
		{"meetings_decayed", "DOUBLE"},
		{"sent_count", duckDBTypeBigInt},
		{"meeting_count", duckDBTypeBigInt},
		{"modality_mask", "UTINYINT"},
		{"last_at", "TIMESTAMP"},
	},
	DatasetRelationshipDaily: {
		{"canonical_id", duckDBTypeBigInt},
		{"event_date", "DATE"},
		{"sent_units", duckDBTypeBigInt},
		{"received_units", duckDBTypeBigInt},
		{"meeting_units", duckDBTypeBigInt},
		{"sent_count", duckDBTypeBigInt},
		{"meeting_count", duckDBTypeBigInt},
		{"modality_mask", "UTINYINT"},
		{"last_at", "TIMESTAMP"},
	},
}

func validateDatasetSchema(
	ctx context.Context,
	db sqlExecutor,
	dataset, relation string,
) error {
	expected, ok := datasetSchemas[dataset]
	if !ok {
		return fmt.Errorf("validate %s schema: no expected schema", dataset)
	}
	rows, err := db.QueryContext(ctx, "DESCRIBE SELECT * FROM "+relation)
	if err != nil {
		return fmt.Errorf("validate %s schema: %w", dataset, err)
	}
	defer func() { _ = rows.Close() }()

	actual := make([]schemaColumn, 0, len(expected))
	for rows.Next() {
		var name, typ string
		var nullable, key, defaultValue, extra sql.NullString
		if err := rows.Scan(
			&name,
			&typ,
			&nullable,
			&key,
			&defaultValue,
			&extra,
		); err != nil {
			return fmt.Errorf("validate %s schema: scan: %w", dataset, err)
		}
		actual = append(actual, schemaColumn{name: name, typ: typ})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("validate %s schema: iterate: %w", dataset, err)
	}
	if len(actual) != len(expected) {
		return fmt.Errorf(
			"validate %s schema: expected %d columns, got %d",
			dataset,
			len(expected),
			len(actual),
		)
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return fmt.Errorf(
				"validate %s schema: column %d expected %s %s, got %s %s",
				dataset,
				i+1,
				expected[i].name,
				expected[i].typ,
				actual[i].name,
				actual[i].typ,
			)
		}
	}
	return nil
}

func ownerRelationshipCountSQL(relationship, clusters, owners string) string {
	return strings.TrimSpace(`
		WITH owner_canon AS (
			SELECT DISTINCT coalesce(c.canonical_id, try_cast(o.participant_id AS BIGINT))
			       AS canonical_id
			FROM ` + owners + ` o
			LEFT JOIN ` + clusters + ` c ON c.participant_id = o.participant_id
		)
		SELECT count(*) FROM ` + relationship + ` r
		JOIN owner_canon o USING (canonical_id)
	`)
}
