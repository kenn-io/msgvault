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

func (s *Store) listCardDAVBindingsTx(
	ctx context.Context, tx *loggedTx, personIDs []int64,
) (map[int64][]CardDAVBinding, error) {
	bindings := make(map[int64][]CardDAVBinding, len(personIDs))
	for _, id := range personIDs {
		bindings[id] = []CardDAVBinding{}
	}
	if len(personIDs) == 0 {
		return bindings, nil
	}
	placeholders, args := sortedIDPlaceholders(personIDs)
	rows, err := tx.QueryContext(ctx, `SELECT resource.person_id, account.connection_name,
		book.display_name, resource.href, COALESCE(resource.remote_uid, ''),
		resource.mapping_status
		FROM carddav_resources resource
		JOIN carddav_address_books book ON book.id = resource.address_book_id
		JOIN carddav_accounts account ON account.id = book.account_id
		WHERE resource.person_id IN (`+placeholders+`)
		ORDER BY resource.person_id, account.connection_name, book.display_name,
			resource.href, resource.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list CardDAV person bindings: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var personID int64
		var binding CardDAVBinding
		if err := rows.Scan(&personID, &binding.Connection, &binding.Book,
			&binding.Href, &binding.RemoteUID, &binding.MappingStatus); err != nil {
			return nil, fmt.Errorf("scan CardDAV person binding: %w", err)
		}
		bindings[personID] = append(bindings[personID], binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate CardDAV person bindings: %w", err)
	}
	return bindings, nil
}

func (s *Store) attachCardDAVBindingsTx(
	ctx context.Context, tx *loggedTx, person *Person,
) error {
	if person == nil {
		return nil
	}
	bindings, err := s.listCardDAVBindingsTx(ctx, tx, []int64{person.ID})
	if err != nil {
		return err
	}
	if len(bindings[person.ID]) > 0 {
		person.CardDAVBindings = bindings[person.ID]
	}
	return nil
}
