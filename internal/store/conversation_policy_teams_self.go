package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// NormalizeTeamsConversationSelfContext binds a fetched roster's provider ID to
// an already confirmed identity for this conversation's Teams source. It does
// not confirm an account, admit rich profile data, or authorize a directory cache.
func (s *Store) NormalizeTeamsConversationSelfContext(ctx context.Context, room int64, providerID, rosterEmail string) (int64, bool, error) {
	providerID = strings.TrimSpace(providerID)
	rosterEmail = strings.TrimSpace(rosterEmail)
	if room <= 0 || providerID == "" || !looksLikeEmail(rosterEmail) {
		return 0, false, nil
	}
	var participant int64
	var confirmed bool
	err := s.withPolicyAttributionTxContext(ctx, func(tx *loggedTx) error {
		var sourceID int64
		var sourceType string
		err := tx.QueryRowContext(ctx, `SELECT c.source_id,src.source_type FROM conversations c JOIN sources src ON src.id=c.source_id WHERE c.id=?`, room).Scan(&sourceID, &sourceType)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if sourceType != "teams" {
			return nil
		}
		self, err := resolveAccountIdentityWith(ctx, tx, sourceID, rosterEmail)
		if errors.Is(err, ErrAccountIdentityNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		email := strings.ToLower(self.Identifier)
		previous, _, err := participantIdentifierTargetTx(ctx, tx, "teams", providerID)
		if err != nil {
			return err
		}
		var target int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM participants WHERE email_address=?`, email).Scan(&target)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if target == 0 && len(self.ParticipantIDs) > 0 {
			target = self.ParticipantIDs[0]
		}
		if previous != 0 && previous != target {
			// Self normalization may absorb a bare archival alias, but must not carry
			// contact fields from a previously rich identity across the Cold boundary.
			var rich bool
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(email_address,'')<>'' OR COALESCE(phone_number,'')<>'' OR COALESCE(domain,'')<>'' FROM participants WHERE id=?`, previous).Scan(&rich); err != nil {
				return err
			}
			if rich {
				return nil
			}
		}
		if previous == target && target != 0 {
			participant = target
			confirmed = true
			return nil
		}
		state, err := s.captureParticipantConversationPolicyTx(ctx, tx, previous, target)
		if err != nil {
			return err
		}
		if target == 0 {
			target, err = s.ensureParticipantByIdentifierTx(ctx, tx, "email", email, "")
			if err != nil {
				return err
			}
		}
		if previous != 0 && previous != target {
			if err := s.mergeParticipantsTx(ctx, tx, previous, target); err != nil {
				return err
			}
		}
		if err := s.setParticipantIdentifierTx(ctx, tx, target, "teams", providerID); err != nil {
			return err
		}
		if err := s.finishConversationPolicyLifecycleTx(ctx, tx, state); err != nil {
			return err
		}
		participant = target
		confirmed = true
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	return participant, confirmed, nil
}
