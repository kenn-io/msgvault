package store

import "context"

// SetAttributionAfterLockHookForTest pauses every attribution entry after it
// holds the identity row and its source locks, before the sync fence.
func (s *Store) SetAttributionAfterLockHookForTest(fn func(sourceIDs []int64)) func() {
	s.attributionAfterLockHook = fn
	return func() { s.attributionAfterLockHook = nil }
}

// SetAccountAttributionAfterReadHookForTest pauses the refresh owner after it
// read a row's type and current attribution.
func (s *Store) SetAccountAttributionAfterReadHookForTest(fn func(messageID int64)) func() {
	s.accountAttributionAfterReadHook = fn
	return func() { s.accountAttributionAfterReadHook = nil }
}

// SetAccountRepairPageSizeForTest shrinks the repair page so a test can stop
// a run between committed pages.
func (s *Store) SetAccountRepairPageSizeForTest(size int) {
	s.accountRepairPageSizeOverride = size
}

// RefreshAccountAttributionPlainTxForTest calls the refresh owner inside a
// plain transaction that took no attribution locks.
func (s *Store) RefreshAccountAttributionPlainTxForTest(ctx context.Context, messageID int64) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		_, err := s.refreshAccountAttributionTx(ctx, tx, messageID, deliveryInput{})
		return err
	})
}

// RefreshAccountAttributionLockingSourcesForTest calls the refresh owner
// inside a shared attribution entry that locks only sourceIDs.
func (s *Store) RefreshAccountAttributionLockingSourcesForTest(ctx context.Context, messageID int64, sourceIDs ...int64) error {
	return s.withAttributionTxContext(ctx, attributionLock{Sources: sourceIDs}, func(tx *loggedTx) error {
		_, err := s.refreshAccountAttributionTx(ctx, tx, messageID, deliveryInput{})
		return err
	})
}

// RefreshAccountAttributionForTest re-derives one row from stored inputs.
func (s *Store) RefreshAccountAttributionForTest(ctx context.Context, messageID int64) error {
	return s.withMessageAttributionTxContext(ctx, messageID, func(tx *loggedTx) error {
		_, err := s.refreshAccountAttributionTx(ctx, tx, messageID, deliveryInput{})
		return err
	})
}

// LockIdentityInSharedAttributionTxForTest asks for the exclusive identity
// lock inside a shared attribution entry.
func (s *Store) LockIdentityInSharedAttributionTxForTest(ctx context.Context, sourceID int64) error {
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		return s.lockIdentityMutationTxContext(ctx, tx)
	})
}

// ErrAttributionLockMissingForTest and ErrAttributionLockUpgradeForTest expose
// the lock-contract errors.
var (
	ErrAttributionLockMissingForTest = errAttributionLockMissing
	ErrAttributionLockUpgradeForTest = errAttributionLockUpgrade
)
