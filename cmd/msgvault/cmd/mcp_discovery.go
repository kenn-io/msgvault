package cmd

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

// applyMCPDiscovery admits only the scoped CardDAV contracts implemented for
// this caller. It preserves the existing catalogs and never opts in to writes.
func applyMCPDiscovery(ctx context.Context, client *daemonclient.Client, opts *mcpserver.ServeOptions, version string, delegated bool) error {
	opts.ScopedCardDAVPreview = nil
	opts.ScopedCardDAVApprove = nil
	opts.ScopedCardDAVReconcile = nil
	if !strings.HasPrefix(version, "3.") || !daemonclient.APISchemaVersionAtLeast(version, daemonclient.ScopedCardDAVMinAPISchemaVersion) {
		return nil
	}
	descriptor, err := client.MCPCapabilities(ctx)
	if err != nil {
		return fmt.Errorf("discover scoped CardDAV contracts: %w", err)
	}
	if descriptor.Delegated != delegated {
		return fmt.Errorf("%w: different caller mode", daemonclient.ErrMCPCapabilitiesUnavailable)
	}
	if mcpScopedRouteAdmitted(descriptor, "previewScopedCardDAVPublication", http.MethodGet, "preview") {
		opts.ScopedCardDAVPreview = client
	}
	if mcpScopedRouteAdmitted(descriptor, "approveScopedCardDAVPublication", http.MethodPost, "approve", "approval_token", "idempotency_key") {
		opts.ScopedCardDAVApprove = client
	}
	if mcpScopedRouteAdmitted(descriptor, "reconcileScopedCardDAVPublication", http.MethodPost, "reconcile", "approval_token", "idempotency_key") {
		opts.ScopedCardDAVReconcile = client
	}
	return nil
}

func mcpScopedRouteAdmitted(descriptor *apiprotocol.MCPCapabilities, operation, method, suffix string, fields ...string) bool {
	if descriptor == nil || descriptor.Version != 1 {
		return false
	}
	for _, route := range descriptor.Routes {
		if route.OperationID != operation || route.Method != method || route.Path != "/api/v1/carddav/scoped/publications/{person_id}/"+suffix || len(route.QueryParameters) != 0 || len(route.RequestProperties) != len(fields) {
			continue
		}
		if slices.ContainsFunc(fields, func(field string) bool { return !slices.Contains(route.RequestProperties, field) }) {
			return false
		}
		return true
	}
	return false
}
