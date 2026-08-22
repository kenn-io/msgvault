package store

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitSchemaUpgradesBeforeSourceResourceIdentityIndexes(t *testing.T) {
	require := require.New(t)
	st, err := OpenForTest(filepath.Join(t.TempDir(), "pre-source-resource.db"))
	require.NoError(err)
	t.Cleanup(func() { _ = st.Close() })
	require.NoError(st.InitSchema())
	_, err = st.db.Exec(`DROP INDEX idx_person_names_property_identity`)
	require.NoError(err)
	_, err = st.db.Exec(`ALTER TABLE person_names DROP COLUMN source_resource_uid`)
	require.NoError(err)
	_, err = st.db.Exec(`DELETE FROM applied_migrations WHERE name = ?`,
		migrationVCardSourceResourceIdentity)
	require.NoError(err)

	require.NoError(st.InitSchema(),
		"bootstrap indexes must not reference columns added by later migrations")
	var columnCount int
	require.NoError(st.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('person_names')
		WHERE name = 'source_resource_uid'`).Scan(&columnCount))
	assert.Equal(t, 1, columnCount)
	var indexSQL string
	require.NoError(st.db.QueryRow(`SELECT sql FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_person_names_property_identity'`).Scan(&indexSQL))
	assert.Contains(t, indexSQL, "COALESCE(source_resource_uid, '')")
}
