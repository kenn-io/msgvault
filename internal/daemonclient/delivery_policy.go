package daemonclient

import (
	"context"
	"encoding/json/v2"
	"errors"
	"go.kenn.io/msgvault/internal/store"
	apiclient "go.kenn.io/msgvault/pkg/client"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// GetDeliveryPolicy uses the generated contract. The daemon independently
// rejects delegated credentials; a client header cannot confer owner authority.
func (c *Client) GetDeliveryPolicy(ctx context.Context, q store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	body, err := deliveryClientBody[generated.GetDeliveryPolicyBody](q)
	if err != nil {
		return nil, err
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.GetDeliveryPolicyResp, error) {
		return api.GetDeliveryPolicyWithResponse(ctx, &generated.GetDeliveryPolicyRequestOptions{Body: body})
	})
	if err != nil {
		return nil, deliveryClientError(err)
	}
	var result store.DeliveryPolicyState
	if response.JSON200 == nil {
		return nil, errors.New("delivery policy response was empty")
	}
	err = json.Unmarshal(response.Body, &result)
	return &result, err
}
func (c *Client) SetDeliveryPolicy(ctx context.Context, w store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	body, err := deliveryClientBody[generated.SetDeliveryPolicyBody](w)
	if err != nil {
		return nil, err
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.SetDeliveryPolicyResp, error) {
		return api.SetDeliveryPolicyWithResponse(ctx, &generated.SetDeliveryPolicyRequestOptions{Body: body, Header: &generated.SetDeliveryPolicyHeaders{XMsgvaultDeliveryPolicyWrite: generated.SetDeliveryPolicyHeaderXMsgvaultDeliveryPolicyWriteTrue}})
	})
	if err != nil {
		return nil, deliveryClientError(err)
	}
	var result store.DeliveryPolicyReceipt
	if response.JSON200 == nil {
		return nil, errors.New("delivery policy response was empty")
	}
	err = json.Unmarshal(response.Body, &result)
	return &result, err
}
func (c *Client) ClearDeliveryPolicy(ctx context.Context, w store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	body, err := deliveryClientBody[generated.ClearDeliveryPolicyBody](w)
	if err != nil {
		return nil, err
	}
	response, err := APIResponse(c, func(api *apiclient.Client) (*generated.ClearDeliveryPolicyResp, error) {
		return api.ClearDeliveryPolicyWithResponse(ctx, &generated.ClearDeliveryPolicyRequestOptions{Body: body, Header: &generated.ClearDeliveryPolicyHeaders{XMsgvaultDeliveryPolicyWrite: "true"}})
	})
	if err != nil {
		return nil, deliveryClientError(err)
	}
	var result store.DeliveryPolicyReceipt
	if response.JSON200 == nil {
		return nil, errors.New("delivery policy response was empty")
	}
	err = json.Unmarshal(response.Body, &result)
	return &result, err
}
func deliveryClientBody[T any](input any) (*T, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	var result T
	err = json.Unmarshal(b, &result)
	return &result, err
}

type DeliveryPolicyError struct {
	Code, Message string
	Status        int
}

func (e *DeliveryPolicyError) Error() string { return e.Code + ": " + e.Message }
func (e *DeliveryPolicyError) Unwrap() error {
	return &APIError{Code: e.Code, Message: e.Message, Status: e.Status}
}
func deliveryClientError(err error) error {
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		return &DeliveryPolicyError{Code: apiErr.Code, Message: apiErr.Message, Status: apiErr.Status}
	}
	return err
}
