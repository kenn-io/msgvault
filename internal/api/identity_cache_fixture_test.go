package api

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/duckdbutil"
	"go.kenn.io/msgvault/internal/identityindex"
)

// ensureIdentityCacheFixtureDatasets keeps hand-built API fixtures compatible
// with the version-15 cache contract. Relationship endpoint tests build real
// populated datasets; unrelated legacy endpoint fixtures only need readable,
// schema-correct empty files.
func ensureIdentityCacheFixtureDatasets(
	t *testing.T,
	db *sql.DB,
	analyticsDir string,
) {
	t.Helper()
	queries := map[string]string{
		identityindex.DatasetActivity: `
			SELECT NULL::BIGINT AS message_id, NULL::BIGINT AS conversation_id,
			       NULL::BIGINT AS source_id, NULL::VARCHAR AS source_type,
			       NULL::TIMESTAMP AS occurred_at, NULL::VARCHAR AS message_type,
			       NULL::VARCHAR AS conversation_type, NULL::VARCHAR AS entry_kind,
			       NULL::BOOLEAN AS is_chat, NULL::BOOLEAN AS is_from_me,
			       NULL::INTEGER AS attachment_count,
			       NULL::BOOLEAN AS deleted_from_source,
			       NULL::BIGINT AS canonical_id,
			       NULL::VARCHAR AS participant_domain,
			       NULL::BOOLEAN AS is_direct,
			       NULL::BOOLEAN AS is_conversation_member,
			       NULL::BOOLEAN AS is_sender, NULL::BOOLEAN AS is_author,
			       NULL::BOOLEAN AS is_owner, NULL::BIGINT AS occurred_year
			WHERE false`,
		identityindex.DatasetPeople: `
			SELECT NULL::BIGINT AS canonical_id, NULL::VARCHAR AS display_label,
			       NULL::BOOLEAN AS partial_label, []::BIGINT[] AS member_ids,
			       []::VARCHAR[] AS search_values, NULL::BOOLEAN AS is_owner,
			       NULL::BIGINT AS activity_count, NULL::BIGINT AS file_count,
			       NULL::TIMESTAMP AS first_at, NULL::TIMESTAMP AS last_at,
			       []::STRUCT(source_type VARCHAR, count BIGINT)[] AS source_counts,
			       []::STRUCT(
				       source_id BIGINT, source_type VARCHAR,
				       activity_count BIGINT, file_count BIGINT,
				       first_at TIMESTAMP, last_at TIMESTAMP
			       )[] AS source_rollups
			WHERE false`,
		identityindex.DatasetDomains: `
			SELECT NULL::VARCHAR AS domain, NULL::BIGINT AS activity_count,
			       NULL::BIGINT AS person_count, NULL::BIGINT AS file_count,
			       NULL::TIMESTAMP AS first_at, NULL::TIMESTAMP AS last_at,
			       []::STRUCT(source_type VARCHAR, count BIGINT)[] AS source_counts
			WHERE false`,
		identityindex.DatasetRelationshipDaily: `
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

func TestIdentityCacheFixtureUsesExactV15PeopleSchema(t *testing.T) {
	root := t.TempDir()
	db, err := duckdbutil.Open(
		t.Context(),
		duckdbutil.BuilderPolicy(filepath.Join(root, "duckdb-tmp")),
	)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ensureIdentityCacheFixtureDatasets(t, db, root)

	rows, err := db.Query(`
		DESCRIBE SELECT * FROM read_parquet(?)
	`, filepath.Join(root, identityindex.DatasetPeople, "*.parquet"))
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var columns []string
	for rows.Next() {
		var name, typ string
		var nullable, key, defaultValue, extra sql.NullString
		require.NoError(t, rows.Scan(
			&name,
			&typ,
			&nullable,
			&key,
			&defaultValue,
			&extra,
		))
		columns = append(columns, name)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{
		"canonical_id",
		"display_label",
		"partial_label",
		"member_ids",
		"search_values",
		"is_owner",
		"activity_count",
		"file_count",
		"first_at",
		"last_at",
		"source_counts",
		"source_rollups",
	}, columns)
}
