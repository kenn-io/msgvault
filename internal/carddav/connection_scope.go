package carddav

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrConnectionMismatch    = errors.New("CardDAV operation belongs to another connection")
	ErrConnectionUnavailable = errors.New("CardDAV connection is unavailable")
)

// ForConnection creates a client scope before the service is published to a
// controller or scheduler. A saved credential's generation also fences a
// retained client after credentials change. Use zero before discovery binds credentials.
func (s *Service) ForConnection(name string, generation int64) *Service {
	if s == nil {
		return nil
	}
	clone := *s
	clone.connectionName = name
	clone.connectionGeneration = generation
	return &clone
}

func (s *Service) ConnectionName() string {
	if s.connectionName == "" {
		return config.DefaultCardDAVConnection
	}
	return s.connectionName
}

func (s *Service) scopedAccount(ctx context.Context) (*store.CardDAVAccount, error) {
	account, err := s.store.GetCardDAVAccountByNameContext(ctx, s.ConnectionName())
	if err != nil {
		return nil, err
	}
	if s.connectionGeneration > 0 && (account == nil || account.ConnectionGeneration != s.connectionGeneration) {
		return nil, store.ErrCardDAVStalePlan
	}
	return account, nil
}

func (s *Service) scopedAccountID(ctx context.Context) (int64, error) {
	account, err := s.scopedAccount(ctx)
	if err != nil {
		return 0, err
	}
	if account != nil {
		return account.ID, nil
	}
	// Preserve default history and empty-store reads before first discovery.
	if s.ConnectionName() == config.DefaultCardDAVConnection {
		return store.DefaultCardDAVAccountID, nil
	}
	return 0, ErrConnectionUnavailable
}

func (s *Service) scopedBooks(ctx context.Context) ([]store.CardDAVAddressBook, error) {
	id, err := s.scopedAccountID(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.ListCardDAVAddressBooksContext(ctx, id)
}

func (s *Service) requireOwnBook(ctx context.Context, bookID int64) error {
	owner, err := s.store.GetCardDAVAccountForBookContext(ctx, bookID)
	if err != nil {
		return err
	}
	if owner == nil {
		return store.ErrCardDAVAddressBookNotFound
	}
	if owner.ConnectionName != s.ConnectionName() {
		return ErrConnectionMismatch
	}
	if s.connectionGeneration > 0 && owner.ConnectionGeneration != s.connectionGeneration {
		return store.ErrCardDAVStalePlan
	}
	return nil
}

func (s *Service) checkRetry(ctx context.Context) error {
	id, err := s.scopedAccountID(ctx)
	if err != nil {
		return err
	}
	err = s.store.CheckCardDAVRetryAfterContext(ctx, id)
	if !errors.Is(err, store.ErrCardDAVRetryAfter) {
		return err
	}
	gate, gateErr := s.store.GetCardDAVRetryAfterContext(ctx, id)
	if gateErr != nil {
		return errors.Join(err, gateErr)
	}
	if gate != nil {
		return errors.Join(err, &StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: max(time.Nanosecond, time.Until(*gate))})
	}
	return err
}

func (s *Service) setRetry(ctx context.Context, gate time.Time) error {
	id, err := s.scopedAccountID(ctx)
	if err != nil {
		return err
	}
	return s.store.SetCardDAVRetryAfterContext(ctx, gate, id)
}

func (s *Service) scopedPublicationIDs(ctx context.Context) ([]int64, error) {
	id, err := s.scopedAccountID(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.ListCardDAVPublicationPersonIDsContext(ctx, id)
}

func (s *Service) scopedConflicts(ctx context.Context) ([]store.CardDAVConflict, error) {
	id, err := s.scopedAccountID(ctx)
	if err != nil {
		return nil, err
	}
	return s.store.ListCardDAVConflictsContext(ctx, true, id)
}

// SyncFailure exposes the same safe fixed messages recorded in run history.
func SyncFailure(err error) (string, string) { return cardDAVSyncPublicFailure(err) }
