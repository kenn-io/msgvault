package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/export"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/mime"
	"go.kenn.io/msgvault/internal/store"
)

// RepairRequest names exactly one archived provider message. SourceID zero
// leaves the reference unscoped; a positive value disambiguates one source.
type RepairRequest struct {
	Reference string
	SourceID  int64
}

// RepairResult is the stable identity and repaired subject printed by callers.
type RepairResult struct {
	InternalID      int64
	SourceID        int64
	SourceMessageID string
	Subject         string
}

// RepairMessage fetches and strictly prepares one Gmail snapshot, publishes
// all MIME attachment bytes durably, then invokes the guarded atomic store
// replacement. Nothing before the final store call mutates archive rows, and
// failed repairs remove any unreferenced blobs introduced by the attempt.
func (s *Syncer) RepairMessage(ctx context.Context, request RepairRequest) (_ *RepairResult, repairErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request.Reference = strings.TrimSpace(request.Reference)
	if request.Reference == "" {
		return nil, errors.New("repair message reference is required")
	}
	if request.SourceID < 0 {
		return nil, errors.New("repair message source ID must be positive")
	}
	targets, err := s.store.FindRepairMessageTargetsContext(ctx, request.Reference, request.SourceID)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("repair message %q not found", request.Reference)
	}
	if len(targets) != 1 {
		return nil, fmt.Errorf("repair message reference %q is ambiguous across %d archive rows", request.Reference, len(targets))
	}
	target := targets[0]
	if target.SourceType != "gmail" {
		return nil, fmt.Errorf("repair message source %d is %s, not gmail", target.SourceID, target.SourceType)
	}
	profile, err := s.client.GetProfile(ctx)
	if err != nil {
		return nil, fmt.Errorf("read authenticated Gmail account: %w", err)
	}
	if profile == nil {
		return nil, errors.New("read authenticated Gmail account: provider returned no profile")
	}
	authenticated := store.NormalizeIdentifierForCompare(strings.TrimSpace(profile.EmailAddress))
	archived := store.NormalizeIdentifierForCompare(strings.TrimSpace(target.SourceIdentifier))
	if authenticated == "" || archived == "" || authenticated != archived {
		return nil, fmt.Errorf(
			"authenticated Gmail account %q does not match archive source %q",
			profile.EmailAddress, target.SourceIdentifier,
		)
	}

	raw, err := s.client.GetMessageRaw(ctx, target.SourceMessageID)
	if err != nil {
		return nil, fmt.Errorf("fetch Gmail message %q: %w", target.SourceMessageID, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("fetch Gmail message %q: provider returned no message", target.SourceMessageID)
	}
	if raw.ID != target.SourceMessageID {
		return nil, fmt.Errorf("gmail returned message ID %q for requested ID %q", raw.ID, target.SourceMessageID)
	}
	labels, err := s.client.ListLabels(ctx)
	if err != nil {
		return nil, fmt.Errorf("read Gmail label catalog: %w", err)
	}
	labelRefs, err := repairLabelRefs(raw.LabelIDs, labels)
	if err != nil {
		return nil, err
	}
	prepared, err := s.prepareMessage(target.SourceID, raw, raw.ThreadID, true)
	if err != nil {
		return nil, err
	}
	attachmentWrites := make([]store.AttachmentWrite, 0, len(prepared.attachments))
	createdAttachments := make(map[string]export.DurableAttachmentReceipt)
	defer func() {
		if repairErr == nil || len(createdAttachments) == 0 {
			return
		}
		if err := s.cleanupRepairAttachments(context.WithoutCancel(ctx), createdAttachments); err != nil {
			repairErr = errors.Join(repairErr, fmt.Errorf("clean up repair attachments: %w", err))
		}
	}()
	for i := range prepared.attachments {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		attachment := &prepared.attachments[i]
		if s.opts.AttachmentsDir == "" {
			return nil, fmt.Errorf("publish attachment %q: attachments directory is required", attachment.Filename)
		}
		receipt, err := export.StoreAttachmentFileDurable(s.opts.AttachmentsDir, attachment)
		if receipt.Created {
			createdAttachments[receipt.StoragePath] = receipt
		}
		if err != nil {
			return nil, fmt.Errorf("publish attachment %q: %w", attachment.Filename, err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		role, roleSource := store.AttachmentRoleFromMIME(
			attachment.Disposition, attachment.IsInline, attachment.ContentID,
		)
		attachmentWrites = append(attachmentWrites, store.AttachmentWrite{
			Filename: attachment.Filename, MIMEType: attachment.ContentType,
			StoragePath: receipt.StoragePath, ContentHash: attachment.ContentHash,
			Size: int64(len(attachment.Content)), Role: role, RoleSource: roleSource,
			SourcePartKey: attachment.PartKey, ContentID: attachment.ContentID,
		})
	}

	participants, participantIndex := repairParticipants(prepared)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	guard := target.MessageIdentityGuard
	messageID, err := s.store.PersistRepairMessageWithParticipantsContext(
		ctx, guard, participants, func(participantIDs []int64) *store.MessagePersistData {
			participantMap := make(map[string]int64, len(participantIndex))
			for email, index := range participantIndex {
				participantMap[email] = participantIDs[index]
			}
			message := *prepared.message
			if len(prepared.from) > 0 && prepared.from[0].Email != "" {
				if senderID, ok := participantMap[prepared.from[0].Email]; ok {
					message.SenderID = sql.NullInt64{Int64: senderID, Valid: true}
				}
			}
			recipients := []store.RecipientSet{
				buildRecipientSet("from", prepared.from, participantMap),
				buildRecipientSet("to", prepared.to, participantMap),
				buildRecipientSet("cc", prepared.cc, participantMap),
				buildRecipientSet("bcc", prepared.bcc, participantMap),
			}
			return &store.MessagePersistData{
				Message: &message,
				Conversation: &store.ConversationPersistData{
					SourceConversationID: prepared.threadID,
					ConversationType:     prepared.conversationType,
					Title:                prepared.conversationTitle,
				},
				BodyText:                  sql.NullString{String: prepared.bodyText, Valid: prepared.bodyText != ""},
				BodyHTML:                  sql.NullString{String: prepared.bodyHTML, Valid: prepared.bodyHTML != ""},
				RawMIME:                   prepared.rawMIME,
				Recipients:                recipients,
				LabelRefs:                 labelRefs,
				MIMEAttachmentReplacement: &attachmentWrites,
				FTS: &store.FTSDoc{
					Subject: message.Subject.String, Body: prepared.bodyText,
					FromAddr: joinEmails(prepared.from), ToAddrs: joinEmails(prepared.to),
					CcAddrs: joinEmails(prepared.cc),
				},
			}
		},
	)
	if err != nil {
		return nil, err
	}
	return &RepairResult{
		InternalID: messageID, SourceID: target.SourceID,
		SourceMessageID: target.SourceMessageID, Subject: prepared.message.Subject.String,
	}, nil
}

func (s *Syncer) cleanupRepairAttachments(
	ctx context.Context,
	receipts map[string]export.DurableAttachmentReceipt,
) error {
	paths := make([]string, 0, len(receipts))
	for storagePath := range receipts {
		paths = append(paths, storagePath)
	}
	sort.Strings(paths)

	return s.store.WithExclusiveLock(ctx, func() error {
		var cleanupErr error
		for _, storagePath := range paths {
			referenced, err := s.store.IsAttachmentPathReferenced(storagePath)
			if err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("check attachment %q references: %w", storagePath, err))
				continue
			}
			if referenced {
				continue
			}
			if err := export.RemoveAttachmentFileDurable(s.opts.AttachmentsDir, receipts[storagePath]); err != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove attachment %q: %w", storagePath, err))
			}
		}
		return cleanupErr
	})
}

func repairParticipants(data *messageData) ([]store.ParticipantPersistData, map[string]int) {
	byEmail := make(map[string]mime.Address)
	for _, addresses := range [][]mime.Address{data.from, data.to, data.cc, data.bcc} {
		for _, address := range addresses {
			if address.Email != "" {
				if _, exists := byEmail[address.Email]; !exists {
					byEmail[address.Email] = address
				}
			}
		}
	}
	emails := make([]string, 0, len(byEmail))
	for email := range byEmail {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	participants := make([]store.ParticipantPersistData, len(emails))
	index := make(map[string]int, len(emails))
	for i, email := range emails {
		address := byEmail[email]
		participants[i] = store.ParticipantPersistData{
			EmailAddress: email, DisplayName: address.Name, Domain: address.Domain,
		}
		index[email] = i
	}
	return participants, index
}

func repairLabelRefs(messageLabelIDs []string, labels []*gmail.Label) ([]store.MessageLabelRef, error) {
	catalog := make(map[string]*gmail.Label, len(labels))
	for _, label := range labels {
		if label != nil {
			catalog[label.ID] = label
		}
	}
	refs := make([]store.MessageLabelRef, 0, len(messageLabelIDs))
	for _, id := range messageLabelIDs {
		label, ok := catalog[id]
		if !ok {
			return nil, fmt.Errorf("label %q is missing from provider catalog", id)
		}
		labelType := label.Type
		if labelType == "" {
			labelType = "user"
			if store.IsSystemLabel(id) {
				labelType = labelTypeSystem
			}
		}
		refs = append(refs, store.MessageLabelRef{
			SourceLabelID: id,
			Info:          store.LabelInfo{Name: label.Name, Type: labelType, SystemRole: label.SystemRole},
		})
	}
	return refs, nil
}
