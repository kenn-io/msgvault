package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/query/querytest"
	"go.kenn.io/msgvault/internal/store"
)

type stubEmailFamilyBackend struct {
	family *store.EmailFamily
	err    error
}

func (b stubEmailFamilyBackend) EmailFamilyContext(context.Context, int64) (*store.EmailFamily, error) {
	return b.family, b.err
}

// The store owns what the family holds; this checks the tool is listed read
// only, for the owner only, and reports what the backend returns or fails with.
func TestEmailFamilyToolIsReadOnlyAndOwnerOnly(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	call := func(opts ServeOptions) rawRPCResponse {
		return rawModernCall(t, opts, HTTPOptions{}, "tools/call", map[string]any{"name": ToolGetEmailFamily, "arguments": map[string]any{"message_id": 7}})
	}

	opts := ServeOptions{Engine: &querytest.MockEngine{}, EmailFamilyBackend: stubEmailFamilyBackend{family: &store.EmailFamily{AnchorMessageID: 7}}}
	listed := toolsByName(t, rawListTools(t, opts, false))
	require.Contains(listed, ToolGetEmailFamily)
	assert.Equal(true, toolReadOnlyHint(t, listed[ToolGetEmailFamily]))
	content := toolStructuredContent(t, call(opts).Result)
	assert.EqualValues(7, content["anchor_message_id"])

	failing := &handlers{emailFamilyBackend: stubEmailFamilyBackend{err: errors.New("database unavailable")}}
	_, err := failing.getEmailFamily(t.Context(), toolRequest{arguments: map[string]any{"message_id": float64(7)}})
	require.Error(err)

	assert.NotContains(toolsByName(t, rawListTools(t, ServeOptions{Engine: &querytest.MockEngine{}}, false)), ToolGetEmailFamily,
		"a daemon older than the route has no backend")
	delegated := ServeOptions{
		Engine: &querytest.MockEngine{}, EmailFamilyBackend: opts.EmailFamilyBackend, DelegatedOnly: true,
		GrantPermissions: []string{string(agentgrant.PermissionMessageRead)},
	}
	assert.NotContains(toolsByName(t, rawListTools(t, delegated, false)), ToolGetEmailFamily,
		"message.read must not expose the owner-only family read")
}
