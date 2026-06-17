// Package query - PostgreSQL query engine construction.
//
// PostgreSQL support is provided by the dialect-parameterized SQLiteEngine
// (see sqlite.go). NewPostgreSQLEngine constructs an engine configured for
// PostgreSQL SQL (tsvector FTS, to_char time truncation, $N placeholders).
// The underlying engine implementation is the same struct used for SQLite.

package query

import (
	"database/sql"
	"errors"
)

// ErrNotImplemented is a sentinel returned by engine methods that the current
// backend cannot satisfy. Handlers wrap their engine-capability checks around
// it so the API can return a stable status code rather than 500.
var ErrNotImplemented = errors.New("query: method not implemented for this engine")

// pgEngine wraps a dialect-parameterized engine for PostgreSQL. It
// embeds the Engine interface — not *SQLiteEngine — so the TextEngine
// methods (ListConversations, TextAggregate, TextSearch, …) defined on
// *SQLiteEngine are NOT promoted onto pgEngine. This is intentional:
// internal/query/sqlite_text.go uses FTS5 MATCH and strftime(), neither
// of which is valid PostgreSQL. Until a PostgreSQL TextEngine
// implementation exists, callers that type-assert the engine to
// query.TextEngine should cleanly get a failed assertion rather than
// silently sending SQLite SQL to PostgreSQL at runtime.
type pgEngine struct {
	Engine
}

// NewPostgreSQLEngine creates a query engine backed by PostgreSQL. The engine
// uses PostgreSQLQueryDialect for all SQL generation: $N placeholders via
// Rebind, to_char() time truncation, tsvector @@ for full-text search.
//
// The returned value is the Engine interface (not the concrete
// *SQLiteEngine) so the SQLite-specific TextEngine implementation is
// hidden from type assertions on the PG path.
func NewPostgreSQLEngine(db *sql.DB) Engine {
	return &pgEngine{Engine: NewEngineWithDialect(db, PostgreSQLQueryDialect{})}
}

// NewEngine picks the appropriate engine for the given database. isPostgres
// selects between PostgreSQLQueryDialect (true) and SQLiteQueryDialect (false).
// This is the preferred entry point for callers that have a Store with an
// unknown backend — pass store.IsPostgres() as the flag.
//
// The return type is the Engine interface so the SQLite-only TextEngine
// is hidden when isPostgres is true.
func NewEngine(db *sql.DB, isPostgres bool) Engine {
	if isPostgres {
		return NewPostgreSQLEngine(db)
	}
	return NewSQLiteEngine(db)
}
