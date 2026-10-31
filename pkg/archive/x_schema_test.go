//go:build cgo

package archive_test

import (
	"crypto/rand"
	"database/sql"
	"github.com/jackc/pgx/v5"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestXSchemaUpgradeAndReceiptLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x-archive.db")
	require.NoError(t, archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	_, err = st.DB().Exec(`DROP TABLE IF EXISTS x_reply_attempts; DROP TABLE IF EXISTS x_post_versions; UPDATE archive_metadata SET value='15' WHERE key='schema_version'`)
	require.NoError(t, err)
	reader, err := archive.OpenSQLite(t.Context(), path)
	if reader != nil {
		require.NoError(t, reader.Close())
	}
	require.ErrorContains(t, err, "run setup")
	require.NoError(t, archive.SetupSQLite(t.Context(), path))
	_, err = st.DB().Exec(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,remote_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, source.ID, "100", "removed-draft", "attempt-1", 1, "200", "200", "digest", "sent", "reservation-1", "300")
	require.NoError(t, err)
	reader, err = archive.OpenSQLite(t.Context(), path)
	require.NoError(t, err)
	defer func() { require.NoError(t, reader.Close()) }()
	var remote string
	require.NoError(t, reader.Store().DB().QueryRow("SELECT remote_id FROM x_reply_attempts WHERE draft_id='removed-draft'").Scan(&remote))
	assert.Equal(t, "300", remote)
	require.NoError(t, reader.PurgeSource(t.Context(), source.ID))
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM x_reply_attempts").Scan(&count))
	assert.Zero(t, count)
}

func TestXSchemaPostgreSQLUpgradeAndReceiptLifetime(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires a disposable PostgreSQL database")
	}
	admin, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	schema := "x_schema_" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		_, err := admin.Exec("DROP SCHEMA IF EXISTS " + pgx.Identifier{schema}.Sanitize() + " CASCADE")
		assert.NoError(t, err)
	})
	opts := archive.PostgreSQL{URL: dsn, Schema: schema}
	require.NoError(t, archive.Setup(t.Context(), opts))
	runtime, err := archive.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	st := runtime.Store()
	source, err := st.GetOrCreateSource("x", "100")
	require.NoError(t, err)
	_, err = st.DB().Exec(`DROP TABLE x_reply_attempts; DROP TABLE x_post_versions; UPDATE archive_metadata SET value='15' WHERE key='schema_version'`)
	require.NoError(t, err)
	reader, err := archive.Open(t.Context(), opts)
	if reader != nil {
		require.NoError(t, reader.Close())
	}
	require.ErrorContains(t, err, "run setup")
	require.NoError(t, archive.Setup(t.Context(), opts))
	_, err = st.DB().Exec(st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,remote_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), source.ID, "100", "removed-draft", "attempt-1", 1, "200", "200", "digest", "sent", "reservation-1", "300")
	require.NoError(t, err)
	reader, err = archive.Open(t.Context(), opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	var remote string
	require.NoError(t, reader.Store().DB().QueryRow("SELECT remote_id FROM x_reply_attempts WHERE draft_id='removed-draft'").Scan(&remote))
	assert.Equal(t, "300", remote)
	require.NoError(t, reader.PurgeSource(t.Context(), source.ID))
	var count int
	require.NoError(t, st.DB().QueryRow("SELECT count(*) FROM x_reply_attempts").Scan(&count))
	assert.Zero(t, count)
}
