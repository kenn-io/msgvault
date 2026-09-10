package sync

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func (s *Syncer) relocateIMAPMessage(
	ctx context.Context,
	expected store.MessageIdentityGuard,
	raw *gmail.RawMessage,
	threadID string,
	labelMap map[string]int64,
	replaceLabels bool,
	preserveLabels bool,
) error {
	prepared, err := s.prepareMessage(expected.SourceID, raw, threadID, false)
	if err != nil {
		return err
	}
	return s.persistPreparedIMAPRelocation(ctx, expected, prepared, labelMap, replaceLabels, preserveLabels)
}

func (s *Syncer) relocateIMAPMessageToTarget(
	ctx context.Context, sourceID int64, target gmail.MessageRelocationTarget,
	raw *gmail.RawMessage, threadID string, labelMap map[string]int64,
) error {
	if target.SourceID != sourceID || target.InternalID <= 0 || target.SourceMessageID == "" ||
		raw == nil || raw.ID != target.NewSourceMessageID {
		return errors.New("invalid IMAP relocation target")
	}
	savedOrigin, err := s.savedIMAPContentOrigin(target.InternalID)
	if err != nil {
		return err
	}
	// A surviving Drafts copy may take over the location, but it must not
	// replace the final snapshot even if the original Sent mailbox is gone.
	destinationOrigin := s.imapContentOrigin(target.NewSourceMessageID)
	canonicalOrigin := s.imapContentOrigin(target.SourceMessageID)
	if !destinationOrigin.canRefresh(savedOrigin, false) || !destinationOrigin.canRefresh(canonicalOrigin, false) {
		return s.adoptIMAPRelocationLocation(ctx, target, raw, labelMap)
	}
	prepared, err := s.prepareMessage(sourceID, raw, threadID, false)
	if err != nil {
		return err
	}
	expectedID := mime.NormalizeMessageID(target.RFC822MessageID)
	if expectedID == "" || mime.NormalizeMessageID(prepared.message.RFC822MessageID.String) != expectedID {
		return fmt.Errorf("IMAP relocation candidate %q changed RFC822 identity", raw.ID)
	}
	return s.persistPreparedIMAPRelocation(ctx, store.MessageIdentityGuard{
		ID: target.InternalID, SourceID: target.SourceID, SourceMessageID: target.SourceMessageID,
	}, prepared, labelMap, s.labelsSnapshotComplete(), s.defersAuthoritativeLabelReconciliation())
}

// adoptIMAPRelocationLocation rekeys the guarded row to its surviving
// location without refreshing the snapshot. The destination either lacks
// trusted outgoing placement or is a Drafts copy superseded by Sent, so its
// fetched bytes cannot replace archived content; the location is
// still adopted with the pre-relocation rekey semantics. Identity is checked
// with the tolerant parser so attacker-controlled MIME cannot strand the run
// behind a strict-parse failure.
func (s *Syncer) adoptIMAPRelocationLocation(
	ctx context.Context,
	target gmail.MessageRelocationTarget, raw *gmail.RawMessage, labelMap map[string]int64,
) error {
	parsed, parseErr := mime.ParseWithRecovery(raw.Raw, "")
	if parseErr != nil {
		s.logger.Warn("IMAP relocation identity parse recovered with errors",
			"id", raw.ID, "error", textutil.FirstLine(parseErr.Error()))
	}
	if parsed == nil {
		return fmt.Errorf("parse IMAP relocation candidate %q", raw.ID)
	}
	expectedID := mime.NormalizeMessageID(target.RFC822MessageID)
	if expectedID == "" || mime.NormalizeMessageID(parsed.MessageID) != expectedID {
		return fmt.Errorf("IMAP relocation candidate %q changed RFC822 identity", raw.ID)
	}
	// One guarded transaction adopts the location and, for clients that do
	// not defer authoritative labels, reconciles them: a late failure or
	// cancellation rolls the rekey back with the labels, keeping the old
	// composite key that the next retry rediscovers the candidate with.
	_, err := s.store.AdoptMessageSourceIDContext(
		ctx,
		target.InternalID, target.SourceMessageID, target.NewSourceMessageID,
		!s.defersAuthoritativeLabelReconciliation(),
		labelIDsFor(raw.LabelIDs, labelMap),
		s.labelsSnapshotComplete(),
	)
	if err != nil {
		return fmt.Errorf("adopt IMAP relocation location: %w", err)
	}
	return nil
}

func (s *Syncer) persistPreparedIMAPRelocation(
	ctx context.Context, expected store.MessageIdentityGuard, prepared *messageData,
	labelMap map[string]int64, replaceLabels, preserveLabels bool,
) error {
	attachmentWrites, _, err := s.publishMIMEAttachments(ctx, prepared.attachments)
	if err != nil {
		return err
	}
	participants, participantIndex := preparedMessageParticipants(prepared)
	labelIDs := labelIDsFor(prepared.gmailLabelIDs, labelMap)

	messageID, err := s.store.PersistIMAPRelocationWithParticipantsContext(
		ctx,
		expected,
		participants,
		func(participantIDs []int64) *store.MessagePersistData {
			persist := buildPreparedSnapshot(
				prepared, participantIndex, participantIDs, attachmentWrites)
			persist.LabelIDs = labelIDs
			persist.PreserveLabels = preserveLabels
			return persist
		},
		replaceLabels,
	)
	if err != nil {
		return err
	}
	// A relocated snapshot carries the same remote-image archival hook as
	// ordinary ingest: the refreshed HTML may reference tracked image URLs the
	// previous location never had. The retained internal ID keeps already
	// archived images attached, so repeat syncs do not re-download them.
	if s.opts.RemoteImages != nil && prepared.message.MessageType == store.MessageTypeEmail {
		archived := s.opts.RemoteImages.Archive(
			ctx, s.store, s.opts.AttachmentsDir, messageID, prepared.bodyHTML)
		for _, imageErr := range archived.Errors {
			s.logger.Warn("failed to archive remote image",
				"message", messageID, "error", imageErr)
		}
	}
	return nil
}

func (s *Syncer) publishMIMEAttachments(
	ctx context.Context,
	attachments []mime.Attachment,
) ([]store.AttachmentWrite, map[string]export.DurableAttachmentReceipt, error) {
	writes := make([]store.AttachmentWrite, 0, len(attachments))
	created := make(map[string]export.DurableAttachmentReceipt)
	for i := range attachments {
		if err := ctx.Err(); err != nil {
			return nil, created, err
		}
		attachment := &attachments[i]
		if s.opts.AttachmentsDir == "" {
			return nil, created, fmt.Errorf(
				"publish attachment %q: attachments directory is required",
				attachment.Filename,
			)
		}
		receipt, err := export.StoreAttachmentFileDurable(
			s.opts.AttachmentsDir, attachment)
		if receipt.Created {
			created[receipt.StoragePath] = receipt
		}
		if err != nil {
			return nil, created, fmt.Errorf(
				"publish attachment %q: %w", attachment.Filename, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, created, err
		}
		role, roleSource := store.AttachmentRoleFromMIME(
			attachment.Disposition, attachment.IsInline, attachment.ContentID)
		writes = append(writes, store.AttachmentWrite{
			Filename: attachment.Filename, MIMEType: attachment.ContentType,
			StoragePath: receipt.StoragePath, ContentHash: attachment.ContentHash,
			Size: int64(len(attachment.Content)), Role: role, RoleSource: roleSource,
			SourcePartKey: attachment.PartKey, ContentID: attachment.ContentID,
		})
	}
	return writes, created, nil
}
