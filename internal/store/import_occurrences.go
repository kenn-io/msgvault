package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
)

// ImportOccurrenceAllocation retains opaque native duplicate matching evidence.
// Evidence contains one hash per optional evidence field; an empty value means
// unknown. No body, name, address, attachment path or raw metadata is retained.
type ImportOccurrenceAllocation struct {
	Base            string
	Number          int
	SourceMessageID string
	Evidence        [7]string
}

func validImportOccurrenceHash(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateImportOccurrence(allocation ImportOccurrenceAllocation) error {
	if !validImportOccurrenceHash(allocation.Base) || allocation.Number < 0 || !strings.HasPrefix(allocation.SourceMessageID, "imazing_csv:") || !validImportOccurrenceHash(strings.TrimPrefix(allocation.SourceMessageID, "imazing_csv:")) {
		return errors.New("invalid import occurrence allocation")
	}
	for _, value := range allocation.Evidence {
		if value != "" && !validImportOccurrenceHash(value) {
			return errors.New("invalid import occurrence evidence")
		}
	}
	return nil
}

// ImportOccurrenceAllocations reads only the current native source lifetime.
// The caller owns its existing native sync; no second scheduler is introduced.
func (s *Store) ImportOccurrenceAllocations(ctx context.Context, sourceID int64) ([]ImportOccurrenceAllocation, error) {
	result := make([]ImportOccurrenceAllocation, 0)
	if s.syncGeneration == nil || s.syncGeneration.sourceID != sourceID {
		return nil, errors.New("import occurrence requires native source execution")
	}
	err := s.withMCPEventTx(ctx, func(tx *sql.Tx, _ MCPEventClock) error {
		lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, "source", sourceID, sourceID)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT base_hash,occurrence,source_message_id,evidence_hashes FROM import_occurrence_allocations WHERE source_generation=? AND import_format='imazing_csv' ORDER BY base_hash,occurrence`), lifetime.source)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var allocation ImportOccurrenceAllocation
			var encoded string
			if err = rows.Scan(&allocation.Base, &allocation.Number, &allocation.SourceMessageID, &encoded); err != nil {
				return err
			}
			if err = json.Unmarshal([]byte(encoded), &allocation.Evidence); err != nil {
				return fmt.Errorf("decode import occurrence evidence: %w", err)
			}
			if err = validateImportOccurrence(allocation); err != nil {
				return err
			}
			result = append(result, allocation)
		}
		return rows.Err()
	})
	return result, err
}

// recordImportOccurrenceTx commits an allocation with the canonical snapshot.
// No foreign key to messages is used: deletion leaves the matching tombstone.
func (s *Store) recordImportOccurrenceTx(ctx context.Context, tx *sql.Tx, sourceID int64, allocation ImportOccurrenceAllocation) error {
	if err := validateImportOccurrence(allocation); err != nil {
		return err
	}
	if s.syncGeneration == nil || s.syncGeneration.sourceID != sourceID {
		return errors.New("import occurrence requires native source execution")
	}
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, "source", sourceID, sourceID)
	if err != nil {
		return err
	}
	var previousID, encoded string
	// #nosec G701 -- SQL is fixed; Rebind changes placeholders and all values are bound.
	err = tx.QueryRowContext(ctx, s.Rebind(`SELECT source_message_id,evidence_hashes FROM import_occurrence_allocations WHERE source_generation=? AND import_format='imazing_csv' AND base_hash=? AND occurrence=?`), lifetime.source, allocation.Base, allocation.Number).Scan(&previousID, &encoded)
	if err == nil {
		if previousID != allocation.SourceMessageID {
			return errors.New("import occurrence identity conflict")
		}
		var previous [7]string
		if err = json.Unmarshal([]byte(encoded), &previous); err != nil {
			return err
		}
		// Omitted current evidence must not erase stronger retained matching data.
		for i, value := range allocation.Evidence {
			if value == "" {
				allocation.Evidence[i] = previous[i]
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	raw, err := json.Marshal(allocation.Evidence)
	if err != nil {
		return err
	}
	// #nosec G701 -- SQL is fixed; Rebind changes placeholders and all values are bound.
	_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO import_occurrence_allocations(source_generation,source_id,import_format,base_hash,occurrence,source_message_id,evidence_hashes) VALUES (?,?,'imazing_csv',?,?,?,?) ON CONFLICT(source_generation,import_format,base_hash,occurrence) DO UPDATE SET evidence_hashes=excluded.evidence_hashes`), lifetime.source, sourceID, allocation.Base, allocation.Number, allocation.SourceMessageID, string(raw))
	return err
}
