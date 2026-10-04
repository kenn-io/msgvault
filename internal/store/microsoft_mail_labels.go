package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const microsoftCategoryPrefix = "msmail-category:"

// EnsureMicrosoftMailFoldersContext updates folder labels without adopting a
// category label whose display name happens to match a folder.
func (s *Store) EnsureMicrosoftMailFoldersContext(ctx context.Context, sourceID int64, labels map[string]LabelInfo) (map[string]int64, error) {
	var result map[string]int64
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		for providerID, info := range labels {
			if strings.HasPrefix(providerID, microsoftCategoryPrefix) {
				return errors.New("microsoft folder identity uses the category namespace")
			}
			var existingID sql.NullString
			err := q.QueryRow(`SELECT source_label_id FROM labels WHERE source_id=? AND name=?`, sourceID, info.Name).Scan(&existingID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("check Microsoft folder name: %w", err)
			}
			if strings.HasPrefix(existingID.String, microsoftCategoryPrefix) {
				return fmt.Errorf("microsoft folder %q conflicts with an archived category", info.Name)
			}
		}
		var err error
		result, err = ensureLabelsBatchWith(q, sourceID, labels)
		return err
	})
	return result, err
}

// ReconcileMicrosoftMailLabelsContext saves a folder and an optional category
// snapshot. Missing categories preserve the previous snapshot; an empty slice
// clears them. The result reports a folder change, not a category change.
func (s *Store) ReconcileMicrosoftMailLabelsContext(ctx context.Context, messageID int64, folderID *int64, categories *[]string) (bool, error) {
	var folderChanged bool
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `UPDATE messages SET source_message_id=source_message_id
 WHERE id=? AND EXISTS (SELECT 1 FROM sources s WHERE s.id=messages.source_id AND s.source_type='msmail')
 RETURNING source_id`, messageID).Scan(&sourceID); err != nil {
			return fmt.Errorf("resolve Microsoft message labels: %w", err)
		}
		var err error
		folderChanged, err = s.reconcileMicrosoftMailLabelsTx(ctx, tx, sourceID, messageID, folderID, categories)
		return err
	})
	if err != nil {
		return false, err
	}
	return folderChanged, nil
}

func (s *Store) reconcileMicrosoftMailLabelsTx(ctx context.Context, tx *loggedTx, sourceID, messageID int64, folderID *int64, categories *[]string) (bool, error) {
	q := boundQuerier{ctx: ctx, q: tx}
	if folderID != nil {
		var source int64
		var providerID sql.NullString
		if err := q.QueryRow(`SELECT source_id,source_label_id FROM labels WHERE id=?`, *folderID).Scan(&source, &providerID); err != nil {
			return false, fmt.Errorf("resolve Microsoft folder label: %w", err)
		}
		if source != sourceID || providerID.String == "" || strings.HasPrefix(providerID.String, microsoftCategoryPrefix) {
			return false, errors.New("microsoft folder label does not belong to the message source")
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT l.id,l.source_id,l.source_label_id
 FROM message_labels ml JOIN labels l ON l.id=ml.label_id WHERE ml.message_id=?`, messageID)
	if err != nil {
		return false, fmt.Errorf("read Microsoft message labels: %w", err)
	}
	desired := make([]int64, 0)
	var folders []int64
	for rows.Next() {
		var id int64
		var source sql.NullInt64
		var providerID sql.NullString
		if err := rows.Scan(&id, &source, &providerID); err != nil {
			_ = rows.Close()
			return false, err
		}
		category := source.Valid && source.Int64 == sourceID && strings.HasPrefix(providerID.String, microsoftCategoryPrefix)
		nativeFolder := source.Valid && source.Int64 == sourceID && providerID.String != "" && !category
		if nativeFolder {
			folders = append(folders, id)
		}
		if (category && categories != nil) || (nativeFolder && folderID != nil) {
			continue
		}
		desired = append(desired, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	folderChanged := false
	if folderID != nil {
		desired = append(desired, *folderID)
		folderChanged = len(folders) != 1 || folders[0] != *folderID
	}
	catalogChanged := false
	if categories != nil {
		for _, category := range *categories {
			providerID, name := microsoftCategoryPrefix+category, "Category: "+category
			var id int64
			var previousName string
			var kind sql.NullString
			err := q.QueryRow(`SELECT id,name,label_type FROM labels WHERE source_id=? AND source_label_id=?`, sourceID, providerID).Scan(&id, &previousName, &kind)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				// Do not merge a same-name folder into a category. A unique-name
				// conflict rolls back the whole transaction instead.
				err = q.QueryRow(`INSERT INTO labels (source_id,source_label_id,name,label_type) VALUES (?,?,?,'user') RETURNING id`, sourceID, providerID, name).Scan(&id)
				catalogChanged = true
			case err == nil && (previousName != name || kind.String != "user"):
				_, err = q.Exec(`UPDATE labels SET name=?,label_type='user',system_role=NULL WHERE id=?`, name, id)
				catalogChanged = true
			}
			if err != nil {
				return false, fmt.Errorf("save Microsoft category %q: %w", category, err)
			}
			desired = append(desired, id)
		}
	}
	changed, err := s.reconcileMessageLabelsTxContext(ctx, tx, messageID, desired, true)
	if err != nil {
		return false, err
	}
	if changed || catalogChanged {
		if err := s.bumpDerivedDataRevision(tx, true); err != nil {
			return false, err
		}
	}
	return folderChanged, nil
}
