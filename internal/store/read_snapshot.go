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
		level := slog.LevelWarn
		if errors.Is(err, context.Canceled) {
			level = slog.LevelDebug
		}
		slog.Log(ctx, level, "sql tx begin failed", "request_id", RequestIDFromContext(ctx), "error", err.Error(), "duration_ms", time.Since(start).Milliseconds())
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

// HasReadSnapshotContext reports whether readers share a snapshot transaction.
// Reads using that transaction must run through completion without overlapping.
func HasReadSnapshotContext(ctx context.Context) bool {
	_, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot)
	return ok
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

// RunIndependentReads runs reads that do not use each other's results and
// returns the first error in argument order, as running them in order would.
// Outside a read snapshot each read takes its own pool connection, so the
// reads run at the same time and all of them finish before it returns.
// Inside a snapshot every read shares one transaction, which serves one
// statement at a time, so the reads run in order and stop at the first error.
// Each read must finish with its rows before it returns and must write only
// state that no other read touches.
func RunIndependentReads(ctx context.Context, db *sql.DB, reads ...func(context.Context) error) error {
	if snapshot, ok := ctx.Value(readSnapshotKey{}).(*readSnapshot); len(reads) < 2 || (ok && snapshot.db == db) {
		for _, read := range reads {
			if err := read(ctx); err != nil {
				return err
			}
		}
		return nil
	}
	errs := make([]error, len(reads))
	var wg sync.WaitGroup
	for i, read := range reads {
		wg.Go(func() { errs[i] = read(ctx) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
