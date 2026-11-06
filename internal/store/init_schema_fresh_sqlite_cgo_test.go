//go:build cgo

package store

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil/sqlobserve"
)

func freshObservedStore(t *testing.T, path string, hook func(*sqlite3.SQLiteConn) error) *Store {
	t.Helper()
	db, _ := sqlobserve.OpenFile(t, path+defaultSQLiteParams, hook)
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
		"statistics": "SELECT tbl, idx, stat FROM sqlite_stat1 ORDER BY tbl, idx",
	}
	for _, table := range []string{"communication_services", "attribute_definitions", "relationship_types", "collections", "embedding_change_clock", "source_maintenance_lock", "carddav_discovery_lock", "person_sweep_change_clock", "operation_history_state", "document_index_state"} {
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
	require.NoError(t, s.initSchemaContext(t.Context(), 0))
	s.optimizeSQLiteBestEffort(t.Context(), "schema initialization", false)
	return freshSchemaRows(t, s)
}

func TestInitSchemaContext_FreshSQLiteCommitsTogether(t *testing.T) {
	var commits atomic.Int64
	s := freshObservedStore(t, filepath.Join(t.TempDir(), "fresh.db"), func(conn *sqlite3.SQLiteConn) error {
		conn.RegisterCommitHook(func() int { commits.Add(1); return 0 })
		return nil
	})
	var synchronous int
	require.NoError(t, s.db.QueryRowContext(t.Context(), "PRAGMA synchronous").Scan(&synchronous))
	require.Equal(t, 2, synchronous, "use production FULL durability")
	before := commits.Load()
	require.NoError(t, s.InitSchemaContext(t.Context()))
	assert.LessOrEqual(t, commits.Load()-before, int64(5), "commit the whole fresh setup together")
	assert.Equal(t, referenceFreshSchema(t), freshSchemaRows(t, s))
}

type freshBusyDialect struct {
	Dialect

	busy chan struct{}
	once sync.Once
}

func (d *freshBusyDialect) IsBusyError(err error) bool {
	busy := d.Dialect.IsBusyError(err)
	if busy {
		d.once.Do(func() { close(d.busy) })
	}
	return busy
}

func TestInitSchemaContext_TwoFreshSQLiteInitializers(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "shared.db")
	locked, busy := make(chan struct{}), make(chan struct{})
	var once sync.Once
	a := freshObservedStore(t, path, func(conn *sqlite3.SQLiteConn) error {
		conn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if action == sqlite3.SQLITE_CREATE_TABLE && table == "communication_services" {
				once.Do(func() {
					close(locked)
					select {
					case <-busy:
					case <-ctx.Done():
					}
				})
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	b := freshObservedStore(t, path, nil)
	b.dialect = &freshBusyDialect{Dialect: b.dialect, busy: busy}
	done := make(chan error, 1)
	go func() { done <- a.InitSchemaContext(ctx) }()
	select {
	case <-locked:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	err := b.InitSchemaContext(ctx)
	aErr := <-done
	require.NoError(t, err)
	require.NoError(t, aErr)
	select {
	case <-busy:
	default:
		require.FailNow(t, "the second initializer must encounter the first writer")
	}
	assert.Equal(t, referenceFreshSchema(t), freshSchemaRows(t, b))
}

func TestInitSchemaContext_ExistingSQLiteConcurrentWriter(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "versioned", true: "legacy"}[legacy], func(t *testing.T) {
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
			require.NoError(t, s.InitSchemaContext(t.Context()))
			writer = freshObservedStore(t, path, nil)
			if legacy {
				_, err := s.db.ExecContext(t.Context(), "DELETE FROM archive_metadata WHERE key = 'schema_version'")
				require.NoError(t, err)
			}
			active = true
			err := s.InitSchemaContext(t.Context())
			require.NoError(t, writeErr)
			require.NoError(t, err)
			require.True(t, written)
			var identifier string
			require.NoError(t, s.db.QueryRowContext(t.Context(), "SELECT identifier FROM sources").Scan(&identifier))
			assert.Equal(t, "writer@example.com", identifier)
		})
	}
}

func TestInitSchemaContext_FreshSQLiteCancelAndRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	path := filepath.Join(t.TempDir(), "cancel.db")
	s := freshObservedStore(t, path, func(conn *sqlite3.SQLiteConn) error {
		conn.RegisterAuthorizer(func(action int, table, _, _ string) int {
			if action == sqlite3.SQLITE_CREATE_TABLE && table == "communication_services" {
				cancel()
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	require.ErrorIs(t, s.InitSchemaContext(ctx), context.Canceled)
	var count int
	require.NoError(t, s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table'").Scan(&count))
	assert.Zero(t, count, "roll back the partial fresh setup")
	require.NoError(t, s.db.Close())
	retry := freshObservedStore(t, path, nil)
	require.NoError(t, retry.InitSchemaContext(t.Context()))
	assert.Equal(t, referenceFreshSchema(t), freshSchemaRows(t, retry))
}
