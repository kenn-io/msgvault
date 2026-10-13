package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const microsoftCategoryPrefix = "msmail-category:"

// EnsureMicrosoftMailFoldersContext updates folder labels. A category label
// holding a folder's display name moves to a distinct name so the two never merge.
func (s *Store) EnsureMicrosoftMailFoldersContext(ctx context.Context, sourceID int64, labels map[string]LabelInfo) (map[string]int64, error) {
	var result map[string]int64
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{sourceID}}, func(tx *loggedTx) error {
		q := boundQuerier{ctx: ctx, q: tx}
		folderNames := make(map[string]bool, len(labels))
		for providerID, info := range labels {
			if strings.HasPrefix(providerID, microsoftCategoryPrefix) {
				return errors.New("microsoft folder identity uses the category namespace")
			}
			folderNames[info.Name] = true
		}
		for _, info := range labels {
			var id int64
			var providerID sql.NullString
			err := q.QueryRow(`SELECT id,source_label_id FROM labels WHERE source_id=? AND name=?`, sourceID, info.Name).Scan(&id, &providerID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("check Microsoft folder name: %w", err)
			}
			if !strings.HasPrefix(providerID.String, microsoftCategoryPrefix) {
				continue
			}
			name, err := microsoftCategoryLabelName(q, sourceID, id, strings.TrimPrefix(providerID.String, microsoftCategoryPrefix), folderNames)
			if err != nil {
				return err
			}
			if _, err := q.Exec(`UPDATE labels SET name=? WHERE id=?`, name, id); err != nil {
				return fmt.Errorf("rename Microsoft category label: %w", err)
			}
		}
		var err error
		result, err = ensureLabelsBatchWith(q, sourceID, labels, labelFlipsTx(ctx, tx, sourceID))
		return err
	})
	return result, err
}

// microsoftCategoryLabelName returns "Category: <name>", or the first
// "Category: <name> (n)" that no other label of the source or reserved name uses.
func microsoftCategoryLabelName(q boundQuerier, sourceID, labelID int64, category string, reserved map[string]bool) (string, error) {
	base := "Category: " + category
	for n := 1; ; n++ {
		name := base
		if n > 1 {
			name = fmt.Sprintf("%s (%d)", base, n)
		}
		if reserved[name] {
			continue
		}
		var owner int64
		err := q.QueryRow(`SELECT id FROM labels WHERE source_id=? AND name=?`, sourceID, name).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && owner == labelID) {
			return name, nil
		}
		if err != nil {
			return "", fmt.Errorf("check Microsoft category name: %w", err)
		}
	}
}

// ReconcileMicrosoftMailLabelsContext saves a folder and an optional category
// snapshot. Missing categories preserve the previous snapshot; an empty slice
// clears them. The result reports a folder change, not a category change.
func (s *Store) ReconcileMicrosoftMailLabelsContext(ctx context.Context, messageID int64, folderID *int64, categories *[]string) (bool, error) {
	var folderChanged bool
	err := s.withMessageAttributionTxContext(ctx, messageID, func(tx *loggedTx) error {
		var sourceID int64
		if err := tx.QueryRowContext(ctx, `SELECT source_id FROM messages
 WHERE id=? AND EXISTS (SELECT 1 FROM sources s WHERE s.id=messages.source_id AND s.source_type='msmail')`, messageID).Scan(&sourceID); err != nil {
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
	if categories != nil && len(*categories) > 0 {
		// Match the provider's Unicode case rules while keeping each label's ID stable.
		rows, err := tx.QueryContext(ctx, `SELECT source_label_id FROM labels WHERE source_id=? AND source_label_id LIKE ?`, sourceID, microsoftCategoryPrefix+"%")
		if err != nil {
			return false, fmt.Errorf("read Microsoft category identities: %w", err)
		}
		var categoryProviderIDs []string
		for rows.Next() {
			var providerID string
			if err := rows.Scan(&providerID); err != nil {
				_ = rows.Close()
				return false, err
			}
			categoryProviderIDs = append(categoryProviderIDs, providerID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return false, err
		}
		if err := rows.Close(); err != nil {
			return false, err
		}
		for _, category := range *categories {
			providerID := microsoftCategoryPrefix + category
			if i := slices.IndexFunc(categoryProviderIDs, func(existing string) bool {
				return strings.EqualFold(existing, providerID)
			}); i >= 0 {
				providerID = categoryProviderIDs[i]
			}
			var id int64
			var previousName string
			var kind sql.NullString
			err := q.QueryRow(`SELECT id,name,label_type FROM labels WHERE source_id=? AND source_label_id=?`, sourceID, providerID).Scan(&id, &previousName, &kind)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return false, fmt.Errorf("read Microsoft category %q: %w", category, err)
			}
			name, nameErr := microsoftCategoryLabelName(q, sourceID, id, category, nil)
			if nameErr != nil {
				return false, nameErr
			}
			switch {
			case errors.Is(err, sql.ErrNoRows):
				err = q.QueryRow(`INSERT INTO labels (source_id,source_label_id,name,label_type) VALUES (?,?,?,'user') RETURNING id`, sourceID, providerID, name).Scan(&id)
				categoryProviderIDs = append(categoryProviderIDs, providerID)
				catalogChanged = true
			case err == nil && (previousName != name || kind.String != "user"):
				_, err = q.Exec(`UPDATE labels SET name=?,label_type='user',system_role=NULL WHERE id=?`, name, id)
				catalogChanged = true
			}
			if err != nil {
				return false, fmt.Errorf("save Microsoft category %q: %w", category, err)
			}
			if !slices.Contains(desired, id) {
				desired = append(desired, id)
			}
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
