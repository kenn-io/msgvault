package identityindex

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// materializeContributions keeps the compact logical and daily scoring grains
// across append builds. Only new activity edges are reduced on an append;
// global rankings still read the small daily contribution table.
func (b builder) materializeContributions(ctx context.Context, activity string, effectiveAt time.Time) error {
	temperatureSQL := buildRelationshipTemperatureDailySQL(activity, effectiveAt)
	logicalSQL := buildLogicalActivityMaterializationSQL(activity)
	if b.opts.Mode == ModeIncremental &&
		datasetContainsParquet(b.opts.CommittedRoot, DatasetLogicalContributions) &&
		datasetContainsParquet(b.opts.CommittedRoot, DatasetTemperatureContributions) {
		delta := b.deltaActivityRelation()
		reuseTemperature, err := b.canReuseTemperatureContributions(ctx, effectiveAt)
		if err != nil {
			return err
		}
		if reuseTemperature {
			temperatureSQL = mergeTemperatureContributionsSQL(
				b.committed(DatasetTemperatureContributions),
				buildRelationshipTemperatureDailySQL(delta, effectiveAt),
			)
		}
		logicalSQL = mergeLogicalContributionsSQL(
			b.committed(DatasetLogicalContributions),
			buildLogicalActivityMaterializationSQL(delta),
		)
	}
	if err := b.materializeBuildTable(ctx, temperatureBuildRelation,
		temperatureSQL, "relationship_temperature_daily"); err != nil {
		return err
	}
	if err := b.materializeBuildTable(ctx, logicalBuildRelation,
		logicalSQL, "logical_activity"); err != nil {
		return err
	}
	return nil
}

// A previously future-dated message can enter the score window without a
// new activity shard. In that rare case the daily score reduction must read
// the committed activity once instead of adding only the new contribution.
func (b builder) canReuseTemperatureContributions(ctx context.Context, effectiveAt time.Time) (bool, error) {
	var priorCutoff sql.NullTime
	query := `SELECT max(temperature_effective_at) FROM read_parquet('` +
		quoteSQLString(b.committed(DatasetPeople)) + `')`
	if err := b.db.QueryRowContext(ctx, query).Scan(&priorCutoff); err != nil {
		return false, fmt.Errorf("inspect prior relationship score window: %w", err)
	}
	if !priorCutoff.Valid {
		return false, nil
	}
	if effectiveAt.Before(priorCutoff.Time) {
		return false, nil
	}
	var newlyEligible bool
	query = `SELECT EXISTS (
		SELECT 1 FROM read_parquet('` + quoteSQLString(b.committed("messages")) +
		`', hive_partitioning=true, union_by_name=true)
		WHERE sent_at > ? AND sent_at <= ?)`
	if err := b.db.QueryRowContext(ctx, query, priorCutoff.Time, effectiveAt).Scan(&newlyEligible); err != nil {
		return false, fmt.Errorf("inspect newly eligible relationship messages: %w", err)
	}
	return !newlyEligible, nil
}

func mergeTemperatureContributionsSQL(committed, delta string) string {
	return fmt.Sprintf(`
WITH contributions AS (
	SELECT * FROM read_parquet('%s')
	UNION ALL
	SELECT * FROM (%s)
)
SELECT canonical_id, event_date,
       sum(sent_count)::BIGINT AS sent_count,
       sum(received_count)::BIGINT AS received_count,
       sum(meeting_count)::BIGINT AS meeting_count,
       sum(email_count)::BIGINT AS email_count,
       sum(chat_count)::BIGINT AS chat_count,
       sum(total_count)::BIGINT AS total_count,
       bit_or(modality_mask)::UTINYINT AS modality_mask,
       max(last_at)::TIMESTAMP AS last_at
FROM contributions
GROUP BY canonical_id, event_date`, quoteSQLString(committed), delta)
}

func mergeLogicalContributionsSQL(committed, delta string) string {
	return fmt.Sprintf(`
WITH old_rows AS (
	SELECT * FROM read_parquet('%s')
), new_rows AS (
	SELECT * FROM (%s)
), combined AS (
	SELECT * FROM old_rows UNION ALL SELECT * FROM new_rows
), unit_parts AS (
	SELECT entry_key, max(attachment_count) AS attachment_count
	FROM old_rows WHERE relation_kind IN (1, 2, 4) GROUP BY entry_key
	UNION ALL
	SELECT entry_key, max(attachment_count) AS attachment_count
	FROM new_rows WHERE relation_kind IN (1, 2, 4) GROUP BY entry_key
), units AS (
	SELECT c.entry_key,
	       arg_max(c.anchor_message_id, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS anchor_message_id,
	       arg_max(c.conversation_id, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS conversation_id,
	       arg_max(c.source_id, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS source_id,
	       arg_max(c.source_type, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS source_type,
	       max(c.occurred_at)::TIMESTAMP AS occurred_at,
	       arg_max(c.message_type, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS message_type,
	       arg_max(c.entry_kind, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS entry_kind,
	       arg_max(c.is_from_me, struct_pack(at := c.occurred_at, id := c.anchor_message_id)) AS is_from_me,
	       max(p.attachment_count)::BIGINT AS attachment_count,
	       bool_or(c.is_owner) AS with_owner
	FROM combined c
	LEFT JOIN (SELECT entry_key, sum(attachment_count) AS attachment_count
	           FROM unit_parts GROUP BY entry_key) p USING (entry_key)
	WHERE c.relation_kind IN (1, 2, 4)
	GROUP BY c.entry_key
), merged AS (
	SELECT relation_kind, entry_key, canonical_id, domain,
	       bool_or(c.is_author AND c.anchor_message_id = u.anchor_message_id) AS is_author,
	       bool_or(c.is_owner) AS is_owner
	FROM combined c
	LEFT JOIN units u USING (entry_key)
	GROUP BY relation_kind, entry_key, canonical_id, domain
)
SELECT m.relation_kind, m.entry_key,
       CASE WHEN m.relation_kind = 3 THEN NULL::BIGINT ELSE u.anchor_message_id END AS anchor_message_id,
       CASE WHEN m.relation_kind = 3 THEN NULL::BIGINT ELSE u.conversation_id END AS conversation_id,
       CASE WHEN m.relation_kind = 3 THEN NULL::BIGINT ELSE u.source_id END AS source_id,
       CASE WHEN m.relation_kind = 3 THEN NULL::VARCHAR ELSE u.source_type END AS source_type,
       CASE WHEN m.relation_kind = 3 THEN NULL::TIMESTAMP ELSE u.occurred_at END AS occurred_at,
       CASE WHEN m.relation_kind = 3 THEN NULL::VARCHAR ELSE u.message_type END AS message_type,
       CASE WHEN m.relation_kind = 3 THEN NULL::VARCHAR ELSE u.entry_kind END AS entry_kind,
       CASE WHEN m.relation_kind = 3 THEN NULL::BOOLEAN ELSE u.is_from_me END AS is_from_me,
       CASE WHEN m.relation_kind = 3 THEN NULL::BIGINT ELSE u.attachment_count END AS attachment_count,
       m.canonical_id,
       CASE WHEN m.relation_kind = 1 THEN m.is_author ELSE NULL::BOOLEAN END AS is_author,
       CASE WHEN m.relation_kind = 1 THEN m.is_owner ELSE NULL::BOOLEAN END AS is_owner,
       CASE WHEN m.relation_kind = 1 THEN u.with_owner ELSE NULL::BOOLEAN END AS with_owner,
       m.domain
FROM merged m LEFT JOIN units u USING (entry_key)`, quoteSQLString(committed), delta)
}
