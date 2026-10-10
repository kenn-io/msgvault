package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

var _ inboxcontrol.TriageEvidenceStore = (*Store)(nil)

// ValidateInboxTriageEvidence checks references without fetching message bodies.
// Mail evidence belongs to that exact message; chat evidence may name any active
// archived message within the same source and conversation.
func (s *Store) ValidateInboxTriageEvidence(ctx context.Context, target inboxcontrol.Target, messageIDs []int64) error {
	if target.Validate() != nil || len(messageIDs) > 100 {
		return inboxcontrol.ErrInvalid
	}
	seen := map[int64]bool{}
	for _, id := range messageIDs {
		if id <= 0 || seen[id] {
			return inboxcontrol.ErrInvalid
		}
		seen[id] = true
	}
	return s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		source := inboxcontrol.SourceIdentity{SourceID: target.SourceID, SourceType: target.SourceType, SourceIdentifier: target.SourceIdentifier, AccountID: target.AccountID}
		if err := validateInboxCandidateSource(ctx, tx, source); err != nil {
			return err
		}
		if err := validateInboxArchiveTarget(ctx, tx, target); err != nil {
			return err
		}
		for _, id := range messageIDs {
			query := `SELECT COUNT(*) FROM messages WHERE id=? AND source_id=? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`
			args := []any{id, target.SourceID}
			if target.Scope == inboxcontrol.ScopeChat {
				query += ` AND conversation_id=?`
				args = append(args, target.ItemID)
			} else if id != target.ItemID {
				return inboxcontrol.ErrDenied
			}
			var count int
			if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
				return fmt.Errorf("resolve triage evidence membership: %w", err)
			}
			if count != 1 {
				return inboxcontrol.ErrDenied
			}
		}
		return nil
	})
}
