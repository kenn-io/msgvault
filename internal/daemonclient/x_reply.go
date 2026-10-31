package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	xapi "go.kenn.io/msgvault/internal/x"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// XReplySendMinAPISchemaVersion uses the same unreleased X API minor as
// ingestion. Health is the capability contract; older daemons cannot send.
const XReplySendMinAPISchemaVersion = "3.29.0"

func (c *Client) requireXReplySend(ctx context.Context) error {
	supported, err := c.SupportsAPISchemaVersion(ctx, XReplySendMinAPISchemaVersion)
	if err != nil {
		return fmt.Errorf("check daemon x_reply_send capability: %w", err)
	}
	if !supported {
		return fmt.Errorf("x_reply_send requires daemon API schema %s or newer; upgrade the selected daemon", XReplySendMinAPISchemaVersion)
	}
	return nil
}

// PrepareReply returns owner-only review data through the authenticated daemon.
// Adoption can perform a separately budgeted lookup; this method never posts.
func (c *Client) PrepareReply(ctx context.Context, draftID string, revision int64, adoptPost string) (xapi.ReplyPreparation, error) {
	if err := c.requireXReplySend(ctx); err != nil {
		return xapi.ReplyPreparation{}, err
	}
	body := generated.PrepareReplyBody{DraftID: draftID, Revision: revision, AdoptPost: optionalString(adoptPost)}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.PrepareReplyResp, error) {
		return client.PrepareReplyWithResponse(ctx, &generated.PrepareReplyRequestOptions{Body: &body})
	})
	if err != nil {
		return xapi.ReplyPreparation{}, err
	}
	if response.JSON200 == nil {
		return xapi.ReplyPreparation{}, errors.New("empty reply preparation response")
	}
	var result xapi.ReplyPreparation
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return result, fmt.Errorf("decode reply preparation: %w", err)
	}
	return result, nil
}
