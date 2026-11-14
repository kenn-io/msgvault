package store

import (
	"context"
	"fmt"
)

func (s *Store) migrateAttributeValueJSON(ctx context.Context) error {
	if !s.IsPostgreSQL() {
		return nil
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
			var isJSONB bool
			if err := tx.QueryRowContext(ctx, `SELECT data_type = 'jsonb' FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ? AND column_name = 'value_json'`, table).Scan(&isJSONB); err != nil {
				return fmt.Errorf("inspect attribute JSON column: %w", err)
			}
			if isJSONB {
				if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` ALTER COLUMN value_json TYPE JSON USING value_json::JSON`); err != nil {
					return fmt.Errorf("convert attribute JSON column: %w", err)
				}
			}
		}
		return nil
	})
}
