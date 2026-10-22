package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/textimport"
)

// Native MMS archive keys include participant database IDs. The raw native
// identity below survives participant merges without changing archive dedupe.
// Only its domain-separated hash is retained, with no archive foreign key.
func synctechMMSImportIdentity(owner string, raw []byte) (string, error) {
	var mms struct {
		Timestamp  time.Time
		MessageBox *int
		MessageID  sql.NullString
		Parts      []struct{ Text sql.NullString }
		Addresses  []struct{ Address string }
	}
	if err := json.Unmarshal(raw, &mms); err != nil {
		return "", fmt.Errorf("decode native MMS identity: %w", err)
	}
	if mms.MessageBox == nil || mms.Timestamp.IsZero() || strings.TrimSpace(owner) == "" {
		return "", errors.New("incomplete native MMS identity")
	}
	messageID := mms.MessageID.String
	if messageID == "" {
		var text []string
		for _, part := range mms.Parts {
			if part.Text.Valid {
				text = append(text, part.Text.String)
			}
		}
		messageID = strings.Join(text, "\n")
	}
	participants := []string{textimport.NormalizeAddress(owner).Value}
	for _, address := range mms.Addresses {
		if strings.TrimSpace(address.Address) == "" || address.Address == "insert-address-token" {
			continue
		}
		participants = append(participants, textimport.NormalizeAddress(address.Address).Value)
	}
	slices.Sort(participants)
	participants = slices.Compact(participants)
	projection := struct {
		Domain, Timestamp, MessageID string
		Participants                 []string
	}{"msgvault.synctech.mms-import-identity.v1", mms.Timestamp.UTC().Format(time.RFC3339Nano), messageID, participants}
	encoded, err := json.Marshal(projection, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) retainSynctechMMSImportIdentityTx(ctx context.Context, tx *sql.Tx, generation string, sourceID int64, nativeID, owner string, raw []byte, rawFormat string) (bool, error) {
	if rawFormat != "synctech_sms_xml_json" {
		return false, errors.New("invalid native MMS identity format")
	}
	hash, err := synctechMMSImportIdentity(owner, raw)
	if err != nil {
		return false, err
	}
	// #nosec G701 -- SQL is fixed; Rebind changes placeholders and all values are bound.
	result, err := tx.ExecContext(ctx, s.Rebind(`INSERT INTO import_message_identities(source_generation,import_format,identity_hash,source_id,source_message_id) VALUES (?,'synctech_sms',?,?,?) ON CONFLICT(source_generation,import_format,identity_hash) DO NOTHING`), generation, hash, sourceID, nativeID)
	if err != nil {
		return false, err
	}
	added, err := result.RowsAffected()
	return added == 0, err
}

// Legacy adoption precedes capture and native identity loss. Page metadata,
// then read each raw record by primary key. No body scan or external I/O occurs
// while the caller holds the identity, source and Events writer fences.
func (s *Store) adoptLegacySynctechMMSIdentitiesTx(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	var after int64
	for {
		parameters := append(append([]any(nil), args...), after)
		rows, err := tx.QueryContext(ctx, s.Rebind(`SELECT m.id,m.source_id,m.source_message_id,s.identifier FROM messages m JOIN sources s ON s.id=m.source_id LEFT JOIN mcp_source_resources sr ON sr.source_id=m.source_id WHERE s.source_type='synctech_sms' AND m.message_type='mms' AND (`+predicate+`) AND m.id>? AND NOT EXISTS(SELECT 1 FROM import_message_identities i WHERE i.source_generation=sr.generation AND i.import_format='synctech_sms' AND i.source_message_id=m.source_message_id) ORDER BY m.id LIMIT 128`), parameters...)
		if err != nil {
			return err
		}
		type identity struct {
			id, source      int64
			nativeID, owner string
		}
		page := make([]identity, 0, 128)
		err = func() (scanErr error) {
			defer func() { scanErr = errors.Join(scanErr, rows.Close()) }()
			for rows.Next() {
				var item identity
				if err = rows.Scan(&item.id, &item.source, &item.nativeID, &item.owner); err != nil {
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
			lifetime, lifetimeErr := s.ensureMCPScopeLifetime(ctx, tx, "source", item.source, item.source)
			if lifetimeErr != nil {
				return lifetimeErr
			}
			var encoded []byte
			var compression sql.NullString
			var format string
			if err = tx.QueryRowContext(ctx, s.Rebind(`SELECT raw_data,compression,raw_format FROM message_raw WHERE message_id=?`), item.id).Scan(&encoded, &compression, &format); err != nil {
				return fmt.Errorf("preserve native MMS import identity: %w", err)
			}
			raw, decodeErr := decodeMessageRaw(encoded, compression)
			if decodeErr != nil {
				return fmt.Errorf("preserve native MMS import identity: %w", decodeErr)
			}
			if _, err = s.retainSynctechMMSImportIdentityTx(ctx, tx, lifetime.source, item.source, item.nativeID, item.owner, raw, format); err != nil {
				return fmt.Errorf("preserve native MMS import identity: %w", err)
			}
			after = item.id
		}
		if len(page) < 128 {
			return nil
		}
	}
}

// A native backfill can arrive after capture startup. Retain its logical MMS
// identity with the canonical row, without an import run, decision or event.
func (s *Store) retainBackfillSynctechMMSIdentityTx(ctx context.Context, tx *loggedTx, data *MessagePersistData) error {
	if data.Message.MessageType != "mms" || data.RawFormat != "synctech_sms_xml_json" {
		return nil
	}
	var sourceType, owner string
	sourceID := data.Message.SourceID
	if err := tx.QueryRowContext(ctx, `SELECT source_type,identifier FROM sources WHERE id=?`, sourceID).Scan(&sourceType, &owner); err != nil {
		return err
	}
	if sourceType != "synctech_sms" {
		return nil
	}
	lifetime, err := s.ensureMCPScopeLifetime(ctx, tx.Tx, "source", sourceID, sourceID)
	if err != nil {
		return err
	}
	_, err = s.retainSynctechMMSImportIdentityTx(ctx, tx.Tx, lifetime.source, sourceID, data.Message.SourceMessageID, owner, data.RawMIME, data.RawFormat)
	return err
}
