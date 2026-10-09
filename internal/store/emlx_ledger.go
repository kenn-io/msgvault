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

// EmlxTargetState reads completion evidence alongside live message/raw metadata.
// It deliberately does not read MIME or message bodies.
type EmlxTargetState struct {
	MessageID    int64
	HasRaw       bool
	InternalDate sql.NullTime
	Item         *SourceImportItem
}

func emlxHex(s string) bool {
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
func emlxRoot(prefix string) bool {
	return len(prefix) == 65 && prefix[64] == '/' && emlxHex(prefix[:64])
}
func emlxIdentity(provider, id string) bool {
	switch provider {
	case "emlx-target":
		return strings.HasPrefix(id, "emlx-") && emlxHex(strings.TrimPrefix(id, "emlx-"))
	case "emlx-occurrence":
		if len(id) <= 65 || !emlxRoot(id[:65]) {
			return false
		}
		rel := id[65:]
		return filepath.IsLocal(rel) && path.Clean(rel) == rel && !strings.Contains(rel, "\\")
	}
	return false
}

// PutEmlxLedgerItemContext publishes a fenced dirty/complete transition.
func (s *Store) PutEmlxLedgerItemContext(ctx context.Context, item SourceImportItem) error {
	if !emlxIdentity(item.Provider, item.ProviderID) || (item.Status != "pending" && item.Status != "imported") {
		return errors.New("invalid EMLX ledger identity or status")
	}
	err := s.withSyncSourceWriteContext(ctx, item.SourceID, func(q querier) error {
		_, err := q.Exec(fmt.Sprintf(`INSERT INTO source_import_items
   (source_id,provider,provider_id,name,checksum,size,modified_at,imported_at,status,records_imported,error_message,created_at,updated_at)
   VALUES (?,?,?,?,?,?,?,?,?,?,?,%s,%s)
   ON CONFLICT(source_id,provider,provider_id) DO UPDATE SET name=excluded.name,checksum=excluded.checksum,
   size=excluded.size,modified_at=excluded.modified_at,imported_at=excluded.imported_at,status=excluded.status,
   records_imported=excluded.records_imported,error_message=excluded.error_message,updated_at=%s`, s.dialect.Now(), s.dialect.Now(), s.dialect.Now()),
			item.SourceID, item.Provider, item.ProviderID, item.Name, item.Checksum, item.Size, item.ModifiedAt, item.ImportedAt, item.Status, item.RecordsImported, item.ErrorMessage)
		return err
	})
	if err != nil {
		return fmt.Errorf("publish EMLX ledger: %w", err)
	}
	return nil
}

// InvalidateEmlxRootContext revokes every selected-root receipt in one statement,
// including paths that a later discovery cannot visit. Targets and archives remain.
func (s *Store) InvalidateEmlxRootContext(ctx context.Context, sourceID int64, rootPrefix string) error {
	if !emlxRoot(rootPrefix) {
		return errors.New("invalid EMLX root prefix")
	}
	return s.withSyncSourceWriteContext(ctx, sourceID, func(q querier) error {
		_, err := q.Exec(`UPDATE source_import_items SET status='pending',checksum=NULL WHERE source_id=? AND provider='emlx-occurrence' AND substr(provider_id,1,65)=?`, sourceID, rootPrefix)
		return err
	})
}

// ListEmlxOccurrencesContext pages bounded receipts without holding rows during writes.
func (s *Store) ListEmlxOccurrencesContext(ctx context.Context, sourceID int64, rootPrefix string, afterID int64, limit int) ([]SourceImportItem, error) {
	if !emlxRoot(rootPrefix) || afterID < 0 || limit <= 0 || limit > 200 {
		return nil, errors.New("invalid EMLX receipt page")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,source_id,provider,provider_id,COALESCE(name,''),COALESCE(checksum,''),size,modified_at,imported_at,status,records_imported,error_message
 FROM source_import_items WHERE source_id=? AND provider='emlx-occurrence' AND substr(provider_id,1,65)=? AND id>? ORDER BY id LIMIT ?`, sourceID, rootPrefix, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("list EMLX receipts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SourceImportItem
	for rows.Next() {
		var item SourceImportItem
		if err := rows.Scan(&item.ID, &item.SourceID, &item.Provider, &item.ProviderID, &item.Name, &item.Checksum, &item.Size, &item.ModifiedAt, &item.ImportedAt, &item.Status, &item.RecordsImported, &item.ErrorMessage); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// EmlxTargetsContext bounds queries to 200 identities, including missing targets.
func (s *Store) EmlxTargetsContext(ctx context.Context, sourceID int64, ids []string) (map[string]EmlxTargetState, error) {
	out := make(map[string]EmlxTargetState, len(ids))
	for len(ids) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := min(200, len(ids))
		chunk := ids[:n]
		ids = ids[n:]
		args := []any{sourceID}
		for _, id := range chunk {
			if !emlxIdentity("emlx-target", id) {
				return nil, errors.New("invalid EMLX target")
			}
			args = append(args, id)
			out[id] = EmlxTargetState{}
		}
		marks := strings.TrimSuffix(strings.Repeat("?,", n), ",")
		rows, err := s.db.QueryContext(ctx, `SELECT source_message_id,id,internal_date,EXISTS(SELECT 1 FROM message_raw r WHERE r.message_id=m.id) FROM messages m WHERE source_id=? AND deleted_at IS NULL AND source_message_id IN (`+marks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var st EmlxTargetState
			if err = rows.Scan(&id, &st.MessageID, &st.InternalDate, &st.HasRaw); err != nil {
				break
			}
			out[id] = st
		}
		err = joinRowsError(rows, err)
		if err != nil {
			return nil, err
		}
		rows, err = s.db.QueryContext(ctx, `SELECT provider_id,id,COALESCE(checksum,''),status FROM source_import_items WHERE source_id=? AND provider='emlx-target' AND provider_id IN (`+marks+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var item SourceImportItem
			if err = rows.Scan(&item.ProviderID, &item.ID, &item.Checksum, &item.Status); err != nil {
				break
			}
			item.SourceID = sourceID
			item.Provider = "emlx-target"
			st := out[item.ProviderID]
			st.Item = &item
			out[item.ProviderID] = st
		}
		err = joinRowsError(rows, err)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func joinRowsError(rows interface {
	Err() error
	Close() error
}, err error) error {
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	return err
}

// EmlxOccurrenceContext reads one receipt with cancellation. A missing row is a
// cold occurrence. Point reads keep discovery memory bounded on large sources.
func (s *Store) EmlxOccurrenceContext(ctx context.Context, sourceID int64, id string) (*SourceImportItem, error) {
	if !emlxIdentity("emlx-occurrence", id) {
		return nil, errors.New("invalid EMLX occurrence")
	}
	item := SourceImportItem{SourceID: sourceID, Provider: "emlx-occurrence", ProviderID: id}
	err := s.db.QueryRowContext(ctx, `SELECT id,COALESCE(name,''),COALESCE(checksum,''),status FROM source_import_items WHERE source_id=? AND provider='emlx-occurrence' AND provider_id=?`, sourceID, id).Scan(&item.ID, &item.Name, &item.Checksum, &item.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // A missing receipt is a valid cold occurrence.
	}
	if err != nil {
		return nil, fmt.Errorf("read EMLX occurrence: %w", err)
	}
	return &item, nil
}
