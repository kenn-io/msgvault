package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

// ValidationOptions describes the staged datasets that must form a complete
// relationship index generation. ActivityPath overrides the default staged
// activity glob when validating an incremental build over live plus staged
// shards.
type ValidationOptions struct {
	OutputRoot             string
	RequiredOutputDatasets []string
	ActivityPath           string
}

// Validate rejects malformed or internally inconsistent relationship indexes
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

	activityPath := opts.ActivityPath
	if strings.TrimSpace(activityPath) == "" {
		activityPath = parquetDatasetGlob(opts.OutputRoot, DatasetActivity)
	}
	relations := map[string]string{
		DatasetActivity: activityRelation(activityPath, true),
		DatasetPeople: activityRelation(
			filepath.Join(opts.OutputRoot, DatasetPeople, "*.parquet"),
			false,
		),
		DatasetDomains: activityRelation(
			filepath.Join(opts.OutputRoot, DatasetDomains, "*.parquet"),
			false,
		),
		DatasetRelationshipDaily: activityRelation(
			filepath.Join(opts.OutputRoot, DatasetRelationshipDaily, "*.parquet"),
			false,
		),
	}
	for _, dataset := range opts.RequiredOutputDatasets {
		relation, ok := relations[dataset]
		if !ok {
			return fmt.Errorf("validate %s schema: dataset is not part of the relationship index", dataset)
		}
		if err := validateDatasetSchema(ctx, db, dataset, relation); err != nil {
			return err
		}
	}

	activity := relations[DatasetActivity]
	people := relations[DatasetPeople]
	domains := relations[DatasetDomains]
	daily := relations[DatasetRelationshipDaily]
	checks := []struct {
		dataset   string
		invariant string
		query     string
	}{
		{
			DatasetActivity,
			"duplicate message/canonical/domain keys",
			`SELECT count(*) FROM (
				SELECT message_id, canonical_id, participant_domain
				FROM ` + activity + `
				GROUP BY ALL HAVING count(*) > 1
			)`,
		},
		{
			DatasetActivity,
			"rows have no edge origin",
			`SELECT count(*) FROM ` + activity + `
			 WHERE canonical_id IS NOT NULL
			   AND NOT is_direct AND NOT is_conversation_member`,
		},
		{
			DatasetActivity,
			"invalid participantless message sentinels",
			`SELECT count(*) FROM ` + activity + `
			 WHERE canonical_id IS NULL
			   AND (participant_domain IS NOT NULL
			        OR is_direct OR is_conversation_member
			        OR is_sender OR is_author OR is_owner)`,
		},
		{
			DatasetPeople,
			"duplicate canonical IDs",
			`SELECT count(*) FROM (
				SELECT canonical_id FROM ` + people + `
				GROUP BY canonical_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetPeople,
			"source rollups do not decompose identity totals",
			`SELECT count(*)
			 FROM ` + people + ` p
			 WHERE len(p.source_rollups) = 0
			    OR (SELECT sum(item.activity_count)::BIGINT
			        FROM unnest(p.source_rollups) AS source(item))
			       IS DISTINCT FROM p.activity_count
			    OR (SELECT sum(item.file_count)::BIGINT
			        FROM unnest(p.source_rollups) AS source(item))
			       IS DISTINCT FROM p.file_count
			    OR (SELECT min(item.first_at)::TIMESTAMP
			        FROM unnest(p.source_rollups) AS source(item))
			       IS DISTINCT FROM p.first_at
			    OR (SELECT max(item.last_at)::TIMESTAMP
			        FROM unnest(p.source_rollups) AS source(item))
			       IS DISTINCT FROM p.last_at
			    OR (SELECT sum(item.count)::BIGINT
			        FROM unnest(p.source_counts) AS source(item))
			       IS DISTINCT FROM p.activity_count`,
		},
		{
			DatasetDomains,
			"duplicate domain keys",
			`SELECT count(*) FROM (
				SELECT domain FROM ` + domains + `
				GROUP BY domain HAVING count(*) > 1
			)`,
		},
		{
			DatasetRelationshipDaily,
			"duplicate canonical/date keys",
			`SELECT count(*) FROM (
				SELECT canonical_id, event_date FROM ` + daily + `
				GROUP BY canonical_id, event_date HAVING count(*) > 1
			)`,
		},
		{
			DatasetRelationshipDaily,
			"canonical IDs absent from relationship_people",
			`SELECT count(*) FROM (
				SELECT DISTINCT d.canonical_id
				FROM ` + daily + ` d
				LEFT JOIN ` + people + ` p USING (canonical_id)
				WHERE p.canonical_id IS NULL
			)`,
		},
		{
			DatasetRelationshipDaily,
			"owner canonical IDs are present",
			`SELECT count(*) FROM ` + daily + ` d
			 JOIN ` + people + ` p USING (canonical_id)
			 WHERE p.is_owner`,
		},
		{
			DatasetRelationshipDaily,
			"invalid components or modality mask",
			`SELECT count(*) FROM ` + daily + `
			 WHERE canonical_id IS NULL OR event_date IS NULL
			    OR sent_units IS NULL OR sent_units < 0
			    OR received_units IS NULL OR received_units < 0
			    OR meeting_units IS NULL OR meeting_units < 0
			    OR modality_mask IS NULL
			    OR (modality_mask & 7::UTINYINT) IS DISTINCT FROM modality_mask
			    OR last_at IS NULL OR last_at::DATE IS DISTINCT FROM event_date`,
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
const duckDBTypeBoolean = "BOOLEAN"

var datasetSchemas = map[string][]schemaColumn{
	DatasetActivity: {
		{"message_id", duckDBTypeBigInt},
		{"conversation_id", duckDBTypeBigInt},
		{"source_id", duckDBTypeBigInt},
		{"source_type", "VARCHAR"},
		{"occurred_at", "TIMESTAMP"},
		{"message_type", "VARCHAR"},
		{"conversation_type", "VARCHAR"},
		{"entry_kind", "VARCHAR"},
		{"is_chat", duckDBTypeBoolean},
		{"is_from_me", duckDBTypeBoolean},
		{"attachment_count", "INTEGER"},
		{"has_attachments", duckDBTypeBoolean},
		{"deleted_from_source", duckDBTypeBoolean},
		{"canonical_id", duckDBTypeBigInt},
		{"participant_domain", "VARCHAR"},
		{"is_direct", duckDBTypeBoolean},
		{"is_conversation_member", duckDBTypeBoolean},
		{"is_sender", duckDBTypeBoolean},
		{"is_author", duckDBTypeBoolean},
		{"is_owner", duckDBTypeBoolean},
		{"occurred_year", duckDBTypeBigInt},
	},
	DatasetPeople: {
		{"canonical_id", duckDBTypeBigInt},
		{"display_label", "VARCHAR"},
		{"partial_label", duckDBTypeBoolean},
		{"member_ids", "BIGINT[]"},
		{"search_values", "VARCHAR[]"},
		{"is_owner", duckDBTypeBoolean},
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
	DatasetDomains: {
		{"domain", "VARCHAR"},
		{"activity_count", duckDBTypeBigInt},
		{"person_count", duckDBTypeBigInt},
		{"file_count", duckDBTypeBigInt},
		{"first_at", "TIMESTAMP"},
		{"last_at", "TIMESTAMP"},
		{"source_counts", "STRUCT(source_type VARCHAR, count BIGINT)[]"},
	},
	DatasetRelationshipDaily: {
		{"canonical_id", duckDBTypeBigInt},
		{"event_date", "DATE"},
		{"sent_units", duckDBTypeBigInt},
		{"received_units", duckDBTypeBigInt},
		{"meeting_units", duckDBTypeBigInt},
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
