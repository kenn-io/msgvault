package store

import (
	"context"
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// ObserveMicrosoftMailStateContext persists sync's labels and provider metadata
// with one source revision. Partial observations invalidate prior actionable
// state; missing categories preserve labels, while known empty clears them.
// The caller already holds the source execution fence.
func (s *Store) ObserveMicrosoftMailStateContext(ctx context.Context, state inboxcontrol.State, folderID *int64, categories *[]string) (bool, error) {
	target := state.Target
	if target.SourceType != "msmail" || target.Validate() != nil || state.Source != (inboxcontrol.SourceIdentity{}) || state.ObservedAt.IsZero() {
		return false, inboxcontrol.ErrInvalid
	}
	complete := microsoftInboxMetadataComplete(state)
	if complete {
		if categories == nil || folderID == nil {
			return false, inboxcontrol.ErrInvalid
		}
		// The metadata and label snapshots must have the same category collection.
		if len(*categories) != len(state.Tags) {
			return false, inboxcontrol.ErrInvalid
		}
		for i, category := range *categories {
			if state.Tags[i] != category {
				return false, inboxcontrol.ErrInvalid
			}
		}
	} else if state.Read != nil || state.Inbox != nil || state.Location != "" || state.Revision != "" || state.Tags != nil {
		return false, inboxcontrol.ErrInvalid
	}
	var changed bool
	err := s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{target.SourceID}}, func(tx *loggedTx) error {
		if err := validateInboxArchiveObservationTarget(ctx, tx, target); err != nil {
			return err
		}
		hash, err := inboxcontrol.SemanticFingerprint(state)
		if err != nil {
			return err
		}
		if err := validateInboxReconcileFreshness(ctx, tx, target, state, hash); err != nil {
			return err
		}
		if complete && folderID != nil {
			var providerID string
			if err := tx.QueryRowContext(ctx, `SELECT source_label_id FROM labels WHERE id=? AND source_id=?`, *folderID, target.SourceID).Scan(&providerID); err != nil {
				return fmt.Errorf("resolve observed Microsoft folder: %w", err)
			}
			if providerID != state.Location {
				return inboxcontrol.ErrInvalid
			}
		}
		changed, err = s.reconcileMicrosoftMailLabelsTx(ctx, tx, target.SourceID, target.ItemID, folderID, categories)
		if err != nil {
			return err
		}
		_, err = observeInboxStateTx(ctx, tx, state)
		return err
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

func microsoftInboxMetadataComplete(state inboxcontrol.State) bool {
	return state.Read != nil && state.Inbox != nil && strings.TrimSpace(state.Location) != "" && strings.TrimSpace(state.Revision) != ""
}
