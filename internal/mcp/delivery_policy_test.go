package mcp

import (
	"context"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type policyMCPBackend struct {
	writes int
	last   store.DeliveryPolicyWrite
}

func (b *policyMCPBackend) GetDeliveryPolicy(context.Context, store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	return &store.DeliveryPolicyState{PersonUID: "synthetic-uid", EffectivePolicy: store.DeliveryDraftOnly, Revision: 1}, nil
}

func (b *policyMCPBackend) SetDeliveryPolicy(_ context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	b.writes++
	b.last = write
	return &store.DeliveryPolicyReceipt{}, nil
}

func (b *policyMCPBackend) ClearDeliveryPolicy(_ context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	b.writes++
	b.last = write
	return &store.DeliveryPolicyReceipt{}, nil
}

func TestDeliveryPolicyMCPSeparateOptInAndDelegation(t *testing.T) {
	assertions := Assert.New(t)

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

func TestDeliveryPolicyMCPWriteNeedsConfirmation(t *testing.T) {
	assertions := Assert.New(t)
	requirements := require.New(t)

	backend := &policyMCPBackend{}
	opts := ServeOptions{Engine: &querytest.MockEngine{}, DeliveryPolicies: backend, AllowDeliveryPolicyWrites: true}
	args := map[string]any{"person_uid": "synthetic-uid", "expected_revision": 1, "policy": "send_allowed", "scope_acknowledgement": "person_all_routes", "reason": "Explicit synthetic approval"}
	result := confirmedCallTool(t, opts, ToolSetDeliveryPolicy, args, false)
	requirements.Equal(true, result["isError"])
	assertions.Zero(backend.writes)
	result = confirmedCallTool(t, opts, ToolSetDeliveryPolicy, args, true)
	assertions.NotEqual(true, result["isError"])
	assertions.Equal(1, backend.writes)
	assertions.Equal(store.DeliveryPolicyWrite{
		PersonUID: "synthetic-uid", ExpectedRevision: 1, Policy: store.DeliverySendAllowed,
		ScopeAcknowledgement: "person_all_routes", Reason: "Explicit synthetic approval",
	}, backend.last)
}

func TestDeliveryPolicyMCPConfirmationPrompt(t *testing.T) {
	for _, test := range []struct {
		tool, prefix string
	}{
		{ToolSetDeliveryPolicy, "Set delivery policy: "},
		{ToolClearDeliveryPolicy, "Clear delivery policy: "},
	} {
		t.Run(test.tool, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)

			backend := &policyMCPBackend{}
			opts := ServeOptions{Engine: &querytest.MockEngine{}, DeliveryPolicies: backend, AllowDeliveryPolicyWrites: true}
			args := map[string]any{"person_uid": "synthetic-uid", "endpoint": "alice@example.test", "expected_revision": 3, "policy": "send_allowed", "scope_acknowledgement": "person_all_routes", "reason": "Explicit synthetic approval"}
			var prompt string
			confirmedCallTool(t, opts, test.tool, args, false, func(message string) { prompt = message })
			require.True(strings.HasPrefix(prompt, test.prefix), "prompt: %s", prompt)
			require.True(strings.HasSuffix(prompt, ". No message will be sent."), "prompt: %s", prompt)
			assert.JSONEq(`{"person_uid":"synthetic-uid","endpoint":"alice@example.test","expected_revision":3,"policy":"send_allowed","scope_acknowledgement":"person_all_routes","reason":"Explicit synthetic approval"}`, strings.TrimSuffix(strings.TrimPrefix(prompt, test.prefix), ". No message will be sent."))
			assert.Zero(backend.writes)
		})
	}
}
