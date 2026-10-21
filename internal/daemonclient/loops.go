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

// LoopListOptions selects loops. Zero values use the daemon's defaults.
type LoopListOptions struct {
	Limit int
}

// ListLoops returns the open loops, oldest first.
func (c *Client) ListLoops(ctx context.Context, opts LoopListOptions) (generated.LoopListResponse, error) {
	query := &generated.ListLoopsQuery{}
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

// MarkLoopDone marks the loop anchored at messageID done.
func (c *Client) MarkLoopDone(ctx context.Context, messageID int64) (generated.LoopDecisionResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.MarkLoopDoneResp, error) {
		return client.MarkLoopDoneWithResponse(ctx, &generated.MarkLoopDoneRequestOptions{
			PathParams: &generated.MarkLoopDonePath{MessageID: messageID},
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

// SnoozeLoop hides the loop anchored at messageID until the given time.
func (c *Client) SnoozeLoop(ctx context.Context, messageID int64, until time.Time) (generated.LoopDecisionResponse, error) {
	resp, err := APIResponse(c, func(client *apiclient.Client) (*generated.SnoozeLoopResp, error) {
		return client.SnoozeLoopWithResponse(ctx, &generated.SnoozeLoopRequestOptions{
			PathParams: &generated.SnoozeLoopPath{MessageID: messageID},
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
