package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
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
	readDefinition(ToolGetDeliveryPolicy, "Inspect stored and effective contact delivery policy, inheritance, exact native target, revisions and reason. Use canonical person UID and an exact source/account/endpoint; omit target to inspect the broader person default. Archive proof never establishes live reachability or send permission.", outputSchemaFor[store.DeliveryPolicyQuery](), outputSchemaFor[store.DeliveryPolicyState](), (*handlers).getDeliveryPolicy),
	explicitlyConfirmedWriteDefinition(ToolSetDeliveryPolicy, "Explicit owner-authorized policy edit after user confirmation. Distinct from drafting/sending. Requires current policy revision; send_allowed requires reviewed binding digest and person revision. A person-wide change must acknowledge person_all_routes, a broader scope. send_allowed permits explicit send admission only; drafting remains available, provider authorization and platform confirmation still apply.", outputSchemaFor[store.DeliveryPolicyWrite](), outputSchemaFor[store.DeliveryPolicyReceipt](), (*handlers).setDeliveryPolicy, toolSecurityDeliveryPolicyWrite),
	explicitlyConfirmedWriteDefinition(ToolClearDeliveryPolicy, "Clear an exact policy override to inheritance after explicit owner confirmation. Omit target to reset the person-wide default to draft_only. Requires current policy revision and reason. Never sends or changes existing drafts.", outputSchemaFor[store.DeliveryPolicyWrite](), outputSchemaFor[store.DeliveryPolicyReceipt](), (*handlers).clearDeliveryPolicy, toolSecurityDeliveryPolicyWrite),
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
		return deliveryToolFailure("invalid_policy", err.Error()), nil
	}
	if err := store.ValidateDeliveryPolicyQuery(query); err != nil {
		return deliveryToolFailure("invalid_policy", err.Error()), nil
	}
	state, err := h.deliveryPolicies.GetDeliveryPolicy(ctx, query)
	return deliveryToolResult(state, err)
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
		return deliveryToolFailure("invalid_policy", err.Error()), nil
	}
	if err := store.ValidateDeliveryPolicyQuery(write.Query); err != nil {
		return deliveryToolFailure("invalid_policy", err.Error()), nil
	}
	if write.ExpectedRevision < 1 || write.Reason == "" || (!reset && write.Policy != store.DeliveryDraftOnly && write.Policy != store.DeliverySendAllowed) || (!reset && write.Query.Target == nil && write.ScopeAcknowledgement != "person_all_routes") {
		return deliveryToolFailure("invalid_policy", "Require revision, reason, valid policy and explicit broader scope acknowledgment"), nil
	}
	// Bind confirmation to the full exact request, including revision and scope.
	details, err := json.Marshal(write)
	if err != nil {
		return deliveryToolResult(nil, store.ErrDeliveryPolicyInvalid)
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
	return deliveryToolResult(receipt, err)
}
func deliveryToolFailure(code, message string) *toolResult {
	result, _ := jsonResult(map[string]any{"code": code, "error": message})
	result.isError = true
	return result
}
func deliveryToolResult(value any, err error) (*toolResult, error) {
	if err != nil {
		if remote, ok := errors.AsType[*daemonclient.DeliveryPolicyError](err); ok {
			return deliveryToolFailure(remote.Code, daemonclient.SafeMCPError(err).Error()), nil
		}
		return deliveryToolFailure(store.DeliveryErrorCode(err), daemonclient.SafeMCPError(err).Error()), nil
	}
	if value == nil {
		return nil, newInternalError("delivery policy response", errors.New("empty response"))
	}
	return jsonResult(value)
}
