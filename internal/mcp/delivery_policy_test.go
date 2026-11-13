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
	writes  int
	last    store.DeliveryPolicyWrite
	method  string
	receipt *store.DeliveryPolicyReceipt
}

func (b *policyMCPBackend) GetDeliveryPolicy(context.Context, store.DeliveryPolicyQuery) (*store.DeliveryPolicyState, error) {
	return &store.DeliveryPolicyState{PersonUID: "synthetic-uid", EffectivePolicy: store.DeliveryDraftOnly, Revision: 1}, nil
}

func (b *policyMCPBackend) SetDeliveryPolicy(_ context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	b.writes++
	b.last = write
	b.method = ToolSetDeliveryPolicy
	return b.receipt, nil
}

func (b *policyMCPBackend) ClearDeliveryPolicy(_ context.Context, write store.DeliveryPolicyWrite) (*store.DeliveryPolicyReceipt, error) {
	b.writes++
	b.last = write
	b.method = ToolClearDeliveryPolicy
	return b.receipt, nil
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
	for _, test := range []struct {
		name, tool, prefix, promptJSON string
		write                          store.DeliveryPolicyWrite
	}{
		{
			name: "set person default", tool: ToolSetDeliveryPolicy, prefix: "Set delivery policy: ",
			promptJSON: `{"person_uid":"synthetic-uid","expected_revision":1,"policy":"send_allowed","scope_acknowledgement":"person_all_routes","reason":"Explicit synthetic approval"}`,
			write:      store.DeliveryPolicyWrite{PersonUID: "synthetic-uid", ExpectedRevision: 1, Policy: store.DeliverySendAllowed, ScopeAcknowledgement: "person_all_routes", Reason: "Explicit synthetic approval"},
		},
		{
			name: "set address override", tool: ToolSetDeliveryPolicy, prefix: "Set delivery policy: ",
			promptJSON: `{"person_uid":"synthetic-uid","endpoint":"alice@example.test","expected_revision":3,"policy":"send_allowed","scope_acknowledgement":"person_all_routes","reason":"Explicit synthetic approval"}`,
			write:      store.DeliveryPolicyWrite{PersonUID: "synthetic-uid", Endpoint: "alice@example.test", ExpectedRevision: 3, Policy: store.DeliverySendAllowed, ScopeAcknowledgement: "person_all_routes", Reason: "Explicit synthetic approval"},
		},
		{
			name: "clear address override", tool: ToolClearDeliveryPolicy, prefix: "Clear delivery policy: ",
			promptJSON: `{"person_uid":"synthetic-uid","endpoint":"alice@example.test","expected_revision":3,"reason":"Restore inheritance"}`,
			write:      store.DeliveryPolicyWrite{PersonUID: "synthetic-uid", Endpoint: "alice@example.test", ExpectedRevision: 3, Reason: "Restore inheritance"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			assert := Assert.New(t)
			require := require.New(t)
			state := store.DeliveryPolicyState{
				PersonUID: test.write.PersonUID, Endpoint: test.write.Endpoint, Revision: test.write.ExpectedRevision,
				EffectivePolicy: store.DeliveryDraftOnly, InheritanceSource: "system_default", Reason: "draft_required",
			}
			if test.tool == ToolSetDeliveryPolicy {
				state.StoredPolicy = new(store.DeliverySendAllowed)
				state.EffectivePolicy = store.DeliverySendAllowed
				state.InheritanceSource, state.Reason = "person_default", "explicit_person_default"
				if state.Endpoint != "" {
					state.InheritanceSource, state.Reason = "contact_method", "explicit_approval"
				}
			}
			backend := &policyMCPBackend{receipt: &store.DeliveryPolicyReceipt{Before: state, After: state}}
			opts := ServeOptions{Engine: &querytest.MockEngine{}, DeliveryPolicies: backend, AllowDeliveryPolicyWrites: true}
			args := map[string]any{"person_uid": "synthetic-uid", "expected_revision": test.write.ExpectedRevision, "reason": test.write.Reason}
			if test.tool == ToolSetDeliveryPolicy {
				args["policy"], args["scope_acknowledgement"] = "send_allowed", "person_all_routes"
			}
			if test.write.Endpoint != "" {
				args["endpoint"] = test.write.Endpoint
			}
			var prompt string
			result := confirmedCallTool(t, opts, test.tool, args, false, func(message string) { prompt = message })
			require.Equal(true, result["isError"])
			assert.Zero(backend.writes)
			require.True(strings.HasPrefix(prompt, test.prefix), "prompt: %s", prompt)
			require.True(strings.HasSuffix(prompt, ". No message will be sent."), "prompt: %s", prompt)
			assert.JSONEq(test.promptJSON, strings.TrimSuffix(strings.TrimPrefix(prompt, test.prefix), ". No message will be sent."))
			result = confirmedCallTool(t, opts, test.tool, args, true)
			assert.NotEqual(true, result["isError"])
			assert.Equal(1, backend.writes)
			assert.Equal(test.tool, backend.method)
			assert.Equal(test.write, backend.last)
		})
	}
}
