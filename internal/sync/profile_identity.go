package sync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/oauth"
	"go.kenn.io/msgvault/internal/provideridentity"
	"go.kenn.io/msgvault/internal/store"
)

// refreshProfileIdentity uses the already-fetched authenticated Gmail profile.
// IMAP's profile is constructed from configuration, so it is not OAuth evidence.
func (s *Syncer) refreshProfileIdentity(ctx context.Context, source *store.Source, profile *gmail.Profile) error {
	if source.SourceType != sourceTypeGmail {
		return nil
	}
	if profile == nil {
		return errors.New("read authenticated Gmail identity: provider returned no profile")
	}
	evidence, err := provideridentity.GmailProfileEvidence(source, profile.EmailAddress)
	if err != nil {
		return err
	}
	address := evidence[0].Identifier
	if err := s.mergeProfileIdentitySignals(ctx, source.ID, address); err != nil {
		// Ownership evidence is best-effort like page discovery. Every profile read,
		// including a no-op incremental run, retries so an idle mailbox can recover.
		s.logger.Warn("Gmail OAuth identity refresh failed", "source_id", source.ID, "error", err)
	}
	return nil
}

func (s *Syncer) mergeProfileIdentitySignals(ctx context.Context, sourceID int64, address string) error {
	confirmed, err := s.store.ListAccountIdentitiesContext(ctx, sourceID)
	if err != nil {
		return fmt.Errorf("list confirmed profile identities: %w", err)
	}
	var candidates []store.IdentityConfirmation
	for _, identity := range confirmed {
		if oauth.SameGoogleAccount(identity.Address, address) {
			candidates = append(candidates, store.IdentityConfirmation{Identifier: identity.Address, Signals: []string{"oauth"}})
		}
	}
	// The atomic refresh-only method skips ownership removed after the list.
	_, err = s.store.MergeConfirmedAccountIdentitySignalsContext(ctx, sourceID, candidates)
	if err != nil {
		return fmt.Errorf("merge authenticated profile evidence: %w", err)
	}
	return nil
}
