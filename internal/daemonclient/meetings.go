package daemonclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/meetingcontent"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetMeetingContext renders a deterministic meeting context packet through
// the daemon. The generated request retains omitted versus explicit-empty IDs.
func (c *Client) GetMeetingContext(
	ctx context.Context,
	body generated.GetMeetingContextBody,
) (*meetingcontent.PacketResult, error) {
	response, err := APIResponse(c,
		func(client *apiclient.Client) (*generated.GetMeetingContextResp, error) {
			return client.GetMeetingContextWithResponse(ctx,
				&generated.GetMeetingContextRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("get meeting context: empty response")
	}
	return decodeMeetingResponse[meetingcontent.PacketResult]("get meeting context", response.Body)
}

// ListMeetingActionItems lists source-reported meeting actions through the
// daemon, including its filter-bound continuation cursor and scope coverage.
func (c *Client) ListMeetingActionItems(
	ctx context.Context,
	body generated.ListMeetingActionItemsBody,
) (*meetingcontent.ActionsPage, error) {
	response, err := APIResponse(c,
		func(client *apiclient.Client) (*generated.ListMeetingActionItemsResp, error) {
			return client.ListMeetingActionItemsWithResponse(ctx,
				&generated.ListMeetingActionItemsRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("list meeting action items: empty response")
	}
	return decodeMeetingResponse[meetingcontent.ActionsPage]("list meeting action items", response.Body)
}

// GetMeetingMetrics returns duration coverage and monthly totals from the
// daemon. Decoding the wire response directly preserves nullable averages and
// nil-versus-empty collections from the reviewed HTTP contract.
func (c *Client) GetMeetingMetrics(
	ctx context.Context,
	body generated.GetMeetingMetricsBody,
) (*meetingcontent.Metrics, error) {
	response, err := APIResponse(c,
		func(client *apiclient.Client) (*generated.GetMeetingMetricsResp, error) {
			return client.GetMeetingMetricsWithResponse(ctx,
				&generated.GetMeetingMetricsRequestOptions{Body: &body})
		})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("get meeting metrics: empty response")
	}
	return decodeMeetingResponse[meetingcontent.Metrics]("get meeting metrics", response.Body)
}

func decodeMeetingResponse[T any](operation string, body []byte) (*T, error) {
	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("%s: decode response: %w", operation, err)
	}
	return &result, nil
}
