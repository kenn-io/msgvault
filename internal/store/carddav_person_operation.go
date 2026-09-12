package store

import (
	"context"
	"sync"
)

type cardDAVPersonOperation struct {
	token chan struct{}
	users int
}

// AcquireCardDAVPersonOperation serializes network operations on a person
// across every Service and sync-scoped view using this Store. Database revisions
// remain authoritative across processes; no database transaction is held here.
func (s *Store) AcquireCardDAVPersonOperation(ctx context.Context, personID int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if personID <= 0 {
		return nil, ErrPersonNotFound
	}
	base := s.withoutSyncScope()
	base.cardDAVPersonOperationsMu.Lock()
	if base.cardDAVPersonOperations == nil {
		base.cardDAVPersonOperations = make(map[int64]*cardDAVPersonOperation)
	}
	operation := base.cardDAVPersonOperations[personID]
	if operation == nil {
		operation = &cardDAVPersonOperation{token: make(chan struct{}, 1)}
		operation.token <- struct{}{}
		base.cardDAVPersonOperations[personID] = operation
	}
	operation.users++
	base.cardDAVPersonOperationsMu.Unlock()
	drop := func() {
		base.cardDAVPersonOperationsMu.Lock()
		operation.users--
		if operation.users == 0 {
			delete(base.cardDAVPersonOperations, personID)
		}
		base.cardDAVPersonOperationsMu.Unlock()
	}
	select {
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	case <-operation.token:
	}
	if err := ctx.Err(); err != nil {
		operation.token <- struct{}{}
		drop()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { operation.token <- struct{}{}; drop() }) }, nil
}
