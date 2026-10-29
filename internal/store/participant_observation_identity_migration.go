package store

import (
	"context"
	"fmt"
)

// ensureParticipantObservationIdentityIndex includes the provider and source
// bindings that distinguish current chat observations for one address. The
// schema file's IF NOT EXISTS cannot replace the older index on upgraded
// archives, so this versioned migration rebuilds it explicitly.
func (s *Store) ensureParticipantObservationIdentityIndex(ctx context.Context) error {
	return s.runMaintenance(ctx, func(ctx context.Context, tx *loggedTx) error {
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_participant_observations_identity`); err != nil {
			return fmt.Errorf("drop old participant observation identity index: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX idx_participant_observations_identity
			ON participant_contact_observations(
				participant_id, source_id, address_kind, service_id, scope_kind, scope_value,
				normalized_value, COALESCE(provider_user_id, ''), COALESCE(source_ref, '')
			) WHERE active_until IS NULL AND superseded_at IS NULL`); err != nil {
			return fmt.Errorf("create participant observation identity index: %w", err)
		}
		return nil
	})
}
