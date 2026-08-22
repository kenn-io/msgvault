package store

import (
	"context"
	"fmt"
)

// ensureDocumentVectorPublicationRecoverySchema removes source-side RESTRICT
// constraints from the durable token ledger. Extraction/chunk identity is an
// immutable snapshot used for liveness checks; it must outlive source-derived
// rows so backend cleanup can still enumerate the opaque token. The generation
// RESTRICT constraint remains the authority preventing premature ledger purge.
func (s *Store) ensureDocumentVectorPublicationRecoverySchema(ctx context.Context) error {
	return s.runOnceMigration(ctx, migrationDocumentVectorPublicationRecovery, false,
		func(ctx context.Context) error {
			return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
				if s.IsPostgreSQL() {
					return migratePostgresDocumentVectorPublicationRecovery(ctx, tx)
				}
				return migrateSQLiteDocumentVectorPublicationRecovery(ctx, tx)
			})
		})
}

func migratePostgresDocumentVectorPublicationRecovery(ctx context.Context, tx *loggedTx) error {
	if _, err := tx.ExecContext(ctx, `
		DO $$
		DECLARE source_constraint RECORD;
		BEGIN
			FOR source_constraint IN
				SELECT conname
				FROM pg_constraint
				WHERE conrelid = 'document_vector_publications'::regclass
				  AND confrelid IN ('document_extractions'::regclass, 'document_chunks'::regclass)
			LOOP
				EXECUTE format(
					'ALTER TABLE document_vector_publications DROP CONSTRAINT %I',
					source_constraint.conname
				);
			END LOOP;
		END
		$$`); err != nil {
		return fmt.Errorf("drop PostgreSQL document vector source constraints: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_document_vector_publications_cleanup`); err != nil {
		return fmt.Errorf("drop PostgreSQL document vector cleanup index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX idx_document_vector_publications_cleanup
		ON document_vector_publications(generation_id, backend_cleaned_at, token)`); err != nil {
		return fmt.Errorf("create PostgreSQL document vector cleanup index: %w", err)
	}
	return nil
}

func migrateSQLiteDocumentVectorPublicationRecovery(ctx context.Context, tx *loggedTx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_list('document_vector_publications')`)
	if err != nil {
		return fmt.Errorf("inspect SQLite document vector publication constraints: %w", err)
	}
	hasSourceConstraint := false
	for rows.Next() {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if err := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan SQLite document vector publication constraint: %w", err)
		}
		if table == "document_extractions" || table == "document_chunks" {
			hasSourceConstraint = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate SQLite document vector publication constraints: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close SQLite document vector publication constraints: %w", err)
	}
	if hasSourceConstraint {
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_document_vector_publications_cleanup`); err != nil {
			return fmt.Errorf("drop legacy SQLite document vector cleanup index: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE document_vector_publications
			RENAME TO document_vector_publications_source_fk_legacy`); err != nil {
			return fmt.Errorf("rename legacy SQLite document vector publications: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE document_vector_publications (
				generation_id INTEGER NOT NULL REFERENCES document_vector_generations(id) ON DELETE RESTRICT,
				extraction_id TEXT NOT NULL,
				extraction_profile_id TEXT NOT NULL,
				canonical_blob_hash TEXT NOT NULL CHECK (length(canonical_blob_hash) = 64),
				extraction_input_key TEXT NOT NULL,
				chunk_id INTEGER NOT NULL,
				chunk_key TEXT NOT NULL,
				chunk_checksum TEXT NOT NULL,
				source_sequence INTEGER NOT NULL,
				token TEXT NOT NULL UNIQUE,
				state TEXT NOT NULL CHECK (state IN ('pending', 'ready', 'failed')),
				lease_owner TEXT,
				lease_fence INTEGER NOT NULL DEFAULT 0,
				lease_until DATETIME,
				attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
				next_retry_at DATETIME,
				error_code TEXT,
				backend_cleaned_at DATETIME,
				created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (generation_id, extraction_id, chunk_id)
			)`); err != nil {
			return fmt.Errorf("create recoverable SQLite document vector publications: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO document_vector_publications (
				generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
				extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence,
				token, state, lease_owner, lease_fence, lease_until, attempt_count,
				next_retry_at, error_code, backend_cleaned_at, created_at, updated_at
			)
			SELECT generation_id, extraction_id, extraction_profile_id, canonical_blob_hash,
				extraction_input_key, chunk_id, chunk_key, chunk_checksum, source_sequence,
				token, state, lease_owner, lease_fence, lease_until, attempt_count,
				next_retry_at, error_code, backend_cleaned_at, created_at, updated_at
			FROM document_vector_publications_source_fk_legacy`); err != nil {
			return fmt.Errorf("copy SQLite document vector publications: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DROP TABLE document_vector_publications_source_fk_legacy`); err != nil {
			return fmt.Errorf("drop legacy SQLite document vector publications: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_document_vector_publications_cleanup`); err != nil {
		return fmt.Errorf("drop SQLite document vector cleanup index: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE INDEX idx_document_vector_publications_cleanup
		ON document_vector_publications(generation_id, backend_cleaned_at, token)`); err != nil {
		return fmt.Errorf("create SQLite document vector cleanup index: %w", err)
	}
	return nil
}
