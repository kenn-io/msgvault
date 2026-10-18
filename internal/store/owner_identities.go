package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// OwnerIdentity distinguishes a configured primary address from confirmed aliases.
type OwnerIdentity struct {
	Address string
	Primary bool
}

func (s *Store) OwnerIdentitiesContext(ctx context.Context, sourceID int64) ([]OwnerIdentity, error) {
	ctx, release, err := s.BeginReadSnapshotContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	r := s.snapshotReader(ctx)
	rows, err := r.QueryContext(ctx, `SELECT address FROM account_identities WHERE source_id = ? ORDER BY address`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("read meeting owner identities: %w", err)
	}
	defer func() { _ = rows.Close() }()
	identities := map[string]bool{}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			return nil, err
		}
		if address = strings.ToLower(strings.TrimSpace(address)); address != "" {
			identities[address] = false
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var config sql.NullString
	if err := r.QueryRowContext(ctx, `SELECT sync_config FROM sources WHERE id = ?`, sourceID).Scan(&config); err != nil {
		return nil, err
	}
	var primary struct {
		Email string `json:"account_email"`
	}
	if config.Valid {
		if err := json.Unmarshal([]byte(config.String), &primary); err != nil {
			return nil, fmt.Errorf("decode meeting owner configuration: %w", err)
		}
	}
	if address := strings.ToLower(strings.TrimSpace(primary.Email)); address != "" {
		if _, confirmed := identities[address]; !confirmed {
			identities[address] = true
		}
	}
	result := make([]OwnerIdentity, 0, len(identities))
	for address, primary := range identities {
		result = append(result, OwnerIdentity{Address: address, Primary: primary})
	}
	slices.SortFunc(result, func(a, b OwnerIdentity) int {
		if a.Primary != b.Primary {
			if a.Primary {
				return 1
			}
			return -1
		}
		return strings.Compare(a.Address, b.Address)
	})
	return result, nil
}
