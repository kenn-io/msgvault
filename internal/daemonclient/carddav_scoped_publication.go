package daemonclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// PreviewScopedCardDAVPublication reads the exact preview using the current
// caller's person and address-book grant; it never selects the owner route.
func (c *Client) PreviewScopedCardDAVPublication(ctx context.Context, id int64) (*generated.CardDAVPublicationPreviewResponse, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.PreviewScopedCardDAVPublicationResp, error) {
		return api.PreviewScopedCardDAVPublicationWithResponse(ctx, &generated.PreviewScopedCardDAVPublicationRequestOptions{PathParams: &generated.PreviewScopedCardDAVPublicationPath{PersonID: id}})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("scoped CardDAV preview response was empty")
	}
	return response.JSON200, nil
}

// ApproveScopedCardDAVPublication preserves the reviewed token and original
// idempotency key on retries. A dispatching receipt does not prove publication.
func (c *Client) ApproveScopedCardDAVPublication(ctx context.Context, id int64, token, key string) (*generated.CardDAVScopedPublicationReceiptResponse, error) {
	response, err := APIResponseWithStatuses(c, []int{http.StatusOK, http.StatusAccepted}, func(api *apiclient.Client) (*generated.ApproveScopedCardDAVPublicationResp, error) {
		return api.ApproveScopedCardDAVPublicationWithResponse(ctx, &generated.ApproveScopedCardDAVPublicationRequestOptions{
			PathParams: &generated.ApproveScopedCardDAVPublicationPath{PersonID: id},
			Body:       &generated.ApproveScopedCardDAVPublicationBody{ApprovalToken: token, IdempotencyKey: key},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 != nil {
		return response.JSON200, nil
	}
	if response.JSON202 != nil {
		return response.JSON202, nil
	}
	return nil, errors.New("scoped CardDAV approval receipt was empty")
}

// ReconcileScopedCardDAVPublication observes the original receipt through the
// GET-only recovery route. It does not issue another remote update.
func (c *Client) ReconcileScopedCardDAVPublication(ctx context.Context, id int64, token, key string) (*generated.CardDAVScopedPublicationReceiptResponse, error) {
	response, err := APIResponseWithStatuses(c, []int{http.StatusOK, http.StatusAccepted}, func(api *apiclient.Client) (*generated.ReconcileScopedCardDAVPublicationResp, error) {
		return api.ReconcileScopedCardDAVPublicationWithResponse(ctx, &generated.ReconcileScopedCardDAVPublicationRequestOptions{
			PathParams: &generated.ReconcileScopedCardDAVPublicationPath{PersonID: id},
			Body:       &generated.ReconcileScopedCardDAVPublicationBody{ApprovalToken: token, IdempotencyKey: key},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 != nil {
		return response.JSON200, nil
	}
	if response.JSON202 != nil {
		return response.JSON202, nil
	}
	return nil, errors.New("scoped CardDAV recovery receipt was empty")
}

// GetScopedCardDAVPublicationReceipt reads a retained receipt with the current
// owner credential, even after the delegated grant or source resources are gone.
func (c *Client) GetScopedCardDAVPublicationReceipt(ctx context.Context, id string) (*generated.CardDAVScopedPublicationReceiptResponse, error) {
	receiptID, err := uuid.Parse(id)
	if err != nil {
		return nil, fmt.Errorf("parse scoped CardDAV publication receipt ID: %w", err)
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetScopedCardDAVPublicationReceiptResp, error) {
		return api.GetScopedCardDAVPublicationReceiptWithResponse(ctx, &generated.GetScopedCardDAVPublicationReceiptRequestOptions{
			PathParams: &generated.GetScopedCardDAVPublicationReceiptPath{ReceiptID: receiptID},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("scoped CardDAV publication receipt response was empty")
	}
	return response.JSON200, nil
}
