package daemonclient

import (
	"context"
	"errors"
	"time"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// LoopNotFoundCode is the daemon's error code for a message that anchors no
// open loop.
const LoopNotFoundCode = "loop_not_found"

// LoopDecisionNotFoundCode is returned when a message has no decision to clear.
const LoopDecisionNotFoundCode = "loop_decision_not_found"

// LoopListOptions selects loops. Zero values use the daemon's defaults.
type LoopListOptions struct {
	PersonID      int64
	Limit         int
	IncludeClosed bool
}

// ListLoops returns the open loops, oldest first.
func (c *Client) ListLoops(ctx context.Context, opts LoopListOptions) (generated.LoopListResponse, error) {
	query := &generated.ListLoopsQuery{}
	if opts.IncludeClosed {
		query.IncludeClosed = &opts.IncludeClosed
	}
	if opts.PersonID != 0 {
		query.PersonID = &opts.PersonID
	}
	if opts.Limit != 0 {
		limit := int64(opts.Limit)
		query.Limit = &limit
	}
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ListLoopsResp, error) {
		return client.ListLoopsWithResponse(ctx, &generated.ListLoopsRequestOptions{Query: query})
	})
	if err != nil {
		return generated.LoopListResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.LoopListResponse{}, errors.New("empty open loops response")
	}
	return *resp.JSON200, nil
}

// MarkLoopDone marks the loop anchored at messageID done. Pass a list item's
// question IDs to mark only those; none covers every visible question.
func (c *Client) MarkLoopDone(ctx context.Context, messageID int64, questions []int64) (generated.LoopDecisionResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.MarkLoopDoneResp, error) {
		return client.MarkLoopDoneWithResponse(ctx, &generated.MarkLoopDoneRequestOptions{
			PathParams: &generated.MarkLoopDonePath{MessageID: messageID},
			Query:      &generated.MarkLoopDoneQuery{QuestionMessageIds: questions},
		})
	})
	if err != nil {
		return generated.LoopDecisionResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.LoopDecisionResponse{}, errors.New("empty open loop decision response")
	}
	return *resp.JSON200, nil
}

// SnoozeLoop hides the same questions as MarkLoopDone until the given time.
func (c *Client) SnoozeLoop(ctx context.Context, messageID int64, questions []int64, until time.Time) (generated.LoopDecisionResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SnoozeLoopResp, error) {
		return client.SnoozeLoopWithResponse(ctx, &generated.SnoozeLoopRequestOptions{
			PathParams: &generated.SnoozeLoopPath{MessageID: messageID},
			Query:      &generated.SnoozeLoopQuery{QuestionMessageIds: questions},
			Body:       &generated.SnoozeLoopBody{Until: until},
		})
	})
	if err != nil {
		return generated.LoopDecisionResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.LoopDecisionResponse{}, errors.New("empty open loop decision response")
	}
	return *resp.JSON200, nil
}

// ReopenLoop clears a done or snooze decision on the question at messageID.
func (c *Client) ReopenLoop(ctx context.Context, messageID int64) (generated.LoopDecisionResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.ReopenLoopResp, error) {
		return client.ReopenLoopWithResponse(ctx, &generated.ReopenLoopRequestOptions{
			PathParams: &generated.ReopenLoopPath{MessageID: messageID},
		})
	})
	if err != nil {
		return generated.LoopDecisionResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.LoopDecisionResponse{}, errors.New("empty open loop decision response")
	}
	return *resp.JSON200, nil
}

// SetCommitmentDecision updates a cited promise's owner state.
func (c *Client) SetCommitmentDecision(ctx context.Context, id, state string, until *time.Time) (generated.CommitmentDecisionResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SetCommitmentDecisionResp, error) {
		return client.SetCommitmentDecisionWithResponse(ctx, &generated.SetCommitmentDecisionRequestOptions{
			PathParams: &generated.SetCommitmentDecisionPath{CommitmentID: id},
			Body:       &generated.SetCommitmentDecisionBody{State: generated.CommitmentDecisionRequestState(state), Until: until},
		})
	})
	if err != nil {
		return generated.CommitmentDecisionResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.CommitmentDecisionResponse{}, errors.New("empty commitment decision response")
	}
	return *resp.JSON200, nil
}

// CreateCommitmentAgendaIssue creates or finds the cited task for a promise.
func (c *Client) CreateCommitmentAgendaIssue(ctx context.Context, id string) (generated.CommitmentAgendaResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.CreateCommitmentAgendaIssueResp, error) {
		return client.CreateCommitmentAgendaIssueWithResponse(ctx, &generated.CreateCommitmentAgendaIssueRequestOptions{
			PathParams: &generated.CreateCommitmentAgendaIssuePath{CommitmentID: id},
		})
	})
	if err != nil {
		return generated.CommitmentAgendaResponse{}, err
	}
	if resp.JSON200 == nil {
		return generated.CommitmentAgendaResponse{}, errors.New("empty commitment agenda issue response")
	}
	return *resp.JSON200, nil
}
