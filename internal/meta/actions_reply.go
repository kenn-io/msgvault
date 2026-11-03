package meta

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

func isReply(kind ActionKind) bool { return kind == ActionPublicReply || kind == ActionPrivateReply }
func (a *Actions) privateReplyChecks(ctx context.Context, t Target, req ActionRequest, result *ActionResult) error {
	if req.DryRun {
		rows, err := a.st.ListMetaCommentActionsContext(ctx, t.SourceID, t.CommentID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.Action != string(ActionPrivateReply) || row.State == "failed" {
				continue
			}
			result.Receipt = receipt(row)
			if row.State == "succeeded" {
				return actionFailure("already_sent", result, nil)
			}
			// A local preview still supplies the exact binding for explicit resolution.
			result.Warnings = append(result.Warnings, "prior_private_reply_outcome_unknown")
		}
	}
	if req.DryRun && result.Receipt == nil && t.IsFromMe {
		return actionFailure("private_reply_not_allowed", result, nil)
	}
	if req.DryRun && result.Receipt == nil && !a.privateReplyWindowOpen(t) {
		return actionFailure("private_reply_window_closed", result, nil)
	}
	prior, err := a.st.HasRecentPrivateCommentReplyToAuthorContext(ctx, t.SourceID, t.MessageID, t.AuthorID, a.opts.Now().Add(-7*24*time.Hour))
	if err != nil {
		return err
	}
	if prior {
		result.Warnings = append(result.Warnings, "prior_private_reply_to_author")
	}
	return nil
}
func (a *Actions) privateReplyWindowOpen(t Target) bool {
	return !t.SentAt.IsZero() && a.opts.Now().Sub(t.SentAt) < 7*24*time.Hour
}
func (a *Actions) dispatchReply(ctx context.Context, t Target, req ActionRequest, result *ActionResult, row store.MetaCommentAction, api ActionAPI) (*ActionResult, error) {
	var obj Object
	var err error
	out := store.MetaCommentActionOutcome{}
	if req.Action == ActionPublicReply {
		obj, err = api.ReplyToComment(ctx, t.CommentID, req.Body)
		out.RemoteID = obj.ID
	} else {
		var private PrivateReplyResult
		private, err = api.PrivateReply(ctx, t.Asset, t.CommentID, req.Body)
		out.RemoteID, out.RemoteRecipientID = private.MessageID, private.RecipientID
	}
	result, finishErr := a.finish(ctx, t, result, row, err, true, out)
	if finishErr != nil {
		return result, finishErr
	}
	if req.Action == ActionPublicReply && obj.CreatedTime != "" && obj.Parent != nil && obj.Parent.ID != "" {
		local, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		id, err := NewImporter(a.st, nil).ArchiveReply(local, t.App, t.Asset, obj, t.RootID)
		if err != nil {
			return result, actionFailure("remote_accepted_local_failed", result, err)
		}
		result.Receipt.ReplyMessageID, result.Receipt.ArchiveComplete = id, true
	}
	return result, nil
}
