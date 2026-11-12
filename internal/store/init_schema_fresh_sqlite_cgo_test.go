//go:build cgo

package store

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func freshObservedStore(t *testing.T, path string, hook func(*sqlite3.SQLiteConn) error) *Store {
	t.Helper()
	db, _ := sqlobserve.OpenFile(t, path+defaultSQLiteParams, hook)
	setSQLitePoolSize(db)
	require.NoError(t, db.PingContext(t.Context()))
	dialect := &SQLiteDialect{}
	return &Store{db: newLoggedDB(db, dialect.Rebind), dialect: dialect}
}

func freshSchemaRows(t *testing.T, s *Store) map[string][][]any {
	t.Helper()
	queries := map[string]string{
		"schema":     "SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master ORDER BY type, name",
		"migrations": "SELECT name, version FROM applied_migrations ORDER BY name",
		"version":    "SELECT value FROM archive_metadata WHERE key = 'schema_version'",
	}
	for _, table := range []string{"communication_services", "attribute_definitions", "relationship_types", "collections", "embedding_change_clock", "source_maintenance_lock", "carddav_discovery_lock", "person_sweep_change_clock", "operation_history_state"} {
		columns, err := s.db.QueryContext(t.Context(), "PRAGMA table_info("+table+")")
		require.NoError(t, err)
		var names []string
		for columns.Next() {
			var id, required, primary int
			var name, kind string
			var defaultValue any
			require.NoError(t, columns.Scan(&id, &name, &kind, &required, &defaultValue, &primary))
			if name != "created_at" && name != "updated_at" {
				names = append(names, name)
			}
		}
		require.NoError(t, columns.Err())
		require.NoError(t, columns.Close())
		queries[table] = "SELECT " + strings.Join(names, ", ") + " FROM " + table + " ORDER BY 1"
	}
	result := make(map[string][][]any)
	for name, query := range queries {
		rows, err := s.db.QueryContext(t.Context(), query)
		require.NoError(t, err)
		columns, err := rows.Columns()
		require.NoError(t, err)
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			require.NoError(t, rows.Scan(pointers...))
			result[name] = append(result[name], values)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
	}
	return result
}

func referenceFreshSchema(t *testing.T) map[string][][]any {
	t.Helper()
	s := freshObservedStore(t, filepath.Join(t.TempDir(), "reference.db"), nil)
	require.NoError(t, s.initSchemaContext(t.Context()))
	s.optimizeSQLiteBestEffort(t.Context(), "schema initialization", false)
	return freshSchemaRows(t, s)
}

func TestInitSchemaContext_FreshSQLiteCommitsTogether(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	var commits atomic.Int64
	var setupCommits atomic.Int64
	setupCommits.Store(-1)
	s := freshObservedStore(t, filepath.Join(t.TempDir(), "fresh.db"), func(conn *sqlite3.SQLiteConn) error {
		conn.RegisterCommitHook(func() int { commits.Add(1); return 0 })
		conn.RegisterAuthorizer(func(action int, name, _, _ string) int {
			if action == sqlite3.SQLITE_PRAGMA && strings.EqualFold(name, "optimize") {
				// Authorize before execution so maintenance commits stay outside this count.
				setupCommits.CompareAndSwap(-1, commits.Load())
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	var synchronous int
	require.NoError(s.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous))
	require.Equal(2, synchronous, "use production FULL durability")
	before := commits.Load()
	require.NoError(s.InitSchemaContext(t.Context()))
	assert.Equal(int64(1), setupCommits.Load()-before, "commit setup once before planner maintenance")
	assert.Equal(referenceFreshSchema(t), freshSchemaRows(t, s))
}

func TestInitSchemaContext_ExistingSQLiteConcurrentWriter(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "existing.db")
	var active, written bool
	var writer *Store
	var writeErr error
	s := freshObservedStore(t, path, func(conn *sqlite3.SQLiteConn) error {
		conn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if active && !written && action == sqlite3.SQLITE_INSERT && table == "embedding_change_clock" {
				written = true
				_, writeErr = writer.db.ExecContext(t.Context(), "INSERT INTO sources (source_type, identifier) VALUES ('gmail', 'writer@example.com')")
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	require.NoError(s.InitSchemaContext(t.Context()))
	writer = freshObservedStore(t, path, nil)
	_, err := s.db.ExecContext(t.Context(), "DELETE FROM archive_metadata WHERE key = 'schema_version'")
	require.NoError(err)
	active = true
	err = s.InitSchemaContext(t.Context())
	require.NoError(writeErr)
	require.NoError(err)
	require.True(written)
	var identifier string
	require.NoError(s.db.QueryRowContext(t.Context(), "SELECT identifier FROM sources").Scan(&identifier))
	assert.Equal(t, "writer@example.com", identifier)
}
