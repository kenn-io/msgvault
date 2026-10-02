package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"regexp"
	"slices"

	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpCardDAVPrefix = "/api/v1/carddav"

var mcpCardDAVConnectionName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type mcpCardDAVRoute struct {
	name, id, method, path string
	contextID, contextPath string
	query                  []string
	properties             []string
}

var mcpCardDAVRoutes = []mcpCardDAVRoute{
	{name: "list_carddav_connections", id: "listCardDAVConnections", method: http.MethodGet, path: mcpCardDAVPrefix + "/connections"},
	{name: "list_carddav_books", id: "listCardDAVBooks", method: http.MethodGet, path: mcpCardDAVPrefix + "/books"},
	{name: "list_carddav_runs", id: "listCardDAVRuns", method: http.MethodGet, path: mcpCardDAVPrefix + "/runs", query: []string{"limit", "before_id"}},
	{name: "get_carddav_connection_status", id: "getCardDAVStatus", method: http.MethodGet, path: mcpCardDAVPrefix + "/status"},
	{name: "sync_carddav_connections", id: "syncCardDAV", method: http.MethodPost, path: mcpCardDAVPrefix + "/sync", properties: []string{"full"}, contextID: "getCardDAVStatus", contextPath: mcpCardDAVPrefix + "/status"},
	{name: "update_carddav_book_roles", id: "updateCardDAVBookRoles", method: http.MethodPatch, path: mcpCardDAVPrefix + "/books/{id}", properties: []string{"write_target", "subscribed", "lookup_source"}, contextID: "listCardDAVBooks", contextPath: mcpCardDAVPrefix + "/books"},
	{name: "list_carddav_conflicts", id: "listCardDAVConflicts", method: http.MethodGet, path: mcpCardDAVPrefix + "/conflicts"},
	{name: "get_carddav_conflict", id: "getCardDAVConflict", method: http.MethodGet, path: mcpCardDAVPrefix + "/conflicts/{id}"},
	{name: "resolve_carddav_conflict", id: "resolveCardDAVConflict", method: http.MethodPost, path: mcpCardDAVPrefix + "/conflicts/{id}/resolve", properties: []string{"choice"}, contextID: "getCardDAVConflict", contextPath: mcpCardDAVPrefix + "/conflicts/{id}"},
	{name: "unpublish_carddav_person", id: "unpublishCardDAVPerson", method: http.MethodDelete, path: mcpCardDAVPrefix + "/publications/{person_id}", contextID: "getCardDAVPublication", contextPath: mcpCardDAVPrefix + "/publications/{person_id}"},
}

// These options are constructed only by the fixed owning adapter. They keep
// additive query/body fields independent of generated pending-PR clients.
type mcpCardDAVOptions struct {
	path, query map[string]any
	body        any
}

func (o *mcpCardDAVOptions) GetPathParams() (map[string]any, error) { return o.path, nil }
func (o *mcpCardDAVOptions) GetQuery() (map[string]any, error)      { return o.query, nil }
func (o *mcpCardDAVOptions) GetBody() any                           { return o.body }
func (o *mcpCardDAVOptions) GetHeader() (map[string]string, error)  { return map[string]string{}, nil }

type cardDAVScopeArguments struct {
	Connection *string `json:"connection,omitempty"`
}
type cardDAVHistoryArguments struct {
	Connection *string `json:"connection,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
	BeforeID   *int64  `json:"before_id,omitempty"`
}
type cardDAVSyncArguments struct {
	Connection *string `json:"connection,omitempty"`
	Full       bool    `json:"full,omitempty"`
}
type cardDAVRolesArguments struct {
	BookID       int64 `json:"book_id"`
	WriteTarget  *bool `json:"write_target"`
	Subscribed   *bool `json:"subscribed"`
	LookupSource *bool `json:"lookup_source"`
}
type cardDAVConflictArguments struct {
	ConflictID int64 `json:"conflict_id"`
}
type cardDAVResolveArguments struct {
	ConflictID int64  `json:"conflict_id"`
	Choice     string `json:"choice"`
}
type cardDAVPersonArguments struct {
	PersonID int64 `json:"person_id"`
}

func decodeMCPCardDAVArguments[T any](args map[string]any) (T, error) {
	for _, value := range args {
		if value == nil {
			var empty T
			return empty, errors.New("null argument")
		}
	}
	return decodeMCPOperationArguments[T](args)
}

func (b *daemonMCPOperations) SupportsCardDAVConnections() bool { return b.cardDAVNamed }

func (b *daemonMCPOperations) cardDAVQuery(connection *string) (map[string]any, error) {
	query := map[string]any{}
	if connection != nil {
		if !b.cardDAVNamed || !mcpCardDAVConnectionName.MatchString(*connection) {
			return nil, errors.New("unsupported connection selector")
		}
		query["connection"] = *connection
	}
	return query, nil
}

func (b *daemonMCPOperations) cardDAVOperationOptions(name string, args map[string]any) (*mcpCardDAVOptions, bool, error) {
	if !slices.ContainsFunc(mcpCardDAVRoutes, func(route mcpCardDAVRoute) bool { return route.name == name }) {
		return nil, false, nil
	}
	options := &mcpCardDAVOptions{}
	var err error
	switch name {
	case "list_carddav_connections", "list_carddav_conflicts":
		_, err = decodeMCPCardDAVArguments[struct{}](args)
	case "list_carddav_books", "get_carddav_connection_status":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVScopeArguments](args)
		err = decodeErr
		if err == nil {
			options.query, err = b.cardDAVQuery(input.Connection)
		}
	case "list_carddav_runs":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVHistoryArguments](args)
		err = decodeErr
		if err == nil {
			options.query, err = b.cardDAVQuery(input.Connection)
		}
		if err == nil {
			if input.Limit != nil {
				if *input.Limit < 1 || *input.Limit > 100 {
					err = errors.New("invalid limit")
				} else {
					options.query["limit"] = *input.Limit
				}
			}
			if input.BeforeID != nil {
				if *input.BeforeID <= 0 {
					err = errors.New("invalid cursor")
				} else {
					options.query["before_id"] = *input.BeforeID
				}
			}
		}
	case "sync_carddav_connections":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVSyncArguments](args)
		err = decodeErr
		if err == nil {
			_, err = b.cardDAVQuery(input.Connection)
		}
		if err == nil {
			body := map[string]any{"full": input.Full}
			if input.Connection != nil {
				body["connection"] = *input.Connection
			}
			options.body = body
		}
	case "update_carddav_book_roles":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVRolesArguments](args)
		err = decodeErr
		if err == nil && (input.BookID <= 0 || input.WriteTarget == nil || input.Subscribed == nil || input.LookupSource == nil) {
			err = errors.New("missing book roles")
		}
		if err == nil {
			options.path = map[string]any{"id": input.BookID}
			options.body = map[string]any{"write_target": *input.WriteTarget, "subscribed": *input.Subscribed, "lookup_source": *input.LookupSource}
		}
	case "get_carddav_conflict":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVConflictArguments](args)
		err = decodeErr
		if err == nil && input.ConflictID <= 0 {
			err = errors.New("invalid conflict")
		}
		options.path = map[string]any{"id": input.ConflictID}
	case "resolve_carddav_conflict":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVResolveArguments](args)
		err = decodeErr
		if err == nil && (input.ConflictID <= 0 || input.Choice != "keep_local" && input.Choice != "keep_remote") {
			err = errors.New("invalid resolution")
		}
		options.path = map[string]any{"id": input.ConflictID}
		options.body = map[string]any{"choice": input.Choice}
	case "unpublish_carddav_person":
		input, decodeErr := decodeMCPCardDAVArguments[cardDAVPersonArguments](args)
		err = decodeErr
		if err == nil && input.PersonID <= 0 {
			err = errors.New("invalid person")
		}
		options.path = map[string]any{"person_id": input.PersonID}
	}
	return options, true, err
}

func (b *daemonMCPOperations) executeCardDAVOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	options, handled, err := b.cardDAVOperationOptions(name, args)
	if !handled {
		return nil, false, nil
	}
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments are a fixed structured refusal.
	}
	var route mcpCardDAVRoute
	for _, candidate := range mcpCardDAVRoutes {
		if candidate.name == name {
			route = candidate
			break
		}
	}
	var result *mcpserver.OperationResult
	switch name {
	case "list_carddav_connections":
		result, err = readMCPOperationJSON[mcpserver.CardDAVConnections](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "list_carddav_books":
		result, err = readMCPOperationJSON[mcpserver.CardDAVBooks](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "list_carddav_runs":
		result, err = readMCPOperationJSON[mcpserver.CardDAVRuns](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "get_carddav_connection_status":
		result, err = readMCPOperationJSON[mcpserver.CardDAVStatus](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "sync_carddav_connections":
		result, err = readMCPOperationJSON[mcpserver.CardDAVSync](ctx, b.client, route.method, route.path, options, http.StatusOK)
		if result != nil && !result.IsError {
			if output, ok := result.Output.(mcpserver.CardDAVSync); ok {
				result.IsError = output.Status == "partial" || output.Status == "failed"
			}
		}
	case "update_carddav_book_roles":
		result, err = readMCPOperationJSON[mcpserver.CardDAVBook](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "list_carddav_conflicts":
		result, err = readMCPOperationJSON[generated.CardDAVConflictsResponse](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "get_carddav_conflict":
		result, err = readMCPOperationJSON[generated.CardDAVConflictDetailResponse](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "resolve_carddav_conflict":
		result, err = readMCPOperationJSON[generated.CardDAVConflictResolutionResponse](ctx, b.client, route.method, route.path, options, http.StatusOK)
	case "unpublish_carddav_person":
		result, err = readMCPOperationJSON[generated.CardDAVPublicationResponse](ctx, b.client, route.method, route.path, options, http.StatusOK)
		if result != nil && result.IsError {
			// One read of durable state retains queued/remote status. It never
			// repeats the write, even when its completion is uncertain.
			data, encodeErr := json.Marshal(result.Output)
			var failure mcpserver.CardDAVUnpublicationFailure
			if encodeErr == nil && json.Unmarshal(data, &failure) == nil {
				current, readErr := readMCPOperationJSON[generated.CardDAVPublicationResponse](ctx, b.client, http.MethodGet, mcpCardDAVPrefix+"/publications/{person_id}", options, http.StatusOK)
				if readErr == nil && current != nil && !current.IsError {
					if publication, ok := current.Output.(generated.CardDAVPublicationResponse); ok {
						failure.Publication = &publication
					}
				}
				result.Output = failure
			}
		}
	}
	return result, true, err
}

func (b *daemonMCPOperations) cardDAVOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	options, handled, err := b.cardDAVOperationOptions(name, args)
	if !handled {
		return "", false, nil
	}
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	var subject *mcpserver.OperationResult
	var effects string
	switch name {
	case "sync_carddav_connections":
		input, _ := decodeMCPCardDAVArguments[cardDAVSyncArguments](args)
		query, _ := b.cardDAVQuery(input.Connection)
		if input.Connection == nil && b.cardDAVNamed {
			subject, err = b.ExecuteOperation(ctx, "list_carddav_connections", nil)
		} else {
			subject, err = readMCPOperationJSON[mcpserver.CardDAVStatus](ctx, b.client, http.MethodGet, mcpCardDAVPrefix+"/status", &mcpCardDAVOptions{query: query}, http.StatusOK)
		}
		effects = "Contact the saved CardDAV destinations and reconcile inbound contacts and queued publication mutations. Omitted scope runs enabled connections; selected manual scope can run a disabled one. Busy/unavailable connections may not start a run; partial results remain failures."
	case "update_carddav_book_roles":
		subject, err = b.ExecuteOperation(ctx, "list_carddav_books", nil)
		effects = "Replace the selected book's three roles. Only one global publication target is allowed; pending publications can prevent switching. Future sync can import contacts or publish approved profiles under these roles."
	case "resolve_carddav_conflict":
		input, _ := decodeMCPCardDAVArguments[cardDAVResolveArguments](args)
		subject, err = b.ExecuteOperation(ctx, "get_carddav_conflict", map[string]any{"conflict_id": input.ConflictID})
		effects = "Apply the selected allowed conflict resolution through the owning connection; this may change local profile evidence or publish to the remote address book."
	case "unpublish_carddav_person":
		subject, err = readMCPOperationJSON[generated.CardDAVPublicationResponse](ctx, b.client, http.MethodGet, mcpCardDAVPrefix+"/publications/{person_id}", options, http.StatusOK)
		effects = "Remove the publication intent and reconcile remote deletion. Preserve queued delete/remote state until the daemon completes it."
	default:
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_not_supported"}
	}
	if err != nil {
		return "", true, err
	}
	if subject == nil || subject.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "carddav_context_unavailable"}
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Arguments map[string]any `json:"arguments"`
		Current   any            `json:"current"`
		Effects   string         `json:"effects"`
	}{name, args, subject.Output, effects}, json.Deterministic(true))
	return string(data), true, err
}
