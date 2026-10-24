package store

import (
	"context"
	"fmt"
	"log/slog"
)

// retiredDocumentTables lists the retired pipeline's tables, children first
// because the vector rows restrict profile deletion.
var retiredDocumentTables = []string{
	"document_vector_publications",
	"document_vector_build_progress",
	"document_vector_generations",
	"document_vector_consents",
	"document_vector_provider_usage",
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
	"document_index_state",
	"document_extraction_runs",
	"document_embedding_runs",
}

var retiredDocumentCursorColumns = []string{
	"optimistic_document_key", "reconcile_document_key", "backstop_document_key",
}

func (s *Store) dropDocumentIndex(ctx context.Context) error {
	postgres := s.IsPostgreSQL()
	err := s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		statements := []string{
			"DROP TRIGGER IF EXISTS trg_person_sweep_documents_message_update",
			"DROP TRIGGER IF EXISTS trg_document_message_type_revision",
			"DROP TABLE IF EXISTS document_chunks_fts",
		}
		columnExists := `SELECT COUNT(*) FROM pragma_table_info('person_sweep_cursors') WHERE name = ?`
		if postgres {
			statements = []string{
				"DROP FUNCTION IF EXISTS msgvault_append_person_sweep_document_changes(BIGINT, BIGINT, BIGINT, TEXT, TEXT)",
				"DROP TRIGGER IF EXISTS trg_document_message_type_revision ON messages",
			}
		}
		present := func(query, name string) (bool, error) {
			var n int
			err := tx.QueryRowContext(ctx, query, name).Scan(&n)
			return n > 0, err
		}
		statements = append(statements,
			"DELETE FROM attachment_change_consumers WHERE consumer_key = 'document-index/v1'",
			"DELETE FROM person_sweep_cursors WHERE source_lane = 'document_text'",
		)
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("drop document index: %w", err)
			}
		}
		for _, table := range retiredDocumentTables {
			if _, err := tx.ExecContext(ctx, "DROP TABLE IF EXISTS "+table); err != nil {
				return fmt.Errorf("drop retired document table %s: %w", table, err)
			}
		}
		if postgres {
			for _, function := range []string{
				"bump_document_index_revision_after_occurrence_delete",
				"bump_document_index_revision_after_message_type_update",
			} {
				if _, err := tx.ExecContext(ctx, "DROP FUNCTION IF EXISTS "+function+"()"); err != nil {
					return fmt.Errorf("drop retired document function %s: %w", function, err)
				}
			}
		}
		for _, column := range retiredDocumentCursorColumns {
			drop := "ALTER TABLE person_sweep_cursors DROP COLUMN "
			if postgres {
				drop += "IF EXISTS "
			} else {
				exists, err := present(columnExists, column)
				if err != nil {
					return fmt.Errorf("find retired cursor column %s: %w", column, err)
				}
				if !exists {
					continue
				}
			}
			if _, err := tx.ExecContext(ctx, drop+column); err != nil {
				return fmt.Errorf("drop retired cursor column %s: %w", column, err)
			}
		}
		return pruneAttachmentChanges(boundQuerier{ctx: ctx, q: tx})
	})
	if err != nil {
		return err
	}
	slog.Info("deleted retired document rows during schema migration")
	return nil
}
