package store

import (
	"context"
	"fmt"
)

func (s *Store) dropDocumentMessageTriggers(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		statements := []string{
			"DROP TRIGGER IF EXISTS trg_person_sweep_documents_message_update",
			"DROP TRIGGER IF EXISTS trg_document_message_type_revision",
		}
		if s.IsPostgreSQL() {
			statements = []string{
				"DROP TRIGGER IF EXISTS trg_document_message_type_revision ON messages",
				"DROP FUNCTION IF EXISTS msgvault_append_person_sweep_document_changes(BIGINT, BIGINT, BIGINT, TEXT, TEXT)",
				"DROP FUNCTION IF EXISTS bump_document_index_revision_after_message_type_update()",
			}
		}
		statements = append(statements, "DELETE FROM attachment_change_consumers WHERE consumer_key = 'document-index/v1'")
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("drop document message triggers: %w", err)
			}
		}
		return pruneAttachmentChanges(boundQuerier{ctx: ctx, q: tx})
	})
}
