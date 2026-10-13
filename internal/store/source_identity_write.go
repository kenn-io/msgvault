package store

import "context"

// Source identity changes invalidate resource authority even with capture off.
// Keep the same outer ordering as event admission and attributed native writers.
func (s *Store) withSourceIdentityWrite(ctx context.Context, sourceID int64, write func(*loggedTx) error) error {
	if err := s.requireSyncSource(sourceID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
		return err
	}
	if err := s.fenceSyncGenerationTx(ctx, tx); err != nil {
		return err
	}
	if s.captureMCPEnabled() {
		if _, err := s.mcpClockLock(ctx, tx.Tx); err != nil {
			return mcpSafeError(err)
		}
	}
	if err := write(tx); err != nil {
		return err
	}
	return tx.Commit()
}
