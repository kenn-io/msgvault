package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/calcontrol"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// ControlCalendar uses the generated authenticated request path. It preserves
// omitted patch fields and returns complete remote/archive receipts unchanged.
func (c *Client) ControlCalendar(ctx context.Context, request calcontrol.Request) (*calcontrol.Result, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode calendar request: %w", err)
	}
	var body generated.ControlCalendarBody
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, fmt.Errorf("build calendar request: %w", err)
	}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.ControlCalendarResp, error) {
		return client.ControlCalendarWithResponse(ctx, &generated.ControlCalendarRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("calendar control: empty response")
	}
	var result calcontrol.Result
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, fmt.Errorf("decode calendar control: %w", err)
	}
	return &result, nil
}
