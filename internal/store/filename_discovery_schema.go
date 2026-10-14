package store

import (
	"context"
	"fmt"
)

// ensureFilenameDiscoveryInfrastructure creates only empty infrastructure.
// Maintenance registers its journal consumer and starts a finite generation;
// schema setup never reads attachments or builds filename postings.
func (s *Store) ensureFilenameDiscoveryInfrastructure(ctx context.Context) error {
	boolean, collation := "INTEGER", "BINARY"
	if s.IsPostgreSQL() {
		boolean, collation = "BOOLEAN", `"C"`
	}
	statements := []string{
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS filename_discovery_state (
			singleton BIGINT NOT NULL PRIMARY KEY DEFAULT 1 CHECK (singleton = 1),
			generation BIGINT NOT NULL DEFAULT 0,
			revision BIGINT NOT NULL DEFAULT 0,
			phase TEXT NOT NULL DEFAULT 'bootstrap' CHECK (phase IN ('bootstrap', 'catchup', 'ready')),
			scan_after BIGINT NOT NULL DEFAULT 0,
			scan_end BIGINT NOT NULL DEFAULT 0,
			baseline BIGINT NOT NULL DEFAULT 0,
			catchup_fence BIGINT NOT NULL DEFAULT 0,
			last_sequence BIGINT NOT NULL DEFAULT 0,
			skipped BIGINT NOT NULL DEFAULT 0,
			gc_turn %s NOT NULL DEFAULT FALSE
		)`, boolean),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS filename_discovery_entries (
			generation BIGINT NOT NULL,
			attachment_id BIGINT NOT NULL,
			normalized_name TEXT NOT NULL DEFAULT '',
			name_hash TEXT COLLATE %s NOT NULL DEFAULT '',
			applied_sequence BIGINT NOT NULL DEFAULT 0,
			skipped %s NOT NULL DEFAULT FALSE,
			PRIMARY KEY (generation, attachment_id)
		)`, collation, boolean),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS filename_discovery_tokens (
			generation BIGINT NOT NULL,
			token_key TEXT COLLATE %s NOT NULL,
			attachment_id BIGINT NOT NULL,
			PRIMARY KEY (generation, token_key, attachment_id),
			FOREIGN KEY (generation, attachment_id)
				REFERENCES filename_discovery_entries(generation, attachment_id) ON DELETE CASCADE
		)`, collation),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS filename_discovery_deletes (
			generation BIGINT NOT NULL,
			delete_key TEXT COLLATE %s NOT NULL,
			token_key TEXT COLLATE %s NOT NULL,
			attachment_id BIGINT NOT NULL,
			PRIMARY KEY (generation, delete_key, token_key, attachment_id),
			FOREIGN KEY (generation, attachment_id)
				REFERENCES filename_discovery_entries(generation, attachment_id) ON DELETE CASCADE
		)`, collation, collation),
		`CREATE INDEX IF NOT EXISTS idx_filename_discovery_name
			ON filename_discovery_entries(generation, name_hash, attachment_id)`,
		// The entries primary key supplies bounded generation cleanup. Child
		// indexes keep its cascades and per-occurrence reindexing point-scoped.
		`CREATE INDEX IF NOT EXISTS idx_filename_discovery_tokens_entry
			ON filename_discovery_tokens(generation, attachment_id)`,
		`CREATE INDEX IF NOT EXISTS idx_filename_discovery_deletes_entry
			ON filename_discovery_deletes(generation, attachment_id)`,
		`INSERT INTO filename_discovery_state(singleton) VALUES (1)
			ON CONFLICT (singleton) DO NOTHING`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create filename discovery infrastructure: %w", err)
		}
	}
	return nil
}
