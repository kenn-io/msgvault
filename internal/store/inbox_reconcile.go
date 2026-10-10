package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/url"

	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

// ReconcileInboxProviderState applies a verified observation under the source
// lease. An IMAP move requires its exact COPYUID mapping in after.Target; it
// never resolves Message-ID, retires unrelated mailboxes, or discards bodies.
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
	if target.SourceType == sourceTypeIMAP {
		identity.Mailbox, identity.UIDValidity, identity.UID = target.Mailbox, target.UIDValidity, target.UID
	}
	if identity != target {
		return fmt.Errorf("%w: reconciliation changes item identity", inboxcontrol.ErrInvalid)
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{target.SourceID}}, func(tx *loggedTx) error {
		if err := validateInboxReconcileFreshness(ctx, tx, before.Target, after, afterHash); err != nil {
			return err
		}
		if target.SourceType == sourceTypeIMAP {
			if err := s.reconcileInboxIMAPTx(ctx, tx, target, after); err != nil {
				return err
			}
		}
		if (target.SourceType == sourceTypeGmail || target.SourceType == "msmail") && after.Tags != nil {
			native := EmailTagTarget{MessageID: target.ItemID, SourceID: target.SourceID, SourceMessageID: target.ProviderID, Provider: target.SourceType}
			snapshot := &emailtags.Result{Provider: target.SourceType, Tags: after.Tags, Verified: true}
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

func (s *Store) reconcileInboxIMAPTx(ctx context.Context, tx *loggedTx, target inboxcontrol.Target, after inboxcontrol.State) error {
	if err := validateInboxArchiveIdentity(ctx, tx, target); err != nil {
		return err
	}
	// A repeat reconciliation is safe only if the exact destination already
	// points to the same archived item. Conflicting mappings never get replaced.
	var destinationID int64
	err := tx.QueryRowContext(ctx, `SELECT message_id FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ?`,
		target.SourceID, after.Target.Mailbox, after.Target.UIDValidity, after.Target.UID).Scan(&destinationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resolve inbox destination membership: %w", err)
	}
	if err == nil && destinationID != target.ItemID {
		return inboxcontrol.ErrConflict
	}
	if err := validateInboxIMAPMembership(ctx, tx, target); err != nil {
		if !errors.Is(err, inboxcontrol.ErrInvalid) || destinationID != target.ItemID || after.Target == target {
			return err
		}
	}
	var epoch uint32
	err = tx.QueryRowContext(ctx, `SELECT uidvalidity FROM imap_folder_state WHERE source_id = ? AND mailbox = ?`, target.SourceID, after.Target.Mailbox).Scan(&epoch)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resolve inbox destination epoch: %w", err)
	}
	if err == nil && epoch != after.Target.UIDValidity {
		return inboxcontrol.ErrConflict
	}
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext,highest_modseq) VALUES(?,?,?,0,'0') ON CONFLICT(source_id,mailbox) DO NOTHING`, target.SourceID, after.Target.Mailbox, after.Target.UIDValidity); err != nil {
			return fmt.Errorf("record inbox destination epoch: %w", err)
		}
	}
	flags, err := json.Marshal(after.Flags)
	if err != nil {
		return fmt.Errorf("encode inbox IMAP flags: %w", err)
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO imap_message_memberships
		(source_id, mailbox, uidvalidity, uid, message_id, flags) VALUES (?, ?, ?, ?, ?, %s)
		ON CONFLICT(source_id, mailbox, uidvalidity, uid) DO UPDATE SET flags = excluded.flags
		WHERE imap_message_memberships.message_id = excluded.message_id`, s.dialect.JSONBindExpr()),
		target.SourceID, after.Target.Mailbox, after.Target.UIDValidity, after.Target.UID, target.ItemID, string(flags))
	if err := inboxReceiptTransition(result, err); err != nil {
		return err
	}
	if after.Target != target {
		if _, err := tx.ExecContext(ctx, `DELETE FROM imap_message_memberships WHERE source_id = ? AND mailbox = ? AND uidvalidity = ? AND uid = ? AND message_id = ?`,
			target.SourceID, target.Mailbox, target.UIDValidity, target.UID, target.ItemID); err != nil {
			return fmt.Errorf("retire moved inbox membership: %w", err)
		}
		key, err := inboxStateKey(target)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM inbox_provider_states WHERE target_key = ?`, key); err != nil {
			return fmt.Errorf("retire moved inbox observation: %w", err)
		}
		if err := bumpInboxSourceRevisionTx(ctx, tx, target.SourceID); err != nil {
			return err
		}
	}
	// Invalidate only touched mailbox watermarks. Zero means unknown, forcing
	// normal sync to observe a complete snapshot instead of skipping the change.
	for _, mailbox := range []string{target.Mailbox, after.Target.Mailbox} {
		if _, err := tx.ExecContext(ctx, `UPDATE imap_folder_state SET uidnext = 0, highest_modseq = '0' WHERE source_id = ? AND mailbox = ?`, target.SourceID, mailbox); err != nil {
			return fmt.Errorf("invalidate inbox mailbox sync state: %w", err)
		}
	}
	mailboxes, err := imapMembershipMailboxes(ctx, tx, target.SourceID, target.ItemID)
	if err != nil {
		return err
	}
	labelIDs := make([]int64, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		id, err := ensureIMAPMailboxLabel(ctx, tx, target.SourceID, mailbox)
		if err != nil {
			return err
		}
		labelIDs = append(labelIDs, id)
	}
	if err := s.refreshAccountAttributionIfOutboundChangedTx(ctx, tx, target.ItemID, func() error {
		return replaceMessageLabelsTx(boundQuerier{ctx: ctx, q: tx}, target.ItemID, labelIDs)
	}); err != nil {
		return fmt.Errorf("reconcile inbox mailbox labels: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET deleted_from_source_at = NULL WHERE id = ? AND source_id = ?`, target.ItemID, target.SourceID); err != nil {
		return fmt.Errorf("clear inbox move tombstone: %w", err)
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
	if after.Source.SourceType == sourceTypeIMAP {
		return s.reconcileInboxIMAPFolder(ctx, after, folder)
	}
	if after.Source.SourceType != sourceTypeGmail || after.Source.AccountID != after.Source.SourceIdentifier || folder.UIDValidity != 0 {
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

func (s *Store) reconcileInboxIMAPFolder(ctx context.Context, after inboxcontrol.State, folder inboxcontrol.Folder) error {
	identifier, err := url.Parse(after.Source.SourceIdentifier)
	if err != nil || identifier.User == nil || identifier.User.Username() != after.Source.AccountID || (identifier.Scheme != sourceTypeIMAP && identifier.Scheme != "imaps" && identifier.Scheme != "imap+starttls") || folder.ID != folder.Name || folder.UIDValidity == 0 {
		return inboxcontrol.ErrInvalid
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{after.Source.SourceID}}, func(tx *loggedTx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE id = ? AND identifier = ? AND source_type = 'imap'`, after.Source.SourceID, after.Source.SourceIdentifier).Scan(&count); err != nil {
			return fmt.Errorf("resolve IMAP folder source: %w", err)
		}
		if count != 1 {
			return inboxcontrol.ErrInvalid
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM labels WHERE source_id = ? AND name = ? AND (source_label_id IS NULL OR source_label_id <> ?)`, after.Source.SourceID, folder.Name, folder.ID).Scan(&count); err != nil {
			return fmt.Errorf("check IMAP folder label conflict: %w", err)
		}
		if count != 0 {
			return inboxcontrol.ErrConflict
		}
		var epoch uint32
		err := tx.QueryRowContext(ctx, `SELECT uidvalidity FROM imap_folder_state WHERE source_id = ? AND mailbox = ?`, after.Source.SourceID, folder.ID).Scan(&epoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("resolve IMAP folder epoch: %w", err)
		}
		if err == nil && epoch != folder.UIDValidity {
			return inboxcontrol.ErrConflict
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO imap_folder_state(source_id,mailbox,uidvalidity,uidnext,highest_modseq) VALUES(?,?,?,0,'0') ON CONFLICT(source_id,mailbox) DO NOTHING`, after.Source.SourceID, folder.ID, folder.UIDValidity); err != nil {
			return fmt.Errorf("record IMAP folder epoch: %w", err)
		}
		_, err = ensureIMAPMailboxLabel(ctx, tx, after.Source.SourceID, folder.ID)
		return err
	})
}
