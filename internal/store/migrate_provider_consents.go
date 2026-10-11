package store

import (
	"context"
	"fmt"
)

// migrateProviderConsents copies the three per-purpose consent tables into
// provider_consents, keeping IDs, times and actors, then drops them. A plain
// INSERT aborts on any collision so no revocation history is lost silently.
func (s *Store) migrateProviderConsents(ctx context.Context) error {
	legacy := []struct {
		table   string
		purpose ConsentPurpose
	}{
		{"person_inference_consents", ConsentPeopleInference},
		{"person_enrichment_consents", ConsentPersonEnrichment},
		{"person_semantic_embedding_consents", ConsentPersonSemanticEmbedding},
	}
	present := legacy[:0]
	for _, source := range legacy {
		exists, err := s.tableExistsContext(ctx, source.table)
		if err != nil {
			return fmt.Errorf("check %s: %w", source.table, err)
		}
		if exists {
			present = append(present, source)
		}
	}
	if len(present) == 0 {
		return nil
	}
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		for _, source := range present {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO provider_consents
					(purpose, id, fingerprint, granted_by, granted_at, revoked_by, revoked_at)
				SELECT ?, id, profile_fingerprint, granted_by, granted_at, revoked_by, revoked_at
				FROM `+source.table, string(source.purpose)); err != nil {
				return fmt.Errorf("copy %s: %w", source.table, err)
			}
			if _, err := tx.ExecContext(ctx, `DROP TABLE `+source.table); err != nil {
				return fmt.Errorf("drop %s: %w", source.table, err)
			}
		}
		return nil
	})
}
