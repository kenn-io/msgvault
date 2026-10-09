package daemonclient

import (
	"context"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) DocbankAttachmentStatus(ctx context.Context) (*generated.DocbankAttachmentStatus, error) {
	resp, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetDocbankAttachmentStatusResp, error) {
		return api.GetDocbankAttachmentStatusWithResponse(ctx)
	})
	if err := APIResponseError(resp, err); err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}

func (c *Client) BackfillDocbankAttachments(ctx context.Context) error {
	resp, err := APIResponse(c, func(api *apiclient.Client) (*generated.BackfillDocbankAttachmentsResp, error) {
		return api.BackfillDocbankAttachmentsWithResponse(ctx)
	})
	return APIResponseError(resp, err)
}
