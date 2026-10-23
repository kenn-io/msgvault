package store

import (
	"context"
	"fmt"
)

func (s *Store) dropDocumentMessageTriggers(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		statement := "DROP TRIGGER IF EXISTS trg_person_sweep_documents_message_update"
		if s.IsPostgreSQL() {
			statement = "DROP FUNCTION IF EXISTS msgvault_append_person_sweep_document_changes(BIGINT, BIGINT, BIGINT, TEXT, TEXT)"
		}
		for _, statement := range []string{
			statement,
			"DELETE FROM attachment_change_consumers WHERE consumer_key = 'document-index/v1'",
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("drop document message triggers: %w", err)
			}
		}
		return pruneAttachmentChanges(boundQuerier{ctx: ctx, q: tx})
	})
}
