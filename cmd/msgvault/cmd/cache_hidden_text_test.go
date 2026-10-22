package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCacheExportsHiddenText(t *testing.T) {
	for _, csv := range []bool{false, true} {
		name := "sqlite scanner"
		if csv {
			name = "CSV snapshot"
		}
		t.Run(name, func(t *testing.T) {
			assert, require := assert.New(t), require.New(t)
			if csv {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			}
			dir := setupTestSQLite(t)
			dbPath := filepath.Join(dir, "test.db")
			analyticsDir := filepath.Join(dir, "analytics")
			db, err := sql.Open("sqlite3", dbPath)
			require.NoError(err)
			_, err = db.Exec(`UPDATE messages SET hidden_text_present = TRUE WHERE id = 1`)
			require.NoError(err)
			require.NoError(db.Close())
			_, err = buildCache(dbPath, analyticsDir, false)
			require.NoError(err)
			duckdb, err := sql.Open("duckdb", "")
			require.NoError(err)
			t.Cleanup(func() { require.NoError(duckdb.Close()) })
			var hidden bool
			err = duckdb.QueryRow(`SELECT hidden_text_present FROM read_parquet(?) WHERE id = 1`, filepath.Join(analyticsDir, "messages", "**", "*.parquet")).Scan(&hidden)
			require.NoError(err)
			assert.True(hidden)
		})
	}
}
