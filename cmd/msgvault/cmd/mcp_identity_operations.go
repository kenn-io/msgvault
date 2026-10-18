package cmd

import (
	"context"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identitycontrol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
)

const identityOperationsMinAPISchemaVersion = "3.15.0"

func applyIdentityOperationCapabilities(ctx context.Context, st *daemonclient.Client, opts *mcpserver.ServeOptions, schemaVersion string) {
	opts.IdentityOperations = nil
	opts.IdentityActions = []identitycontrol.Operation{}
	if !strings.HasPrefix(schemaVersion, "3.") || !daemonclient.APISchemaVersionAtLeast(schemaVersion, identityOperationsMinAPISchemaVersion) {
		return
	}
	if opts.DelegatedOnly && !slices.Contains(opts.GrantPermissions, "identity.read") {
		return
	}
	descriptor, err := st.MCPCapabilities(ctx)
	if err != nil || descriptor.Delegated != opts.DelegatedOnly || !descriptor.HasIdentityPreviewContract() || !descriptor.HasIdentityReceiptContract() {
		return
	}
	opts.IdentityOperations = st
	if !descriptor.HasIdentityApplyContract() {
		return
	}
	for _, entry := range []struct {
		operation  identitycontrol.Operation
		permission string
	}{
		{identitycontrol.OperationGraphLink, "identity.link"}, {identitycontrol.OperationPersonLink, "identity.link"},
		{identitycontrol.OperationGraphUnlink, "identity.unlink"}, {identitycontrol.OperationPersonUnlink, "identity.unlink"},
	} {
		if slices.Contains(descriptor.IdentityOperations, string(entry.operation)) && (!opts.DelegatedOnly || slices.Contains(opts.GrantPermissions, entry.permission)) {
			opts.IdentityActions = append(opts.IdentityActions, entry.operation)
		}
	}
}
