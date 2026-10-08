//go:build cgo

package duckdbutil

import (
	"errors"

	"github.com/duckdb/duckdb-go/v2"
)

// Available reports whether this build includes the DuckDB driver.
const Available = true

// IsOutOfMemory reports a DuckDB memory or temporary-storage exhaustion error.
func IsOutOfMemory(err error) bool {
	resourceErr, ok := errors.AsType[*duckdb.Error](err)
	return ok && resourceErr.Type == duckdb.ErrorTypeOutOfMemory
}
