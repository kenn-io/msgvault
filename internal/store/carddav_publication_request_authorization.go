package store

import (
	"context"
	"database/sql"
	"net/http"
)

// AuthorizeCardDAVPublicationRequestContext checks current native authority
// for one mapped update or canonical observation. The transaction ends before
// the caller dispatches its provider request.
func (s *Store) AuthorizeCardDAVPublicationRequestContext(
	ctx context.Context, expected CardDAVPublication, method, target string, authorize PersonEditAuthorizer,
) error {
	if expected.PendingOperation != CardDAVMutationUpdate || expected.PersonID <= 0 || expected.AddressBookID <= 0 || expected.ResolutionConflictID != 0 || expected.ConflictOwned || target != expected.Href || (method != http.MethodPut && method != http.MethodGet) {
		return ErrCardDAVInvalidPlan
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.authorizeCardDAVPublicationRequestTxContext(ctx, tx, expected, method, authorize); err != nil {
			return err
		}
		binding, bound, err := s.cardDAVReceiptBinding(ctx)
		if err != nil {
			return err
		}
		if !bound {
			return nil
		}
		if authorize == nil {
			return ErrCardDAVInvalidPlan
		}
		return s.authorizeCardDAVReceiptRequestTx(ctx, tx, binding, expected)
	})
}

func (s *Store) authorizeCardDAVPublicationRequestTxContext(ctx context.Context, tx *loggedTx, expected CardDAVPublication, method string, authorize PersonEditAuthorizer) error {
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
	if method == http.MethodPut {
		var bookRevision int64
		var subscribed, writeTarget bool
		var canUpdate sql.NullBool
		if err := tx.QueryRowContext(ctx, `SELECT sync_revision, is_subscribed, is_write_target, can_update FROM carddav_address_books WHERE id = ?`, current.AddressBookID).Scan(&bookRevision, &subscribed, &writeTarget, &canUpdate); err != nil {
			return err
		}
		if !subscribed || !writeTarget {
			return ErrCardDAVNoWriteTarget
		}
		if cardDAVCapabilityDenied(canUpdate) {
			return ErrCardDAVReadOnlyAddressBook
		}
		if bookRevision != current.BookSyncRevision {
			return ErrCardDAVStalePlan
		}
		resource, err := findCardDAVResourceForPersonTx(ctx, tx, current.AddressBookID, current.PersonID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		if resource.Href != current.Href || resource.MappingRevision != current.MappingRevision || resource.RemoteETag != current.RemoteETag {
			return ErrCardDAVStalePlan
		}
		snapshot, err := s.loadPersonVCardSnapshotTx(ctx, tx, current.PersonID)
		if err != nil {
			return err
		}
		if snapshot.Fingerprint != current.LocalHash {
			return ErrCardDAVStalePlan
		}
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
	return nil
}
