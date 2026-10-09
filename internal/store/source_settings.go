package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrSourceSettingsInvalid identifies invalid archive settings.
var ErrSourceSettingsInvalid = errors.New("invalid source settings")

// ErrSourceRetired prevents an importer from recreating a merged account.
var ErrSourceRetired = errors.New("source is retired")

// SourceSettings are archive settings, separate from provider credentials and cursors.
type SourceSettings struct {
	Alias              string `json:"alias"`
	HistoryOnly        bool   `json:"history_only"`
	MergedIntoSourceID int64  `json:"merged_into_source_id,omitzero"`
	ReanchorRequired   bool   `json:"reanchor_required"`
}

// SourceSettingsUpdate distinguishes omitted settings from an explicit false.
type SourceSettingsUpdate struct {
	Alias          *string
	HistoryOnly    *bool
	AcceptReanchor bool
	DisplayName    *string
}

const sourceSettingsColumns = `COALESCE((SELECT alias FROM source_settings WHERE source_id = sources.id), ''),
 COALESCE((SELECT history_only FROM source_settings WHERE source_id = sources.id), FALSE),
 COALESCE((SELECT merged_into_source_id FROM source_settings WHERE source_id = sources.id), 0),
 EXISTS (SELECT 1 FROM archive_metadata WHERE key = 'beeper.reanchor_required:' || CAST(sources.id AS TEXT))`

const sourceCatalogColumns = `id, source_type, identifier, display_name, google_user_id,
 last_sync_at, sync_cursor, sync_config, oauth_app, created_at, updated_at, ` + sourceSettingsColumns

func validateSourceAlias(alias string) error {
	if alias == "" || alias != strings.TrimSpace(alias) || !utf8.ValidString(alias) {
		return fmt.Errorf("%w: %s", ErrSourceSettingsInvalid, "identifier alias must be nonempty without surrounding whitespace")
	}
	for _, r := range alias {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: %s", ErrSourceSettingsInvalid, "identifier alias cannot contain control characters")
		}
	}
	return nil
}

func lockSourceMaintenance(ctx context.Context, q contextStatementQuerier) error {
	_, err := q.ExecContext(ctx, `UPDATE source_maintenance_lock SET singleton = singleton WHERE singleton = 1`)
	if err != nil {
		return fmt.Errorf("lock source maintenance: %w", err)
	}
	return nil
}

func checkSourceDisplayNameSelectorConflict(q querier, sourceID int64, displayName string) error {
	var conflict bool
	if err := q.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM source_settings WHERE alias_key = ? AND source_id <> ?
		) OR EXISTS (
			SELECT 1 FROM sources other
			WHERE other.id <> ? AND LOWER(other.identifier) = LOWER(?)
			  AND NOT EXISTS (
				SELECT 1 FROM sources target
				WHERE target.id = ? AND LOWER(target.identifier) = LOWER(?)
			  )
		)
	`, strings.ToLower(displayName), sourceID, sourceID, displayName, sourceID, displayName).Scan(&conflict); err != nil {
		return fmt.Errorf("check display name selector conflict: %w", err)
	}
	if conflict {
		return fmt.Errorf("%w: display name conflicts with another source's identifier or alias", ErrSourceSettingsInvalid)
	}
	return nil
}

func sourceSettingsWith(ctx context.Context, q contextStatementQuerier, sourceID int64) (SourceSettings, error) {
	var settings SourceSettings
	err := q.QueryRowContext(ctx, `SELECT `+sourceSettingsColumns+` FROM sources WHERE id = ?`, sourceID).Scan(
		&settings.Alias, &settings.HistoryOnly, &settings.MergedIntoSourceID, &settings.ReanchorRequired)
	if errors.Is(err, sql.ErrNoRows) {
		return settings, fmt.Errorf("source %d: %w", sourceID, ErrSourceNotFound)
	}
	if err != nil {
		return settings, fmt.Errorf("read source settings: %w", err)
	}
	return settings, nil
}

func (s *Store) GetSourceSettingsContext(ctx context.Context, sourceID int64) (SourceSettings, error) {
	return sourceSettingsWith(ctx, s.db, sourceID)
}

func requireSourceWritableWith(q querier, sourceID int64) error {
	var retired bool
	err := q.QueryRow(`SELECT EXISTS (SELECT 1 FROM source_settings WHERE source_id = ? AND merged_into_source_id IS NOT NULL)`, sourceID).Scan(&retired)
	if err != nil {
		return fmt.Errorf("check source lifecycle: %w", err)
	}
	if retired {
		return fmt.Errorf("source %d: %w", sourceID, ErrSourceRetired)
	}
	return nil
}

// UpdateSourceSettingsContext validates and applies the entire request atomically.
// The provider identifier is immutable; Alias is a user-facing archive selector.
func (s *Store) UpdateSourceSettingsContext(ctx context.Context, sourceID int64, update SourceSettingsUpdate) (SourceSettings, error) {
	var result SourceSettings
	if update.Alias == nil && update.HistoryOnly == nil && !update.AcceptReanchor && update.DisplayName == nil {
		return result, fmt.Errorf("%w: %s", ErrSourceSettingsInvalid, "nothing to update")
	}
	if update.Alias != nil {
		if err := validateSourceAlias(*update.Alias); err != nil {
			return result, err
		}
	}
	if update.DisplayName != nil && strings.TrimSpace(*update.DisplayName) == "" {
		return result, fmt.Errorf("%w: %s", ErrSourceSettingsInvalid, "display name is required")
	}
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if err := lockSourceMaintenance(ctx, tx); err != nil {
			return err
		}
		if err := lockSyncSourceTx(ctx, tx, sourceID); err != nil {
			return err
		}
		current, err := sourceSettingsWith(ctx, tx, sourceID)
		if err != nil {
			return err
		}
		if current.MergedIntoSourceID != 0 {
			return ErrSourceRetired
		}
		var sourceType string
		if err := tx.QueryRowContext(ctx, `SELECT source_type FROM sources WHERE id = ?`, sourceID).Scan(&sourceType); err != nil {
			return err
		}
		if (update.HistoryOnly != nil || update.AcceptReanchor) && sourceType != "beeper" {
			return fmt.Errorf("%w: %s", ErrSourceSettingsInvalid, "history-only and re-anchor settings require a Beeper source")
		}
		if update.DisplayName != nil {
			if err := checkSourceDisplayNameSelectorConflict(boundQuerier{ctx: ctx, q: tx}, sourceID, *update.DisplayName); err != nil {
				return err
			}
		}
		if update.Alias != nil {
			// Compare in Go so both dialects use the same Unicode case folding.
			rows, err := tx.QueryContext(ctx, `SELECT id, identifier, COALESCE(display_name, '') FROM sources WHERE id <> ?`, sourceID)
			if err != nil {
				return err
			}
			conflict := false
			for rows.Next() {
				var id int64
				var identifier string
				var displayName string
				if err := rows.Scan(&id, &identifier, &displayName); err != nil {
					_ = rows.Close()
					return err
				}
				conflict = conflict || strings.EqualFold(identifier, *update.Alias) || strings.EqualFold(displayName, *update.Alias)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
			var aliases int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM source_settings WHERE alias_key = ? AND source_id <> ?`, strings.ToLower(*update.Alias), sourceID).Scan(&aliases); err != nil {
				return err
			}
			if conflict || aliases > 0 {
				return fmt.Errorf("%w: %s", ErrSourceSettingsInvalid, "identifier alias is already used by another source")
			}
			current.Alias = *update.Alias
		}
		if update.HistoryOnly != nil {
			current.HistoryOnly = *update.HistoryOnly
		}
		var aliasKey any
		if current.Alias != "" {
			aliasKey = strings.ToLower(current.Alias)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO source_settings (source_id, alias, alias_key, history_only)
   VALUES (?, ?, ?, ?) ON CONFLICT (source_id) DO UPDATE SET alias = excluded.alias,
   alias_key = excluded.alias_key, history_only = excluded.history_only`, sourceID, current.Alias, aliasKey, current.HistoryOnly); err != nil {
			return fmt.Errorf("update source settings: %w", err)
		}
		if update.AcceptReanchor {
			if _, err := tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key = ?`, BeeperReanchorMarkerKey(sourceID)); err != nil {
				return err
			}
			current.ReanchorRequired = false
		}
		if update.DisplayName != nil {
			if _, err := tx.ExecContext(ctx, `UPDATE sources SET display_name = ?, updated_at = `+s.dialect.Now()+` WHERE id = ?`, *update.DisplayName, sourceID); err != nil {
				return err
			}
		}
		result = current
		return nil
	})
	return result, err
}
