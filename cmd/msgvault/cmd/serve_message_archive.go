package cmd

import (
	"context"
	"database/sql"
	"errors"
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/internal/sourcescope"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.MessageArchiveStore = (*storeAPIAdapter)(nil)

// ArchiveMessage reuses tag source ownership, readback, and guarded persistence.
func (a *storeAPIAdapter) ArchiveMessage(ctx context.Context, id int64, change emailtags.ArchiveChange, grant *agentgrant.Grant, acquireWrite func(context.Context) (func(), error)) (*emailtags.ArchiveResult, error) {
	target, err := a.store.EmailTagTargetContext(ctx, id, "")
	if err != nil {
		if _, ok := errors.AsType[*emailtags.MessageTagError](err); !ok {
			return nil, err
		}
	}
	if target.Provider == sourceTypeBeeper {
		return a.archiveBeeperChat(ctx, id, change, grant, acquireWrite)
	}
	authorize := func(ctx context.Context, target store.EmailTagTarget) error {
		source, err := a.store.GetSourceByIDContext(ctx, target.SourceID)
		if err != nil {
			return err
		}
		if grant != nil {
			ref := agentgrant.SourceRef{Type: source.SourceType, Identifier: source.Identifier}
			selected, ok := grant.MatchingSource(ref)
			if !ok {
				return emailtags.Failure("message_not_found", "Message not found", nil, nil)
			}
			ctx = sourcescope.WithIncomingSenders(ctx, map[int64][]string{source.ID: selected.IncomingSenderKeys})
			allowed, err := a.store.IncomingMessageAllowed(ctx, id)
			if err != nil {
				return err
			}
			if !allowed {
				return emailtags.Failure("message_not_found", "Message not found", nil, nil)
			}
		}
		if source.SourceType != sourceTypeGmail && source.SourceType != "" {
			return emailtags.Failure("archive_unsupported", "Archiving is supported only for Gmail messages", nil, nil)
		}
		if change.WholeChat {
			return emailtags.Failure("invalid_request", "whole_chat applies only to Beeper", nil, nil)
		}
		return nil
	}
	if err == nil && target.Provider == sourceTypeGmail && change.Archived != nil {
		if err := authorize(ctx, target); err != nil {
			return nil, err
		}
		labels, labelsErr := a.store.MessageLabelIDsContext(ctx, id)
		inbox, inboxErr := a.store.LabelIDContext(ctx, target.SourceID, "INBOX")
		if labelsErr == nil && (inboxErr == nil || (len(labels) > 0 && errors.Is(inboxErr, sql.ErrNoRows))) && *change.Archived != slices.Contains(labels, inbox) {
			return &emailtags.ArchiveResult{MessageID: id, SourceID: target.SourceID, Archived: *change.Archived, DryRun: change.DryRun}, nil
		}
	}
	result, err := a.messageTags(ctx, id, nil, "", acquireWrite, &change, authorize)
	if failure, ok := errors.AsType[*emailtags.MessageTagError](err); grant != nil && ok && failure.Code == "message_not_found" {
		return nil, emailtags.Failure("message_not_found", "Message not found", nil, nil)
	}
	if result == nil {
		return nil, err
	}
	return &emailtags.ArchiveResult{MessageID: id, SourceID: result.SourceID, Archived: !slices.Contains(result.Tags, "INBOX"), Changed: slices.Contains(result.Before, "INBOX") != slices.Contains(result.Tags, "INBOX"), DryRun: result.DryRun, Verified: result.Verified}, err
}
