package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/persontiers"
)

// PersonTier is one person's tier computed from the same preview rules and
// counts the tier UI uses. Source is always "observed": the read writes nothing.
type PersonTier struct {
	PersonID int64
	Tier     int
	Source   string
}

// PersonTiersContext evaluates the requested saved people with the live tier
// rules. People outside tier rules are returned as tier 3; IDs that are not
// saved people are absent. It writes nothing and never falls back to stored
// tier state from retired designs.
func (s *Store) PersonTiersContext(ctx context.Context, personIDs []int64, options PersonTierOptions) (map[int64]PersonTier, error) {
	result := make(map[int64]PersonTier, len(personIDs))
	if len(personIDs) == 0 {
		return result, nil
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		plan, err := s.planPersonTiers(ctx, tx, options)
		if err != nil {
			return err
		}
		for start := 0; start < len(personIDs); start += personTierPageSize {
			ids := personIDs[start:min(start+personTierPageSize, len(personIDs))]
			saved, err := savedPersonIDs(ctx, tx, ids)
			if err != nil {
				return err
			}
			signals, _, err := s.personTierPageData(ctx, tx, ids, plan, options)
			if err != nil {
				return err
			}
			for _, id := range ids {
				if _, ok := saved[id]; !ok {
					continue
				}
				decision := persontiers.Evaluate(signals[id], options.Config)
				result[id] = PersonTier{PersonID: id, Tier: decision.Tier, Source: "observed"}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read person tiers: %w", err)
	}
	return result, nil
}

func savedPersonIDs(ctx context.Context, tx *loggedTx, ids []int64) (map[int64]struct{}, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM persons WHERE id IN (`+
		int64Placeholders(len(ids))+`)`, personMergeSnapshotIDArgs(ids)...)
	if err != nil {
		return nil, fmt.Errorf("read saved people for tiers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int64]struct{}, len(ids))
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan saved person for tiers: %w", err)
		}
		result[id] = struct{}{}
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate saved people for tiers: %w", err)
	}
	return result, nil
}
