package store

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitSchemaMigratesLegacyAttachmentOccurrenceIdentity(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	dbPath := filepath.Join(t.TempDir(), "legacy-attachments.db")
	st, err := Open(dbPath)
	require.NoError(err)
	t.Cleanup(func() { require.NoError(st.Close()) })

	_, err = st.db.Exec(`
		CREATE TABLE attachments (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL,
			filename TEXT,
			mime_type TEXT,
			size INTEGER,
			content_hash TEXT,
			storage_path TEXT NOT NULL,
			media_type TEXT,
			width INTEGER,
			height INTEGER,
			duration_ms INTEGER,
			thumbnail_hash TEXT,
			thumbnail_path TEXT,
			source_attachment_id TEXT,
			attachment_metadata JSON,
			encryption_version INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO attachments
			(message_id, filename, mime_type, size, content_hash, storage_path)
		VALUES (99, 'legacy.png', 'image/png', 10, 'legacy-hash', 'legacy/path');
	`)
	require.NoError(err)
	require.NoError(st.InitSchema())

	var role, roleSource string
	var sourcePartKey, contentID *string
	require.NoError(st.db.QueryRow(`
		SELECT attachment_role, role_source, source_part_key, content_id
		FROM attachments WHERE id = 1
	`).Scan(&role, &roleSource, &sourcePartKey, &contentID))
	assert.Equal(string(AttachmentRoleUnknown), role)
	assert.Equal(string(AttachmentRoleSourceUnknown), roleSource)
	assert.Nil(sourcePartKey)
	assert.Nil(contentID)

	indexes := make(map[string]string)
	rows, err := st.db.Query(`
		SELECT name, sql FROM sqlite_master
		WHERE type = 'index'
		  AND name IN ('idx_attachments_msg_source_part', 'idx_attachments_msg_content_hash')
	`)
	require.NoError(err)
	defer func() { require.NoError(rows.Close()) }()
	for rows.Next() {
		var name, sqlText string
		require.NoError(rows.Scan(&name, &sqlText))
		indexes[name] = strings.ToLower(sqlText)
	}
	require.NoError(rows.Err())
	require.Len(indexes, 2)
	assert.Contains(indexes["idx_attachments_msg_source_part"],
		"where source_part_key is not null")
	assert.Contains(indexes["idx_attachments_msg_content_hash"],
		"where source_part_key is null")

	applied, err := st.IsMigrationApplied(migrationAttachmentOccurrenceUnique)
	require.NoError(err)
	assert.True(applied)
}
