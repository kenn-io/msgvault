package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/jackc/pgx/v5/stdlib"
)

const syncExecutionLockCleanupTimeout = 5 * time.Second

type syncExecutionLock interface {
	// The bool reports whether ownership ended, independently of cleanup errors.
	release() (bool, error)
}

type syncRunExecutionLock struct {
	lock            syncExecutionLock
	releaseWhenDone bool
}

type syncExecutionLockState struct {
	mu       sync.Mutex
	byRun    map[int64]syncRunExecutionLock
	bySource map[int64]syncExecutionLock
}

func newSyncExecutionLockState() *syncExecutionLockState {
	return &syncExecutionLockState{
		byRun:    make(map[int64]syncRunExecutionLock),
		bySource: make(map[int64]syncExecutionLock),
	}
}

// SyncExecution owns the process-level execution lock for one source. A
// caller can create more than one durable sync run while retaining the same
// ownership, as Gmail history recovery does for its full and catch-up phases.
type SyncExecution struct {
	store    *Store
	sourceID int64
	lock     syncExecutionLock

	mu       sync.Mutex
	released bool
}

// AcquireSyncExecutionContext takes exclusive ownership of one source and
// recovers rows left running by a worker that no longer owns the lock.
func (s *Store) AcquireSyncExecutionContext(
	ctx context.Context, sourceID int64,
) (*SyncExecution, error) {
	lock, err := s.acquireSyncExecutionLock(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if err := s.recoverAbandonedSyncSource(ctx, sourceID); err != nil {
		return nil, errors.Join(err, s.abandonSyncExecutionLock(sourceID, lock))
	}
	return &SyncExecution{store: s.withoutSyncScope(), sourceID: sourceID, lock: lock}, nil
}

// StartSyncContext creates a durable run under this execution's existing
// source ownership. Completing the run does not release the source lock.
func (e *SyncExecution) StartSyncContext(
	ctx context.Context, syncType, operationID string,
) (int64, error) {
	return e.StartSyncWithRequestContext(ctx, syncType, operationID, "")
}

// StartSyncWithRequestContext creates a durable run and records the request
// identity that controls whether its checkpoint can be resumed.
func (e *SyncExecution) StartSyncWithRequestContext(
	ctx context.Context, syncType, operationID, requestFingerprint string,
) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.released {
		return 0, errors.New("sync execution already released")
	}
	return e.store.startSyncContextWithLock(
		ctx, e.sourceID, syncType, operationID, requestFingerprint, e.lock,
	)
}

// Release gives up source ownership after every phase has stopped.
func (e *SyncExecution) Release() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.released {
		return nil
	}
	var err error
	e.released, err = e.store.releaseOwnedSyncExecutionLock(e.sourceID, e.lock)
	return err
}

func (s *Store) acquireSyncExecutionLock(
	ctx context.Context, sourceID int64,
) (syncExecutionLock, error) {
	base := s.withoutSyncScope()
	state := base.syncExecutionLocks
	if state == nil {
		return &noOpSyncExecutionLock{}, nil
	}

	state.mu.Lock()
	if _, exists := state.bySource[sourceID]; exists {
		state.mu.Unlock()
		return nil, ErrSyncAlreadyActive
	}
	state.bySource[sourceID] = nil
	state.mu.Unlock()

	lock, err := base.acquireBackendSyncExecutionLock(ctx, sourceID)
	if err != nil {
		state.mu.Lock()
		delete(state.bySource, sourceID)
		state.mu.Unlock()
		return nil, err
	}

	state.mu.Lock()
	state.bySource[sourceID] = lock
	state.mu.Unlock()
	return lock, nil
}

func (s *Store) acquireBackendSyncExecutionLock(
	ctx context.Context, sourceID int64,
) (syncExecutionLock, error) {
	if s.IsPostgreSQL() {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire PostgreSQL sync lock connection: %w", err)
		}
		lock := &postgresSyncExecutionLock{conn: conn, sourceID: sourceID, rebind: s.Rebind}
		var acquired bool
		err = conn.QueryRowContext(ctx, s.Rebind(`
			SELECT pg_try_advisory_lock(
				hashtextextended(
					current_schema() || ':msgvault-sync:' || CAST(CAST(? AS BIGINT) AS TEXT), 0
				)
			)`), sourceID).Scan(&acquired)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("acquire PostgreSQL sync lock: %w", err)
		}
		if !acquired {
			_ = conn.Close()
			return nil, ErrSyncAlreadyActive
		}
		return lock, nil
	}

	dbPath := s.sqliteFilesystemPath
	if dbPath == ":memory:" || strings.Contains(dbPath, ":memory:") {
		return &noOpSyncExecutionLock{}, nil
	}

	dbPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve sync lock database path: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(dbPath); resolveErr == nil {
		dbPath = resolved
	}
	lockPath := dbPath + ".sync-" + strconv.FormatInt(sourceID, 10) + ".lock"

	sqliteSyncLockRegistry.mu.Lock()
	if _, exists := sqliteSyncLockRegistry.paths[lockPath]; exists {
		sqliteSyncLockRegistry.mu.Unlock()
		return nil, ErrSyncAlreadyActive
	}
	sqliteSyncLockRegistry.paths[lockPath] = struct{}{}
	sqliteSyncLockRegistry.mu.Unlock()

	fileLock := flock.New(lockPath)
	acquired, err := fileLock.TryLock()
	if err != nil || !acquired {
		sqliteSyncLockRegistry.mu.Lock()
		delete(sqliteSyncLockRegistry.paths, lockPath)
		sqliteSyncLockRegistry.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("acquire SQLite sync lock: %w", err)
		}
		return nil, ErrSyncAlreadyActive
	}
	return &sqliteSyncExecutionLock{lock: fileLock, path: lockPath}, nil
}

func (s *Store) registerSyncExecutionLock(
	sourceID, runID int64, lock syncExecutionLock, releaseWhenDone bool,
) {
	if lock == nil {
		return
	}
	state := s.withoutSyncScope().syncExecutionLocks
	if state == nil {
		return
	}
	state.mu.Lock()
	state.byRun[runID] = syncRunExecutionLock{
		lock:            lock,
		releaseWhenDone: releaseWhenDone,
	}
	if releaseWhenDone {
		state.bySource[sourceID] = lock
	}
	state.mu.Unlock()
}

func (s *Store) abandonSyncExecutionLock(sourceID int64, lock syncExecutionLock) error {
	if lock == nil {
		return nil
	}
	_, err := s.releaseOwnedSyncExecutionLock(sourceID, lock)
	return err
}

func (s *Store) releaseSyncExecutionLock(runID int64) error {
	state := s.withoutSyncScope().syncExecutionLocks
	if state == nil {
		return nil
	}
	state.mu.Lock()
	runLock, exists := state.byRun[runID]
	state.mu.Unlock()
	if !exists {
		return nil
	}
	if !runLock.releaseWhenDone {
		state.mu.Lock()
		delete(state.byRun, runID)
		state.mu.Unlock()
		return nil
	}
	released, err := runLock.lock.release()
	if err != nil {
		err = fmt.Errorf("release sync %d execution lock: %w", runID, err)
	}
	if !released {
		return err
	}
	state.mu.Lock()
	delete(state.byRun, runID)
	for sourceID, sourceLock := range state.bySource {
		if sourceLock == runLock.lock {
			delete(state.bySource, sourceID)
			break
		}
	}
	state.mu.Unlock()
	return err
}

func (s *Store) releaseOwnedSyncExecutionLock(sourceID int64, lock syncExecutionLock) (bool, error) {
	state := s.withoutSyncScope().syncExecutionLocks
	if state == nil {
		return lock.release()
	}
	state.mu.Lock()
	owned := state.bySource[sourceID] == lock
	state.mu.Unlock()
	if !owned {
		return true, nil
	}
	released, err := lock.release()
	if err != nil {
		err = fmt.Errorf("release source %d sync execution lock: %w", sourceID, err)
	}
	if !released {
		return false, err
	}
	state.mu.Lock()
	if state.bySource[sourceID] == lock {
		delete(state.bySource, sourceID)
	}
	for runID, runLock := range state.byRun {
		if runLock.lock == lock {
			delete(state.byRun, runID)
		}
	}
	state.mu.Unlock()
	return true, err
}

func (s *Store) releaseAllSyncExecutionLocks() error {
	state := s.withoutSyncScope().syncExecutionLocks
	if state == nil {
		return nil
	}
	state.mu.Lock()
	locks := make(map[int64]syncExecutionLock, len(state.bySource))
	for sourceID, lock := range state.bySource {
		if lock != nil {
			locks[sourceID] = lock
		}
	}
	state.mu.Unlock()

	var releaseErr error
	for sourceID, lock := range locks {
		if _, err := s.releaseOwnedSyncExecutionLock(sourceID, lock); err != nil {
			releaseErr = errors.Join(releaseErr, err)
		}
	}
	if releaseErr != nil {
		return fmt.Errorf("release sync execution locks: %w", releaseErr)
	}
	return nil
}

type noOpSyncExecutionLock struct {
	_ byte
}

func (*noOpSyncExecutionLock) release() (bool, error) { return true, nil }

var sqliteSyncLockRegistry = struct {
	mu    sync.Mutex
	paths map[string]struct{}
}{paths: make(map[string]struct{})}

type sqliteSyncExecutionLock struct {
	lock *flock.Flock
	path string
}

func (l *sqliteSyncExecutionLock) release() (bool, error) {
	if err := l.lock.Unlock(); err != nil {
		return false, fmt.Errorf("unlock SQLite sync lock: %w", err)
	}
	sqliteSyncLockRegistry.mu.Lock()
	delete(sqliteSyncLockRegistry.paths, l.path)
	sqliteSyncLockRegistry.mu.Unlock()
	return true, nil
}

type postgresSyncExecutionLock struct {
	conn     *sql.Conn
	sourceID int64
	rebind   func(string) string
	mu       sync.Mutex
	released bool
}

func (l *postgresSyncExecutionLock) release() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), syncExecutionLockCleanupTimeout)
	defer cancel()
	var unlockErr, closeErr error
	// Keep access to the session until cleanup finishes; database/sql can discard
	// a failed connection before pgx's asynchronous socket cleanup has finished.
	rawErr := l.conn.Raw(func(driverConn any) error {
		postgresConn, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return fmt.Errorf("unexpected PostgreSQL sync lock driver %T", driverConn)
		}
		conn := postgresConn.Conn()
		var unlocked bool
		unlockErr = conn.QueryRow(ctx, l.rebind(`
			SELECT pg_advisory_unlock(
				hashtextextended(
					current_schema() || ':msgvault-sync:' || CAST(CAST(? AS BIGINT) AS TEXT), 0
				)
			)`), l.sourceID).Scan(&unlocked)
		if unlockErr == nil {
			l.released = true
			if !unlocked {
				unlockErr = errors.New("PostgreSQL sync lock was not held")
			}
			return nil
		}
		unlockErr = fmt.Errorf("unlock PostgreSQL sync lock: %w", unlockErr)
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), syncExecutionLockCleanupTimeout)
		defer cleanupCancel()
		closeErr = conn.Close(cleanupCtx)
		select {
		case <-conn.PgConn().CleanupDone():
			l.released = true
			return driver.ErrBadConn // Discard the closed session instead of pooling it.
		case <-cleanupCtx.Done():
			closeErr = errors.Join(closeErr, cleanupCtx.Err())
			return nil // Retain the connection so cleanup can be retried.
		}
	})
	if l.released && errors.Is(rawErr, driver.ErrBadConn) {
		rawErr = nil
	} else if l.released {
		closeErr = errors.Join(closeErr, l.conn.Close())
	}
	return l.released, errors.Join(unlockErr, closeErr, rawErr)
}
