package store

import (
	"context"
	"fmt"
)

// migrateAttributeDefinitionStability initializes classes once; subsequent seed
// reconciliation deliberately preserves the owner's metadata choices.
func (s *Store) migrateAttributeDefinitionStability(ctx context.Context) error {
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		for _, group := range []struct {
			stability AttributeStability
			ids       []string
		}{
			{AttributeStabilityFixed, []string{AttributeUniversalIDBirthplace, AttributeUniversalIDHowWeMet, AttributeUniversalIDInterestsFunGrowingUp}},
			{AttributeStabilityVolatile, []string{AttributeUniversalIDInterestsFunNow, AttributeUniversalIDAskMeAbout, AttributeUniversalIDLastContacted}},
		} {
			if _, err := tx.ExecContext(ctx, `UPDATE attribute_definitions SET stability = ? WHERE universal_id IN (?, ?, ?)`, group.stability, group.ids[0], group.ids[1], group.ids[2]); err != nil {
				return fmt.Errorf("initialize %s attribute stability: %w", group.stability, err)
			}
		}
		return nil
	})
}
