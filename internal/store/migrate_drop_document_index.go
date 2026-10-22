package store

import (
	"context"
	"fmt"
)

func (s *Store) dropDocumentIndex(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		statements := []string{
			"DROP TRIGGER IF EXISTS trg_person_sweep_documents_message_update",
			"DROP TRIGGER IF EXISTS trg_document_extraction_runs_terminal_immutable",
			"DROP TRIGGER IF EXISTS trg_document_embedding_runs_terminal_immutable",
			"DROP TRIGGER IF EXISTS trg_operation_history_document_extraction_insert",
			"DROP TRIGGER IF EXISTS trg_operation_history_document_extraction_delete",
			"DROP TRIGGER IF EXISTS trg_operation_history_document_extraction_update",
			"DROP TRIGGER IF EXISTS trg_operation_history_document_embedding_insert",
			"DROP TRIGGER IF EXISTS trg_operation_history_document_embedding_delete",
			"DROP TRIGGER IF EXISTS trg_operation_history_document_embedding_update",
			"DROP TRIGGER IF EXISTS trg_document_occurrence_delete_revision",
			"DROP TRIGGER IF EXISTS trg_document_message_type_revision",
			"DROP TRIGGER IF EXISTS trg_document_chunks_fts_insert",
			"DROP TRIGGER IF EXISTS trg_document_chunks_fts_delete",
			"DROP TRIGGER IF EXISTS trg_document_chunks_fts_update",
		}
		if s.IsPostgreSQL() {
			statements = []string{
				"DROP TRIGGER IF EXISTS trg_document_message_type_revision ON messages",
			}
		}
		statements = append(statements,
			"DROP TABLE IF EXISTS document_chunks_fts",
			"DROP TABLE IF EXISTS document_vector_publications",
			"DROP TABLE IF EXISTS document_vector_build_progress",
			"DROP TABLE IF EXISTS document_vector_provider_usage",
			"DROP TABLE IF EXISTS document_vector_consents",
			"DROP TABLE IF EXISTS document_vector_generations",
			"DROP TABLE IF EXISTS document_index_state",
			"DROP TABLE IF EXISTS document_occurrences",
			"DROP TABLE IF EXISTS document_chunk_spans",
			"DROP TABLE IF EXISTS document_chunks",
			"DROP TABLE IF EXISTS document_units",
			"DROP TABLE IF EXISTS document_extraction_heads",
			"DROP TABLE IF EXISTS document_extraction_claims",
			"DROP TABLE IF EXISTS document_extraction_conversions",
			"DROP TABLE IF EXISTS document_extractions",
			"DROP TABLE IF EXISTS document_extraction_rebuild_targets",
			"DROP TABLE IF EXISTS document_extraction_rebuilds",
			"DROP TABLE IF EXISTS document_provider_consents",
			"DROP TABLE IF EXISTS document_extraction_profiles",
			"DROP TABLE IF EXISTS document_embedding_runs",
			"DROP TABLE IF EXISTS document_extraction_runs",
		)
		if s.IsPostgreSQL() {
			statements = append(statements,
				"DROP FUNCTION IF EXISTS msgvault_append_person_sweep_document_changes(BIGINT, BIGINT, BIGINT, TEXT, TEXT)",
				"DROP FUNCTION IF EXISTS bump_document_index_revision_after_occurrence_delete()",
				"DROP FUNCTION IF EXISTS bump_document_index_revision_after_message_type_update()",
			)
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("drop document index: %w", err)
			}
		}
		return nil
	})
}
