package cmd

import (
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func configureInboxReadToolsFromDescriptor(descriptor *apiprotocol.MCPCapabilities, client *daemonclient.Client, opts *mcpserver.ServeOptions) {
	if descriptor == nil {
		return
	}
	readable := !opts.DelegatedOnly || slices.Contains(opts.GrantPermissions, string(agentgrant.PermissionInboxRead))
	if readable && descriptor.HasInboxCandidatesContract() {
		opts.InboxCandidates = client
	}
	if readable && (!opts.DelegatedOnly || slices.Contains(opts.GrantPermissions, string(agentgrant.PermissionInboxContentRead))) && descriptor.HasInboxContextContract() {
		opts.InboxContext = client
	}
}
