package daemonclient

import (
	"context"
	"errors"

	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func (c *Client) GetMailKindStatus(ctx context.Context) (*generated.MailKindStatus, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetMailKindStatusResp, error) {
		return api.GetMailKindStatusWithResponse(ctx)
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("mail kind status response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) GrantMailKindConsent(ctx context.Context, fingerprint string) (*generated.MailKindConsentResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.MailKindConsentResp, error) {
		return api.MailKindConsentWithResponse(ctx, &generated.MailKindConsentRequestOptions{
			Body: &generated.MailKindConsentRequest{DisclosureFingerprint: fingerprint},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("mail kind consent response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) RevokeMailKindConsent(ctx context.Context, fingerprint string) (*generated.MailKindConsentResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.MailKindRevokeResp, error) {
		return api.MailKindRevokeWithResponse(ctx, &generated.MailKindRevokeRequestOptions{
			Body: &generated.MailKindConsentRequest{DisclosureFingerprint: fingerprint},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("mail kind revoke response was empty")
	}
	return response.JSON200, nil
}

func (c *Client) ClassifyMailKind(ctx context.Context, fingerprint string, limit *int64, ids []int64) (*generated.MailKindRunResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ClassifyMailKindResp, error) {
		return api.ClassifyMailKindWithResponse(ctx, &generated.ClassifyMailKindRequestOptions{
			Body: &generated.MailKindRunRequest{DisclosureFingerprint: fingerprint, Limit: limit, MessageIds: ids},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("mail kind classification response was empty")
	}
	return response.JSON200, nil
}
