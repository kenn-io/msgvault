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

type MCPCapabilitiesResponse = apiprotocol.MCPCapabilities

func (s *Server) registerMCPCapabilitiesRoute(root, apiV1 huma.API) {
	registerAPIV1RawHumaJSONRoute[MCPCapabilitiesResponse](apiV1,
		"getMCPCapabilities", http.MethodGet, "/mcp/capabilities",
		"Describe supported MCP daemon operations", func(w http.ResponseWriter, r *http.Request) {
		auth := s.requestAuthentication(r)
		if auth.Mode != AuthModeCaller {
			auth = s.classifyAPIRequestDirect(r)
		}
		if auth.Mode == AuthModeRequired {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
			return
		}
			response := MCPCapabilitiesResponse{
				Version:         1,
				Delegated:       auth.Mode == AuthModeDelegated,
				Routes:          mcpRouteDescriptors(root.OpenAPI()),
				InboxOperations: []string{},
			}
			if _, ok := s.store.(InboxController); ok {
				for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder, inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
					permission, err := op.RequiredPermission()
					if err == nil && (!response.Delegated || inboxDiscoveryAllowed(auth.Grant, permission)) {
						response.InboxOperations = append(response.InboxOperations, string(op))
					}
				}
			}
			if len(response.InboxOperations) > 0 {
				for _, operation := range documentOperations(root.OpenAPI()) {
					if operation.OperationID == "controlInbox" {
						response.Routes = append(response.Routes, mcpRouteDescriptor(root.OpenAPI(), operation))
						break
					}
				}
			}
			sort.Strings(response.InboxOperations)
			sort.Slice(response.Routes, func(i, j int) bool { return response.Routes[i].OperationID < response.Routes[j].OperationID })
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
		if parameter.In == queryParamLocation {
			result.QueryParameters = append(result.QueryParameters, parameter.Name)
		}
	}
	if operation.RequestBody != nil {
		if media, ok := operation.RequestBody.Content["application/json"]; ok && media.Schema != nil {
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

func mcpRouteDescriptors(doc *huma.OpenAPI) []apiprotocol.MCPRouteDescriptor {
	descriptors := make([]apiprotocol.MCPRouteDescriptor, 0)
	for _, operation := range documentOperations(doc) {
		if !mcpOperationDescribed(operation.OperationID) {
			continue
		}
		descriptor := apiprotocol.MCPRouteDescriptor{
			OperationID: operation.OperationID, Method: operation.Method, Path: operation.Path,
			QueryParameters: []string{}, RequestProperties: []string{},
		}
		for _, parameter := range operation.Parameters {
			if parameter.In == queryParamLocation {
				descriptor.QueryParameters = append(descriptor.QueryParameters, parameter.Name)
			}
		}
		if operation.RequestBody != nil {
			if media, ok := operation.RequestBody.Content["application/json"]; ok && media.Schema != nil {
				schema := media.Schema
				if name, ok := strings.CutPrefix(schema.Ref, "#/components/schemas/"); ok {
					schema = doc.Components.Schemas.Map()[name]
				}
				if schema != nil {
					for name := range schema.Properties {
						descriptor.RequestProperties = append(descriptor.RequestProperties, name)
					}
				}
			}
		}
		sort.Strings(descriptor.QueryParameters)
		sort.Strings(descriptor.RequestProperties)
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i, j int) bool { return descriptors[i].OperationID < descriptors[j].OperationID })
	return descriptors
}

// Advertise only routes consumed by the source and profile MCP tools, and only
// when their real routes are registered.
func mcpOperationDescribed(operationID string) bool {
	switch operationID {
	case "listSourceStatus", "listSourceIdentities", "getSchedulerStatus", "triggerSync",
		"getSettings", "patchSettings", "getParticipant", "getCacheBuildStatus",
		"getPersonProfile", "patchPerson", "createPerson":
		return true
	default:
		return false
	}
}
