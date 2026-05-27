package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mattn/go-sqlite3"
	assertpkg "github.com/stretchr/testify/assert"
)

func TestIsSQLiteError_ValueForm(t *testing.T) {
	// Create a sqlite3.Error value
	sqliteErr := sqlite3.Error{
		Code:         sqlite3.ErrConstraint,
		ExtendedCode: sqlite3.ErrConstraintUnique,
	}

	// Wrap the error
	wrappedErr := fmt.Errorf("insert failed: %w", sqliteErr)

	// sqlite3.Error.Error() returns the code description, e.g. "constraint failed"
	assertpkg.True(t, isSQLiteError(wrappedErr, "constraint failed"),
		"isSQLiteError should match constraint error, got: %v", sqliteErr.Error())

	assertpkg.False(t, isSQLiteError(wrappedErr, "no such table"),
		"isSQLiteError should not match unrelated substring")
}

func TestIsSQLiteError_PointerForm(t *testing.T) {
	// Create a *sqlite3.Error pointer
	sqliteErr := &sqlite3.Error{
		Code:         sqlite3.ErrConstraint,
		ExtendedCode: sqlite3.ErrConstraintForeignKey,
	}

	// Wrap the error
	wrappedErr := fmt.Errorf("insert failed: %w", sqliteErr)

	// sqlite3.Error.Error() returns the code description, e.g. "constraint failed"
	assertpkg.True(t, isSQLiteError(wrappedErr, "constraint failed"),
		"isSQLiteError should match constraint error via pointer, got: %v", sqliteErr.Error())

	assertpkg.False(t, isSQLiteError(wrappedErr, "no such table"),
		"isSQLiteError should not match unrelated substring via pointer")
}

func TestIsSQLiteError_TypedNilPointer(t *testing.T) {
	// Create a typed nil *sqlite3.Error (interface value non-nil, underlying pointer nil)
	var sqliteErr *sqlite3.Error

	// Wrap in an interface to create a typed nil scenario
	// errors.As can succeed with typed nil in certain edge cases
	wrappedErr := typedNilError{sqliteErr}

	// This should not panic - the nil guard should protect us
	result := isSQLiteError(wrappedErr, "any")
	assertpkg.False(t, result, "isSQLiteError should return false for typed nil pointer")
}

func TestIsSQLiteError_NonSQLiteError(t *testing.T) {
	plainErr := errors.New("some other error")

	assertpkg.False(t, isSQLiteError(plainErr, "error"), "isSQLiteError should return false for non-sqlite errors")
}

func TestIsSQLiteError_NilError(t *testing.T) {
	assertpkg.False(t, isSQLiteError(nil, "anything"), "isSQLiteError should return false for nil error")
}

func TestSQLiteDialect_IsBusyError(t *testing.T) {
	assert := assertpkg.New(t)
	d := &SQLiteDialect{}

	busy := sqlite3.Error{Code: sqlite3.ErrBusy}
	assert.True(d.IsBusyError(fmt.Errorf("wrapped: %w", busy)), "IsBusyError should match SQLITE_BUSY")

	locked := sqlite3.Error{Code: sqlite3.ErrLocked}
	assert.True(d.IsBusyError(fmt.Errorf("wrapped: %w", locked)), "IsBusyError should match SQLITE_LOCKED")

	constraint := sqlite3.Error{Code: sqlite3.ErrConstraint}
	assert.False(d.IsBusyError(fmt.Errorf("wrapped: %w", constraint)), "IsBusyError should not match non-busy sqlite errors")

	assert.False(d.IsBusyError(errors.New("plain error")), "IsBusyError should not match non-sqlite errors")

	assert.False(d.IsBusyError(nil), "IsBusyError should not match nil")
}

// typedNilError is a helper type that implements error and allows
// errors.As to extract a typed nil *sqlite3.Error.
type typedNilError struct {
	err *sqlite3.Error
}

func (e typedNilError) Error() string {
	return "typed nil error wrapper"
}

func (e typedNilError) As(target any) bool {
	if ptr, ok := target.(**sqlite3.Error); ok {
		*ptr = e.err
		return true
	}
	return false
}
