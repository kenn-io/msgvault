package cmd

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/spf13/pflag"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

// applyMCPDiscovery selects contracts admitted for this caller. Actual source
// ownership, live provider capability and current grants are checked per call.
func applyMCPDiscovery(ctx context.Context, client *daemonclient.Client, opts *mcpserver.ServeOptions, version string, delegated bool) error {
	opts.IdentityOperations = nil
	opts.IdentityActions = []identitycontrol.Operation{}
	opts.PersonMerge = nil
	opts.ScopedCardDAVPreview = nil
	opts.ScopedCardDAVApprove = nil
	opts.ScopedCardDAVReconcile = nil
	opts.SuppressPersonMergeWrites = true
	opts.Inbox = nil
	opts.InboxOperations = nil
	opts.Operations = nil
	opts.OperationCapabilities = nil
	opts.SuppressMessageTagWrites = true
	opts.InboxCandidates = nil
	opts.InboxContext = nil
	opts.InboxTriagePreview = nil
	opts.InboxTriageApply = nil
	if !daemonclient.APISchemaVersionAtLeast(version, daemonclient.InboxMinAPISchemaVersion) {
		return nil
	}
	descriptor, err := client.MCPCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("MCP operation discovery unavailable: %w", err)
	}
	if descriptor.Delegated != delegated {
		opts.Calendar = nil
		opts.DraftCommands = nil
		return fmt.Errorf("%w: MCP operation discovery has a different caller mode", inboxcontrol.ErrDenied)
	}
	if descriptor.Version == 1 && slices.ContainsFunc(descriptor.Routes, func(route apiprotocol.MCPRouteDescriptor) bool {
		return route.OperationID == "getPersonProfile" && route.Method == http.MethodGet && route.Path == "/api/v1/people/{id}"
	}) {
		opts.PersonMerge = client
		opts.SuppressPersonMergeWrites = !mcpRouteAdmitted(descriptor, "mergePersons", "/api/v1/people/{id}/merge", "absorbed_person_id")
	}
	if slices.ContainsFunc(descriptor.Routes, func(route apiprotocol.MCPRouteDescriptor) bool {
		return route.OperationID == "previewScopedCardDAVPublication" && route.Method == http.MethodGet && route.Path == "/api/v1/carddav/scoped/publications/{person_id}/preview"
	}) {
		opts.ScopedCardDAVPreview = client
	}
	if mcpRouteAdmitted(descriptor, "approveScopedCardDAVPublication", "/api/v1/carddav/scoped/publications/{person_id}/approve", "approval_token", "idempotency_key") {
		opts.ScopedCardDAVApprove = client
	}
	if mcpRouteAdmitted(descriptor, "reconcileScopedCardDAVPublication", "/api/v1/carddav/scoped/publications/{person_id}/reconcile", "approval_token", "idempotency_key") {
		opts.ScopedCardDAVReconcile = client
	}
	if daemonclient.APISchemaVersionAtLeast(version, "3.4.0") && descriptor.HasIdentityPreviewContract() && descriptor.HasIdentityReceiptContract() {
		opts.IdentityOperations = client
		if descriptor.HasIdentityApplyContract() {
			for _, name := range descriptor.IdentityOperations {
				action := identitycontrol.Operation(name)
				switch action {
				case identitycontrol.OperationGraphLink, identitycontrol.OperationGraphUnlink, identitycontrol.OperationPersonLink, identitycontrol.OperationPersonUnlink:
					if !slices.Contains(opts.IdentityActions, action) {
						opts.IdentityActions = append(opts.IdentityActions, action)
					}
				}
			}
		}
	}
	operations := newDaemonMCPOperations(client, descriptor)
	opts.Operations = operations
	opts.OperationCapabilities = operations.capabilities()
	if !delegated {
		if people, ok := opts.PeopleBackend.(*daemonclient.PeopleBrowser); ok && supportsNamedPromotion(descriptor) {
			opts.PeopleBackend = daemonMCPNamedPeopleBrowser{PeopleBrowser: people, client: client}
		}
	}
	if !mcpRouteAdmitted(descriptor, "controlCalendar", "/api/v1/calendar/control") {
		opts.Calendar = nil
	}
	opts.DraftCommands = nil
	if mcpRouteAdmitted(descriptor, "runCLI", "/api/v1/cli/run", "args") {
		for _, command := range descriptor.Commands {
			constructor, known := mcpDraftCommandConstructors[command.Name]
			if !known || delegated && (!command.Delegated || !agentDelegatedCommand(command.Name)) {
				continue
			}
			compatible := true
			constructor().Flags().VisitAll(func(flag *pflag.Flag) {
				if !slices.Contains(command.Flags, flag.Name) {
					compatible = false
				}
			})
			if compatible && !slices.Contains(opts.DraftCommands, command.Name) {
				opts.DraftCommands = append(opts.DraftCommands, command.Name)
			}
		}
		slices.Sort(opts.DraftCommands)
	}
	if descriptor.HasInboxCandidatesContract() {
		opts.InboxCandidates = client
	}
	if descriptor.HasInboxContextContract() {
		opts.InboxContext = client
	}
	if mcpRouteAdmitted(descriptor, "previewInboxTriage", "/api/v1/inbox/triage/preview", "source", "items") {
		opts.InboxTriagePreview = client
	}
	if mcpRouteAdmitted(descriptor, "applyInboxTriage", "/api/v1/inbox/triage/apply", "source", "items", "mapping_revision", "archive_revision", "incoming_watermark", "issued_at", "expires_at", "preview_token") {
		opts.InboxTriageApply = client
	}
	if descriptor.HasInboxContract() {
		for _, name := range descriptor.InboxOperations {
			op := inboxcontrol.Operation(name)
			if _, err := op.RequiredPermission(); err == nil {
				opts.InboxOperations = append(opts.InboxOperations, op)
			}
		}
		opts.SuppressMessageTagWrites = !slices.Contains(opts.InboxOperations, inboxcontrol.OpTags)
		if len(opts.InboxOperations) > 0 {
			opts.Inbox = client
		}
	}
	return nil
}
func mcpRouteAdmitted(descriptor *apiprotocol.MCPCapabilities, operation, path string, fields ...string) bool {
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
