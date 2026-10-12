package cmd

import (
	"context"
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func configureInboxReadTools(ctx context.Context, client *daemonclient.Client, opts *mcpserver.ServeOptions) {
	descriptor, err := client.MCPCapabilities(ctx)
	if err != nil {
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
