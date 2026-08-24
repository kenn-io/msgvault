package store

import (
	"context"
	"errors"
	"fmt"
)

const documentVectorOperationLockSQL = `hashtextextended(
	current_database() || ':' || current_schema() || ':msgvault.document_vectors', 0)`

// WithDocumentVectorOperationLock serializes document-vector writers. SQLite
// uses a process-local lock because the archive ownership lock only excludes
// other processes. PostgreSQL uses a schema-scoped advisory lock.
func (s *Store) WithDocumentVectorOperationLock(ctx context.Context, operation func() error) (retErr error) {
	if operation == nil {
		return errors.New("document vector operation is required")
	}
	if !s.IsPostgreSQL() {
		s.documentVectorOperationMu.Lock()
		defer s.documentVectorOperationMu.Unlock()
		return operation()
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire document vector operation connection: %w", err)
	}
	locked := false
	defer func() {
		if locked {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), manualTransactionCleanupTimeout)
			defer cancel()
			var unlocked bool
			unlockErr := conn.QueryRowContext(
				cleanupCtx, `SELECT pg_advisory_unlock(`+documentVectorOperationLockSQL+`)`,
			).Scan(&unlocked)
			if unlockErr != nil {
				retErr = errors.Join(retErr, fmt.Errorf("release document vector operation lock: %w", unlockErr))
			} else if !unlocked {
				retErr = errors.Join(retErr, errors.New("document vector operation lock was not held during release"))
			}
		}
		if closeErr := conn.Close(); closeErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close document vector operation connection: %w", closeErr))
		}
	}()
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(`+documentVectorOperationLockSQL+`)`); err != nil {
		return fmt.Errorf("acquire document vector operation lock: %w", err)
	}
	locked = true
	return operation()
}
