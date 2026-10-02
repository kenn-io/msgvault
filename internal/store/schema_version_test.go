package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSchemaVersionSQLiteCompletion(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
	require.NoError(err)
	t.Cleanup(func() { require.NoError(s.Close()) })
	var version int
	require.NoError(s.DB().QueryRow("PRAGMA user_version").Scan(&version))
	assert.Zero(version)
	require.NoError(s.InitSchemaContext(context.Background()))
	require.NoError(s.DB().QueryRow("PRAGMA user_version").Scan(&version))
	assert.Positive(version, "successful schema initialization must publish a version")
	require.NoError(s.InitSchemaContext(context.Background()))
	var again int
	require.NoError(s.DB().QueryRow("PRAGMA user_version").Scan(&again))
	assert.Equal(version, again)
}

func TestSchemaVersionSQLiteFailedUpgrade(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			s, err := store.OpenForTest(filepath.Join(t.TempDir(), "archive.db"))
			require.NoError(err)
			t.Cleanup(func() { require.NoError(s.Close()) })
			ctx := context.Background()
			if cancelled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			} else {
				_, err = s.DB().Exec("CREATE TABLE archive_metadata (wrong_column TEXT)")
				require.NoError(err)
			}
			require.Error(s.InitSchemaContext(ctx))
			var version int
			require.NoError(s.DB().QueryRow("PRAGMA user_version").Scan(&version))
			assert.Zero(version, "failed upgrade must not certify completion")
		})
	}
}

// Future schema versions are a bounded SQLite integer domain. The property
// observes schema, data and the marker; opening may legitimately manage WAL.
func FuzzSchemaVersionFutureArchive(f *testing.F) {
	for _, v := range []uint32{0, 1, 2, 2147483645, 2147483647, 4294967295} {
		f.Add(v)
	}
	f.Fuzz(func(t *testing.T, input uint32) {
		require := require.New(t)
		assert := assert.New(t)
		version := int64(input%2147483646) + 2
		s, err := store.OpenForTest(filepath.Join(t.TempDir(), "future.db"))
		require.NoError(err)
		t.Cleanup(func() { require.NoError(s.Close()) })
		_, err = s.DB().Exec(fmt.Sprintf("PRAGMA user_version = %d", version))
		require.NoError(err)
		_, err = s.DB().Exec("CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('keep')")
		require.NoError(err)
		require.ErrorContains(s.InitSchemaContext(context.Background()), "newer")
		var got int64
		require.NoError(s.DB().QueryRow("PRAGMA user_version").Scan(&got))
		assert.Equal(version, got)
		var value string
		require.NoError(s.DB().QueryRow("SELECT value FROM sentinel").Scan(&value))
		assert.Equal("keep", value)
		var tables int
		require.NoError(s.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='messages'").Scan(&tables))
		assert.Zero(tables, "future archive must be refused before schema DDL")
	})
}

func TestSchemaVersionLegacyMarker(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	s := testutil.NewTestStore(t)
	ctx := context.Background()
	version, err := s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
	if s.IsPostgreSQL() {
		_, err = s.DB().Exec("DELETE FROM archive_metadata WHERE key='schema_version'")
	} else {
		_, err = s.DB().Exec("PRAGMA user_version = 0")
	}
	require.NoError(err)
	version, err = s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Zero(version)
	require.NoError(s.InitSchemaContext(ctx))
	version, err = s.SchemaVersionContext(ctx)
	require.NoError(err)
	assert.Equal(store.SchemaVersion, version)
}

func TestSchemaVersionPostgresMalformedAndFuture(t *testing.T) {
	req := require.New(t)
	check := assert.New(t)
	s := testutil.NewTestStore(t)
	if !s.IsPostgreSQL() {
		t.Skip("PostgreSQL metadata contract")
	}
	for _, value := range []string{"bad", "-1", "999999999999999999999999999999999", "2"} {
		t.Run(value, func(t *testing.T) {
			req := require.New(t)
			check := assert.New(t)
			_, err := s.DB().Exec(s.Rebind("UPDATE archive_metadata SET value = ? WHERE key='schema_version'"), value)
			req.NoError(err)
			req.Error(s.InitSchemaContext(context.Background()))
			var got string
			req.NoError(s.DB().QueryRow("SELECT value FROM archive_metadata WHERE key='schema_version'").Scan(&got))
			check.Equal(value, got)
		})
	}
	_, err := s.DB().Exec("DROP TABLE archive_metadata")
	req.NoError(err)
	version, err := s.SchemaVersionContext(context.Background())
	req.NoError(err)
	check.Zero(version, "missing legacy metadata must not be created by probe")
}
