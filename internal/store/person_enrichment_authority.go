package store

import "context"

// lockPersonEnrichmentAuthorityMutationTx serializes every mutation that can
// create or remove enrichment authority. Callers take this global gate before
// any person, work, attempt, or suppression-key lock so revocation snapshots
// cannot miss a concurrently enrolled or manually published person.
func (s *Store) lockPersonEnrichmentAuthorityMutationTx(
	ctx context.Context, tx *loggedTx,
) error {
	return s.lockProfileIdentityKeyTxContext(
		ctx, tx, "person-enrichment-authority-mutation", "global")
}
