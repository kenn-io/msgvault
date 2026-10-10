package api

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/identitycontrol"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

type MCPCommandDescriber interface {
	MCPCommandDescriptors() []apiprotocol.MCPCommandDescriptor
}

func (s *Server) registerMCPCapabilitiesRoute(api huma.API) {
	registerAPIV1RawHumaJSONRoute[apiprotocol.MCPCapabilities](api, "getMCPCapabilities", http.MethodGet, "/mcp/capabilities", "Describe admitted MCP daemon operations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		auth := s.classifyAPIRequestDirect(r)
		if auth.Mode == AuthModeRequired {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication is required")
			return
		}
		response := apiprotocol.MCPCapabilities{Version: 1, Delegated: auth.Mode == AuthModeDelegated,
			Routes: []apiprotocol.MCPRouteDescriptor{}, Commands: []apiprotocol.MCPCommandDescriptor{}, InboxOperations: []string{}, IdentityOperations: []string{}}
		allowedRoutes := map[string]bool{"getHealth": true}
		// Owner operational tools use only these fixed registered contracts.
		// Live availability and source eligibility remain per-call checks.
		if !response.Delegated {
			for _, id := range []string{
				"listSourceStatus", "listSourceIdentities", "getSchedulerStatus", "triggerSync",
				"getSettings", "patchSettings", "getParticipant", "getCacheBuildStatus",
				"getPersonProfile", "patchPerson", "createPerson",
			} {
				allowedRoutes[id] = true
			}
		}
		if _, ok := s.store.(ScopedPersonEditStore); ok {
			readable := !response.Delegated || auth.Grant != nil && len(auth.Grant.Persons) > 0 && auth.Grant.HasPermission(agentgrant.PermissionPersonRead)
			allowedRoutes["getPersonProfile"] = readable
			allowedRoutes["getPersonStructuredProfile"] = readable
			editable := readable && (!response.Delegated || auth.Grant.HasPermission(agentgrant.PermissionPersonEdit))
			allowedRoutes["patchPerson"] = editable
			allowedRoutes["patchPersonStructuredProfile"] = editable
		}
		if _, ok := s.store.(ScopedPersonAttributeStore); ok {
			readable := !response.Delegated || auth.Grant != nil && len(auth.Grant.Persons) > 0 && auth.Grant.HasPermission(agentgrant.PermissionPersonRead)
			allowedRoutes["listPersonAttributes"] = readable
			editable := readable && (!response.Delegated || auth.Grant.HasPermission(agentgrant.PermissionPersonEdit))
			allowedRoutes["setPersonAttribute"] = editable
			allowedRoutes["clearPersonAttribute"] = editable
		}
		if _, ok := s.store.(ScopedPersonMergeStore); ok {
			allowedRoutes["mergePersons"] = !response.Delegated || auth.Grant != nil && len(auth.Grant.Persons) >= 2 && auth.Grant.HasPermission(agentgrant.PermissionPersonRead) && auth.Grant.HasPermission(agentgrant.PermissionPersonMerge)
		}
		if _, ok := s.store.(scopedCardDAVReceiptStore); ok {
			allowed := !response.Delegated || auth.Grant != nil && len(auth.Grant.Persons) > 0 && len(auth.Grant.AddressBooks) > 0 && auth.Grant.HasPermission(agentgrant.PermissionPersonRead) && auth.Grant.HasPermission(agentgrant.PermissionCardDAVWrite)
			// Settled receipts live in the archive and remain readable without
			// a provider controller. Pending recovery still checks its transport
			// in the handler; discovery does not admit another publication.
			allowedRoutes["reconcileScopedCardDAVPublication"] = allowed
			if s.cardDAV != nil {
				if _, supported := s.cardDAV.globalOperations().(ScopedCardDAVPublicationOperations); supported {
					allowedRoutes["previewScopedCardDAVPublication"] = allowed
					allowedRoutes["approveScopedCardDAVPublication"] = allowed
				}
			}
		}
		if _, ok := s.store.(identityOperationStore); ok {
			readable := !response.Delegated || auth.Grant != nil && auth.Grant.HasPermission(agentgrant.PermissionIdentityRead) && (len(auth.Grant.Sources) > 0 || len(auth.Grant.Persons) > 0)
			allowedRoutes["previewIdentityOperation"] = readable
			allowedRoutes["getIdentityOperationReceipt"] = !response.Delegated || auth.Grant != nil && auth.Grant.HasPermission(agentgrant.PermissionIdentityRead)
			if readable {
				for _, entry := range []struct {
					operation  identitycontrol.Operation
					permission agentgrant.Permission
				}{
					{identitycontrol.OperationGraphLink, agentgrant.PermissionIdentityLink},
					{identitycontrol.OperationPersonLink, agentgrant.PermissionIdentityLink},
					{identitycontrol.OperationGraphUnlink, agentgrant.PermissionIdentityUnlink},
					{identitycontrol.OperationPersonUnlink, agentgrant.PermissionIdentityUnlink},
				} {
					if !response.Delegated || auth.Grant.HasPermission(entry.permission) {
						response.IdentityOperations = append(response.IdentityOperations, string(entry.operation))
					}
				}
			}
			allowedRoutes["applyIdentityOperation"] = len(response.IdentityOperations) > 0
		}
		if _, ok := s.store.(InboxController); ok {
			for _, op := range []inboxcontrol.Operation{inboxcontrol.OpGetCapabilities, inboxcontrol.OpGetState, inboxcontrol.OpListFolders, inboxcontrol.OpTags, inboxcontrol.OpArchive, inboxcontrol.OpUnarchive, inboxcontrol.OpSetRead, inboxcontrol.OpSetUnread, inboxcontrol.OpMove, inboxcontrol.OpCreateFolder, inboxcontrol.OpReceiptGet, inboxcontrol.OpReconcile} {
				permission, _ := op.RequiredPermission()
				if !response.Delegated || inboxDiscoveryAllowed(auth.Grant, permission) {
					response.InboxOperations = append(response.InboxOperations, string(op))
				}
			}
			allowedRoutes["controlInbox"] = len(response.InboxOperations) > 0
		}
		if _, ok := s.store.(InboxCandidateReader); ok {
			allowedRoutes["listInboxCandidates"] = !response.Delegated || inboxDiscoveryAllowed(auth.Grant, agentgrant.PermissionInboxRead)
		}
		if _, ok := s.store.(InboxContextReader); ok {
			allowedRoutes["getInboxContext"] = !response.Delegated || inboxDiscoveryAllowed(auth.Grant, agentgrant.PermissionInboxContentRead)
		}
		if _, ok := s.store.(InboxTriageMappingReader); ok {
			allowedRoutes["getInboxTriageMappings"] = !response.Delegated || inboxDiscoveryAllowed(auth.Grant, agentgrant.PermissionInboxRead)
		}
		if _, ok := s.store.(InboxTriageController); ok {
			allowed := !response.Delegated || inboxTriageDiscoveryAllowed(auth.Grant)
			allowedRoutes["previewInboxTriage"] = allowed
			allowedRoutes["applyInboxTriage"] = allowed
		}
		if _, ok := s.store.(InboxTriageMappingUpdater); ok {
			allowedRoutes["updateInboxTriageMappings"] = !response.Delegated
		}
		if _, ok := s.store.(CalendarController); ok {
			allowedRoutes["controlCalendar"] = !response.Delegated || auth.Grant != nil && len(auth.Grant.Sources) > 0 &&
				(auth.Grant.HasPermission(agentgrant.PermissionCalendarRead) || auth.Grant.HasPermission(agentgrant.PermissionCalendarEventRead) || auth.Grant.HasPermission(agentgrant.PermissionCalendarWrite))
		}
		if describer, ok := s.store.(MCPCommandDescriber); ok {
			if _, runsCLI := s.store.(CLIRunner); runsCLI {
				for _, command := range describer.MCPCommandDescriptors() {
					if !response.Delegated || command.Delegated && auth.Grant != nil && len(auth.Grant.Sources) > 0 && delegatedCLIRunAdmitted([]string{command.Name}, auth.Grant) {
						response.Commands = append(response.Commands, command)
					}
				}
				allowedRoutes["runCLI"] = len(response.Commands) > 0
			}
		}
		for _, operation := range documentOperations(api.OpenAPI()) {
			if !allowedRoutes[operation.OperationID] {
				continue
			}
			response.Routes = append(response.Routes, mcpRouteDescriptor(api.OpenAPI(), operation))
		}
		sort.Strings(response.InboxOperations)
		sort.Strings(response.IdentityOperations)
		sort.Slice(response.Routes, func(i, j int) bool { return response.Routes[i].OperationID < response.Routes[j].OperationID })
		sort.Slice(response.Commands, func(i, j int) bool { return response.Commands[i].Name < response.Commands[j].Name })
		writeJSON(w, http.StatusOK, response)
	})
}

func inboxDiscoveryAllowed(grant *agentgrant.Grant, permission agentgrant.Permission) bool {
	if grant == nil || !grant.HasPermission(agentgrant.PermissionInboxRead) || !grant.HasPermission(permission) {
		return false
	}
	return slices.ContainsFunc(grant.Sources, func(source agentgrant.SourceRef) bool {
		switch source.Type {
		case "gmail", "imap", "msmail", "beeper":
			return true
		default:
			return false
		}
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

func inboxTriageDiscoveryAllowed(grant *agentgrant.Grant) bool {
	if grant == nil || !grant.HasPermission(agentgrant.PermissionInboxRead) || !grant.HasPermission(agentgrant.PermissionInboxTag) {
		return false
	}
	return slices.ContainsFunc(grant.Sources, func(source agentgrant.SourceRef) bool {
		return source.Type == "gmail" || source.Type == "imap" || source.Type == "msmail"
	})
}
