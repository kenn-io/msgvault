package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/identitycontrol"
)

// IdentityGrantSelection contains only authoritative resource identities for
// owner-selected IDs. Reading it never seeds metadata or refreshes projections.
type IdentityGrantSelection struct {
	Persons      []IdentityPersonEvidence
	AddressBooks []IdentityGrantAddressBook
}

type IdentityGrantAddressBook struct {
	AccountID            int64
	BookID               int64
	CanonicalURL         string
	OwnershipFingerprint string
}

func (s *Store) IdentityGrantSelectionContext(ctx context.Context, personIDs, bookIDs []int64) (*IdentityGrantSelection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	count := len(personIDs) + len(bookIDs)
	if count < 1 || count > 100 {
		return nil, fmt.Errorf("%w: select between 1 and 100 resources", identitycontrol.ErrInvalidRequest)
	}
	for _, ids := range [][]int64{personIDs, bookIDs} {
		seen := make(map[int64]bool, len(ids))
		for _, id := range ids {
			if !identitycontrol.ValidID(id) || seen[id] {
				return nil, fmt.Errorf("%w: resource IDs must be distinct exact positive JSON integers", identitycontrol.ErrInvalidRequest)
			}
			seen[id] = true
		}
	}
	var selection *IdentityGrantSelection
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		selection, err = s.identityGrantSelectionTx(ctx, tx, personIDs, bookIDs)
		return err
	})
	if err != nil {
		return nil, err
	}
	return selection, nil
}

func (s *Store) identityGrantSelectionTx(ctx context.Context, tx *loggedTx, personIDs, bookIDs []int64) (*IdentityGrantSelection, error) {
	selection := &IdentityGrantSelection{Persons: []IdentityPersonEvidence{}, AddressBooks: []IdentityGrantAddressBook{}}
	if len(personIDs) > 0 {
		ph, args := sortedIDPlaceholders(canonicalIdentityIDs(personIDs))
		if err := identityReadRows(ctx, tx, `SELECT id,vcard_uid,revision,vcard_projection_revision FROM persons WHERE id IN (`+ph+`) ORDER BY id`, args, func(rows *loggedRows) error {
			var person IdentityPersonEvidence
			if err := rows.Scan(&person.ID, &person.UID, &person.Revision, &person.ProjectionRevision); err != nil {
				return err
			}
			selection.Persons = append(selection.Persons, person)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("resolve grant people: %w", err)
		}
		if len(selection.Persons) != len(personIDs) {
			return nil, ErrPersonNotFound
		}
	}
	if len(bookIDs) > 0 {
		ph, args := sortedIDPlaceholders(canonicalIdentityIDs(bookIDs))
		if err := identityReadRows(ctx, tx, `SELECT b.id,b.account_id,b.canonical_url,a.connection_name,a.base_url,a.username,a.principal_url,a.home_url FROM carddav_address_books b JOIN carddav_accounts a ON a.id=b.account_id WHERE b.id IN (`+ph+`) ORDER BY b.id`, args, func(rows *loggedRows) error {
			var book IdentityGrantAddressBook
			var name, base, user, principal, home string
			if err := rows.Scan(&book.BookID, &book.AccountID, &book.CanonicalURL, &name, &base, &user, &principal, &home); err != nil {
				return err
			}
			if !identitycontrol.ValidID(book.AccountID) {
				return fmt.Errorf("%w: invalid native account ID", identitycontrol.ErrInvalidRequest)
			}
			var err error
			book.OwnershipFingerprint, err = identityEvidenceHash([]string{name, base, user, principal, home})
			if err != nil {
				return err
			}
			selection.AddressBooks = append(selection.AddressBooks, book)
			return nil
		}); err != nil {
			return nil, fmt.Errorf("resolve grant address books: %w", err)
		}
		if len(selection.AddressBooks) != len(bookIDs) {
			return nil, ErrCardDAVAddressBookNotFound
		}
	}
	return selection, nil
}
