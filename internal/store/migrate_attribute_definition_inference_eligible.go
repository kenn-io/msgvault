package store

import (
	"context"
	"fmt"
)

// migrateAttributeDefinitionInferenceEligible initializes extraction exclusions
// once. Seed reconciliation preserves subsequent owner metadata choices.
func (s *Store) migrateAttributeDefinitionInferenceEligible(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, s.Rebind(`
		UPDATE attribute_definitions SET inference_eligible = FALSE
		WHERE universal_id IN (?, ?)`), AttributeUniversalIDPersonality, AttributeUniversalIDContactFrequency)
	if err != nil {
		return fmt.Errorf("initialize attribute inference eligibility: %w", err)
	}
	return nil
}
