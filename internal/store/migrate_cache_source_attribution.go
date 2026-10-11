package store

import (
	"context"
	"fmt"
)

// Install after legacy columns exist. Source attribution can change the cached
// owner even when the effective is_from_me flag stays true.
func (s *Store) ensureCacheSourceAttribution(ctx context.Context) error {
	if s.IsPostgreSQL() {
		return nil
	}
	return s.runOnceMigration(ctx, "cache_message_source_attribution", 2, false,
		func(ctx context.Context) error {
			return s.withTxContext(ctx, func(tx *loggedTx) error {
				_, err := tx.ExecContext(ctx, `
					DROP TRIGGER IF EXISTS trg_cache_message_facts_update;
					CREATE TRIGGER trg_cache_message_facts_update
					AFTER UPDATE OF sender_id, is_from_me, source_is_from_me,
						has_attachments, attachment_count, account_address, account_path
						ON messages FOR EACH ROW
					WHEN OLD.sender_id IS NOT NEW.sender_id OR OLD.is_from_me IS NOT NEW.is_from_me
						OR OLD.source_is_from_me IS NOT NEW.source_is_from_me
						OR OLD.has_attachments IS NOT NEW.has_attachments
						OR OLD.attachment_count IS NOT NEW.attachment_count
						OR OLD.account_address IS NOT NEW.account_address
						OR OLD.account_path IS NOT NEW.account_path BEGIN
						INSERT INTO cache_related_change_journal (dataset, message_id)
						VALUES ('message_facts', NEW.id);
					END;
					-- Repair cached facts for edits the previous trigger missed.
					-- Message ID zero invalidates facts across the publication.
					INSERT INTO cache_related_change_journal (dataset, message_id)
					VALUES ('message_facts', 0);
				`)
				if err != nil {
					return fmt.Errorf("install source-attribution cache journal: %w", err)
				}
				return nil
			})
		})
}
