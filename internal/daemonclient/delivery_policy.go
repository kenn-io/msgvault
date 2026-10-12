package daemonclient

import (
	"context"
	"errors"

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
	state := deliveryPolicyState(*response.JSON200)
	return &state, nil
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
	return deliveryPolicyReceipt(*response.JSON200), nil
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
	return deliveryPolicyReceipt(*response.JSON200), nil
}

// deliveryPolicyBody builds the one request shape that set and clear share.
func deliveryPolicyBody(w store.DeliveryPolicyWrite) *generated.DeliveryPolicyWrite {
	return &generated.DeliveryPolicyWrite{
		PersonUID: w.PersonUID, Endpoint: optionalString(w.Endpoint), ExpectedRevision: w.ExpectedRevision,
		Policy: optionalString(string(w.Policy)), ScopeAcknowledgement: optionalString(w.ScopeAcknowledgement), Reason: w.Reason,
	}
}

func deliveryPolicyPtr(value *string) *store.DeliveryPolicy {
	if value == nil {
		return nil
	}
	return new(store.DeliveryPolicy(*value))
}

func deliveryPolicyState(in generated.DeliveryPolicyState) store.DeliveryPolicyState {
	out := store.DeliveryPolicyState{
		PersonUID: in.PersonUID, Revision: in.Revision,
		StoredPolicy: deliveryPolicyPtr(in.StoredPolicy), EffectivePolicy: store.DeliveryPolicy(in.EffectivePolicy),
		InheritanceSource: in.InheritanceSource, Reason: in.Reason,
	}
	if in.Endpoint != nil {
		out.Endpoint = *in.Endpoint
	}
	return out
}

func deliveryPolicyReceipt(in generated.DeliveryPolicyReceipt) *store.DeliveryPolicyReceipt {
	out := &store.DeliveryPolicyReceipt{Before: deliveryPolicyState(in.Before), After: deliveryPolicyState(in.After)}
	if a := in.Audit; a != nil {
		out.Audit = &store.DeliveryPolicyAudit{
			Actor: a.Actor, Reason: a.Reason, CreatedAt: a.CreatedAt, Scope: a.Scope,
			BeforePolicy: deliveryPolicyPtr(a.BeforePolicy), AfterPolicy: deliveryPolicyPtr(a.AfterPolicy),
			ContactPointID: a.ContactPointID,
		}
	}
	return out
}
