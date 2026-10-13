package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// InboxExecution excludes both sync and source removal through receipt
// settlement. Its removal fence is distinct from sync ownership so ordinary
// active-sync removal and Archive.PurgeSource retain their existing contracts.
type InboxExecution struct {
	mu        sync.Mutex
	execution *SyncExecution
	fence     syncExecutionLock
}

func (s *Store) AcquireInboxExecutionContext(ctx context.Context, sourceID int64) (*InboxExecution, error) {
	fence, err := s.acquireInboxRemovalFence(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	execution, err := s.AcquireSyncExecutionContext(ctx, sourceID)
	if err != nil {
		_, releaseErr := fence.release()
		return nil, errors.Join(err, releaseErr)
	}
	return &InboxExecution{execution: execution, fence: fence}, nil
}

func (e *InboxExecution) Release() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	syncErr := e.execution.Release()
	e.execution.mu.Lock()
	ended := e.execution.released
	e.execution.mu.Unlock()
	if !ended || e.fence == nil {
		return syncErr
	}
	released, err := e.fence.release()
	if released {
		e.fence = nil
	}
	return errors.Join(syncErr, err)
}

func (s *Store) acquireInboxRemovalFence(ctx context.Context, sourceID int64) (syncExecutionLock, error) {
	if !s.IsPostgreSQL() && strings.Contains(s.sqliteFilesystemPath, ":memory:") {
		// A private in-memory archive has no filesystem lock identity. Scoped
		// Store copies share this DB handle and therefore this process fence.
		key := fmt.Sprintf("inbox-memory:%p:%d", s.db.DB, sourceID)
		sqliteSyncLockRegistry.mu.Lock()
		defer sqliteSyncLockRegistry.mu.Unlock()
		if _, exists := sqliteSyncLockRegistry.paths[key]; exists {
			return nil, ErrSyncAlreadyActive
		}
		sqliteSyncLockRegistry.paths[key] = struct{}{}
		return &inboxMemoryFence{key: key}, nil
	}
	return s.acquireBackendSourceExecutionLock(ctx, sourceID, "inbox-control")
}

type inboxMemoryFence struct {
	key  string
	once sync.Once
}

func (f *inboxMemoryFence) release() (bool, error) {
	f.once.Do(func() {
		sqliteSyncLockRegistry.mu.Lock()
		delete(sqliteSyncLockRegistry.paths, f.key)
		sqliteSyncLockRegistry.mu.Unlock()
	})
	return true, nil
}

func (s *Store) retireInboxReceiptsContext(ctx context.Context, exec sourceIdentityExec, sourceID int64) error {
	_, err := exec(ctx, s.Rebind(`UPDATE inbox_operation_receipts SET source_removed_at = ? WHERE source_id = ? AND source_removed_at IS NULL`), time.Now().UTC().UnixNano(), sourceID)
	if err != nil {
		return fmt.Errorf("retire removed source Inbox receipt authority: %w", err)
	}
	return nil
}

// ValidateInboxReceiptAuthorityContext checks control authority separately
// from the immutable receipt returned by trusted Store audit methods.
func (s *Store) ValidateInboxReceiptAuthorityContext(ctx context.Context, id string) error {
	var current bool
	err := s.db.QueryRowContext(ctx, `SELECT source_removed_at IS NULL FROM inbox_operation_receipts WHERE id = ?`, id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !current) {
		return inboxcontrol.ErrDenied
	}
	if err != nil {
		return fmt.Errorf("%w: validate Inbox receipt authority: %w", inboxcontrol.ErrInternal, err)
	}
	return nil
}
