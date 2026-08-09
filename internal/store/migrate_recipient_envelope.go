package store

import (
	"context"
	"fmt"
	"log/slog"
)

const migrationRecipientEnvelopeUnique = "message_recipients_envelope_unique_index"

type recipientOrphanCleanup struct {
	missingMessageRows     int64
	missingParticipantRows int64
}

func (c recipientOrphanCleanup) total() int64 {
	return c.missingMessageRows + c.missingParticipantRows
}

// ensureRecipientEnvelopeUniqueIndex relaxes message_recipients uniqueness
// from (message_id, participant_id, recipient_type) to that triple plus the
// normalized envelope address. Under the old table-level UNIQUE one
// participant could snapshot only a single envelope address per message and
// type, so two aliases of an already-merged participant in one To: header
// collapsed onto the first-seen address, and a participant merge had to
// delete colliding recipient rows outright — both silently destroying the
// immutable envelope evidence identity discovery classifies from. The new
// unique index keeps writer idempotency (one row per participant AND
// address) while letting distinct alias snapshots coexist.
//
// PostgreSQL drops the table-level constraint by catalog-discovered name.
// SQLite enforces a table-level UNIQUE through an undroppable
// sqlite_autoindex, so a legacy table is rebuilt: copy into a
// constraint-free twin, swap, and recreate the plain indexes the DROP TABLE
// took with it. The copy preserves ids, so mr.id-based ordering is stable
// across the rebuild. A fresh database — created by the current schema files,
// which no longer declare the table-level UNIQUE — has no autoindex and
// skips the rebuild; it only needs the unique index built here, because on
// upgraded databases email_address is a late ADD COLUMN that does not exist
// yet when the schema files run (so the index cannot live there), and this
// migration must therefore run after the legacy ADD COLUMN loop.
//
// Everything runs in one runMaintenance transaction: the copy and the
// unique-index build over the archive's largest-row-count table exceed the
// pool-wide 30s statement_timeout on PostgreSQL (finding S1), and a
// cancelled or failed run rolls back whole — the ledger entry is written
// only after success, so the next open retries.
func (s *Store) ensureRecipientEnvelopeUniqueIndex(ctx context.Context) error {
	return s.runOnceMigration(
		ctx, migrationRecipientEnvelopeUnique, false,
		func(ctx context.Context) error {
			var cleanup recipientOrphanCleanup
			if err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				if s.IsPostgreSQL() {
					if err := dropRecipientTableUniqueConstraintsPG(ctx, tx); err != nil {
						return err
					}
				} else {
					var err error
					cleanup, err = rebuildRecipientTableWithoutUniqueSQLite(ctx, tx)
					if err != nil {
						return err
					}
				}
				if _, err := tx.ExecContext(ctx, `
					CREATE UNIQUE INDEX IF NOT EXISTS idx_message_recipients_envelope
					    ON message_recipients(message_id, participant_id, recipient_type,
					                          LOWER(COALESCE(email_address, '')))
				`); err != nil {
					return fmt.Errorf("create idx_message_recipients_envelope: %w", err)
				}
				return nil
			}); err != nil {
				return err
			}
			if cleanup.total() > 0 {
				slog.Warn("removed dangling message recipients during schema migration",
					"table", "message_recipients",
					"missing_message_rows", cleanup.missingMessageRows,
					"missing_participant_rows", cleanup.missingParticipantRows,
				)
			}
			return nil
		},
	)
}

// dropRecipientTableUniqueConstraintsPG drops every UNIQUE constraint on
// message_recipients by its catalog name. Discovery instead of a hardcoded
// name: the default constraint name is derived (and 63-byte truncated) by
// the server, so trusting pg_constraint is what guarantees the drop matches
// whatever an existing archive actually carries. regclass resolves through
// search_path, consistent with every unqualified statement here.
func dropRecipientTableUniqueConstraintsPG(ctx context.Context, tx *loggedTx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT conname FROM pg_constraint
		WHERE conrelid = 'message_recipients'::regclass AND contype = 'u'
	`)
	if err != nil {
		return fmt.Errorf("list message_recipients unique constraints: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan message_recipients unique constraint: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate message_recipients unique constraints: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close message_recipients unique constraints: %w", err)
	}
	for _, name := range names {
		if _, err := tx.ExecContext(ctx,
			`ALTER TABLE message_recipients DROP CONSTRAINT `+quoteIdentifier(name),
		); err != nil {
			return fmt.Errorf("drop message_recipients constraint %q: %w", name, err)
		}
	}
	return nil
}

// rebuildRecipientTableWithoutUniqueSQLite swaps a legacy message_recipients
// table for one without the table-level UNIQUE. Detection is the presence of
// a sqlite_autoindex on the table: only a table-level UNIQUE creates one
// (INTEGER PRIMARY KEY does not), so its absence means the table already has
// the current shape and there is nothing to rebuild — robust against
// comments in the stored DDL, unlike matching sqlite_master.sql text.
//
// No trigger or view references message_recipients and no foreign key points
// at it, so DROP TABLE plus RENAME inside the transaction is a complete
// swap; only the two plain indexes the DROP took along need recreating (the
// unique index is built by the caller for the rebuilt and fresh paths
// alike). Before the FK-checked copy, dangling legacy rows are removed. Rows
// missing messages go first so a row missing both parents is counted once,
// under the missing-message category. The whole-table copy writes the table
// once through the WAL — a one-time upgrade cost on the order of the table's
// size.
func rebuildRecipientTableWithoutUniqueSQLite(
	ctx context.Context,
	tx *loggedTx,
) (recipientOrphanCleanup, error) {
	var hasTableUnique bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master
			WHERE type = 'index'
			  AND tbl_name = 'message_recipients'
			  AND name LIKE 'sqlite_autoindex%'
		)
	`).Scan(&hasTableUnique); err != nil {
		return recipientOrphanCleanup{}, fmt.Errorf(
			"check message_recipients table-level unique: %w", err,
		)
	}
	if !hasTableUnique {
		return recipientOrphanCleanup{}, nil
	}

	cleanup := recipientOrphanCleanup{}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM message_recipients
		WHERE NOT EXISTS (
			SELECT 1 FROM messages
			WHERE messages.id = message_recipients.message_id
		)
	`)
	if err != nil {
		return cleanup, fmt.Errorf("remove recipients missing messages: %w", err)
	}
	cleanup.missingMessageRows, err = result.RowsAffected()
	if err != nil {
		return cleanup, fmt.Errorf("count recipients missing messages: %w", err)
	}

	result, err = tx.ExecContext(ctx, `
		DELETE FROM message_recipients
		WHERE NOT EXISTS (
			SELECT 1 FROM participants
			WHERE participants.id = message_recipients.participant_id
		)
	`)
	if err != nil {
		return cleanup, fmt.Errorf("remove recipients missing participants: %w", err)
	}
	cleanup.missingParticipantRows, err = result.RowsAffected()
	if err != nil {
		return cleanup, fmt.Errorf("count recipients missing participants: %w", err)
	}

	for _, stmt := range []string{
		`DROP TABLE IF EXISTS message_recipients_new`,
		`CREATE TABLE message_recipients_new (
			id INTEGER PRIMARY KEY,
			message_id INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
			participant_id INTEGER NOT NULL REFERENCES participants(id) ON DELETE CASCADE,
			recipient_type TEXT NOT NULL,
			display_name TEXT,
			email_address TEXT
		)`,
		`INSERT INTO message_recipients_new
			(id, message_id, participant_id, recipient_type, display_name, email_address)
			SELECT id, message_id, participant_id, recipient_type, display_name, email_address
			FROM message_recipients`,
		`DROP TABLE message_recipients`,
		`ALTER TABLE message_recipients_new RENAME TO message_recipients`,
		`CREATE INDEX IF NOT EXISTS idx_message_recipients_message
			ON message_recipients(message_id)`,
		`CREATE INDEX IF NOT EXISTS idx_message_recipients_participant
			ON message_recipients(participant_id, recipient_type)`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return cleanup, fmt.Errorf(
				"rebuild message_recipients without table-level unique: %w", err,
			)
		}
	}
	return cleanup, nil
}
