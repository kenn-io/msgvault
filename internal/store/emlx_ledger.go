package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

// EmlxTargetState reads completion evidence alongside retained message/raw metadata.
// It deliberately does not read MIME or message bodies.
type EmlxTargetState struct {
	MessageID    int64
	HasRaw       bool
	Deleted      bool
	HasLabel     bool
	InternalDate sql.NullTime
	Item         *SourceImportItem
}

// IsEmlxDigest reports whether s is a lowercase hex SHA-256 digest.
func IsEmlxDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isEmlxRootPrefix(prefix string) bool {
	return len(prefix) == 65 && prefix[64] == '/' && IsEmlxDigest(prefix[:64])
}

// IsEmlxTargetID reports whether id names a shared archived EMLX message.
func IsEmlxTargetID(id string) bool {
	digest, ok := strings.CutPrefix(id, "emlx-")
	return ok && IsEmlxDigest(digest)
}

// IsEmlxOccurrenceID reports whether id is a root digest followed by a clean,
// slash-separated relative path.
func IsEmlxOccurrenceID(id string) bool {
	if len(id) <= 65 || !isEmlxRootPrefix(id[:65]) {
		return false
	}
	rel := id[65:]
	return filepath.IsLocal(rel) && filepath.ToSlash(rel) == rel && path.Clean(rel) == rel &&
		!strings.ContainsRune(rel, '\x00')
}

func isEmlxLedgerIdentity(provider, id string) bool {
	switch provider {
	case "emlx-target":
		return IsEmlxTargetID(id)
	case "emlx-occurrence":
		return IsEmlxOccurrenceID(id)
	}
	return false
}

// PutEmlxLedgerItemsContext publishes fenced dirty/complete transitions for
// one source in a single transaction.
func (s *Store) PutEmlxLedgerItemsContext(ctx context.Context, items ...SourceImportItem) error {
	if len(items) == 0 {
		return nil
	}
	for _, item := range items {
		if item.SourceID != items[0].SourceID || !isEmlxLedgerIdentity(item.Provider, item.ProviderID) ||
			(item.Status != "pending" && item.Status != "imported") {
			return errors.New("invalid EMLX ledger identity or status")
		}
	}
	now := s.dialect.Now()
	query := fmt.Sprintf(`INSERT INTO source_import_items
   (source_id,provider,provider_id,name,checksum,size,modified_at,imported_at,status,
    records_imported,error_message,created_at,updated_at)
   VALUES (?,?,?,?,?,?,?,?,?,?,?,%s,%s)
   ON CONFLICT(source_id,provider,provider_id) DO UPDATE SET name=excluded.name,
   checksum=excluded.checksum,size=excluded.size,modified_at=excluded.modified_at,
   imported_at=excluded.imported_at,status=excluded.status,
   records_imported=excluded.records_imported,error_message=excluded.error_message,
   updated_at=%s`, now, now, now)
	err := s.withSyncSourceWriteContext(ctx, items[0].SourceID, func(q querier) error {
		for _, item := range items {
			if _, err := q.Exec(query, item.SourceID, item.Provider, item.ProviderID, item.Name,
				item.Checksum, item.Size, item.ModifiedAt, item.ImportedAt, item.Status,
				item.RecordsImported, item.ErrorMessage); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("publish EMLX ledger: %w", err)
	}
	return nil
}

// InvalidateEmlxRootContext revokes every selected-root receipt in one statement,
// including paths that a later discovery cannot visit. Retain source evidence so
// repair does not replay an unchanged occurrence over newer archived content.
func (s *Store) InvalidateEmlxRootContext(ctx context.Context, sourceID int64, rootPrefix string) error {
	if !isEmlxRootPrefix(rootPrefix) {
		return errors.New("invalid EMLX root prefix")
	}
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		_, err := q.Exec(`UPDATE source_import_items SET status='pending'
 WHERE source_id=? AND provider='emlx-occurrence' AND substr(provider_id,1,65)=?`, sourceID, rootPrefix)
		return err
	})
}

// EmlxOccurrencesContext reads the receipts for ids. Missing ids are cold
// occurrences and are absent from the result.
func (s *Store) EmlxOccurrencesContext(
	ctx context.Context, sourceID int64, ids []string,
) (map[string]SourceImportItem, error) {
	for _, id := range ids {
		if !IsEmlxOccurrenceID(id) {
			return nil, fmt.Errorf("invalid EMLX occurrence %q", id)
		}
	}
	out := make(map[string]SourceImportItem, len(ids))
	err := queryInChunksContext(ctx, s.db, ids, []any{sourceID},
		`SELECT id,provider_id,COALESCE(name,''),COALESCE(checksum,''),status
 FROM source_import_items WHERE source_id=? AND provider='emlx-occurrence' AND provider_id IN (%s)`,
		func(rows *loggedRows) error {
			item := SourceImportItem{SourceID: sourceID, Provider: "emlx-occurrence"}
			if err := rows.Scan(&item.ID, &item.ProviderID, &item.Name, &item.Checksum, &item.Status); err != nil {
				return err
			}
			out[item.ProviderID] = item
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("read EMLX occurrences: %w", err)
	}
	return out, nil
}

// EmlxTargetsContext reads archived message state and completion evidence for
// ids. Every requested id has an entry; a zero MessageID means not archived.
// HasLabel reports whether the message already carries labelID.
func (s *Store) EmlxTargetsContext(
	ctx context.Context, sourceID, labelID int64, ids []string,
) (map[string]EmlxTargetState, error) {
	out := make(map[string]EmlxTargetState, len(ids))
	for _, id := range ids {
		if !IsEmlxTargetID(id) {
			return nil, fmt.Errorf("invalid EMLX target %q", id)
		}
		out[id] = EmlxTargetState{}
	}
	err := queryInChunksContext(ctx, s.db, ids, []any{labelID, sourceID},
		`SELECT m.source_message_id, m.id, m.internal_date,
 EXISTS(SELECT 1 FROM message_raw r WHERE r.message_id=m.id),
 m.deleted_at IS NOT NULL,
 EXISTS(SELECT 1 FROM message_labels ml WHERE ml.message_id=m.id AND ml.label_id=?)
 FROM messages m WHERE m.source_id=? AND m.source_message_id IN (%s)`,
		func(rows *loggedRows) error {
			var id string
			var st EmlxTargetState
			err := rows.Scan(&id, &st.MessageID, &st.InternalDate, &st.HasRaw, &st.Deleted, &st.HasLabel)
			if err != nil {
				return err
			}
			out[id] = st
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("read EMLX targets: %w", err)
	}
	err = queryInChunksContext(ctx, s.db, ids, []any{sourceID},
		`SELECT provider_id,id,COALESCE(checksum,''),status FROM source_import_items
 WHERE source_id=? AND provider='emlx-target' AND provider_id IN (%s)`,
		func(rows *loggedRows) error {
			item := SourceImportItem{SourceID: sourceID, Provider: "emlx-target"}
			if err := rows.Scan(&item.ProviderID, &item.ID, &item.Checksum, &item.Status); err != nil {
				return err
			}
			st := out[item.ProviderID]
			st.Item = &item
			out[item.ProviderID] = st
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("read EMLX target completion: %w", err)
	}
	return out, nil
}
