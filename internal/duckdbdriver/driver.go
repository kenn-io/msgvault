//go:build !(windows && arm64)

package duckdbdriver

import (
	"database/sql"

	// Registers the "duckdb" database/sql driver and links the platform's
	// prebuilt DuckDB library.
	_ "github.com/duckdb/duckdb-go/v2"
)

// Open opens a DuckDB database on platforms with prebuilt bindings.
func Open(dsn string) (*sql.DB, error) {
	return sql.Open("duckdb", dsn)
}
