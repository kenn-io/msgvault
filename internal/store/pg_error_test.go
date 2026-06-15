package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	assertpkg "github.com/stretchr/testify/assert"
)

// TestPostgreSQLDialect_IsBusyError mirrors TestSQLiteDialect_IsBusyError for
// the PostgreSQL dialect. It needs no database: IsBusyError only inspects the
// SQLSTATE on a *pgconn.PgError, so synthetic errors fully exercise it.
func TestPostgreSQLDialect_IsBusyError(t *testing.T) {
	assert := assertpkg.New(t)
	d := &PostgreSQLDialect{}

	// 55P03 lock_not_available, 40P01 deadlock_detected, 57014 query_canceled
	// (statement_timeout) are all treated as retryable busy conditions.
	for _, code := range []string{"55P03", "40P01", "57014"} {
		err := &pgconn.PgError{Code: code}
		assert.True(d.IsBusyError(err), "IsBusyError should match SQLSTATE %s", code)
		assert.True(d.IsBusyError(fmt.Errorf("query failed: %w", err)),
			"IsBusyError should match wrapped SQLSTATE %s", code)
	}

	// 23505 unique_violation is a constraint error, not a busy condition.
	unique := &pgconn.PgError{Code: "23505"}
	assert.False(d.IsBusyError(unique), "IsBusyError should not match SQLSTATE 23505")
	assert.False(d.IsBusyError(fmt.Errorf("insert failed: %w", unique)),
		"IsBusyError should not match wrapped SQLSTATE 23505")

	assert.False(d.IsBusyError(errors.New("plain error")), "IsBusyError should not match non-pg errors")
	assert.False(d.IsBusyError(nil), "IsBusyError should not match nil")
}
