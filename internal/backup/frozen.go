package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	// The sqlite3 driver is registered by internal/store in production; the
	// blank import keeps internal/backup usable standalone (tests, restore).
	_ "github.com/mattn/go-sqlite3"
)

// FreezeCoordinator brackets the freeze window: Begin drains and holds the
// daemon's operation gate, End releases it (docs/architecture/backup-format.md, Freeze Protocol). The pinned read
// transaction — which keeps the main DB file frozen afterwards — is owned by
// FrozenSession, not the coordinator.
type FreezeCoordinator interface {
	Begin(ctx context.Context) error
	End(ctx context.Context) error
}

// NoopFreezeCoordinator is for tests and capture paths with no daemon.
type NoopFreezeCoordinator struct{}

// Begin implements FreezeCoordinator.
func (NoopFreezeCoordinator) Begin(context.Context) error { return nil }

// End implements FreezeCoordinator.
func (NoopFreezeCoordinator) End(context.Context) error { return nil }

const (
	checkpointRetries = 50
	checkpointBackoff = 100 * time.Millisecond

	// freezeEndTimeout bounds the FreezeCoordinator.End call made when
	// closing out the freeze window. It runs against a fresh context
	// (context.Background(), not the caller's request context) because the
	// gate must still be released — and released promptly — even when the
	// caller's context is already canceled or openPinnedSession itself
	// failed; a short, independent bound keeps that release from hanging.
	freezeEndTimeout = 10 * time.Second
)

// FrozenSession holds the pinned read transaction that freezes the main DB
// file in content and size while writers proceed into the WAL.
type FrozenSession struct {
	db        *sql.DB
	tx        *sql.Tx
	PageSize  uint32
	PageCount uint64
}

// OpenFrozenSession executes the freeze protocol: gate -> checkpoint
// TRUNCATE -> pinned read transaction -> capture geometry -> gate release.
//
// The gate is released here, before attachment capture runs, so a gated
// operation that deletes attachment files (remove-account) can race a
// long-running capture and delete a file the pinned transaction still
// references. That backup fails loudly (read or hash error) and is
// retryable; holding the gate through the whole capture window would block
// every daemon write for minutes instead. Accepted limitation — see
// docs/architecture/backup-format.md, Current Limitations.
func OpenFrozenSession(ctx context.Context, dbPath string, fc FreezeCoordinator) (*FrozenSession, error) {
	if err := fc.Begin(ctx); err != nil {
		return nil, fmt.Errorf("backup: freeze begin: %w", err)
	}
	s, err := openPinnedSession(ctx, dbPath)
	endCtx, cancel := context.WithTimeout(context.Background(), freezeEndTimeout)
	endErr := fc.End(endCtx)
	cancel()
	if err != nil {
		if s != nil {
			_ = s.Close()
		}
		if endErr != nil {
			return nil, errors.Join(err, fmt.Errorf("backup: freeze end: %w", endErr))
		}
		return nil, err
	}
	if endErr != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: freeze end: %w", endErr)
	}
	return s, nil
}

func openPinnedSession(ctx context.Context, dbPath string) (*FrozenSession, error) {
	db, err := sql.Open("sqlite3", sqliteURIDSN(dbPath, "_busy_timeout=5000"))
	if err != nil {
		return nil, fmt.Errorf("backup: opening DB %s: %w", dbPath, err)
	}
	db.SetMaxOpenConns(1)
	s := &FrozenSession{db: db}

	var busy, logFrames, checkpointed int
	for attempt := 0; ; attempt++ {
		row := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
		if err := row.Scan(&busy, &logFrames, &checkpointed); err != nil {
			_ = s.Close()
			return nil, fmt.Errorf("backup: wal_checkpoint: %w", err)
		}
		if busy == 0 {
			break
		}
		if attempt >= checkpointRetries {
			_ = s.Close()
			return nil, fmt.Errorf("backup: wal_checkpoint stayed busy after %d attempts (long-running reader?)", attempt)
		}
		select {
		case <-ctx.Done():
			_ = s.Close()
			return nil, ctx.Err()
		case <-time.After(checkpointBackoff):
		}
	}

	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: opening pinned read transaction: %w", err)
	}
	s.tx = tx
	// Touch the schema to materialize the read mark at WAL offset zero.
	var n int64
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master").Scan(&n); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: pinning read snapshot: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_size").Scan(&s.PageSize); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: reading page_size: %w", err)
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA page_count").Scan(&s.PageCount); err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("backup: reading page_count: %w", err)
	}
	return s, nil
}

// Tx exposes the pinned read transaction so an App's FrozenView can run
// its schema queries inside the frozen snapshot.
func (s *FrozenSession) Tx() *sql.Tx { return s.tx }

// Close releases the pinned transaction and connection. Idempotent.
func (s *FrozenSession) Close() error {
	if s.tx != nil {
		_ = s.tx.Rollback()
		s.tx = nil
	}
	if s.db != nil {
		err := s.db.Close()
		s.db = nil
		return err
	}
	return nil
}
