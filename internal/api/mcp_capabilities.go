package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/identitycontrol"
)

// registerMCPCapabilitiesRoute advertises only registered identity contracts.
// Native target authorization is repeated by preview, apply and receipt reads.
func (s *Server) registerMCPCapabilitiesRoute(api huma.API) {
	registerAPIV1RawHumaJSONRoute[apiprotocol.MCPCapabilities](api, "getMCPCapabilities", http.MethodGet, "/mcp/capabilities", "Describe admitted native identity MCP operations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := s.identityOperationAuthentication(r)
		if _, err := identityOperationPrincipal(auth); err != nil {
			s.writeIdentityOperationError(w, err)
			return
		}
		response := apiprotocol.MCPCapabilities{Version: 1, Delegated: auth.Mode == AuthModeDelegated, Routes: []apiprotocol.MCPRouteDescriptor{}, IdentityOperations: []string{}}
		_, native := s.store.(identityOperationStore)
		readable := native && (!response.Delegated || s.identityDiscoveryResourcesCurrent(r, auth.Grant))
		admitted := map[string]bool{"previewIdentityOperation": readable, "getIdentityOperationReceipt": readable}
		if readable {
			for _, entry := range []struct {
				operation  identitycontrol.Operation
				permission agentgrant.Permission
			}{
				{identitycontrol.OperationGraphLink, agentgrant.PermissionIdentityLink}, {identitycontrol.OperationGraphUnlink, agentgrant.PermissionIdentityUnlink},
				{identitycontrol.OperationPersonLink, agentgrant.PermissionIdentityLink}, {identitycontrol.OperationPersonUnlink, agentgrant.PermissionIdentityUnlink},
			} {
				if !response.Delegated || auth.Grant.HasPermission(entry.permission) {
					response.IdentityOperations = append(response.IdentityOperations, string(entry.operation))
				}
			}
		}
		admitted["applyIdentityOperation"] = len(response.IdentityOperations) > 0
		for _, operation := range documentOperations(api.OpenAPI()) {
			if admitted[operation.OperationID] {
				response.Routes = append(response.Routes, mcpRouteDescriptor(api.OpenAPI(), operation))
			}
		}
		sort.Strings(response.IdentityOperations)
		sort.Slice(response.Routes, func(i, j int) bool { return response.Routes[i].OperationID < response.Routes[j].OperationID })
		writeJSON(w, http.StatusOK, response)
	})
}

func (s *Server) identityDiscoveryResourcesCurrent(r *http.Request, grant *agentgrant.Grant) bool {
	if grant == nil || !grant.HasPermission(agentgrant.PermissionIdentityRead) || len(grant.Sources)+len(grant.Persons) == 0 {
		return false
	}
	if len(grant.Sources) > 0 {
		resolver, ok := s.store.(agentGrantSourceResolver)
		if !ok {
			return false
		}
		for _, ref := range grant.Sources {
			source, err := resolver.GetSourceByIDContext(r.Context(), ref.ID)
			if err != nil || source == nil || !grant.Allows(agentgrant.PermissionIdentityRead, agentgrant.SourceRef{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}) {
				return false
			}
		}
	}
	if len(grant.Persons)+len(grant.AddressBooks) > 0 {
		resolver, ok := s.store.(agentGrantResourceResolver)
		if !ok {
			return false
		}
		people, books := []int64{}, []int64{}
		for _, ref := range grant.Persons {
			people = append(people, ref.ID)
		}
		for _, ref := range grant.AddressBooks {
			books = append(books, ref.BookID)
		}
		selection, err := resolver.IdentityGrantSelectionContext(r.Context(), people, books)
		if err != nil {
			return false
		}
		for _, person := range selection.Persons {
			if !grant.AllowsPerson(agentgrant.PermissionIdentityRead, agentgrant.PersonRef{ID: person.ID, UID: person.UID}) {
				return false
			}
		}
		for _, book := range selection.AddressBooks {
			if !grant.AllowsAddressBook(agentgrant.PermissionIdentityRead, agentgrant.AddressBookRef{AccountID: book.AccountID, BookID: book.BookID, CanonicalURL: book.CanonicalURL, OwnershipFingerprint: book.OwnershipFingerprint}) {
				return false
			}
		}
	}
	return true
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
