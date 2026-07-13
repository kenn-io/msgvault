//go:build windows && arm64

package duckdbdriver

import (
	"database/sql"
	"errors"
)

// ErrUnsupportedPlatform reports the missing upstream DuckDB static library.
// Keeping driver linkage out of this build lets SQLite-backed features run
// natively instead of forcing the entire application through x64 emulation.
var ErrUnsupportedPlatform = errors.New(
	"DuckDB analytics are not supported on windows/arm64: " +
		"duckdb-go-bindings does not provide a prebuilt library for this platform",
)

// Open reports the platform limitation without linking the DuckDB driver.
func Open(string) (*sql.DB, error) {
	return nil, ErrUnsupportedPlatform
}
