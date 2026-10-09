package daemonclient

import (
	"context"

	"go.kenn.io/msgvault/internal/accountops"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) MergeCLIAccounts(ctx context.Context, req accountops.MergeRequest) (*store.SourceMergeResult, error) {
	body := generated.MergeCLIAccountsBody{DryRun: req.DryRun}
	if req.From != "" {
		body.From = &req.From
	}
	if req.Into != "" {
		body.Into = &req.Into
	}
	if req.FromSourceIDSet || req.FromSourceID != 0 {
		body.FromSourceID = &req.FromSourceID
	}
	if req.IntoSourceIDSet || req.IntoSourceID != 0 {
		body.IntoSourceID = &req.IntoSourceID
	}
	resp, err := CLIResponse(c, func(client *apiclient.Client) (*generated.MergeCLIAccountsResp, error) {
		return client.MergeCLIAccountsWithResponse(ctx, &generated.MergeCLIAccountsRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	r := resp.JSON200
	return &store.SourceMergeResult{
		FromSourceID: r.FromSourceID, IntoSourceID: r.IntoSourceID,
		MessagesMoved: r.MessagesMoved, DuplicatesHidden: r.DuplicatesHidden,
		AmbiguousMatches: r.AmbiguousMatches, ConversationsMoved: r.ConversationsMoved,
		AttachmentsCopied: r.AttachmentsCopied, IdentitiesMerged: r.IdentitiesMerged,
		CheckpointConflicts: r.CheckpointConflicts, AlreadyMerged: r.AlreadyMerged, DryRun: r.DryRun,
	}, nil
}
