package api

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/identityindex"
)

// ensureIdentityCacheFixtureDatasets keeps hand-built API fixtures compatible
// with the version-15 cache contract. Tests that exercise identity endpoints
// should build real populated datasets; legacy Explore/timeline fixtures only
// need schema-correct empty files so readiness can validate the publication.
func ensureIdentityCacheFixtureDatasets(
	t *testing.T,
	db *sql.DB,
	analyticsDir string,
) {
	t.Helper()
	queries := map[string]string{
		identityindex.DatasetEntryFacts: `
			SELECT NULL::BIGINT AS message_id, NULL::BIGINT AS conversation_id,
			       NULL::BIGINT AS source_id, NULL::VARCHAR AS source_type,
			       NULL::TIMESTAMP AS occurred_at, NULL::VARCHAR AS message_type,
			       NULL::VARCHAR AS conversation_type, NULL::VARCHAR AS entry_kind,
			       NULL::BOOLEAN AS is_chat, NULL::BOOLEAN AS is_from_me,
			       NULL::BOOLEAN AS has_attachments, NULL::INTEGER AS attachment_count,
			       NULL::BOOLEAN AS deleted_from_source, NULL::SMALLINT AS occurred_year
			WHERE false`,
		identityindex.DatasetDirectEdges: `
			SELECT NULL::BIGINT AS message_id, NULL::SMALLINT AS occurred_year,
			       NULL::BIGINT AS participant_id, NULL::VARCHAR AS participant_domain,
			       NULL::BOOLEAN AS is_sender, NULL::BOOLEAN AS is_author
			WHERE false`,
		identityindex.DatasetConversationEdges: `
			SELECT NULL::BIGINT AS conversation_id, NULL::BIGINT AS participant_id,
			       NULL::VARCHAR AS participant_domain
			WHERE false`,
		identityindex.DatasetDirectory: `
			SELECT NULL::BIGINT AS canonical_id, NULL::VARCHAR AS display_label,
			       NULL::BOOLEAN AS partial_label, []::BIGINT[] AS member_ids,
			       []::VARCHAR[] AS search_values, NULL::BOOLEAN AS is_owner
			WHERE false`,
		identityindex.DatasetRollups: `
			SELECT NULL::BIGINT AS canonical_id, NULL::BIGINT AS activity_count,
			       NULL::BIGINT AS file_count, NULL::TIMESTAMP AS first_at,
			       NULL::TIMESTAMP AS last_at,
			       []::STRUCT(source_type VARCHAR, count BIGINT)[] AS source_counts
			WHERE false`,
		identityindex.DatasetDomainRollups: `
			SELECT NULL::VARCHAR AS domain, NULL::BIGINT AS activity_count,
			       NULL::BIGINT AS person_count, NULL::BIGINT AS file_count,
			       NULL::TIMESTAMP AS first_at, NULL::TIMESTAMP AS last_at,
			       []::STRUCT(source_type VARCHAR, count BIGINT)[] AS source_counts
			WHERE false`,
		identityindex.DatasetRelationships: `
			SELECT NULL::BIGINT AS canonical_id, NULL::DATE AS anchor_date,
			       NULL::DOUBLE AS sent_decayed, NULL::DOUBLE AS received_decayed,
			       NULL::DOUBLE AS meetings_decayed, NULL::BIGINT AS sent_count,
			       NULL::BIGINT AS meeting_count, NULL::UTINYINT AS modality_mask,
			       NULL::TIMESTAMP AS last_at
			WHERE false`,
		identityindex.DatasetRelationshipFuture: `
			SELECT NULL::BIGINT AS canonical_id, NULL::DATE AS event_date,
			       NULL::BIGINT AS sent_units, NULL::BIGINT AS received_units,
			       NULL::BIGINT AS meeting_units, NULL::BIGINT AS sent_count,
			       NULL::BIGINT AS meeting_count, NULL::UTINYINT AS modality_mask,
			       NULL::TIMESTAMP AS last_at
			WHERE false`,
	}
	for dataset, selectSQL := range queries {
		dir := filepath.Join(analyticsDir, dataset)
		if _, err := os.Stat(dir); err == nil {
			continue
		}
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := strings.ReplaceAll(
			filepath.ToSlash(filepath.Join(dir, "empty.parquet")),
			"'",
			"''",
		)
		_, err := db.Exec(fmt.Sprintf(
			"COPY (%s) TO '%s' (FORMAT PARQUET)",
			selectSQL,
			path,
		))
		require.NoError(t, err, "write empty %s fixture", dataset)
	}
}
