package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"

	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

var errDeliveryPolicyResponse = errors.New("daemon returned no delivery policy response")

// GetDeliveryPolicy uses the generated contract. The daemon independently
// rejects delegated credentials; a client header cannot confer owner authority.
func (c *Client) GetDeliveryPolicy(ctx context.Context, q store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetDeliveryPolicyResp, error) {
		return api.GetDeliveryPolicyWithResponse(ctx, &generated.GetDeliveryPolicyRequestOptions{
			Body: &generated.GetDeliveryPolicyBody{PersonUID: q.PersonUID, Endpoint: optionalString(q.Endpoint)},
		})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errDeliveryPolicyResponse
	}
	state := &store.DeliveryPolicyState{}
	if err := json.Unmarshal(response.Body, state); err != nil {
		return nil, fmt.Errorf("decode delivery policy response: %w", err)
	}
	return state, nil
}

func (c *Client) SetDeliveryPolicy(ctx context.Context, w store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.SetDeliveryPolicyResp, error) {
		return api.SetDeliveryPolicyWithResponse(ctx, &generated.SetDeliveryPolicyRequestOptions{Body: deliveryPolicyBody(w)})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errDeliveryPolicyResponse
	}
	receipt := &store.DeliveryPolicyReceipt{}
	if err := json.Unmarshal(response.Body, receipt); err != nil {
		return nil, fmt.Errorf("decode set delivery policy response: %w", err)
	}
	return receipt, nil
}

func (c *Client) ClearDeliveryPolicy(ctx context.Context, w store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ClearDeliveryPolicyResp, error) {
		return api.ClearDeliveryPolicyWithResponse(ctx, &generated.ClearDeliveryPolicyRequestOptions{Body: deliveryPolicyBody(w)})
	})
	if err != nil {
		return nil, err
	}
	if response.JSON200 == nil {
		return nil, errDeliveryPolicyResponse
	}
	receipt := &store.DeliveryPolicyReceipt{}
	if err := json.Unmarshal(response.Body, receipt); err != nil {
		return nil, fmt.Errorf("decode clear delivery policy response: %w", err)
	}
	return receipt, nil
}

// deliveryPolicyBody builds the one request shape that set and clear share.
func deliveryPolicyBody(w store.DeliveryPolicyWrite) *generated.DeliveryPolicyWrite {
	return &generated.DeliveryPolicyWrite{
		PersonUID: w.PersonUID, Endpoint: optionalString(w.Endpoint), ExpectedRevision: w.ExpectedRevision,
		Policy: optionalString(string(w.Policy)), ScopeAcknowledgement: optionalString(w.ScopeAcknowledgement), Reason: w.Reason,
	}
}
