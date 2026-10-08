//go:build cgo

package store

import (
	"context"
	"errors"
	"strings"

	"github.com/mattn/go-sqlite3"
)

// isSQLiteError checks if err is a sqlite3.Error with a message containing substr.
// This is more robust than strings.Contains on err.Error() because it first
// type-asserts to the specific driver error type using errors.As.
// Handles both value (sqlite3.Error) and pointer (*sqlite3.Error) forms.
//
// SQLiteDialect's error predicates are thin wrappers around this helper; it also
// services subset.go (which has not been migrated to Dialect).
func isSQLiteError(err error, substr string) bool {
	if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok {
		return strings.Contains(sqliteErr.Error(), substr)
	}
	var sqliteErrPtr *sqlite3.Error
	if errors.As(err, &sqliteErrPtr) && sqliteErrPtr != nil {
		return strings.Contains(sqliteErrPtr.Error(), substr)
	}
	return false
}

// isSQLiteContention reports errors that mean the database was busy rather
// than broken: expected on a loaded archive and retried later.
func isSQLiteContention(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if sqliteErr, ok := errors.AsType[sqlite3.Error](err); ok {
		switch sqliteErr.Code {
		case sqlite3.ErrBusy, sqlite3.ErrLocked, sqlite3.ErrInterrupt:
			return true
		}
	}
	return false
}

// IsBusyError returns true for SQLITE_BUSY and SQLITE_LOCKED. Matching on
// the result code is more robust than substring matching: BUSY surfaces as
// "database is locked" but LOCKED surfaces as "database table is locked",
// so a single substring cannot catch both.
func (d *SQLiteDialect) IsBusyError(err error) bool {
	if err == nil {
		return false
	}
	if serr, ok := errors.AsType[sqlite3.Error](err); ok {
		return serr.Code == sqlite3.ErrBusy || serr.Code == sqlite3.ErrLocked
	}
	var serrPtr *sqlite3.Error
	if errors.As(err, &serrPtr) && serrPtr != nil {
		return serrPtr.Code == sqlite3.ErrBusy || serrPtr.Code == sqlite3.ErrLocked
	}
	return false
}
