package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

func (s *Store) migrateAttributeValueJSON(ctx context.Context) error {
	if !s.IsPostgreSQL() {
		return s.migrateAttributeValueJSONSQLite(ctx)
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

func (s *Store) migrateAttributeValueJSONSQLite(ctx context.Context) (err error) {
	if writableSnapshot(ctx, s.db.DB) != nil {
		return nil // Fresh archives already use the current schema.
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if _, err = conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`)
		err = errors.Join(err, restoreErr)
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return err
	}
	for _, table := range []string{"person_attribute_values", "organization_attribute_values"} {
		var definition string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&definition); err != nil {
			return fmt.Errorf("inspect %s: %w", table, err)
		}
		if strings.Contains(definition, "value_json        TEXT") {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND tbl_name = ? AND sql IS NOT NULL`, table)
		if err != nil {
			return err
		}
		var indexes []string
		for rows.Next() {
			var sql string
			if err := rows.Scan(&sql); err != nil {
				_ = rows.Close()
				return err
			}
			indexes = append(indexes, sql)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		_ = rows.Close()
		prefix := "CREATE TABLE IF NOT EXISTS " + table + " ("
		start := strings.Index(string(schema), prefix)
		if start < 0 {
			return fmt.Errorf("missing schema for %s", table)
		}
		definition = string(schema[start:])
		end := strings.Index(definition, "\n);")
		if end < 0 {
			return fmt.Errorf("incomplete schema for %s", table)
		}
		temp := table + "_json_upgrade"
		definition = strings.Replace(definition[:end+3], prefix, "CREATE TABLE "+temp+" (", 1)
		for _, statement := range []string{definition, `INSERT INTO ` + temp + ` SELECT * FROM ` + table, `UPDATE ` + temp + ` SET value_json = CAST(value_json AS TEXT) WHERE value_json IS NOT NULL`, `DROP TABLE ` + table, `ALTER TABLE ` + temp + ` RENAME TO ` + table} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("rebuild %s: %w", table, err)
			}
		}
		for _, index := range indexes {
			if _, err := tx.ExecContext(ctx, index); err != nil {
				return fmt.Errorf("restore %s index: %w", table, err)
			}
		}
	}
	var violations int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check('person_merge_review_candidates') WHERE parent = 'person_attribute_values'`).Scan(&violations); err != nil {
		return err
	}
	if violations != 0 {
		return errors.New("attribute JSON migration left dangling references")
	}
	return tx.Commit()
}
