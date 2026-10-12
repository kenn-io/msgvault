package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// ReconcileInboxProviderState applies a verified observation under the source
// lease. Reconciliation preserves the exact archived item identity and never
// changes local UI read state or message bodies.
func (s *Store) ReconcileInboxProviderState(ctx context.Context, target inboxcontrol.Target, before, after inboxcontrol.State) error {
	if target == (inboxcontrol.Target{}) {
		return s.reconcileInboxFolder(ctx, before, after)
	}
	if err := target.Validate(); err != nil {
		return err
	}
	if before.Target != target || before.Source != (inboxcontrol.SourceIdentity{}) || after.Source != (inboxcontrol.SourceIdentity{}) || after.ObservedAt.IsZero() {
		return fmt.Errorf("%w: invalid reconciliation evidence", inboxcontrol.ErrInvalid)
	}
	afterHash, err := inboxcontrol.SemanticFingerprint(after)
	if err != nil {
		return err
	}
	identity := after.Target
	if identity != target {
		return fmt.Errorf("%w: reconciliation changes item identity", inboxcontrol.ErrInvalid)
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{target.SourceID}}, func(tx *loggedTx) error {
		if err := validateInboxReconcileFreshness(ctx, tx, before.Target, after, afterHash); err != nil {
			return err
		}
		if (target.SourceType == "gmail" || target.SourceType == "msmail") && after.Tags != nil {
			native := EmailTagTarget{MessageIdentityGuard: MessageIdentityGuard{ID: target.ItemID, SourceID: target.SourceID, SourceMessageID: target.ProviderID}, Provider: target.SourceType}
			snapshot := &emailtags.MessageTagResult{Provider: target.SourceType, Tags: after.Tags, Verified: true}
			if target.SourceType == "gmail" {
				for _, folder := range after.Folders {
					if folder.ID == "" || folder.Name == "" || folder.ParentID != "" || folder.UIDValidity != 0 || IsSystemLabel(folder.ID) {
						return inboxcontrol.ErrInvalid
					}
					snapshot.AvailableTags = append(snapshot.AvailableTags, emailtags.MessageTag{ID: folder.ID, Name: folder.Name})
				}
			}
			if err := s.saveEmailTagsTx(ctx, tx, native, snapshot); err != nil {
				return err
			}
		}
		_, err := observeInboxStateTx(ctx, tx, after)
		return err
	})
}

// Check both touched identities before changing memberships. Equal timestamps
// permit an identical repeat, but never a different semantic observation.
func validateInboxReconcileFreshness(ctx context.Context, tx *loggedTx, before inboxcontrol.Target, after inboxcontrol.State, afterHash string) error {
	targets := []inboxcontrol.Target{after.Target}
	if before != after.Target {
		targets = append(targets, before)
	}
	for _, target := range targets {
		key, err := inboxStateKey(target)
		if err != nil {
			return err
		}
		var observedAt int64
		var semanticHash string
		err = tx.QueryRowContext(ctx, `SELECT observed_at, semantic_hash FROM inbox_provider_states WHERE target_key = ?`, key).Scan(&observedAt, &semanticHash)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("check inbox reconciliation freshness: %w", err)
		}
		if observedAt > after.ObservedAt.UnixNano() || (target == after.Target && observedAt == after.ObservedAt.UnixNano() && semanticHash != afterHash) {
			return inboxcontrol.ErrConflict
		}
	}
	return nil
}

// Source-only provisioning records the independently observed native identity.
// It does not change message membership or infer success from a display name.
func (s *Store) reconcileInboxFolder(ctx context.Context, before, after inboxcontrol.State) error {
	if before.Source.Validate() != nil || before.Source != after.Source || before.Target != (inboxcontrol.Target{}) || after.Target != (inboxcontrol.Target{}) || after.ObservedAt.IsZero() || after.ObservedAt.Before(before.ObservedAt) || after.ProvisionedFolder == nil {
		return inboxcontrol.ErrInvalid
	}
	folder := *after.ProvisionedFolder
	if folder.ID == "" || folder.Name == "" || folder.ParentID != "" {
		return inboxcontrol.ErrInvalid
	}
	found := false
	for _, live := range after.Folders {
		if live == folder {
			found = true
		}
	}
	if !found {
		return inboxcontrol.ErrInvalid
	}
	if after.Source.SourceType != "gmail" || after.Source.AccountID != after.Source.SourceIdentifier || folder.UIDValidity != 0 {
		return inboxcontrol.ErrInvalid
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{after.Source.SourceID}}, func(tx *loggedTx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE id = ? AND identifier = ? AND (source_type = 'gmail' OR source_type = '')`, after.Source.SourceID, after.Source.SourceIdentifier).Scan(&count); err != nil {
			return fmt.Errorf("resolve folder source: %w", err)
		}
		if count != 1 {
			return inboxcontrol.ErrInvalid
		}
		// Provisioning cannot retarget an archived label's message associations.
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM labels WHERE source_id = ? AND name = ? AND (source_label_id IS NULL OR source_label_id <> ?)`, after.Source.SourceID, folder.Name, folder.ID).Scan(&count); err != nil {
			return fmt.Errorf("check folder identity conflict: %w", err)
		}
		if count != 0 {
			return inboxcontrol.ErrConflict
		}
		_, err := ensureLabelsBatchWith(tx, after.Source.SourceID, map[string]LabelInfo{folder.ID: {Name: folder.Name, Type: "user"}}, labelFlipsTx(ctx, tx, after.Source.SourceID))
		return err
	})
}
