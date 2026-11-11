package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
)

// XContentLeaseIdentity identifies a PostgreSQL session without PID reuse.
type XContentLeaseIdentity struct {
	PID          int32
	BackendStart time.Time
	Key          int64
}

// retryPendingXContentLeases retries cleanup of sessions retained by the Store.
func (s *Store) retryPendingXContentLeases() error {
	state := s.withoutSyncScope().syncExecutionLocks
	if state == nil {
		return nil
	}
	state.mu.Lock()
	locks := make([]*postgresSyncExecutionLock, 0, len(state.contentPending))
	for lock := range state.contentPending {
		locks = append(locks, lock)
	}
	state.mu.Unlock()
	var result error
	for _, lock := range locks {
		released, err := lock.release()
		if released {
			state.mu.Lock()
			delete(state.contentPending, lock)
			state.mu.Unlock()
		} else if err == nil {
			err = errors.New("X content lease cleanup incomplete")
		}
		result = errors.Join(result, err)
	}
	return result
}

// AcquireXContentLeaseContext uses a separately owned session so waiters never
// exhaust the archive query pool. Like sync locks, it requires session pooling.
func (s *Store) AcquireXContentLeaseContext(ctx context.Context, exclusive bool) (func() error, XContentLeaseIdentity, error) {
	if !s.IsPostgreSQL() {
		return func() error { return nil }, XContentLeaseIdentity{}, nil
	}
	if err := s.retryPendingXContentLeases(); err != nil {
		return nil, XContentLeaseIdentity{}, fmt.Errorf("retry X content lease cleanup: %w", err)
	}
	db, cleanup, err := openPostgresDB(s.dbPath, false)
	if err != nil {
		return nil, XContentLeaseIdentity{}, err
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		closeErr := db.Close()
		cleanup()
		return nil, XContentLeaseIdentity{}, errors.Join(fmt.Errorf("open X content lease session: %w", err), closeErr)
	}
	lockSQL := `SELECT pg_try_advisory_lock_shared(hashtextextended(current_schema() || ':msgvault-x-content', 0))`
	unlockSQL := `SELECT pg_advisory_unlock_shared(hashtextextended(current_schema() || ':msgvault-x-content', 0))`
	if exclusive {
		lockSQL = `SELECT pg_try_advisory_lock(hashtextextended(current_schema() || ':msgvault-x-content', 0))`
		unlockSQL = `SELECT pg_advisory_unlock(hashtextextended(current_schema() || ':msgvault-x-content', 0))`
	}
	lock := &postgresSyncExecutionLock{conn: conn, unlockSQL: unlockSQL, ownedDB: db, ownedCleanup: cleanup}
	state := s.withoutSyncScope().syncExecutionLocks
	release := func() error {
		released, err := lock.release()
		if !released {
			state.mu.Lock()
			state.contentPending[lock] = struct{}{}
			state.mu.Unlock()
			if err == nil {
				err = errors.New("X content lease cleanup incomplete")
			}
			return err
		}
		state.mu.Lock()
		delete(state.contentPending, lock)
		state.mu.Unlock()
		return err
	}
	var identity XContentLeaseIdentity
	for {
		var acquired bool
		var queryErr error
		rawErr := conn.Raw(func(driverConn any) error {
			pg, ok := driverConn.(*stdlib.Conn)
			if !ok {
				return errors.New("unexpected PostgreSQL content lease driver")
			}
			queryErr = pg.Conn().QueryRow(ctx, lockSQL).Scan(&acquired)
			if queryErr == nil && acquired {
				lock.sourceIDs = append(lock.sourceIDs, 0)
				if !exclusive {
					queryErr = pg.Conn().QueryRow(ctx, `SELECT pg_backend_pid(), backend_start, hashtextextended(current_schema() || ':msgvault-x-content', 0) FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&identity.PID, &identity.BackendStart, &identity.Key)
				}
			}
			return nil
		})
		if err := errors.Join(queryErr, rawErr); err != nil {
			lock.discard = true
			return nil, XContentLeaseIdentity{}, errors.Join(fmt.Errorf("acquire X content lease: %w", err), release())
		}
		if acquired {
			return release, identity, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, XContentLeaseIdentity{}, errors.Join(ctx.Err(), release())
		case <-timer.C:
		}
	}
}
