package store

import (
	"context"
	"fmt"
)

// ImportantDateIdentityStableContext fails closed for identities involved in a
// merge or split. Their old occurrence keys need reconciliation before the
// producer can safely create another item.
func (s *Store) ImportantDateIdentityStableContext(ctx context.Context, personID int64) (bool, error) {
	var stable bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM persons WHERE id = ?)
		AND NOT EXISTS (SELECT 1 FROM person_merges WHERE current_person_id = ? OR survivor_person_id_at_merge = ? OR absorbed_person_id = ?)
		AND NOT EXISTS (SELECT 1 FROM person_splits WHERE source_person_id = ? OR new_person_id = ?)`, personID, personID, personID, personID, personID, personID).Scan(&stable)
	if err != nil {
		return false, fmt.Errorf("check important-date identity: %w", err)
	}
	return stable, nil
}
