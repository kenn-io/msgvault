package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type daemonMCPCardDAV struct {
	*daemonclient.Client
	selector bool
}

func newDaemonMCPCardDAV(client *daemonclient.Client, caps *apiprotocol.MCPCapabilities) *daemonMCPCardDAV {
	b := &daemonMCPCardDAV{Client: client}
	b.selector = hasRoute(caps, "syncCardDAV", http.MethodPost, "/api/v1/carddav/sync") &&
		mcpRequestPropertiesPresent(caps, "syncCardDAV", "connection") &&
		hasRoute(caps, "getCardDAVStatus", http.MethodGet, "/api/v1/carddav/status", "connection")
	return b
}

func (b *daemonMCPCardDAV) SupportsCardDAVSelector() bool { return b.selector }

func cardDAVMCPCapabilities(has mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path string
		query                  []string
		props                  []string
	}{
		{"list_carddav_connections", "listCardDAVConnections", "GET", "/api/v1/carddav/connections", nil, nil},
		{"list_carddav_books", "listCardDAVBooks", "GET", "/api/v1/carddav/books", []string{"connection"}, nil},
		{"list_carddav_runs", "listCardDAVRuns", "GET", "/api/v1/carddav/runs", []string{"connection", "limit", "before_id"}, nil},
		{"list_carddav_conflicts", "listCardDAVConflicts", "GET", "/api/v1/carddav/conflicts", nil, nil},
		{"get_carddav_conflict", "getCardDAVConflict", "GET", "/api/v1/carddav/conflicts/{id}", nil, nil},
		{"update_carddav_book_roles", "updateCardDAVBookRoles", "PATCH", "/api/v1/carddav/books/{id}", nil, []string{"write_target", "subscribed", "lookup_source"}},
		{"resolve_carddav_conflict", "resolveCardDAVConflict", "POST", "/api/v1/carddav/conflicts/{id}/resolve", nil, []string{"choice"}},
		{"unpublish_carddav_person", "unpublishCardDAVPerson", "DELETE", "/api/v1/carddav/publications/{person_id}", nil, nil},
	} {
		if has(r.id, r.method, r.path, r.query...) && mcpRequestPropertiesPresent(caps, r.id, r.props...) {
			names = append(names, r.name)
		}
	}
	// Every write disclosure resolves the book to its owning connection.
	if !slices.Contains(names, "list_carddav_books") {
		names = slices.DeleteFunc(names, func(n string) bool {
			return n == "update_carddav_book_roles" || n == "resolve_carddav_conflict" || n == "unpublish_carddav_person"
		})
	}
	if !slices.Contains(names, "get_carddav_conflict") {
		names = slices.DeleteFunc(names, func(n string) bool { return n == "resolve_carddav_conflict" })
	}
	if !has("getCardDAVPublication", http.MethodGet, "/api/v1/carddav/publications/{person_id}") {
		names = slices.DeleteFunc(names, func(n string) bool { return n == "unpublish_carddav_person" })
	}
	return names
}

type cardDAVArguments struct {
	Connection   *string `json:"connection,omitempty"`
	Limit        *int64  `json:"limit,omitempty"`
	BeforeID     *int64  `json:"before_id,omitempty"`
	BookID       int64   `json:"book_id,omitempty"`
	ConflictID   int64   `json:"conflict_id,omitempty"`
	PersonID     int64   `json:"person_id,omitempty"`
	WriteTarget  *bool   `json:"write_target,omitempty"`
	Subscribed   *bool   `json:"subscribed,omitempty"`
	LookupSource *bool   `json:"lookup_source,omitempty"`
	Choice       string  `json:"choice,omitempty"`
}

func validCardDAVArguments(name string, a cardDAVArguments) bool {
	switch name {
	case "list_carddav_connections", "list_carddav_conflicts", "list_carddav_books", "list_carddav_runs", "get_carddav_conflict", "unpublish_carddav_person":
		return true
	case "update_carddav_book_roles":
		return a.WriteTarget != nil && a.Subscribed != nil && a.LookupSource != nil
	case "resolve_carddav_conflict":
		return a.Choice == "keep_local" || a.Choice == "keep_remote"
	}
	return false
}

func cardDAVRoleRefusal(code string) bool {
	switch code {
	case "conflict", "server_busy", "carddav_unavailable":
		return true
	}
	return false
}

// Native recovery and post-prepare stages can return these fixed refusals
// after a remote or local mutation has already occurred.
func cardDAVUncertainRefusals(enabled bool) []string {
	if !enabled {
		return nil
	}
	return []string{"not_found", "conflict", "carddav_conflict_stale", "carddav_conflict_pending", "carddav_publication_pending", "carddav_retry_after", "carddav_remote_protected", "carddav_remote_invalid", "carddav_inference_review_required", "carddav_review_stale", "carddav_icloud_reconnect_required", "google_authorization_required", "microsoft_authorization_required", "carddav_preview_too_large", "microsoft_contact_too_large", "carddav_upstream_failed", "carddav_storage_failed"}
}

func (b *daemonMCPOperations) executeCardDAVOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !slices.Contains([]string{"list_carddav_connections", "list_carddav_books", "list_carddav_runs", "list_carddav_conflicts", "get_carddav_conflict", "update_carddav_book_roles", "resolve_carddav_conflict", "unpublish_carddav_person"}, name) {
		return nil, false, nil
	}
	a, err := decodeMCPOperationArguments[cardDAVArguments](args)
	if err != nil || !validCardDAVArguments(name, a) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	switch name {
	case "list_carddav_connections":
		result, err = readMCPOperationJSON[generated.CardDAVConnectionsResponse](ctx, b.client, "GET", "/api/v1/carddav/connections", nil, 200)
		if result != nil && !result.IsError {
			out, ok := result.Output.(generated.CardDAVConnectionsResponse)
			if !ok {
				return operationFailure("invalid_operation_response", false), true, nil
			}
			items := make([]mcpserver.CardDAVConnection, 0, len(out.Connections))
			for _, c := range out.Connections {
				items = append(items, mcpserver.CardDAVConnection{AccountID: c.AccountID, Connection: c.Connection, Orphaned: c.Orphaned, Provider: c.Provider, Status: c.Status})
			}
			result.Output = mcpserver.CardDAVConnections{Connections: items}
		}
	case "list_carddav_books":
		result, err = readMCPOperationJSON[generated.CardDAVBooksResponse](ctx, b.client, "GET", "/api/v1/carddav/books", &generated.ListCardDAVBooksRequestOptions{Query: &generated.ListCardDAVBooksQuery{Connection: a.Connection}}, 200)
	case "list_carddav_runs":
		result, err = readMCPOperationJSON[generated.CardDAVRunsResponse](ctx, b.client, "GET", "/api/v1/carddav/runs", &generated.ListCardDAVRunsRequestOptions{Query: &generated.ListCardDAVRunsQuery{Connection: a.Connection, Limit: a.Limit, BeforeID: a.BeforeID}}, 200)
	case "list_carddav_conflicts":
		result, err = readMCPOperationJSON[generated.CardDAVConflictsResponse](ctx, b.client, "GET", "/api/v1/carddav/conflicts", nil, 200)
	case "get_carddav_conflict":
		result, err = readMCPOperationJSON[generated.CardDAVConflictDetailResponse](ctx, b.client, "GET", "/api/v1/carddav/conflicts/{id}", &generated.GetCardDAVConflictRequestOptions{PathParams: &generated.GetCardDAVConflictPath{ID: a.ConflictID}}, 200)
	case "update_carddav_book_roles":
		result, err = readMCPOperationJSONWithUncertainCodes[generated.CardDAVBookResponse](ctx, b.client, "PATCH", "/api/v1/carddav/books/{id}", &generated.UpdateCardDAVBookRolesRequestOptions{PathParams: &generated.UpdateCardDAVBookRolesPath{ID: a.BookID}, Body: &generated.UpdateCardDAVBookRolesBody{WriteTarget: *a.WriteTarget, Subscribed: *a.Subscribed, LookupSource: *a.LookupSource}}, cardDAVRoleRefusal, []string{"not_found"}, 200)
	case "resolve_carddav_conflict":
		result, err = readMCPOperationJSONWithUncertainCodes[generated.CardDAVConflictResolutionResponse](ctx, b.client, "POST", "/api/v1/carddav/conflicts/{id}/resolve", &generated.ResolveCardDAVConflictRequestOptions{PathParams: &generated.ResolveCardDAVConflictPath{ID: a.ConflictID}, Body: &generated.ResolveCardDAVConflictBody{Choice: generated.CardDAVResolveRequestChoice(a.Choice)}}, func(code string) bool {
			if code == "carddav_unavailable" || code == "server_busy" {
				return true
			}
			if a.Choice == "keep_remote" {
				switch code {
				case "carddav_conflict_stale", "carddav_remote_protected", "carddav_remote_invalid", "carddav_retry_after", "carddav_publication_pending":
					return true
				}
			}
			return false
		}, cardDAVUncertainRefusals(a.Choice == "keep_local"), 200)
	case "unpublish_carddav_person":
		result, err = readMCPOperationJSONWithUncertainCodes[generated.CardDAVPublicationResponse](ctx, b.client, "DELETE", "/api/v1/carddav/publications/{person_id}", &generated.UnpublishCardDAVPersonRequestOptions{PathParams: &generated.UnpublishCardDAVPersonPath{PersonID: a.PersonID}}, func(code string) bool { return code == "carddav_unavailable" || code == "server_busy" }, cardDAVUncertainRefusals(true), 200)
	}
	return result, true, err
}

func (b *daemonMCPOperations) cardDAVOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "update_carddav_book_roles" && name != "resolve_carddav_conflict" && name != "unpublish_carddav_person" {
		return "", false, nil
	}
	a, err := decodeMCPOperationArguments[cardDAVArguments](args)
	if err != nil || !validCardDAVArguments(name, a) {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	bookID := a.BookID
	var current any
	switch name {
	case "resolve_carddav_conflict":
		r, _, e := b.executeCardDAVOperation(ctx, "get_carddav_conflict", map[string]any{"conflict_id": a.ConflictID})
		if e != nil || r == nil || r.IsError {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		v, ok := r.Output.(generated.CardDAVConflictDetailResponse)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		bookID, current = v.AddressBook.ID, v
	case "unpublish_carddav_person":
		r, e := readMCPOperationJSON[generated.CardDAVPublicationResponse](ctx, b.client, "GET", "/api/v1/carddav/publications/{person_id}", &generated.GetCardDAVPublicationRequestOptions{PathParams: &generated.GetCardDAVPublicationPath{PersonID: a.PersonID}}, 200)
		if e != nil || r == nil || r.IsError {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		v, ok := r.Output.(generated.CardDAVPublicationResponse)
		if !ok || v.AddressBook == nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
		bookID, current = v.AddressBook.ID, v
	}
	r, _, e := b.executeCardDAVOperation(ctx, "list_carddav_books", nil)
	if e != nil || r == nil || r.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	books, ok := r.Output.(generated.CardDAVBooksResponse)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	var previousTarget *generated.CardDAVBookResponse
	if name == "update_carddav_book_roles" && *a.WriteTarget {
		for i := range books.Books {
			if books.Books[i].WriteTarget && books.Books[i].ID != bookID {
				previousTarget = &books.Books[i]
				break
			}
		}
		if previousTarget != nil && (previousTarget.Connection == nil || *previousTarget.Connection == "") {
			return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
		}
	}
	for _, book := range books.Books {
		if book.ID == bookID && book.Connection != nil && *book.Connection != "" {
			if current == nil {
				current = book
			}
			resolveEffect := "resolve the conflict by keeping the local version; this can create, update or remove a remote contact"
			if a.Choice == "keep_remote" {
				resolveEffect = "resolve the conflict by keeping the remote version; this can change the local person"
			}
			effect := map[string]string{"update_carddav_book_roles": "replace all three address book roles; setting the write target releases the previous target; removing subscription or lookup can demote or remove local CardDAV resource mappings", "resolve_carddav_conflict": resolveEffect, "unpublish_carddav_person": "unpublish the person; this can remove a remote contact while retaining the local person"}[name]
			data, e := json.Marshal(struct {
				Operation      string                         `json:"operation"`
				Connection     string                         `json:"connection"`
				BookID         int64                          `json:"book_id"`
				Current        any                            `json:"current"`
				PreviousTarget *generated.CardDAVBookResponse `json:"previous_write_target,omitempty"`
				Changes        map[string]any                 `json:"changes"`
				Effect         string                         `json:"effect"`
			}{name, *book.Connection, bookID, current, previousTarget, args, effect}, json.Deterministic(true))
			return string(data), true, e
		}
	}
	return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
}
