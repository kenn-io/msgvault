package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
)

// prepareCardDAVSyncRunAccountColumn runs before the schema scripts. PostgreSQL
// resolves index expressions even for CREATE INDEX IF NOT EXISTS, so an old
// sync table needs its account column before the current index can be parsed.
func (s *Store) prepareCardDAVSyncRunAccountColumn(ctx context.Context) error {
	query := `SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'carddav_sync_runs')
  AND NOT EXISTS (SELECT 1 FROM pragma_table_info('carddav_sync_runs') WHERE name = 'account_id')`
	column := `ALTER TABLE carddav_sync_runs ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1 CHECK (account_id > 0)`
	if s.IsPostgreSQL() {
		query = `SELECT to_regclass('carddav_sync_runs') IS NOT NULL AND NOT EXISTS (
   SELECT 1 FROM information_schema.columns WHERE table_schema = current_schema()
    AND table_name = 'carddav_sync_runs' AND column_name = 'account_id')`
		column = `ALTER TABLE carddav_sync_runs ADD COLUMN IF NOT EXISTS account_id BIGINT NOT NULL DEFAULT 1 CHECK (account_id > 0)`
	}
	var missing bool
	if err := s.db.QueryRowContext(ctx, query).Scan(&missing); err != nil {
		return fmt.Errorf("inspect CardDAV history account column: %w", err)
	}
	if missing {
		if _, err := s.db.ExecContext(ctx, column); err != nil && !s.dialect.IsDuplicateColumnError(err) {
			return fmt.Errorf("add CardDAV history account column: %w", err)
		}
	}
	return nil
}

// ensureCardDAVMultiAccountSchema preserves the original account and all book
// ownership while removing the single-account constraint. Sync history uses a
// logical account ID without a foreign key: historical runs can predate discovery.
func (s *Store) ensureCardDAVMultiAccountSchema(ctx context.Context) error {
	if s.IsPostgreSQL() {
		if err := s.upgradeCardDAVAccountsPostgreSQL(ctx); err != nil {
			return err
		}
	} else if err := s.upgradeCardDAVAccountsSQLite(ctx); err != nil {
		return err
	}
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for _, statement := range []string{
			`DROP INDEX IF EXISTS idx_carddav_one_write_target`,
			`CREATE UNIQUE INDEX idx_carddav_one_write_target ON carddav_address_books((1)) WHERE is_write_target = TRUE`,
			`DROP INDEX IF EXISTS idx_carddav_sync_runs_one_active`,
			`CREATE UNIQUE INDEX idx_carddav_sync_runs_one_active ON carddav_sync_runs(account_id) WHERE state = 'running'`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("enforce CardDAV account invariants: %w", err)
			}
		}
		return nil
	})
}

func (s *Store) upgradeCardDAVAccountsPostgreSQL(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		// Keep each constraint's name and actions while widening the account key columns.
		type foreignKeyConstraint struct {
			schema     string
			table      string
			name       string
			definition string
		}
		rows, err := tx.QueryContext(ctx, `SELECT ns.nspname, rel.relname, con.conname, pg_get_constraintdef(con.oid)
   FROM pg_constraint con
   JOIN pg_class rel ON rel.oid = con.conrelid
   JOIN pg_namespace ns ON ns.oid = rel.relnamespace
   WHERE con.contype = 'f' AND con.confrelid = 'carddav_accounts'::regclass
    AND ns.nspname = current_schema()
   ORDER BY ns.nspname, rel.relname, con.conname`)
		if err != nil {
			return fmt.Errorf("inspect CardDAV account foreign keys: %w", err)
		}
		var foreignKeys []foreignKeyConstraint
		for rows.Next() {
			var foreignKey foreignKeyConstraint
			if err := rows.Scan(&foreignKey.schema, &foreignKey.table, &foreignKey.name, &foreignKey.definition); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan CardDAV account foreign key: %w", err)
			}
			foreignKeys = append(foreignKeys, foreignKey)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return fmt.Errorf("read CardDAV account foreign keys: %w", err)
		}
		for _, foreignKey := range foreignKeys {
			table := quoteIdentifier(foreignKey.schema) + "." + quoteIdentifier(foreignKey.table)
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` DROP CONSTRAINT `+quoteIdentifier(foreignKey.name)); err != nil {
				return fmt.Errorf("remove CardDAV account foreign key %s.%s: %w", foreignKey.table, foreignKey.name, err)
			}
		}

		// Only remove the old id=1 check, leaving every other account invariant.
		// The constraint name can differ in an existing archive.
		rows, err = tx.QueryContext(ctx, `SELECT conname FROM pg_constraint
   WHERE conrelid = 'carddav_accounts'::regclass AND contype = 'c'
    AND pg_get_constraintdef(oid) ~ 'id[[:space:]]*=[[:space:]]*1\)'`)
		if err != nil {
			return fmt.Errorf("inspect CardDAV singleton constraint: %w", err)
		}
		var names []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close()
				return err
			}
			names = append(names, name)
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE carddav_accounts DROP CONSTRAINT `+quoteIdentifier(name)); err != nil {
				return fmt.Errorf("remove CardDAV singleton constraint: %w", err)
			}
		}
		for _, statement := range []string{
			`ALTER TABLE carddav_accounts ALTER COLUMN id TYPE BIGINT`,
			`DO $migration$ BEGIN
    ALTER TABLE carddav_accounts ADD CONSTRAINT carddav_accounts_id_positive CHECK (id > 0);
   EXCEPTION WHEN duplicate_object THEN NULL;
   END; $migration$`,
			`ALTER TABLE carddav_account_home_urls ALTER COLUMN account_id TYPE BIGINT`,
			`ALTER TABLE carddav_retry_gate ALTER COLUMN account_id TYPE BIGINT`,
			`ALTER TABLE carddav_address_books ALTER COLUMN account_id TYPE BIGINT`,
			`ALTER TABLE carddav_address_book_urls ALTER COLUMN account_id TYPE BIGINT`,
			`ALTER TABLE carddav_accounts ADD COLUMN IF NOT EXISTS connection_name TEXT NOT NULL DEFAULT 'default'`,
			`CREATE UNIQUE INDEX IF NOT EXISTS idx_carddav_account_connection_name ON carddav_accounts(connection_name)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("upgrade CardDAV accounts: %w", err)
			}
		}
		for _, foreignKey := range foreignKeys {
			table := quoteIdentifier(foreignKey.schema) + "." + quoteIdentifier(foreignKey.table)
			statement := `ALTER TABLE ` + table + ` ADD CONSTRAINT ` + quoteIdentifier(foreignKey.name) + ` ` + foreignKey.definition
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("restore CardDAV account foreign key %s.%s: %w", foreignKey.table, foreignKey.name, err)
			}
		}
		return nil
	})
}

func (s *Store) upgradeCardDAVAccountsSQLite(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire CardDAV account upgrade connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	var definition string
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'carddav_accounts'`).Scan(&definition); err != nil {
		return err
	}
	if strings.Contains(definition, "connection_name") {
		return nil
	}
	// DROP TABLE implicitly deletes parent rows. Disable cascading foreign keys
	// on this pinned connection before entering the rebuild transaction, then
	// restore enforcement before the connection returns to the pool.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
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
	// Use the current production definition rather than a second schema copy.
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read CardDAV account schema: %w", err)
	}
	start := bytes.Index(schema, []byte("CREATE TABLE IF NOT EXISTS carddav_accounts ("))
	if start < 0 {
		return errors.New("CardDAV account schema definition is missing")
	}
	accountSQL := string(schema[start:])
	end := strings.Index(accountSQL, ";")
	if end < 0 {
		return errors.New("CardDAV account schema definition is incomplete")
	}
	accountSQL = strings.Replace(accountSQL[:end+1], "carddav_accounts (", "carddav_accounts_multi_upgrade (", 1)
	for _, statement := range []string{
		`DROP TABLE IF EXISTS carddav_accounts_multi_upgrade`, accountSQL,
		`INSERT INTO carddav_accounts_multi_upgrade
   (id, connection_name, base_url, username, principal_url, home_url, connection_generation,
    discovery_revision, discovered_at, created_at, updated_at)
   SELECT id, 'default', base_url, username, principal_url, home_url, connection_generation,
    discovery_revision, discovered_at, created_at, updated_at FROM carddav_accounts`,
		`DROP TABLE carddav_accounts`,
		`ALTER TABLE carddav_accounts_multi_upgrade RENAME TO carddav_accounts`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild CardDAV accounts: %w", err)
		}
	}
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	invalid := rows.Next()
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if invalid {
		return errors.New("CardDAV account upgrade left dangling references")
	}
	return tx.Commit()
}
