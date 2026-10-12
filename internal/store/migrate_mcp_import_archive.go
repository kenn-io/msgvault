package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const mcpSubscriptionScopeConstraint = "mcp_event_subscriptions_scope_identity"
const mcpSubscriptionScopeCheck = `CHECK ((name='msgvault.import_message_archived' AND scope_kind='archive' AND scope_id=0 AND source_id IS NULL)
    OR (scope_kind<>'archive' AND scope_id>0 AND source_id>0 AND source_id IS NOT NULL))`

// Only archive-wide import subscriptions lack a single source. Existing
// subscriptions, encrypted callback secrets, pending bytes and resource
// bindings keep their original identities throughout this upgrade.
func (s *Store) migrateMCPImportArchiveSubscriptions(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
			for _, statement := range []string{
				`ALTER TABLE mcp_event_subscriptions ALTER COLUMN source_id DROP NOT NULL`,
				`ALTER TABLE mcp_event_subscriptions DROP CONSTRAINT IF EXISTS ` + mcpSubscriptionScopeConstraint,
				`ALTER TABLE mcp_event_subscriptions ADD CONSTRAINT ` + mcpSubscriptionScopeConstraint + ` ` + mcpSubscriptionScopeCheck,
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					return fmt.Errorf("upgrade import subscription source binding: %w", err)
				}
			}
			return nil
		})
	}
	return s.migrateMCPImportArchiveSubscriptionsSQLite(ctx)
}

func (s *Store) migrateMCPImportArchiveSubscriptionsSQLite(ctx context.Context) (err error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	var definition string
	if err := conn.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='mcp_event_subscriptions'`).Scan(&definition); err != nil {
		return err
	}
	// Fresh schemas already carry the invariant; ledger resets are harmless.
	if strings.Contains(definition, mcpSubscriptionScopeConstraint) {
		return nil
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer func() {
		_, restoreErr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys=ON`)
		err = errors.Join(err, restoreErr)
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.mcpIdentityFence(ctx, tx); err != nil {
		return err
	}

	// SQLite DROP TABLE otherwise cascades into resource bindings. Save every
	// explicit index and trigger, rebuild on this pinned connection, then
	// validate references before committing and restoring enforcement.
	rows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE tbl_name='mcp_event_subscriptions' AND type IN ('index','trigger') AND sql IS NOT NULL ORDER BY type,name`)
	if err != nil {
		return err
	}
	var dependentDDL []string
	if err := func() (scanErr error) {
		defer func() { scanErr = errors.Join(scanErr, rows.Close()) }()
		for rows.Next() {
			var statement string
			if err := rows.Scan(&statement); err != nil {
				return err
			}
			dependentDDL = append(dependentDDL, statement)
		}
		return rows.Err()
	}(); err != nil {
		return err
	}
	schema, err := schemaFS.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read import subscription schema: %w", err)
	}
	_, table, found := strings.Cut(string(schema), "CREATE TABLE IF NOT EXISTS mcp_event_subscriptions (")
	if !found {
		return errors.New("import subscription schema is missing")
	}
	body, _, found := strings.Cut(table, ";")
	if !found {
		return errors.New("import subscription schema is incomplete")
	}
	statements := []string{
		`CREATE TABLE mcp_subscriptions_import_upgrade (` + body + `;`,
		`INSERT INTO mcp_subscriptions_import_upgrade (` + mcpSubscriptionColumns + `) SELECT ` + mcpSubscriptionColumns + ` FROM mcp_event_subscriptions`,
		`DROP TABLE mcp_event_subscriptions`,
		`ALTER TABLE mcp_subscriptions_import_upgrade RENAME TO mcp_event_subscriptions`,
	}
	statements = append(statements, dependentDDL...)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild import subscription source binding: %w", err)
		}
	}
	checks, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	invalid := false
	if err := func() (scanErr error) {
		defer func() { scanErr = errors.Join(scanErr, checks.Close()) }()
		invalid = checks.Next()
		return checks.Err()
	}(); err != nil {
		return err
	}
	if invalid {
		return errors.New("import subscription upgrade left dangling references")
	}
	return tx.Commit()
}
