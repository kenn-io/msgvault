//go:build !cgo

package store

import (
	"context"
	"errors"
)

// No SQLite driver errors can occur in a PostgreSQL-only build.
func isSQLiteError(error, string) bool          { return false }
func (d *SQLiteDialect) IsBusyError(error) bool { return false }
func isSQLiteContention(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
