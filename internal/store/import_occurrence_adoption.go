package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/imazingidentity"
)

// adoptLegacyImazingOccurrencesTx runs before generic identity adoption or
// physical identity loss. Callers already own the native identity fence.
// Queries are paged and raw rows are read by primary key; no provider or file IO.
func (s *Store) adoptLegacyImazingOccurrencesTx(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	var after int64
	for {
		parameters := append(append([]any(nil), args...), after)
		rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT s.id,s.identifier FROM sources s WHERE s.source_type='imazing_csv' AND EXISTS (SELECT 1 FROM messages m WHERE m.source_id=s.id AND (`+predicate+`) AND NOT EXISTS (SELECT 1 FROM import_occurrence_allocations a WHERE a.source_message_id=m.source_message_id AND a.source_generation=(SELECT generation FROM mcp_source_resources WHERE source_id=s.id) AND a.import_format='imazing_csv')) AND s.id>? ORDER BY s.id LIMIT 128`), parameters...)
		if err != nil {
			return err
		}
		type source struct {
			id         int64
			identifier string
		}
		page := make([]source, 0, 128)
		err = func() (scanErr error) {
			defer func() { scanErr = errors.Join(scanErr, rows.Close()) }()
			for rows.Next() {
				var item source
				if err = rows.Scan(&item.id, &item.identifier); err != nil {
					return err
				}
				page = append(page, item)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, item := range page {
			if err = ctx.Err(); err != nil {
				return err
			}
			if err = s.adoptLegacyImazingSourceTx(ctx, tx, item.id, item.identifier); err != nil {
				return fmt.Errorf("preserve native import occurrence: %w", err)
			}
			after = item.id
		}
		if len(page) < 128 {
			return nil
		}
	}
}

type legacyImazingOccurrence struct {
	id                int64
	nativeID, service string
	sent              sql.NullTime
	metadata          sql.NullString
}

func (s *Store) adoptLegacyImazingSourceTx(ctx context.Context, tx *sql.Tx, sourceID int64, identifier string) error {
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx, "source", sourceID, sourceID)
	if err != nil {
		return err
	}
	// The retained key set lets a deleted allocated occurrence remain a known
	// prefix member. A gap that predates this evidence is never guessed across.
	known := make(map[string]bool)
	rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT source_message_id FROM messages WHERE source_id=? UNION SELECT source_message_id FROM import_message_decisions WHERE source_generation=? UNION SELECT source_message_id FROM import_occurrence_allocations WHERE source_generation=? AND import_format='imazing_csv'`), sourceID, lifetime.source, lifetime.source)
	if err != nil {
		return err
	}
	err = func() (scanErr error) {
		defer func() { scanErr = errors.Join(scanErr, rows.Close()) }()
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				return err
			}
			known[id] = true
		}
		return rows.Err()
	}()
	if err != nil {
		return err
	}
	groups := make(map[string]map[string]ImportOccurrenceAllocation)
	var after int64
	for {
		rows, err = tx.QueryContext(ctx, s.Rebind(`SELECT m.id,m.source_message_id,m.sent_at,m.message_type,m.metadata FROM messages m WHERE m.source_id=? AND m.id>? AND NOT EXISTS (SELECT 1 FROM import_occurrence_allocations a WHERE a.source_generation=? AND a.import_format='imazing_csv' AND a.source_message_id=m.source_message_id) ORDER BY m.id LIMIT 128`), sourceID, after, lifetime.source)
		if err != nil {
			return err
		}
		page := make([]legacyImazingOccurrence, 0, 128)
		err = func() (scanErr error) {
			defer func() { scanErr = errors.Join(scanErr, rows.Close()) }()
			for rows.Next() {
				var item legacyImazingOccurrence
				if err = rows.Scan(&item.id, &item.nativeID, &item.sent, &item.service, &item.metadata); err != nil {
					return err
				}
				page = append(page, item)
			}
			return rows.Err()
		}()
		if err != nil {
			return err
		}
		for _, item := range page {
			if err = ctx.Err(); err != nil {
				return err
			}
			if !item.sent.Valid {
				return errors.New("legacy_import_identity_unavailable")
			}
			var encoded []byte
			var compression sql.NullString
			var format string
			if err = tx.QueryRowContext(ctx, s.Rebind(`SELECT raw_data,compression,raw_format FROM message_raw WHERE message_id=?`), item.id).Scan(&encoded, &compression, &format); err != nil {
				return errors.New("legacy_import_identity_unavailable")
			}
			if format != "imazing_csv_json" {
				return errors.New("legacy_import_identity_unavailable")
			}
			raw, err := decodeMessageRaw(encoded, compression)
			if err != nil {
				return errors.New("legacy_import_identity_unavailable")
			}
			var fields map[string]string
			if err = json.Unmarshal(raw, &fields); err != nil {
				return errors.New("legacy_import_identity_unavailable")
			}
			base, err := imazingidentity.ArchiveBase(identifier, item.sent.Time, item.service, fields)
			if err != nil {
				return errors.New("legacy_import_identity_unavailable")
			}
			var metadata map[string]jsontext.Value
			if item.metadata.Valid {
				if err = json.Unmarshal([]byte(item.metadata.String), &metadata); err != nil {
					return errors.New("legacy_import_identity_unavailable")
				}
			}
			var values [7]string
			for i, key := range []string{"status", "delivered_date", "read_date", "replying_to", "subject", "text", "attachment"} {
				if value, exists := metadata[key]; exists {
					if err = json.Unmarshal(value, &values[i]); err != nil {
						return errors.New("legacy_import_identity_unavailable")
					}
				}
			}
			if groups[base] == nil {
				groups[base] = make(map[string]ImportOccurrenceAllocation)
			}
			groups[base][item.nativeID] = ImportOccurrenceAllocation{Base: base, SourceMessageID: item.nativeID, Evidence: imazingidentity.EvidenceHashes(values)}
			after = item.id
		}
		if len(page) < 128 {
			break
		}
	}
	bases := make([]string, 0, len(groups))
	for base := range groups {
		bases = append(bases, base)
	}
	slices.Sort(bases)
	for _, base := range bases {
		remaining := groups[base]
		for number := 0; len(remaining) > 0; number++ {
			if err = ctx.Err(); err != nil {
				return err
			}
			id := imazingidentity.OccurrenceID(base, number)
			if !known[id] {
				return errors.New("legacy_import_identity_unavailable")
			}
			allocation, exists := remaining[id]
			if !exists {
				continue
			}
			allocation.Number = number
			if err = validateImportOccurrence(allocation); err != nil {
				return err
			}
			evidence, err := json.Marshal(allocation.Evidence)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, s.Rebind(`INSERT INTO import_occurrence_allocations(source_generation,source_id,import_format,base_hash,occurrence,source_message_id,evidence_hashes) VALUES (?,?,'imazing_csv',?,?,?,?) ON CONFLICT(source_generation,import_format,base_hash,occurrence) DO NOTHING`), lifetime.source, sourceID, base, number, id, string(evidence))
			if err != nil {
				return err
			}
			delete(remaining, id)
		}
	}
	return nil
}
