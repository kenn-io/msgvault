package store

import (
	"context"
	"time"
)

// SetCardDAVPublicationRetryAfterAuthorizedContext records a provider pause
// only with current authority for the exact native pending publication.
func (s *Store) SetCardDAVPublicationRetryAfterAuthorizedContext(
	ctx context.Context, expected CardDAVPublication, retryAfter time.Time, authorize PersonEditAuthorizer,
) error {
	if expected.PersonID <= 0 || expected.AddressBookID <= 0 || expected.PendingOperation == "" || expected.ResolutionConflictID != 0 || expected.ConflictOwned {
		return ErrCardDAVInvalidPlan
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		current, err := s.lockCardDAVPublicationOperationTx(ctx, tx, expected.PersonID, expected.AddressBookID)
		if err != nil {
			return err
		}
		if !cardDAVPublicationIntentMatches(current, expected) {
			return ErrCardDAVStalePlan
		}
		account, err := getCardDAVAccountForBookFrom(ctx, tx.Tx, s.Rebind, current.AddressBookID)
		if err != nil {
			return err
		}
		if account == nil {
			return ErrCardDAVAddressBookNotFound
		}
		if account.ConnectionGeneration != current.ConnectionGeneration {
			return ErrCardDAVStalePlan
		}
		if authorize != nil {
			scope, err := s.identityGrantSelectionTx(ctx, tx, []int64{current.PersonID}, []int64{current.AddressBookID})
			if err != nil {
				return err
			}
			if err := authorize(ctx, scope); err != nil {
				return err
			}
		}
		return s.setCardDAVRetryAfterFrom(ctx, tx, retryAfter, account.ID)
	})
}
