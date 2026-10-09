package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// SQLReader shares SQL reads across pools and snapshots.
type SQLReader interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type readSnapshotKey struct{}

type readSnapshot struct {
	db *sql.DB
	tx *loggedTx
}

// BeginReadSnapshotContext binds subsequent reads of this database to one snapshot.
func (s *Store) BeginReadSnapshotContext(ctx context.Context) (context.Context, func(), error) {
	if snapshot, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot); ok && snapshot.db == s.db.DB {
		return ctx, func() {}, nil
	}
	start := time.Now()
	slog.Debug("sql tx begin", "request_id", RequestIDFromContext(ctx))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		slog.Warn("sql tx begin failed", "request_id", RequestIDFromContext(ctx), "error", err.Error(), "duration_ms", time.Since(start).Milliseconds())
		return ctx, nil, fmt.Errorf("begin read snapshot: %w", err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			err := tx.Rollback()
			if errors.Is(err, sql.ErrTxDone) {
				err = nil
			}
			if err != nil {
				slog.Warn("sql tx rollback failed", "request_id", RequestIDFromContext(ctx), "error", err.Error(), "duration_ms", time.Since(start).Milliseconds())
			} else {
				slog.Debug("sql tx rollback", "request_id", RequestIDFromContext(ctx), "duration_ms", time.Since(start).Milliseconds())
			}
		})
	}
	return context.WithValue(ctx, readSnapshotKey{}, &readSnapshot{db: s.db.DB, tx: tx}), release, nil
}

// WithoutReadSnapshotContext preserves request lifetime and values for physical reads.
func WithoutReadSnapshotContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, readSnapshotKey{}, struct{}{})
}

// ReadDBContext selects the snapshot only for its owning database.
func ReadDBContext(ctx context.Context, db *sql.DB) SQLReader {
	if snapshot, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot); ok && snapshot.db == db {
		return snapshot.tx.Tx
	}
	return db
}
