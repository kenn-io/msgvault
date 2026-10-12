package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// These indexes deliberately exclude bodies and keep fields separate. Folding
// before case-sensitive tokenization uses the same Unicode rules as metadata
// LIKE predicates; the query engine still verifies every candidate with LIKE.
var metadataFTSIndexes = []struct {
	table, index string
	columns      []string
}{
	{"messages", "messages_metadata_fts", []string{"subject", "snippet"}},
	{"participants", "participants_metadata_fts", []string{"email_address", "display_name", "phone_number"}},
	{"message_recipients", "recipients_metadata_fts", []string{"display_name"}},
}

// ensureMetadataFTS publishes readiness only after a complete backfill and
// trigger installation in one transaction. Missing objects force a rebuild.
// A no-FTS setup removes the triggers without opening unloaded virtual tables;
// a later capable setup rebuilds all writes made while indexing was disabled.
func (s *Store) ensureMetadataFTS(ctx context.Context, available bool) error {
	if s.IsPostgreSQL() {
		return nil
	}
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		var version string
		err := tx.QueryRowContext(ctx, `SELECT value FROM archive_metadata WHERE key='metadata_fts_version'`).Scan(&version)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ready := available && version == "1"
		for _, idx := range metadataFTSIndexes {
			for _, name := range []string{idx.index, idx.index + "_insert", idx.index + "_update", idx.index + "_delete"} {
				var count int
				if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name).Scan(&count); err != nil {
					return err
				}
				ready = ready && count == 1
			}
		}
		if ready {
			for _, idx := range metadataFTSIndexes {
				var id int64
				err := tx.QueryRowContext(ctx, "SELECT rowid FROM "+idx.index+" WHERE "+idx.index+` MATCH '"msgvault-index-probe"' LIMIT 1`).Scan(&id)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("probe %s: %w", idx.index, err)
				}
			}
			return nil
		}
		for _, idx := range metadataFTSIndexes {
			for _, event := range []string{"insert", "update", "delete"} {
				if _, err := tx.ExecContext(ctx, "DROP TRIGGER IF EXISTS "+idx.index+"_"+event); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key='metadata_fts_version'`); err != nil {
			return err
		}
		if !available {
			return nil
		}
		for _, idx := range metadataFTSIndexes {
			if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+idx.index); err != nil {
				return err
			}
			columns := strings.Join(idx.columns, ", ")
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(`CREATE VIRTUAL TABLE %s USING fts5(%s, tokenize='trigram case_sensitive 1', detail=full)`, idx.index, columns)); err != nil {
				return err
			}
			folded := func(prefix string) string {
				values := make([]string, len(idx.columns))
				for i, col := range idx.columns {
					values[i] = "msgvault_unicode_lower(COALESCE(" + prefix + col + ", ''))"
				}
				return strings.Join(values, ", ")
			}
			insert := func(prefix string) string {
				return fmt.Sprintf("INSERT INTO %s(rowid, %s) VALUES(%sid, %s);", idx.index, columns, prefix, folded(prefix))
			}
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s(rowid, %s) SELECT id, %s FROM %s", idx.index, columns, folded(""), idx.table)); err != nil {
				return err
			}
			changed := []string{"old.id IS NOT new.id"}
			for _, col := range idx.columns {
				changed = append(changed, "old."+col+" IS NOT new."+col)
			}
			triggerSQL := fmt.Sprintf(`
    CREATE TRIGGER %s_insert AFTER INSERT ON %s BEGIN %s END;
    CREATE TRIGGER %s_delete AFTER DELETE ON %s BEGIN DELETE FROM %s WHERE rowid=old.id; END;
    CREATE TRIGGER %s_update AFTER UPDATE OF id, %s ON %s
    WHEN %s BEGIN DELETE FROM %s WHERE rowid=old.id; %s END;
   `, idx.index, idx.table, insert("new."), idx.index, idx.table, idx.index,
				idx.index, columns, idx.table, strings.Join(changed, " OR "), idx.index, insert("new."))
			if _, err := tx.ExecContext(ctx, triggerSQL); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_metadata(key,value) VALUES('metadata_fts_version','1')`)
		return err
	})
}
