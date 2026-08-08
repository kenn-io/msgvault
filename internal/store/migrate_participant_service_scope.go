package store

import (
	"context"
	"database/sql"
	"fmt"
)

const migrationParticipantServiceScope = "participant_identifiers_service_scope_v1"

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
	applied, err := s.IsMigrationAppliedContext(ctx, migrationParticipantServiceScope)
	if err != nil {
		return err
	}
	if applied {
		return nil
	}
	if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		_, err := tx.ExecContext(ctx, `UPDATE participant_identifiers
			SET service_id = (
				SELECT id FROM communication_services
				WHERE slug = CASE identifier_type
					WHEN 'imessage' THEN 'imessage'
					WHEN 'whatsapp' THEN 'whatsapp'
					WHEN 'matrix' THEN 'matrix'
					WHEN 'discord' THEN 'discord'
					WHEN 'synctech-sms' THEN 'sms'
					WHEN 'synctech_sms' THEN 'sms'
				END
			)
			WHERE identifier_type IN (
				'imessage', 'whatsapp', 'matrix', 'discord',
				'synctech-sms', 'synctech_sms'
			)`)
		if err != nil {
			return fmt.Errorf("backfill participant identifier services: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	return s.MarkMigrationAppliedContext(ctx, migrationParticipantServiceScope)
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
