package store

import (
	"context"
	"fmt"
)

// CardDAVBinding identifies one CardDAV resource that is currently mapped to
// a durable person. Connection and book are their configured display names.
type CardDAVBinding struct {
	Connection    string               `json:"connection"`
	Book          string               `json:"book"`
	Href          string               `json:"href"`
	RemoteUID     string               `json:"remote_uid"`
	MappingStatus CardDAVMappingStatus `json:"mapping_status"`
}

// listCardDAVBindingsTx returns the bindings of the given people, keyed by
// person ID. Every requested person has a non-nil entry. Callers pass a
// bounded set of IDs; listAllCardDAVBindingsTx serves unbounded reads.
func (s *Store) listCardDAVBindingsTx(
	ctx context.Context, tx *loggedTx, personIDs []int64,
) (map[int64][]CardDAVBinding, error) {
	bindings := make(map[int64][]CardDAVBinding, len(personIDs))
	for _, id := range personIDs {
		bindings[id] = []CardDAVBinding{}
	}
	placeholders, args := sortedIDPlaceholders(personIDs)
	if placeholders == "" {
		return bindings, nil
	}
	if err := s.scanCardDAVBindingsTx(ctx, tx, bindings,
		`resource.person_id IN (`+placeholders+`)`, args...); err != nil {
		return nil, err
	}
	return bindings, nil
}

// listAllCardDAVBindingsTx returns every mapped binding without binding one
// SQL variable per person, so full person listings stay within driver limits.
func (s *Store) listAllCardDAVBindingsTx(
	ctx context.Context, tx *loggedTx,
) (map[int64][]CardDAVBinding, error) {
	bindings := map[int64][]CardDAVBinding{}
	if err := s.scanCardDAVBindingsTx(ctx, tx, bindings,
		`resource.person_id IS NOT NULL`); err != nil {
		return nil, err
	}
	return bindings, nil
}

func (s *Store) scanCardDAVBindingsTx(
	ctx context.Context, tx *loggedTx, into map[int64][]CardDAVBinding,
	filter string, args ...any,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT resource.person_id, account.connection_name,
		book.display_name, resource.href, COALESCE(resource.remote_uid, ''),
		resource.mapping_status
		FROM carddav_resources resource
		JOIN carddav_address_books book ON book.id = resource.address_book_id
		JOIN carddav_accounts account ON account.id = book.account_id
		WHERE `+filter+`
		ORDER BY resource.person_id, account.connection_name, book.display_name,
			resource.href, resource.id`, args...)
	if err != nil {
		return fmt.Errorf("list CardDAV person bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var personID int64
		var binding CardDAVBinding
		if err := rows.Scan(&personID, &binding.Connection, &binding.Book,
			&binding.Href, &binding.RemoteUID, &binding.MappingStatus); err != nil {
			return fmt.Errorf("scan CardDAV person binding: %w", err)
		}
		into[personID] = append(into[personID], binding)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate CardDAV person bindings: %w", err)
	}
	return nil
}
