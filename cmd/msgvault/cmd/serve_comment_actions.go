package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/meta"
	"go.kenn.io/msgvault/internal/store"
)

var _ api.CommentActionStore = (*storeAPIAdapter)(nil)

func commentFailure(code string, cause error) *meta.ActionError {
	return &meta.ActionError{Code: code, Message: "Comment action: " + code, Cause: cause}
}
func (a *storeAPIAdapter) commentProfile(ctx context.Context, sourceID int64) (*config.FacebookCommentsSource, error) {
	src, err := a.store.GetSourceByIDContext(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	app, page, ok := meta.ParseSourceIdentifier(src.Identifier)
	if !ok || src.SourceType != meta.SourceType || a.config == nil {
		return nil, nil //nolint:nilnil // An unconfigured comment profile is a valid absence state.
	}
	return a.config.FacebookCommentsSourceFor(app, page), nil
}
func (a *storeAPIAdapter) RunCommentAction(ctx context.Context, id int64, req meta.ActionRequest, acquireWrite func(context.Context) (func(), error)) (*meta.ActionResult, error) {
	ctx = a.invocationContext(ctx)
	req.MessageID = id
	if !req.DryRun && acquireWrite != nil {
		release, err := acquireWrite(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	actions := meta.NewActions(a.store, meta.ActionsOptions{
		Allowed: func(id int64, kind meta.ActionKind) (bool, error) {
			profile, err := a.commentProfile(ctx, id)
			return profile != nil && profile.ActionAllowed(string(kind)), err
		},
		OpenAPI: func(ctx context.Context, src *store.Source) (meta.ActionAPI, error) {
			profile, err := a.commentProfile(ctx, src.ID)
			if err != nil {
				return nil, err
			}
			if profile == nil {
				return nil, errors.New("no matching comment profile")
			}
			client, err := configuredFacebookCommentsClient(*profile)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
	})
	result, err := actions.Run(ctx, req)
	if result != nil && result.Receipt != nil && result.Receipt.State == "succeeded" {
		local, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		if source, e := a.store.GetSourceByIDContext(local, result.SourceID); e == nil {
			a.refreshDraftCache(local, source)
		}
	}
	return result, err
}
func (a *storeAPIAdapter) ListCommentActions(ctx context.Context, id int64) ([]meta.Receipt, error) {
	m, err := a.store.MetaCommentTargetContext(ctx, id)
	if errors.Is(err, store.ErrMessageNotFound) {
		return nil, commentFailure("message_not_found", err)
	}
	if err != nil {
		return nil, err
	}
	if m.MessageType != meta.SourceType || m.ParentMessageID == 0 {
		return nil, commentFailure("not_a_comment", nil)
	}
	rows, err := a.store.ListMessageMetaCommentActionsContext(ctx, id)
	if err != nil {
		return nil, err
	}
	result := make([]meta.Receipt, 0, len(rows))
	for _, row := range rows {
		result = append(result, meta.Receipt{Action: meta.ActionKind(row.Action), ErrorCode: row.ErrorCode, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, ID: row.ID, State: row.State, RemoteID: row.RemoteID, RecipientID: row.RemoteRecipientID, ArchiveComplete: row.ArchiveComplete, ReplyMessageID: row.ReplyMessageID.Int64})
	}
	return result, nil
}
func (a *storeAPIAdapter) UpdateCommentTriage(ctx context.Context, id int64, state, expect string, acquireWrite func(context.Context) (func(), error)) (string, error) {
	if acquireWrite != nil {
		release, err := acquireWrite(ctx)
		if err != nil {
			return "", err
		}
		defer release()
	}
	var expected *store.CommentTriageState
	if expect != "" {
		v := store.CommentTriageState(expect)
		expected = &v
	}
	got, err := a.store.SetCommentTriageContext(ctx, id, store.CommentTriageState(state), expected)
	if errors.Is(err, store.ErrCommentTriageConflict) {
		return "", commentFailure("triage_conflict", err)
	}
	if errors.Is(err, store.ErrCommentTriageInvalid) {
		return "", commentFailure("invalid_request", err)
	}
	if err == nil {
		local, cancel := localDraftEvidenceContext(ctx)
		defer cancel()
		if target, e := a.store.MetaCommentTargetContext(local, id); e == nil {
			if source, e := a.store.GetSourceByIDContext(local, target.SourceID); e == nil {
				a.refreshDraftCache(local, source)
			}
		}
	}
	return string(got), err
}
func (a *storeAPIAdapter) ListCommentQueue(ctx context.Context, f api.CommentQueueRequest) (*api.CommentQueueResponse, error) {
	if err := api.ValidateCommentQueueRequest(f); err != nil {
		return nil, err
	}
	result := &api.CommentQueueResponse{Items: []api.CommentQueueItem{}}
	filter := store.CommentQueueFilter{State: f.State, Limit: f.Limit, BeforeID: f.BeforeID}
	if f.Source != "" {
		sources, err := a.store.ListSourcesContext(ctx, meta.SourceType)
		if err != nil {
			return nil, err
		}
		for _, source := range sources {
			if source.Identifier == f.Source {
				filter.SourceID = source.ID
				break
			}
		}
		if filter.SourceID == 0 {
			return result, nil
		}
	}
	rows, err := a.store.ListCommentQueueContext(ctx, filter)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		var metadata struct {
			Ad     bool  `json:"ad"`
			Hidden *bool `json:"hidden"`
		}
		if err := json.Unmarshal([]byte(row.Metadata), &metadata); err != nil {
			return nil, commentFailure("local_failed", err)
		}
		moderation := "unknown"
		if metadata.Hidden != nil {
			moderation = "visible"
			if *metadata.Hidden {
				moderation = "hidden"
			}
		}
		actions, err := a.ListCommentActions(ctx, row.MessageID)
		if err != nil {
			return nil, err
		}
		result.Items = append(result.Items, api.CommentQueueItem{MessageID: row.MessageID, SourceID: row.SourceID, SourceIdentifier: row.SourceIdentifier, Provider: "facebook", ConversationID: row.ConversationID, CommentID: row.CommentID, ParentMessageID: row.ParentMessageID, Author: api.CommentQueueAuthor{ParticipantID: row.AuthorParticipantID, DisplayName: row.AuthorName}, Snippet: row.Snippet, SentAt: row.SentAt, Ad: metadata.Ad, Triage: string(row.Triage), Moderation: moderation, DeletedFromSource: row.DeletedFromSource, PageReplied: row.PageReplied, PrivateReplyOpenUntil: row.PrivateReplyOpenUntil, Actions: actions})
	}
	if len(result.Items) > 0 {
		result.NextBeforeID = result.Items[len(result.Items)-1].MessageID
	}
	return result, nil
}
