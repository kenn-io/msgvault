package identityindex

import (
	"context"
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
	future := activityRelation(
		filepath.Join(opts.OutputRoot, DatasetRelationshipFuture, "*.parquet"),
		false,
	)

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
			DatasetRelationships,
			"duplicate canonical IDs",
			`SELECT count(*) FROM (
				SELECT canonical_id FROM ` + relationships + `
				GROUP BY canonical_id HAVING count(*) > 1
			)`,
		},
		{
			DatasetRelationshipFuture,
			"duplicate canonical/date pairs",
			`SELECT count(*) FROM (
				SELECT canonical_id, event_date FROM ` + future + `
				GROUP BY canonical_id, event_date HAVING count(*) > 1
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
			DatasetRelationshipFuture,
			"canonical IDs absent from the directory",
			`SELECT count(*) FROM ` + future + ` r
			 LEFT JOIN ` + directory + ` d USING (canonical_id)
			 WHERE d.canonical_id IS NULL`,
		},
		{
			DatasetRelationshipFuture,
			"future identities have no relationship rollup",
			`SELECT count(*) FROM (
				SELECT DISTINCT f.canonical_id FROM ` + future + ` f
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
			DatasetRelationshipFuture,
			"owner canonical IDs are present",
			ownerRelationshipCountSQL(future, clusters, owners),
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
			DatasetRelationshipFuture,
			"rows are not after the anchor date",
			`SELECT count(*) FROM ` + future +
				` WHERE event_date <= DATE '` +
				quoteSQLString(opts.AnchorDate.UTC().Format(time.DateOnly)) + `'`,
		},
		{
			DatasetRelationshipFuture,
			"non-empty rows have null last_at",
			`SELECT count(*) FROM ` + future + ` WHERE last_at IS NULL`,
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
