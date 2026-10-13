package api

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func (s *Server) registerMCPCapabilitiesRoute(api huma.API) {
	registerAPIV1RawHumaJSONRoute[apiprotocol.MCPCapabilities](api, "getMCPCapabilities", http.MethodGet, "/mcp/capabilities", "Describe admitted MCP daemon operations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := s.requestAuthentication(r)
		if auth.Mode != AuthModeCaller {
			auth = s.classifyAPIRequestDirect(r)
		}
		if auth.Mode == AuthModeRequired {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
			return
		}
		response := apiprotocol.MCPCapabilities{Version: 1, Delegated: auth.Mode == AuthModeDelegated, Routes: []apiprotocol.MCPRouteDescriptor{}, InboxOperations: []string{}}
		if _, ok := s.store.(InboxController); ok {
			for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder, inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
				permission, err := op.RequiredPermission()
				if err == nil && (!response.Delegated || inboxDiscoveryAllowed(auth.Grant, permission)) {
					response.InboxOperations = append(response.InboxOperations, string(op))
				}
			}
		}
		if len(response.InboxOperations) > 0 {
			for _, operation := range documentOperations(api.OpenAPI()) {
				if operation.OperationID == "controlInbox" {
					response.Routes = append(response.Routes, mcpRouteDescriptor(api.OpenAPI(), operation))
				}
			}
		}
		sort.Strings(response.InboxOperations)
		writeJSON(w, http.StatusOK, response)
	})
}

func inboxDiscoveryAllowed(grant *agentgrant.Grant, permission agentgrant.Permission) bool {
	return grant != nil && grant.HasPermission(agentgrant.PermissionInboxRead) && grant.HasPermission(permission) && slices.ContainsFunc(grant.Sources, func(source agentgrant.SourceRef) bool {
		return source.ID > 0 && source.Type == "gmail" && source.Identifier != ""
	})
}

func mcpRouteDescriptor(doc *huma.OpenAPI, operation *huma.Operation) apiprotocol.MCPRouteDescriptor {
	result := apiprotocol.MCPRouteDescriptor{OperationID: operation.OperationID, Method: operation.Method, Path: operation.Path, QueryParameters: []string{}, RequestProperties: []string{}}
	for _, parameter := range operation.Parameters {
		if parameter.In == "query" {
			result.QueryParameters = append(result.QueryParameters, parameter.Name)
		}
	}
	if operation.RequestBody != nil {
		if media := operation.RequestBody.Content["application/json"]; media != nil && media.Schema != nil {
			schema := media.Schema
			if name, ok := strings.CutPrefix(schema.Ref, "#/components/schemas/"); ok {
				schema = doc.Components.Schemas.Map()[name]
			}
			if schema != nil {
				for name := range schema.Properties {
					result.RequestProperties = append(result.RequestProperties, name)
				}
			}
		}
	}
	sort.Strings(result.QueryParameters)
	sort.Strings(result.RequestProperties)
	return result
}
