package store

import (
	"context"
	"database/sql"
	"fmt"
)

const (
	migrationParticipantServiceScope   = "participant_identifiers_service_scope_v1"
	migrationParticipantServiceScopeV2 = "participant_identifiers_service_scope_v2"
)

// ensureParticipantIdentifierServiceScopeIndex runs after the legacy-column
// migrations. Schema scripts run before those migrations, so an index that
// names the new columns cannot safely live in schema.sql/schema_pg.sql: on a
// legacy table CREATE TABLE IF NOT EXISTS is a no-op and the index build would
// fail before the columns could be added.
func (s *Store) ensureParticipantIdentifierServiceScopeIndex(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `
			CREATE INDEX IF NOT EXISTS idx_participant_identifiers_service_scope
			    ON participant_identifiers(
			        service_id, scope_kind, scope_value, identifier_value
			    )
			    WHERE service_id IS NOT NULL
		`); err != nil {
			return fmt.Errorf("create idx_participant_identifiers_service_scope: %w", err)
		}
		return nil
	})
}

func (s *Store) ensureParticipantIdentifierServiceScope(ctx context.Context) error {
	v1Applied, err := s.IsMigrationAppliedContext(ctx, migrationParticipantServiceScope)
	if err != nil {
		return err
	}
	v2Applied, err := s.IsMigrationAppliedContext(ctx, migrationParticipantServiceScopeV2)
	if err != nil {
		return err
	}
	if v1Applied && v2Applied {
		return nil
	}
	if err := s.runMaintenance(ctx, repairParticipantIdentifierServiceScope); err != nil {
		return err
	}
	if !v1Applied {
		if err := s.MarkMigrationAppliedContext(ctx, migrationParticipantServiceScope); err != nil {
			return err
		}
	}
	if !v2Applied {
		return s.MarkMigrationAppliedContext(ctx, migrationParticipantServiceScopeV2)
	}
	return nil
}

func repairParticipantIdentifierServiceScope(
	ctx context.Context, tx *loggedTx,
) error {
	rows, err := tx.QueryContext(ctx, `SELECT id, identifier_type, identifier_value
		FROM participant_identifiers ORDER BY id`)
	if err != nil {
		return fmt.Errorf("list participant identifiers for classification: %w", err)
	}
	type identifier struct {
		id    int64
		kind  string
		value string
	}
	identifiers := make([]identifier, 0)
	for rows.Next() {
		var item identifier
		if err := rows.Scan(&item.id, &item.kind, &item.value); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan participant identifier for classification: %w", err)
		}
		identifiers = append(identifiers, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("list participant identifiers for classification: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close participant identifier classification rows: %w", err)
	}
	for _, item := range identifiers {
		serviceSlug, scopeKind, scopeValue := participantIdentifierClassificationValues(
			item.kind, item.value,
		)
		if serviceSlug == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE participant_identifiers SET
			service_id = (SELECT id FROM communication_services WHERE slug = ?),
			scope_kind = ?, scope_value = ? WHERE id = ?`,
			serviceSlug, scopeKind, scopeValue, item.id,
		); err != nil {
			return fmt.Errorf("classify participant identifier %d: %w", item.id, err)
		}
	}
	return nil
}

func (s *Store) classifiedIdentifierServiceSlugs(
	ctx context.Context,
) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		pi.identifier_type, pi.identifier_value, cs.slug
		FROM participant_identifiers pi
		LEFT JOIN communication_services cs ON cs.id = pi.service_id
		ORDER BY pi.identifier_type, pi.identifier_value`)
	if err != nil {
		return nil, fmt.Errorf("list classified participant identifiers: %w", err)
	}
	defer func() { _ = rows.Close() }()
	classified := make(map[string]string)
	for rows.Next() {
		var kind, value string
		var slug sql.NullString
		if err := rows.Scan(&kind, &value, &slug); err != nil {
			return nil, fmt.Errorf("scan classified participant identifier: %w", err)
		}
		classified[kind+":"+value] = slug.String
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list classified participant identifiers: %w", err)
	}
	return classified, nil
}
