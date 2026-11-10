package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildCacheMessageIDAcrossExports(t *testing.T) {
	for _, mode := range []string{"scanner", "csv"} {
		t.Run(mode, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			if mode == "csv" {
				t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", "1")
			}
			dir := setupTestSQLite(t)
			dbPath := filepath.Join(dir, "test.db")
			analytics := filepath.Join(dir, "analytics")
			db, err := sql.Open("sqlite3", dbPath)
			requirements.NoError(err)
			defer func() { _ = db.Close() }()
			_, err = db.Exec(`UPDATE messages SET rfc822_message_id = 'Case-ID@example.test' WHERE id = 1`)
			requirements.NoError(err)
			_, err = buildCache(dbPath, analytics, false)
			requirements.NoError(err)
			duck, err := sql.Open("duckdb", "")
			requirements.NoError(err)
			defer func() { _ = duck.Close() }()
			pattern := filepath.Join(analytics, "messages", "**", "*.parquet")
			var id sql.NullString
			requirements.NoError(duck.QueryRow(`SELECT rfc822_message_id FROM read_parquet(?) WHERE id = 1`, pattern).Scan(&id))
			assertions.Equal(sql.NullString{String: "Case-ID@example.test", Valid: true}, id)
			requirements.NoError(duck.QueryRow(`SELECT rfc822_message_id FROM read_parquet(?) WHERE id = 2`, pattern).Scan(&id))
			assertions.False(id.Valid)

			_, err = db.Exec(`INSERT INTO messages (id,source_id,source_message_id,conversation_id,subject,sent_at,rfc822_message_id)
    VALUES (6,1,'msg6',105,'New message','2024-03-15 10:00:00','<legacy@example.test>')`)
			requirements.NoError(err)
			result, err := buildCache(dbPath, analytics, false)
			requirements.NoError(err)
			assertions.False(result.Skipped)
			requirements.NoError(duck.QueryRow(`SELECT rfc822_message_id FROM read_parquet(?) WHERE id = 6`, pattern).Scan(&id))
			assertions.Equal(sql.NullString{String: "<legacy@example.test>", Valid: true}, id)
			var count int
			requirements.NoError(duck.QueryRow(`SELECT count(*) FROM read_parquet(?) WHERE id IN (1,6)`, pattern).Scan(&count))
			assertions.Equal(2, count)

			_, err = db.Exec(`DELETE FROM message_recipients; DELETE FROM message_labels; DELETE FROM attachments; DELETE FROM messages;`)
			requirements.NoError(err)
			_, err = buildCache(dbPath, analytics, true)
			requirements.NoError(err)
			var kind string
			requirements.NoError(duck.QueryRow(`SELECT typeof(first(rfc822_message_id)), count(*) FROM read_parquet(?)`, pattern).Scan(&kind, &count))
			assertions.Equal("VARCHAR", kind)
			assertions.Zero(count)
		})
	}
}
