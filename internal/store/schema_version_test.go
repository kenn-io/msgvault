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

// schemaContractDigests and schemaContractPostgresDigests record, per
// SchemaVersion, the digest of what that version builds on each backend.
// Append only.
var (
	schemaContractDigests = []string{
		1: "99160ef94757713c504e16818c9d664932699459ad2d8090e89c60d40e5120d2",
	}
	schemaContractPostgresDigests = []string{
		1: "f4dea875d11e5728cd2f0bcbd81e67b8d6356e73b96cc5491494564d6a1064d6",
	}
)

func TestSchemaVersionContract(t *testing.T) {
	checkSchemaContract(t, "schemaContractDigests", schemaContractDigests, schemaContractDigest(t))
	if s := testutil.NewTestStore(t); s.IsPostgreSQL() {
		checkSchemaContract(t, "schemaContractPostgresDigests", schemaContractPostgresDigests, schemaContractPostgresDigest(t, s))
	} else if len(schemaContractPostgresDigests) != len(schemaContractDigests) {
		assert.Failf(t, "missing PostgreSQL schema digest", "append the PostgreSQL digest for SchemaVersion %d to schemaContractPostgresDigests; "+
			"this test prints it when run with MSGVAULT_TEST_DB set to a PostgreSQL URL", store.SchemaVersion)
	}
}

func checkSchemaContract(t *testing.T, list string, digests []string, digest string) {
	t.Helper()
	last := len(digests) - 1
	if last != store.SchemaVersion || digests[last] != digest {
		assert.Failf(t, "SchemaVersion not bumped", "schema or migration list changed without a SchemaVersion bump: "+
			"set store.SchemaVersion to %d and append %q to %s", max(last+1, store.SchemaVersion), digest, list)
	}
}

// schemaContractPostgresDigest hashes the tables, columns, constraints,
// indexes, views, triggers and functions a fresh PostgreSQL archive gets, plus
// its migration ledger. Schema names are stripped because each test runs in
// its own database or schema; extension-owned objects are skipped.
func schemaContractPostgresDigest(t *testing.T, s *store.Store) string {
	t.Helper()
	parts := queryStrings(t, s, `
WITH ns AS (SELECT oid, nspname FROM pg_namespace WHERE nspname = current_schema()),
own AS (SELECT c.oid, c.relname, c.relkind FROM pg_class c JOIN ns ON c.relnamespace = ns.oid
	WHERE NOT EXISTS (SELECT 1 FROM pg_depend d
		WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e'))
SELECT replace(def, (SELECT nspname FROM ns) || '.', '') FROM (
	SELECT 'column ' || o.relname || '.' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod) ||
		CASE WHEN a.attnotnull THEN ' not null' ELSE '' END ||
		CASE WHEN a.attidentity <> '' THEN ' identity ' || a.attidentity::text ELSE '' END ||
		coalesce(' collate ' || coll.collname, '') ||
		coalesce(' default ' || pg_get_expr(ad.adbin, ad.adrelid), '') ||
		CASE WHEN a.attgenerated <> '' THEN ' generated' ELSE '' END AS def
	FROM own o JOIN pg_attribute a ON a.attrelid = o.oid
	LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
	LEFT JOIN pg_type ty ON ty.oid = a.atttypid
	LEFT JOIN pg_collation coll ON coll.oid = a.attcollation AND a.attcollation <> ty.typcollation
	WHERE o.relkind IN ('r', 'p', 'v', 'm') AND a.attnum > 0 AND NOT a.attisdropped
	UNION ALL
	SELECT 'constraint ' || o.relname || '.' || con.conname || ' ' || pg_get_constraintdef(con.oid)
	FROM own o JOIN pg_constraint con ON con.conrelid = o.oid
	-- PostgreSQL 18 lists NOT NULL as constraints; attnotnull already covers it.
	WHERE con.contype <> 'n'
	UNION ALL
	SELECT 'index ' || pg_get_indexdef(o.oid) FROM own o WHERE o.relkind = 'i'
	UNION ALL
	SELECT 'sequence ' || o.relname || ' ' || format_type(sq.seqtypid, NULL) || ' ' || sq.seqstart || ' ' ||
		sq.seqincrement || ' ' || sq.seqmin || ' ' || sq.seqmax || ' ' || sq.seqcache || ' ' || sq.seqcycle
	FROM own o JOIN pg_sequence sq ON sq.seqrelid = o.oid
	UNION ALL
	SELECT 'view ' || o.relname || ' ' || pg_get_viewdef(o.oid) FROM own o WHERE o.relkind IN ('v', 'm')
	UNION ALL
	SELECT 'trigger ' || pg_get_triggerdef(tg.oid) FROM own o JOIN pg_trigger tg ON tg.tgrelid = o.oid
	WHERE NOT tg.tgisinternal
	UNION ALL
	SELECT 'function ' || pg_get_functiondef(p.oid) FROM pg_proc p JOIN ns ON p.pronamespace = ns.oid
	WHERE p.prokind IN ('f', 'p')
		AND NOT EXISTS (SELECT 1 FROM pg_depend d
			WHERE d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e')
) defs ORDER BY 1`)
	for i, part := range parts {
		parts[i] = normalizeSQL(part)
	}
	parts = append(parts, queryStrings(t, s, `SELECT name || ' ' || version FROM applied_migrations ORDER BY name`)...)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}

// schemaContractDigest hashes what InitSchemaContext builds on a fresh SQLite
// archive (schema objects and migration ledger rows) plus both dialects'
// legacy column statements and the FTS and PostgreSQL schema files. FTS
// objects exist only under the fts5 build tag, so they come from
// schema_sqlite.sql instead.
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
	// Legacy ADD COLUMN statements only run on old archives, so a fresh one
	// never shows them.
	for _, d := range []store.Dialect{&store.SQLiteDialect{}, &store.PostgreSQLDialect{}} {
		for _, m := range d.LegacyColumnMigrations() {
			parts = append(parts, normalizeSQL(m.SQL))
		}
	}
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
