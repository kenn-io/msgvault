package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ProviderIdentityRecord is provider metadata, never an ownership assertion.
// ID is the opaque provider object ID scoped by its collection and account.
// Removed means absent from the latest complete inventory, not unconfirmed.
type ProviderIdentityRecord struct {
	ID            string
	Identifier    string
	Kind          string
	State         string
	ForDomain     string
	Description   string
	CreatedAt     string
	LastMessageAt string
	Removed       bool
}

func listProviderIdentityRecords(ctx context.Context, q contextRowsQuerier, sourceID int64, provider string) ([]ProviderIdentityRecord, error) {
	rows, err := q.QueryContext(ctx, `SELECT provider_id,identifier,kind,state,for_domain,description,created_at,last_message_at,removed FROM provider_identity_records WHERE source_id=? AND provider=? ORDER BY provider_id`, sourceID, provider)
	if err != nil {
		return nil, fmt.Errorf("list provider identity metadata: %w", err)
	}
	defer func() { _ = rows.Close() }()
	records := []ProviderIdentityRecord{}
	for rows.Next() {
		var r ProviderIdentityRecord
		if err := rows.Scan(&r.ID, &r.Identifier, &r.Kind, &r.State, &r.ForDomain, &r.Description, &r.CreatedAt, &r.LastMessageAt, &r.Removed); err != nil {
			return nil, fmt.Errorf("scan provider identity metadata: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *Store) ListProviderIdentityRecordsContext(ctx context.Context, sourceID int64, provider string) ([]ProviderIdentityRecord, error) {
	return listProviderIdentityRecords(ctx, s.db, sourceID, provider)
}

// ApplyProviderIdentitySnapshotContext atomically saves a complete inventory,
// confirms supplied strong evidence and repairs attribution. An identical
// nonempty state returns before opening a write transaction. Failed reads and
// incomplete inventories must not call this method.
func (s *Store) ApplyProviderIdentitySnapshotContext(ctx context.Context, sourceID int64, provider, state string, records []ProviderIdentityRecord, confirmations []IdentityConfirmation) ([]IdentityConfirmationOutcome, bool, error) {
	return s.applyProviderIdentitySnapshotContext(ctx, sourceID, provider, state, records, confirmations, false)
}

// ConfirmProviderIdentitySnapshotContext applies an explicit owner's selection
// even when provider metadata is unchanged. Callers must validate the complete
// selection against current strong provider evidence before calling. The bool
// reports inventory changes; outcomes report the selected ownership merges.
func (s *Store) ConfirmProviderIdentitySnapshotContext(ctx context.Context, sourceID int64, provider, state string, records []ProviderIdentityRecord, confirmations []IdentityConfirmation) ([]IdentityConfirmationOutcome, bool, error) {
	return s.applyProviderIdentitySnapshotContext(ctx, sourceID, provider, state, records, confirmations, true)
}

func (s *Store) applyProviderIdentitySnapshotContext(ctx context.Context, sourceID int64, provider, state string, records []ProviderIdentityRecord, confirmations []IdentityConfirmation, ownerSelection bool) ([]IdentityConfirmationOutcome, bool, error) {
	if sourceID <= 0 || strings.TrimSpace(provider) == "" {
		return nil, false, errors.New("provider snapshot requires a source and provider")
	}
	normalized, err := normalizeIdentityConfirmations(confirmations)
	if err != nil {
		return nil, false, err
	}
	incoming := make(map[string]ProviderIdentityRecord, len(records))
	for _, r := range records {
		if r.ID == "" || r.Removed {
			return nil, false, errors.New("invalid provider identity snapshot record")
		}
		if _, exists := incoming[r.ID]; exists {
			return nil, false, errors.New("duplicate provider identity object ID")
		}
		incoming[r.ID] = r
	}
	sameState := false
	if state != "" {
		var previous string
		err := s.db.QueryRowContext(ctx, `SELECT state FROM provider_identity_snapshots WHERE source_id=? AND provider=?`, sourceID, provider).Scan(&previous)
		if err == nil && previous == state {
			if !ownerSelection {
				return []IdentityConfirmationOutcome{}, false, nil
			}
			sameState = true
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("read provider identity state: %w", err)
		}
	}
	var outcomes []IdentityConfirmationOutcome
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		previous, err := listProviderIdentityRecords(ctx, tx, sourceID, provider)
		if err != nil {
			return err
		}
		prior := make(map[string]ProviderIdentityRecord, len(previous))
		for _, r := range previous {
			prior[r.ID] = r
		}
		changedAddresses := make(map[string]bool)
		for _, r := range records {
			if sameState {
				continue
			}
			if old, ok := prior[r.ID]; ok && old == r {
				continue
			}
			changedAddresses[NormalizeIdentifierForCompare(r.Identifier)] = true
			_, err := tx.ExecContext(ctx, `INSERT INTO provider_identity_records (source_id,provider,provider_id,identifier,kind,state,for_domain,description,created_at,last_message_at,removed) VALUES (?,?,?,?,?,?,?,?,?,?,FALSE) ON CONFLICT (source_id,provider,provider_id) DO UPDATE SET identifier=excluded.identifier,kind=excluded.kind,state=excluded.state,for_domain=excluded.for_domain,description=excluded.description,created_at=excluded.created_at,last_message_at=excluded.last_message_at,removed=FALSE`, sourceID, provider, r.ID, r.Identifier, r.Kind, r.State, r.ForDomain, r.Description, r.CreatedAt, r.LastMessageAt)
			if err != nil {
				return fmt.Errorf("save provider identity metadata: %w", err)
			}
		}
		for _, r := range previous {
			if _, ok := incoming[r.ID]; ok || r.Removed || sameState {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE provider_identity_records SET removed=TRUE WHERE source_id=? AND provider=? AND provider_id=?`, sourceID, provider, r.ID); err != nil {
				return fmt.Errorf("mark vanished provider identity: %w", err)
			}
		}
		var inserted []normalizedIdentityConfirmation
		for _, confirmation := range normalized {
			if !ownerSelection && !changedAddresses[confirmation.normalized] {
				continue
			}
			added, err := s.mergeAccountIdentitySignalsTx(ctx, tx, sourceID, confirmation.identifier, confirmation.signals, newIdentifierMatch(confirmation.identifier))
			if err != nil {
				return err
			}
			outcomes = append(outcomes, IdentityConfirmationOutcome{Identifier: confirmation.identifier, Added: added, Signals: confirmation.signals})
			if added {
				inserted = append(inserted, confirmation)
			}
		}
		if err := s.refreshConfirmedIdentityAttributionTx(ctx, tx, sourceID, inserted); err != nil {
			return err
		}
		if !sameState {
			if _, err := tx.ExecContext(ctx, `INSERT INTO provider_identity_snapshots (source_id,provider,state) VALUES (?,?,?) ON CONFLICT (source_id,provider) DO UPDATE SET state=excluded.state`, sourceID, provider, state); err != nil {
				return fmt.Errorf("save provider identity state: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return outcomes, !sameState, nil
}
