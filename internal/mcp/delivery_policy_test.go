package mcp

import (
	"context"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
	"testing"
)

type policyMCPBackend struct{ writes int }

func (b *policyMCPBackend) GetDeliveryPolicy(context.Context, store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	return &store.DeliveryPolicyState{PersonUID: "synthetic-uid", EffectivePolicy: store.DeliveryDraftOnly, PolicyRevision: 1}, nil
}
func (b *policyMCPBackend) SetDeliveryPolicy(context.Context, store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	b.writes++
	return &store.DeliveryPolicyReceipt{}, nil
}
func (b *policyMCPBackend) ClearDeliveryPolicy(context.Context, store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	b.writes++
	return &store.DeliveryPolicyReceipt{}, nil
}
func TestDeliveryPolicyMCPSeparateOptInAndDelegation(t *testing.T) {
	assertions := assert.New(t)

	backend := &policyMCPBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, DeliveryPolicies: backend}
	tools := toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, ToolGetDeliveryPolicy)
	assertions.NotContains(tools, ToolSetDeliveryPolicy)
	opts.AllowDeliveryPolicyWrites = true
	tools = toolsByName(t, rawListTools(t, opts, true))
	assertions.Contains(tools, ToolSetDeliveryPolicy)
	assertions.Contains(tools, ToolClearDeliveryPolicy)
	tools = toolsByName(t, rawListTools(t, opts, false))
	assertions.NotContains(tools, ToolSetDeliveryPolicy)
	opts.DelegatedOnly = true
	tools = toolsByName(t, rawListTools(t, opts, true))
	assertions.NotContains(tools, ToolSetDeliveryPolicy)
	assertions.NotContains(tools, ToolGetDeliveryPolicy)
}
func TestDeliveryPolicyMCPConfirmationBeforeAnyWrite(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	backend := &policyMCPBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, DeliveryPolicies: backend, AllowDeliveryPolicyWrites: true}
	args := map[string]any{"query": map[string]any{"person_uid": "synthetic-uid"}, "expected_revision": 1, "expected_person_revision": 1, "binding_digest": "synthetic-binding", "policy": "send_allowed", "scope_acknowledgement": "person_all_routes", "reason": "Explicit synthetic approval"}
	result := confirmedCallTool(t, opts, ToolSetDeliveryPolicy, args, false)
	requirements.Equal(true, result["isError"])
	assertions.Zero(backend.writes)
	result = confirmedCallTool(t, opts, ToolSetDeliveryPolicy, args, true)
	assertions.NotEqual(true, result["isError"])
	assertions.Equal(1, backend.writes)
}
