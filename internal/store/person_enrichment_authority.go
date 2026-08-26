package store

import "context"

// lockPersonEnrichmentAuthorityMutationTx serializes every operation that can
// create, consume, or remove enrichment authority. Callers take this global
// gate before any person, work, attempt, or suppression-key lock so an
// authority removal commits before a later dispatch can consume stale state.
func (s *Store) lockPersonEnrichmentAuthorityMutationTx(
	ctx context.Context, tx *loggedTx,
) error {
	return s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-enrichment-authority-mutation", "global")
}
