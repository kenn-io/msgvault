//go:build sqlite_vec

package sqlitevec

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetadataReadOnlyOpenDuringWriter(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "vectors.db")
	rw, err := Open(ctx, Options{Path: path, Dimension: 4})
	require.NoError(err)
	defer func() { require.NoError(rw.Close()) }()
	_, err = rw.DB().Exec("CREATE TABLE pending_embeddings (message_id INTEGER)")
	require.NoError(err)
	conn, err := rw.DB().Conn(ctx)
	require.NoError(err)
	defer func() { require.NoError(conn.Close()) }()
	_, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE")
	require.NoError(err)
	defer func() {
		_, err := conn.ExecContext(ctx, "ROLLBACK")
		require.NoError(err)
	}()
	ro, err := Open(ctx, Options{Path: path + "?_query_only=false", Dimension: 4, MetadataReadOnly: true})
	require.NoError(err, "metadata reader must not migrate while a writer owns the WAL")
	defer func() { require.NoError(ro.Close()) }()
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			_ = conn.Close()
		}
	}()
	for range 2 {
		c, err := ro.DB().Conn(ctx)
		require.NoError(err)
		_, err = c.ExecContext(ctx, "CREATE TABLE forbidden_write (id INTEGER)")
		require.ErrorContains(err, "readonly")
		connections = append(connections, c)
	}
	var count int
	require.NoError(ro.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='pending_embeddings'").Scan(&count))
	assert.Equal(1, count)
}

func TestMetadataReadOnlyRequiresExistingVectorFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.db")
	_, err := Open(t.Context(), Options{Path: path, MetadataReadOnly: true})
	require.ErrorContains(t, err, "existing vectors.db required")
	assert.NoFileExists(t, path)
}
