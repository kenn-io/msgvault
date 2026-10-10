package daemonclient

import (
	"context"
	"errors"
	"net/http"

	"go.kenn.io/msgvault/internal/identitycontrol"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// IdentityOutcomeUnknownError requires read-only receipt reconciliation before
// deciding whether an unacknowledged identity write needs another attempt.
type IdentityOutcomeUnknownError struct {
	IdempotencyKey string
	Cause          error
}

func (e *IdentityOutcomeUnknownError) Error() string {
	return "identity outcome unknown; read the receipt using the original idempotency key"
}

func (e *IdentityOutcomeUnknownError) Unwrap() error { return e.Cause }

// PreviewIdentityOperation reads the native scope and signs one exact intent.
func (c *Client) PreviewIdentityOperation(ctx context.Context, request identitycontrol.PreviewRequest) (*generated.IdentityOperationPreviewResponse, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	target := generated.IdentityTarget{ParticipantID: request.Target.ParticipantID}
	if request.Target.OtherParticipantID != 0 {
		target.OtherParticipantID = &request.Target.OtherParticipantID
	}
	if request.Target.PersonID != 0 {
		target.PersonID = &request.Target.PersonID
	}
	body := generated.IdentityPreviewRequest{Operation: generated.IdentityPreviewRequestOperation(request.Operation), Target: target}
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.PreviewIdentityOperationResp, error) {
		return client.PreviewIdentityOperationWithResponse(ctx, &generated.PreviewIdentityOperationRequestOptions{Body: &body})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity preview response was empty")
	}
	return response.JSON200, nil
}

// ApplyIdentityOperation submits one preview and preserves its native receipt.
func (c *Client) ApplyIdentityOperation(ctx context.Context, request generated.IdentityOperationApplyRequest) (*generated.IdentityOperationApplyResponse, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	// Bind this mutation to the reviewed daemon endpoint even for owner clients.
	// Clone the transport settings so other requests retain their own policy.
	operationClient := *c
	httpClient := *c.httpClient
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	operationClient.httpClient = &httpClient
	operationClient.typedClient = nil
	response, err := APIResponse(&operationClient, func(client *apiclient.Client) (*generated.ApplyIdentityOperationResp, error) {
		return client.ApplyIdentityOperationWithResponse(ctx, &generated.ApplyIdentityOperationRequestOptions{Body: &request})
	})
	if err != nil {
		if apiError, ok := errors.AsType[*APIError](err); !ok || apiError.Status >= http.StatusInternalServerError {
			return nil, &IdentityOutcomeUnknownError{IdempotencyKey: request.IdempotencyKey, Cause: err}
		}
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, &IdentityOutcomeUnknownError{IdempotencyKey: request.IdempotencyKey, Cause: errors.New("identity apply response was empty")}
	}
	return response.JSON200, nil
}

// GetIdentityOperationReceipt reads a committed outcome without replaying it.
func (c *Client) GetIdentityOperationReceipt(ctx context.Context, key string) (*generated.IdentityReceipt, error) {
	return c.identityOperationReceipt(ctx, generated.GetIdentityOperationReceiptQuery{IdempotencyKey: &key})
}

// GetIdentityOperationReceiptByID is an owner recovery path after grant expiry.
func (c *Client) GetIdentityOperationReceiptByID(ctx context.Context, id string) (*generated.IdentityReceipt, error) {
	return c.identityOperationReceipt(ctx, generated.GetIdentityOperationReceiptQuery{ReceiptID: &id})
}

// GetIdentityOperationReceiptForPrincipal lets the owner recover a receipt
// after its original delegated principal has expired or been revoked.
func (c *Client) GetIdentityOperationReceiptForPrincipal(ctx context.Context, principal, key string) (*generated.IdentityReceipt, error) {
	return c.identityOperationReceipt(ctx, generated.GetIdentityOperationReceiptQuery{Principal: &principal, IdempotencyKey: &key})
}

func (c *Client) identityOperationReceipt(ctx context.Context, query generated.GetIdentityOperationReceiptQuery) (*generated.IdentityReceipt, error) {
	response, err := APIResponse(c, func(client *apiclient.Client) (*generated.GetIdentityOperationReceiptResp, error) {
		return client.GetIdentityOperationReceiptWithResponse(ctx, &generated.GetIdentityOperationReceiptRequestOptions{Query: &query})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errors.New("identity receipt response was empty")
	}
	return response.JSON200, nil
}
