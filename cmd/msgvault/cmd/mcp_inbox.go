package cmd

import (
	"context"
	"slices"

	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

func admitInboxMCP(ctx context.Context, client *daemonclient.Client, opts *mcpserver.ServeOptions, owner bool) {
	descriptor, err := client.MCPCapabilities(ctx)
	if err != nil || !descriptor.HasInboxContract() {
		return
	}
	for _, op := range inboxMCPOperations(opts.GrantPermissions, owner) {
		if slices.Contains(descriptor.InboxOperations, string(op)) {
			opts.InboxOperations = append(opts.InboxOperations, op)
		}
	}
	if len(opts.InboxOperations) != 0 {
		opts.Inbox = client
	}
}

// inboxMCPOperations limits discovery to the current grant. The daemon still
// reauthorizes the exact source/action before every dispatch or receipt replay.
func inboxMCPOperations(permissions []string, owner bool) []inboxcontrol.Operation {
	var allowed []inboxcontrol.Operation
	for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpTags, inboxcontrol.OpCreateFolder, inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
		required, err := op.RequiredPermission()
		if err == nil && (owner || (slices.Contains(permissions, string(agentgrant.PermissionInboxRead)) && slices.Contains(permissions, string(required)))) {
			allowed = append(allowed, op)
		}
	}
	return allowed
}
