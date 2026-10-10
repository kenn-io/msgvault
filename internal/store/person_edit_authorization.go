package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/identitycontrol"
)

// PersonEditAuthorizer receives native resource identities, never caller-supplied
// ownership. Returning an error rolls back the complete existing person write.
// It runs inside the write transaction and must not call Store methods that
// open another transaction or connection. Write contention may invoke it again.
type PersonEditAuthorizer func(context.Context, *IdentityGrantSelection) error

// PersonEditScopeContext reads the current affected people and imported or
// published address-book ownership without acquiring the write fence.
func (s *Store) PersonEditScopeContext(ctx context.Context, personID int64) (*IdentityGrantSelection, error) {
	return s.readPersonEditScopeContext(ctx, personID, true)
}

// PersonProfileEditScopeContext resolves only the person and its own native
// address books. Structured profile values do not change counterpart projections.
func (s *Store) PersonProfileEditScopeContext(ctx context.Context, personID int64) (*IdentityGrantSelection, error) {
	return s.readPersonEditScopeContext(ctx, personID, false)
}

func (s *Store) readPersonEditScopeContext(ctx context.Context, personID int64, includeCounterparts bool) (*IdentityGrantSelection, error) {
	var scope *IdentityGrantSelection
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var err error
		scope, err = s.personEditScopeTx(ctx, tx, personID, includeCounterparts)
		return err
	})
	if err != nil {
		return nil, err
	}
	return scope, nil
}

// lockAuthorizedPersonAttributeWriteTx takes the ownership fence before the
// existing generation, target and definition locks. Person authorization follows
// those locks so catalog exposure can still acquire its person rows in order.
func (s *Store) lockAuthorizedPersonAttributeWriteTx(ctx context.Context, tx *loggedTx, authorize PersonEditAuthorizer) error {
	if authorize == nil {
		return nil
	}
	return s.lockIdentityMutationTxContext(ctx, tx)
}

func (s *Store) authorizePersonEditTx(ctx context.Context, tx *loggedTx, personID int64, includeCounterparts bool, authorize PersonEditAuthorizer) error {
	if authorize == nil {
		return nil
	}
	// Existing relationship writers update this person's projection before
	// committing. Holding its row keeps their affected-set changes after this
	// authorization and commit, even if they inserted an uncommitted edge first.
	if err := s.lockEmploymentPeopleTx(ctx, tx, personID); err != nil {
		return err
	}
	scope, err := s.personEditScopeTx(ctx, tx, personID, includeCounterparts)
	if err != nil {
		return err
	}
	return authorize(ctx, scope)
}

func (s *Store) personEditScopeTx(ctx context.Context, tx *loggedTx, personID int64, includeCounterparts bool) (*IdentityGrantSelection, error) {
	if !identitycontrol.ValidID(personID) {
		return nil, fmt.Errorf("%w: exact positive person ID required", identitycontrol.ErrInvalidRequest)
	}
	var people []int64
	query := `SELECT p.id FROM persons p WHERE p.id = ?`
	args := []any{personID}
	if includeCounterparts {
		query += ` OR EXISTS (
 SELECT 1 FROM person_relationships r WHERE (r.source_person_id = ? AND r.target_person_id = p.id)
 OR (r.target_person_id = ? AND r.source_person_id = p.id))`
		args = append(args, personID, personID)
	}
	if err := identityReadRows(ctx, tx, query+` ORDER BY p.id LIMIT 101`, args, func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		people = append(people, id)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("resolve person edit affected people: %w", err)
	}
	found := false
	for _, id := range people {
		found = found || id == personID
	}
	if len(people) > 100 {
		return nil, ErrIdentityOperationTooLarge
	}
	if !found {
		return nil, ErrPersonNotFound
	}
	return s.personEditSelectionTx(ctx, tx, people)
}

func (s *Store) personEditSelectionTx(ctx context.Context, tx *loggedTx, people []int64) (*IdentityGrantSelection, error) {
	if len(people) == 0 || len(people) > 100 {
		return nil, ErrIdentityOperationTooLarge
	}
	ph, ids := sortedIDPlaceholders(people)
	args := append(append([]any(nil), ids...), ids...)
	var books []int64
	if err := identityReadRows(ctx, tx, `SELECT b.id FROM carddav_address_books b WHERE EXISTS (
 SELECT 1 FROM carddav_resources r WHERE r.address_book_id = b.id AND r.person_id IN (`+ph+`)) OR EXISTS (
 SELECT 1 FROM carddav_publications p WHERE p.address_book_id = b.id AND p.person_id IN (`+ph+`)) ORDER BY b.id LIMIT 101`, args, func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		books = append(books, id)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("resolve person edit owning address books: %w", err)
	}
	if len(people)+len(books) > 100 {
		return nil, ErrIdentityOperationTooLarge
	}
	return s.identityGrantSelectionTx(ctx, tx, people, books)
}
