package cmd

import (
	"context"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.MessageArchiveStore = (*storeAPIAdapter)(nil)

// ArchiveMessage reuses tag source ownership, readback, and guarded persistence.
func (a *storeAPIAdapter) ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange, grant *agentgrant.Grant, acquireWrite func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error) {
	authorize := func(ctx context.Context) error {
		source, err := a.store.GetMessageSourceContext(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrMessageNotFound) {
				return emailtags.Failure("message_not_found", "Message not found", nil, err)
			}
			return err
		}
		if grant != nil {
			ref := agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier}
			if !grant.Allows(agentgrant.PermissionMessageArchive, ref) {
				return emailtags.Failure("message_not_found", "Message not found", nil, nil)
			}
			if !grant.AllowsArchiveSender(ref, "") {
				senders, err := a.store.GetMessageRecipientsContext(ctx, id, "from")
				if err != nil {
					return err
				}
				if len(senders) == 0 {
					return emailtags.Failure("message_not_found", "Message not found", nil, nil)
				}
				for _, sender := range senders {
					_, key, parseErr := parseStoredMailbox(sender.EnvelopeAddress)
					if parseErr != nil || !grant.AllowsArchiveSender(ref, key) {
						return emailtags.Failure("message_not_found", "Message not found", nil, nil)
					}
				}
			}
		}
		if source.SourceType != "gmail" && source.SourceType != "" {
			return emailtags.Failure("archive_unsupported", "Archiving is supported only for Gmail messages", nil, nil)
		}
		return nil
	}
	result, err := a.messageTags(ctx, id, nil, "", acquireWrite, &change, authorize)
	if result == nil {
		return nil, err
	}
	return &emailtags.ArchiveResult{MessageID: id, SourceID: result.SourceID, Archived: !slices.Contains(result.Tags, "INBOX"), Changed: slices.Contains(result.Before, "INBOX") != slices.Contains(result.Tags, "INBOX"), DryRun: result.DryRun, Verified: result.Verified}, err
}
