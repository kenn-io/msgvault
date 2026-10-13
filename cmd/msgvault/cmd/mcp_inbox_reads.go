package cmd

import (
	"net/http"
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
	triagePermission := readable && (!opts.DelegatedOnly || slices.Contains(opts.GrantPermissions, string(agentgrant.PermissionInboxTag)))
	if triagePermission && admittedMCPRoute(descriptor, "previewInboxTriage", "/api/v1/inbox/triage/preview", "source", "items") {
		opts.InboxTriagePreview = client
	}
	if triagePermission && admittedMCPRoute(descriptor, "applyInboxTriage", "/api/v1/inbox/triage/apply", "source", "items", "mapping_revision", "archive_revision", "incoming_watermark", "issued_at", "expires_at", "preview_token") {
		opts.InboxTriageApply = client
	}
}

func admittedMCPRoute(descriptor *apiprotocol.MCPCapabilities, operation, path string, fields ...string) bool {
	for _, route := range descriptor.Routes {
		if route.OperationID != operation || route.Method != http.MethodPost || route.Path != path {
			continue
		}
		for _, field := range fields {
			if !slices.Contains(route.RequestProperties, field) {
				return false
			}
		}
		return true
	}
	return false
}
