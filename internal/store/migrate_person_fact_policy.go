package store

import (
	"context"
	"fmt"
	"strings"
)

const personFactPolicyStatusCheck = `CHECK (reason IN ('source-deleted', 'source-edited', 'scope-unlinked', 'identity-reassigned', 'source-reimported', 'scope-relinked', 'conversation-cold', 'conversation-full'))`

func (s *Store) migratePersonFactConversationPolicyStatus(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if s.IsPostgreSQL() {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_evidence_status_events DROP CONSTRAINT IF EXISTS person_fact_evidence_status_events_reason_check`); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `ALTER TABLE person_fact_evidence_status_events ADD CONSTRAINT person_fact_evidence_status_events_reason_check `+personFactPolicyStatusCheck)
			return err
		}
		var ddl string
		if err := tx.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='person_fact_evidence_status_events'`).Scan(&ddl); err != nil {
			return err
		}
		if strings.Contains(ddl, "'conversation-cold'") {
			return nil
		}
		// This append-only leaf has no incoming foreign keys. Rebuild only this
		// table, retaining every event ID and the existing lookup index.
		for _, statement := range []string{
			`CREATE TABLE person_fact_evidence_status_policy (id INTEGER PRIMARY KEY,person_id INTEGER NOT NULL REFERENCES persons(id) ON DELETE CASCADE,generation_id INTEGER NOT NULL REFERENCES person_fact_generations(id) ON DELETE CASCADE,evidence_id INTEGER NOT NULL REFERENCES person_fact_evidence(id) ON DELETE CASCADE,evidence_key TEXT NOT NULL,source_version TEXT NOT NULL,supported INTEGER NOT NULL CHECK(supported IN(0,1)),reason TEXT NOT NULL ` + personFactPolicyStatusCheck + `,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,UNIQUE(person_id,generation_id,evidence_key,source_version))`,
			`INSERT INTO person_fact_evidence_status_policy SELECT * FROM person_fact_evidence_status_events`,
			`DROP TABLE person_fact_evidence_status_events`,
			`ALTER TABLE person_fact_evidence_status_policy RENAME TO person_fact_evidence_status_events`,
			`CREATE INDEX idx_person_fact_evidence_status_latest ON person_fact_evidence_status_events(person_id,evidence_key,source_version,id DESC)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("rebuild policy evidence status: %w", err)
			}
		}
		return nil
	})
}
