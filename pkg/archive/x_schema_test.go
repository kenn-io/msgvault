//go:build cgo

package archive_test

import (
	"context"
	"crypto/rand"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/archive"
)

func TestXSchemaUpgradeAndReceiptLifetime(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	path := filepath.Join(t.TempDir(), "x-archive.db")
	_testRequire.NoError(archive.SetupSQLite(t.Context(), path))
	st, err := store.Open(path)
	_testRequire.NoError(err)
	defer func() { require.NoError(t, st.Close()) }()
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE IF EXISTS x_reply_attempts; DROP TABLE IF EXISTS x_post_versions; UPDATE archive_metadata SET value='15' WHERE key='schema_version'`)
	_testRequire.NoError(err)
	reader, err := archive.OpenSQLite(t.Context(), path)
	if reader != nil {
		_testRequire.NoError(reader.Close())
	}
	_testRequire.ErrorContains(err, "run setup")
	_testRequire.NoError(archive.SetupSQLite(t.Context(), path))
	_, err = st.DB().ExecContext(t.Context(), `INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,remote_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, source.ID, "100", "removed-draft", "attempt-1", 1, "200", "200", "digest", "sent", "reservation-1", "300")
	_testRequire.NoError(err)
	reader, err = archive.OpenSQLite(t.Context(), path)
	_testRequire.NoError(err)
	defer func() { require.NoError(t, reader.Close()) }()
	var remote string
	_testRequire.NoError(reader.Store().DB().QueryRowContext(t.Context(), "SELECT remote_id FROM x_reply_attempts WHERE draft_id='removed-draft'").Scan(&remote))
	_testAssert.Equal("300", remote)
	_testRequire.NoError(reader.PurgeSource(t.Context(), source.ID))
	var count int
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM x_reply_attempts").Scan(&count))
	_testAssert.Zero(count)
}

func TestXSchemaPostgreSQLUpgradeAndReceiptLifetime(t *testing.T) {
	_testAssert := assert.New(t)
	_testRequire := require.New(t)

	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if !store.IsPostgresURL(dsn) {
		t.Skip("requires a disposable PostgreSQL database")
	}
	admin, err := sql.Open("pgx", dsn)
	_testRequire.NoError(err)
	t.Cleanup(func() { require.NoError(t, admin.Close()) })
	schema := "x_schema_" + strings.ToLower(rand.Text())
	t.Cleanup(func() {
		_, err := admin.ExecContext(context.WithoutCancel(t.Context()), "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
		assert.NoError(t, err)
	})
	opts := archive.PostgreSQL{URL: dsn, Schema: schema}
	_testRequire.NoError(archive.Setup(t.Context(), opts))
	runtime, err := archive.Open(t.Context(), opts)
	_testRequire.NoError(err)
	t.Cleanup(func() { assert.NoError(t, runtime.Close()) })
	st := runtime.Store()
	source, err := st.GetOrCreateSource("x", "100")
	_testRequire.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), `DROP TABLE x_reply_attempts; DROP TABLE x_post_versions; UPDATE archive_metadata SET value='15' WHERE key='schema_version'`)
	_testRequire.NoError(err)
	reader, err := archive.Open(t.Context(), opts)
	if reader != nil {
		_testRequire.NoError(reader.Close())
	}
	_testRequire.ErrorContains(err, "run setup")
	_testRequire.NoError(archive.Setup(t.Context(), opts))
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO x_reply_attempts(source_id,owner_id,draft_id,attempt_id,draft_revision,target_post_id,source_conversation_id,body_digest,state,reservation_id,remote_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), source.ID, "100", "removed-draft", "attempt-1", 1, "200", "200", "digest", "sent", "reservation-1", "300")
	_testRequire.NoError(err)
	reader, err = archive.Open(t.Context(), opts)
	_testRequire.NoError(err)
	t.Cleanup(func() { assert.NoError(t, reader.Close()) })
	var remote string
	_testRequire.NoError(reader.Store().DB().QueryRowContext(t.Context(), "SELECT remote_id FROM x_reply_attempts WHERE draft_id='removed-draft'").Scan(&remote))
	_testAssert.Equal("300", remote)
	_testRequire.NoError(reader.PurgeSource(t.Context(), source.ID))
	var count int
	_testRequire.NoError(st.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM x_reply_attempts").Scan(&count))
	_testAssert.Zero(count)
}
