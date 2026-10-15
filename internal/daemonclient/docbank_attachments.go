package daemonclient

import (
	"context"
	"net/http"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) DocbankAttachmentStatus(ctx context.Context) (*generated.DocbankAttachmentStatus, error) {
	resp, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetDocbankAttachmentStatusResp, error) {
		return api.GetDocbankAttachmentStatusWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	return resp.JSON200, nil
}

func (c *Client) BackfillDocbankAttachments(ctx context.Context) error {
	_, err := APIResponseWithStatuses(c, []int{http.StatusAccepted}, func(api *apiclient.Client) (*generated.BackfillDocbankAttachmentsResp, error) {
		return api.BackfillDocbankAttachmentsWithResponse(ctx)
	})
	return err
}
