package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
)

func TestDailyNoteRetryClassifier(t *testing.T) {
	assert := assert.New(t)
	sqliteStore := &Store{dialect: &SQLiteDialect{}}
	postgresStore := &Store{dialect: &PostgreSQLDialect{}}

	assert.True(dailyNoteRetryable(context.Background(), sqliteStore,
		sqlite3.Error{Code: sqlite3.ErrBusy}))
	assert.False(dailyNoteRetryable(context.Background(), sqliteStore,
		sqlite3.Error{Code: sqlite3.ErrConstraint}))
	for _, code := range []string{"40P01", "40001"} {
		err := fmt.Errorf("wrapped: %w", &pgconn.PgError{Code: code})
		assert.True(dailyNoteRetryable(context.Background(), postgresStore, err), code)
	}
	for _, code := range []string{"57014", "55P03", "23505"} {
		assert.False(dailyNoteRetryable(context.Background(), postgresStore,
			&pgconn.PgError{Code: code}), code)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.False(dailyNoteRetryable(cancelled, postgresStore,
		&pgconn.PgError{Code: "40001"}))
	assert.False(dailyNoteRetryable(context.Background(), postgresStore, errors.New("plain")))
}
