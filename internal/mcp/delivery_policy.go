package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/store"
)

const (
	ToolGetDeliveryPolicy   = "get_delivery_policy"
	ToolSetDeliveryPolicy   = "set_delivery_policy"
	ToolClearDeliveryPolicy = "clear_delivery_policy"
)

// DeliveryPolicyBackend is owner-only and separate from ordinary draft tools.
// The daemon is the authority; MCP never invents a route or grants itself policy
// write permission. Each mutation additionally requires client confirmation.
type DeliveryPolicyBackend interface {
	GetDeliveryPolicy(ctx context.Context, query store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error)
	SetDeliveryPolicy(ctx context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error)
	ClearDeliveryPolicy(ctx context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error)
}

var stableDeliveryPolicyDefinitions = []toolDefinition{
	readDefinition(ToolGetDeliveryPolicy, "Inspect stored and effective contact delivery policy, inheritance, saved address, delivery policy revision and reason. Use the canonical person UID and an exact saved email address; omit endpoint to inspect the person default. Archive proof never establishes live reachability or send permission.", outputSchemaFor[store.DeliveryPolicyQuery](), outputSchemaFor[store.DeliveryPolicyState](), (*handlers).getDeliveryPolicy),
	explicitlyConfirmedWriteDefinition(ToolSetDeliveryPolicy, "Explicit owner-authorized policy edit after user confirmation. Distinct from drafting/sending. Requires expected_revision, the delivery policy revision from get_delivery_policy, which is not the person profile revision; send_allowed on an address needs an active declared email contact point of the person that no other person holds. A person-wide send_allowed must acknowledge person_all_routes, a broader scope. send_allowed permits explicit send admission only; drafting remains available, provider authorization and platform confirmation still apply.", outputSchemaFor[store.DeliveryPolicyWrite](), outputSchemaFor[store.DeliveryPolicyReceipt](), (*handlers).setDeliveryPolicy, toolSecurityDeliveryPolicyWrite),
	explicitlyConfirmedWriteDefinition(ToolClearDeliveryPolicy, "Clear an address policy to inheritance after explicit owner confirmation. Omit endpoint to reset the person-wide default to draft_only. Requires expected_revision, the delivery policy revision from get_delivery_policy, and a reason. Never sends or changes existing drafts.", outputSchemaFor[store.DeliveryPolicyWrite](), outputSchemaFor[store.DeliveryPolicyReceipt](), (*handlers).clearDeliveryPolicy, toolSecurityDeliveryPolicyWrite),
}

func decodeDeliveryArguments(req toolRequest, dst any) error {
	b, err := json.Marshal(req.GetArguments())
	if err != nil {
		return store.ErrDeliveryPolicyInvalid
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return store.ErrDeliveryPolicyInvalid
	}
	return nil
}

func (h *handlers) getDeliveryPolicy(ctx context.Context, req toolRequest) (*toolResult, error) {
	var query store.DeliveryPolicyQuery
	if err := decodeDeliveryArguments(req, &query); err != nil {
		return toolErrorResult("invalid_policy: " + err.Error()), nil //nolint:nilerr // MCP tool errors are successful protocol responses.
	}
	state, err := h.deliveryPolicies.GetDeliveryPolicy(ctx, query)
	return mcpDeliveryPolicyResult(state, err)
}

func (h *handlers) setDeliveryPolicy(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.writeDeliveryPolicy(ctx, req, false)
}

func (h *handlers) clearDeliveryPolicy(ctx context.Context, req toolRequest) (*toolResult, error) {
	return h.writeDeliveryPolicy(ctx, req, true)
}

func (h *handlers) writeDeliveryPolicy(ctx context.Context, req toolRequest, reset bool) (*toolResult, error) {
	var write store.DeliveryPolicyWrite
	if err := decodeDeliveryArguments(req, &write); err != nil {
		return toolErrorResult("invalid_policy: " + err.Error()), nil //nolint:nilerr // MCP tool errors are successful protocol responses.
	}
	// Bind confirmation to the full exact request, including revision and scope.
	details, err := json.Marshal(write)
	if err != nil {
		return toolErrorResult("invalid_policy"), nil //nolint:nilerr // MCP tool errors are successful protocol responses.
	}
	if err := req.confirmUserAction(ctx, fmt.Sprintf("Confirm delivery policy operation (clear=%t): %s. No message will be sent.", reset, details)); err != nil {
		return confirmationToolError(err)
	}
	var receipt *store.DeliveryPolicyReceipt
	if reset {
		receipt, err = h.deliveryPolicies.ClearDeliveryPolicy(ctx, write)
	} else {
		receipt, err = h.deliveryPolicies.SetDeliveryPolicy(ctx, write)
	}
	return mcpDeliveryPolicyResult(receipt, err)
}

func mcpDeliveryPolicyResult[T any](value *T, err error) (*toolResult, error) {
	if err != nil {
		return toolErrorResult(daemonclient.SafeMCPError(err).Error()), nil
	}
	return jsonResult(value)
}
