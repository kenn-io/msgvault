package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestSchemaVersionSQLiteFutureArchive(t *testing.T) {
	for _, version := range []int64{store.SchemaVersion + 1, math.MaxInt32} {
		t.Run(strconv.FormatInt(version, 10), func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
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
	for _, value := range []string{"bad", "-1", "999999999999999999999999999999999", strconv.Itoa(store.SchemaVersion + 1)} {
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

// schemaContractDigests records, per SchemaVersion, the digest of the schema
// files and migration list that version certifies. Append only.
var schemaContractDigests = []string{
	1: "3809495eed9b8cc44c4f3d9615bf2ea9966074a4546d754985ed4e7403af64b6",
}

func TestSchemaVersionContract(t *testing.T) {
	digest := schemaContractDigest(t)
	last := len(schemaContractDigests) - 1
	if last != store.SchemaVersion || schemaContractDigests[last] != digest {
		t.Fatalf("schema files or migration list changed without a SchemaVersion bump: "+
			"set store.SchemaVersion to %d and append %q to schemaContractDigests", last+1, digest)
	}
}

// schemaContractDigest hashes what InitSchemaContext builds on a fresh SQLite
// archive (schema objects and migration ledger rows) plus the FTS and
// PostgreSQL schema files. FTS objects exist only under the fts5 build tag, so
// they come from schema_sqlite.sql instead.
func schemaContractDigest(t *testing.T) string {
	t.Helper()
	s, err := store.OpenForTest(filepath.Join(t.TempDir(), "contract.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.NoError(t, s.InitSchemaContext(context.Background()))
	var parts []string
	for _, object := range queryStrings(t, s, `SELECT type || ' ' || name || ' ' || sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite!_%' ESCAPE '!' AND name NOT LIKE '%!_fts%' ESCAPE '!'
		ORDER BY type, name`) {
		parts = append(parts, normalizeSQL(object))
	}
	parts = append(parts, queryStrings(t, s, `SELECT name || ' ' || version FROM applied_migrations ORDER BY name`)...)
	for _, name := range []string{"schema_sqlite.sql", "schema_pg.sql"} {
		data, err := os.ReadFile(name)
		require.NoError(t, err)
		parts = append(parts, normalizeSQL(string(data)))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

func queryStrings(t *testing.T, s *store.Store, query string) []string {
	t.Helper()
	rows, err := s.DB().Query(query)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var value string
		require.NoError(t, rows.Scan(&value))
		out = append(out, value)
	}
	require.NoError(t, rows.Err())
	return out
}

// normalizeSQL drops comments and collapses whitespace outside quoted text, so
// comment edits and CRLF checkouts keep the digest while literals stay exact.
func normalizeSQL(sql string) string {
	var out strings.Builder
	var quote byte
	space := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			for i < len(sql) && sql[i] != '\n' {
				i++
			}
			space = true
			continue
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			space = true
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteByte(c)
	}
	return out.String()
}

func TestNormalizeSQL(t *testing.T) {
	assert.Equal(t, "CREATE TABLE t ( a TEXT DEFAULT ' -- x  ' );", normalizeSQL("-- note\r\nCREATE  TABLE t (\r\n  a TEXT DEFAULT ' -- x  ' -- trailing\r\n);\r\n"))
	assert.NotEqual(t, normalizeSQL("SELECT ' '"), normalizeSQL("SELECT '  '"))
}
