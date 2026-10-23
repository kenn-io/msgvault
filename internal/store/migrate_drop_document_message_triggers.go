package store

import (
	"context"
	"fmt"
	"log/slog"
)

// retiredDocumentTables lists the retired pipeline's tables, children first.
// The upgrade empties them so extracted text cannot outlive its message; the
// vector rows go first because they restrict profile deletion.
var retiredDocumentTables = []string{
	"document_vector_publications",
	"document_vector_build_progress",
	"document_vector_generations",
	"document_vector_consents",
	"document_chunk_spans",
	"document_chunks",
	"document_units",
	"document_extraction_heads",
	"document_extraction_claims",
	"document_extraction_conversions",
	"document_extractions",
	"document_extraction_rebuild_targets",
	"document_extraction_rebuilds",
	"document_provider_consents",
	"document_extraction_profiles",
	"document_occurrences",
}

func (s *Store) dropDocumentMessageTriggers(ctx context.Context) error {
	var deletedRows int64
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		deletedRows = 0
		statement := "DROP TRIGGER IF EXISTS trg_person_sweep_documents_message_update"
		if s.IsPostgreSQL() {
			statement = "DROP FUNCTION IF EXISTS msgvault_append_person_sweep_document_changes(BIGINT, BIGINT, BIGINT, TEXT, TEXT)"
		}
		statements := []string{
			statement,
			"DELETE FROM attachment_change_consumers WHERE consumer_key = 'document-index/v1'",
			"DELETE FROM person_sweep_cursors WHERE source_lane = 'document_text'",
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("drop document message triggers: %w", err)
			}
		}
		for _, table := range retiredDocumentTables {
			result, err := tx.ExecContext(ctx, "DELETE FROM "+table)
			if err != nil {
				return fmt.Errorf("delete retired document rows from %s: %w", table, err)
			}
			count, err := result.RowsAffected()
			if err != nil {
				return fmt.Errorf("count retired document rows from %s: %w", table, err)
			}
			deletedRows += count
		}
		return pruneAttachmentChanges(boundQuerier{ctx: ctx, q: tx})
	})
	if err != nil {
		return err
	}
	slog.Info("deleted retired document rows during schema migration", "rows", deletedRows)
	return nil
}
