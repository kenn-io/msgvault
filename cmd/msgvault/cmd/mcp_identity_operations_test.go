package cmd

import (
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/testutil"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/mcp"
)

// Use the production stdio startup, current native discovery and real SDK tool
// list: a delegated link grant must not admit unlink or owner archive tools.
func TestMCPIdentityProductionDiscoveryFiltersActions(t *testing.T) {
	for _, tc := range []struct {
		name               string
		permissions        []string
		optIn              bool
		read, link, unlink bool
	}{
		{"reader", []string{"identity.read"}, true, true, false, false},
		{"link", []string{"identity.read", "identity.link"}, true, true, true, false},
		{"unlink", []string{"identity.read", "identity.unlink"}, true, true, false, true},
		{"writes disabled", []string{"identity.read", "identity.link", "identity.unlink"}, false, true, false, false},
		{"missing read", []string{"identity.link"}, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			prior := mcpAllowIdentityDecisions
			mcpAllowIdentityDecisions = tc.optIn
			t.Cleanup(func() { mcpAllowIdentityDecisions = prior })
			f := newDraftReplyFixture(t)
			server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
			session := mcpDraftTestSession(mcpDraftAgentContext(t, server, f.source.ID, tc.permissions), t)
			names := mcpDraftToolNames(t, session)
			for _, entry := range []struct {
				name string
				want bool
			}{
				{mcp.ToolPreviewIdentityOperation, tc.read}, {mcp.ToolGetIdentityReceipt, tc.read},
				{mcp.ToolLinkParticipantIdentity, tc.link}, {mcp.ToolLinkParticipantToPerson, tc.link},
				{mcp.ToolUnlinkParticipantIdentity, tc.unlink}, {mcp.ToolUnlinkParticipantFromPerson, tc.unlink},
			} {
				if entry.want {
					assertions.Contains(names, entry.name)
				} else {
					assertions.NotContains(names, entry.name)
				}
			}
			if tc.read {
				first, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
				requirements.NoError(err)
				second, err := f.store.EnsureParticipant(testutil.IMAPTestUsername, "", "example.com")
				requirements.NoError(err)
				preview, err := session.CallTool(t.Context(), &sdkmcp.CallToolParams{Name: mcp.ToolPreviewIdentityOperation, Arguments: map[string]any{"operation": "graph-link", "participant_id": first, "other_participant_id": second}})
				requirements.NoError(err)
				requirements.False(preview.IsError, mcpDraftResultText(t, preview))
				state, err := f.store.IdentityOperationPreviewContext(t.Context(), identitycontrol.OperationGraphLink, identitycontrol.IdentityTarget{ParticipantID: first, OtherParticipantID: second})
				requirements.NoError(err)
				assertions.Empty(state.Links, "native identity preview must not mutate")
			}
			assertions.NotContains(names, "search_metadata")
			assertions.NotContains(names, "promote_person")
		})
	}
}

func TestMCPIdentityDiscoveryClearsPriorAdmissionForOlderDaemon(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	f := newDraftReplyFixture(t)
	server := mcpDraftTestDaemon(t, f.grantedAdapter(), nil)
	client, err := daemonclient.New(daemonclient.Config{URL: server.URL, APIKey: "owner-test-key", AllowInsecure: true})
	requirements.NoError(err)
	t.Cleanup(func() { _ = client.Close() })
	opts := mcp.ServeOptions{}
	requirements.NoError(applyMCPDiscovery(t.Context(), client, &opts, "3.4.0", false))
	requirements.NotNil(opts.IdentityOperations)
	assertions.Len(opts.IdentityActions, 4)
	requirements.NoError(applyMCPDiscovery(t.Context(), client, &opts, "3.3.0", false))
	assertions.Nil(opts.IdentityOperations)
	assertions.NotNil(opts.IdentityActions)
	assertions.Empty(opts.IdentityActions)
}
