package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/emailtags"
)

// EmailTagTarget binds one archived message to its exact provider identity.
type EmailTagTarget struct {
	MessageIdentityGuard

	Provider    string
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

// EmailTagTargetContext resolves metadata only; it never guesses by Message-ID.
func (s *Store) EmailTagTargetContext(ctx context.Context, id int64, mailbox string) (EmailTagTarget, error) {
	var target EmailTagTarget
	err := s.db.QueryRowContext(ctx, `SELECT m.id,m.source_id,m.source_message_id,s.source_type
 FROM messages m JOIN sources s ON s.id=m.source_id
 WHERE m.id=? AND m.deleted_at IS NULL AND m.deleted_from_source_at IS NULL`, id).Scan(&target.ID, &target.SourceID, &target.SourceMessageID, &target.Provider)
	if errors.Is(err, sql.ErrNoRows) {
		return target, emailtags.Failure("message_not_found", "message is not present in the archive source", nil, ErrMessageNotFound)
	}
	if err != nil {
		return target, fmt.Errorf("resolve email tag target: %w", err)
	}
	if target.Provider == "" {
		target.Provider = "gmail"
	}
	if target.Provider != "imap" {
		if mailbox != "" {
			return target, emailtags.Failure("invalid_request", "mailbox selection applies only to IMAP", nil, nil)
		}
		if target.Provider != "gmail" && target.Provider != "msmail" {
			return target, emailtags.Failure("unsupported_provider", "tags can be edited only on connected Gmail, IMAP, and Microsoft Graph messages", nil, nil)
		}
		return target, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT mm.mailbox,mm.uidvalidity,mm.uid
 FROM imap_message_memberships mm
 JOIN imap_folder_state fs ON fs.source_id=mm.source_id AND fs.mailbox=mm.mailbox AND fs.uidvalidity=mm.uidvalidity
 WHERE mm.message_id=? AND mm.source_id=?`, id, target.SourceID)
	if err != nil {
		return target, fmt.Errorf("resolve IMAP tag membership: %w", err)
	}
	defer func() { _ = rows.Close() }()
	count := 0
	var sole EmailTagTarget
	selected := false
	for rows.Next() {
		var name string
		var epoch, uid uint32
		if err := rows.Scan(&name, &epoch, &uid); err != nil {
			return target, err
		}
		if mailbox != "" && name != mailbox {
			continue
		}
		count++
		sole = target
		sole.Mailbox, sole.UIDValidity, sole.UID = name, epoch, uid
		if fmt.Sprintf("%s|%d", name, uid) == target.SourceMessageID {
			target.Mailbox, target.UIDValidity, target.UID = name, epoch, uid
			selected = true
		}
	}
	if err := rows.Err(); err != nil {
		return target, err
	}
	if !selected && count == 1 {
		target = sole
		selected = true
	}
	if !selected || target.UIDValidity == 0 || target.UID == 0 {
		return target, emailtags.Failure("stale_identity", "no unique current IMAP membership; sync the account and specify the mailbox before retrying", nil, nil)
	}
	return target, nil
}

// SaveEmailTagsContext persists only an authoritative provider snapshot, guarded
// by the same message/source identity that was resolved before the remote write.
func (s *Store) SaveEmailTagsContext(ctx context.Context, target EmailTagTarget, result *emailtags.MessageTagResult) error {
	if result == nil || !result.Verified || result.DryRun || result.Provider != target.Provider {
		return errors.New("tag snapshot is not verified")
	}
	if target.Provider == "imap" && (result.Flags == nil || result.Mailbox != target.Mailbox || result.UIDValidity != target.UIDValidity || result.UID != target.UID) {
		return errors.New("IMAP snapshot does not match the target membership")
	}
	return s.withAttributionTxContext(ctx, attributionLock{Sources: []int64{target.SourceID}}, func(tx *loggedTx) error {
		// The attribution transaction holds the source lock while we recheck identity.
		var locked int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM messages
   WHERE id=? AND source_id=? AND source_message_id=? AND deleted_at IS NULL AND deleted_from_source_at IS NULL`, target.ID, target.SourceID, target.SourceMessageID).Scan(&locked)
		if err != nil {
			return fmt.Errorf("archived tag identity changed: %w", err)
		}
		if target.Provider == "imap" {
			flags, err := json.Marshal(result.Flags)
			if err != nil {
				return err
			}
			return s.refreshAccountAttributionIfOutboundChangedTx(ctx, tx, target.ID, func() error {
				err := tx.QueryRowContext(ctx, `UPDATE imap_message_memberships SET flags=?,updated_at=CURRENT_TIMESTAMP
    WHERE source_id=? AND message_id=? AND mailbox=? AND uidvalidity=? AND uid=?
    AND EXISTS (SELECT 1 FROM imap_folder_state fs WHERE fs.source_id=imap_message_memberships.source_id AND fs.mailbox=imap_message_memberships.mailbox AND fs.uidvalidity=imap_message_memberships.uidvalidity)
    RETURNING message_id`, string(flags), target.SourceID, target.ID, target.Mailbox, target.UIDValidity, target.UID).Scan(&locked)
				if err != nil {
					return fmt.Errorf("IMAP tag membership changed: %w", err)
				}
				return nil
			})
		}
		if target.Provider == "msmail" {
			_, err := s.reconcileMicrosoftMailLabelsTx(ctx, tx, target.SourceID, target.ID, nil, &result.Tags)
			return err
		}
		if target.Provider != "gmail" {
			return errors.New("unsupported tag provider")
		}
		q := boundQuerier{ctx: ctx, q: tx}
		descriptors := make(map[string]LabelInfo)
		catalogChanged := false
		available := make(map[string]string, len(result.AvailableTags))
		for _, tag := range result.AvailableTags {
			available[tag.ID] = tag.Name
		}
		for _, tag := range result.Tags {
			if IsSystemLabel(tag) {
				descriptors[tag] = LabelInfo{Name: tag, Type: "system", SystemRole: GmailSystemRoleForLabelID(tag)}
			} else if name, ok := available[tag]; ok {
				descriptors[tag] = LabelInfo{Name: name, Type: "user"}
			}
		}
		pending := make([]string, 0, len(descriptors))
		for tag := range descriptors {
			pending = append(pending, tag)
		}
		for i := 0; i < len(pending); i++ {
			tag := pending[i]
			info := descriptors[tag]
			var name string
			var kind, role sql.NullString
			err := q.QueryRow(`SELECT name,label_type,system_role FROM labels WHERE source_id=? AND source_label_id=?`, target.SourceID, tag).Scan(&name, &kind, &role)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err == nil && IsSystemLabel(tag) {
				// Tag readback carries system IDs, not provider display names.
				info.Name = name
				descriptors[tag] = info
			}
			if errors.Is(err, sql.ErrNoRows) || name != info.Name || kind.String != info.Type || !labelSystemRoleMatches(role, info.SystemRole) {
				catalogChanged = true
			}
			if err == nil && name == info.Name {
				continue
			}
			// Include only live labels that must move aside for this rename.
			// The batch's two-phase rename then keeps their identities distinct.
			var conflict sql.NullString
			err = q.QueryRow(`SELECT source_label_id FROM labels WHERE source_id=? AND name=?`, target.SourceID, info.Name).Scan(&conflict)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("check Gmail label name %q: %w", info.Name, err)
			}
			if _, included := descriptors[conflict.String]; included {
				continue
			}
			if name, ok := available[conflict.String]; ok {
				descriptors[conflict.String] = LabelInfo{Name: name, Type: "user"}
				pending = append(pending, conflict.String)
			}
		}
		ids, err := ensureLabelsBatchWith(q, target.SourceID, descriptors, labelFlipsTx(ctx, tx, target.SourceID))
		if err != nil {
			return err
		}
		desired := make([]int64, 0, len(result.Tags))
		for _, tag := range result.Tags {
			id, ok := ids[tag]
			if !ok {
				if err := q.QueryRow(`SELECT id FROM labels WHERE source_id=? AND source_label_id=?`, target.SourceID, tag).Scan(&id); err != nil {
					return fmt.Errorf("gmail label %q is unavailable locally; sync the account: %w", tag, err)
				}
			}
			desired = append(desired, id)
		}
		changed, err := s.reconcileMessageLabelsTxContext(ctx, tx, target.ID, desired, true)
		if err != nil {
			return err
		}
		if changed || catalogChanged {
			return s.bumpDerivedDataRevision(tx, true)
		}
		return nil
	})
}

// GmailSystemRoleForLabelID returns Gmail's canonical role, independent of the
// localized label name returned to users.
func GmailSystemRoleForLabelID(sourceLabelID string) string {
	switch sourceLabelID {
	case "SENT":
		return LabelSystemRoleSent
	case "DRAFT":
		return LabelSystemRoleDrafts
	}
	return ""
}
