package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/apiprotocol"
)

type MCPCapabilitiesResponse = apiprotocol.MCPCapabilities

func (s *Server) registerMCPCapabilitiesRoute(root, apiV1 huma.API) {
	registerAPIV1RawHumaJSONRoute[MCPCapabilitiesResponse](apiV1,
		"getMCPCapabilities", http.MethodGet, "/mcp/capabilities",
		"Describe supported MCP daemon operations", func(w http.ResponseWriter, _ *http.Request) {
			response := MCPCapabilitiesResponse{
				Version: 1,
				Routes:  mcpRouteDescriptors(root.OpenAPI()),
			}
			writeJSON(w, http.StatusOK, response)
		})
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
			if parameter.In == "query" {
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
