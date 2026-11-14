package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func employmentMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	body := []string{"person_id", "organization_id", "source", "title", "role", "department", "location", "address_id", "description", "start_date", "end_date", "is_current", "is_primary", "source_ref", "confidence", "expected_primary"}
	for _, r := range []struct {
		name, id, method, path string
		query                  []string
	}{
		{"get_employment", "getEmployment", http.MethodGet, "/api/v1/employments/{id}", nil},
		{"list_person_employments", "listPersonEmployments", http.MethodGet, "/api/v1/people/{id}/employments", []string{"current_only", "limit", "offset"}},
		{"list_organization_employments", "listOrganizationEmployments", http.MethodGet, "/api/v1/organizations/{id}/employments", []string{"current_only", "limit", "offset"}},
	} {
		if hasRoute(r.id, r.method, r.path, r.query...) {
			names = append(names, r.name)
		}
	}
	for _, r := range []struct {
		name, id, method, path string
		properties             []string
		needsGet               bool
	}{
		{"create_employment", "createEmployment", http.MethodPost, "/api/v1/employments", body, false},
		{"replace_employment", "patchEmployment", http.MethodPatch, "/api/v1/employments/{id}", body, true},
		{"end_employment", "endEmployment", http.MethodPost, "/api/v1/employments/{id}/end", []string{"end_date"}, true},
		{"delete_employment", "deleteEmployment", http.MethodDelete, "/api/v1/employments/{id}", nil, true},
	} {
		if hasRoute(r.id, r.method, r.path) && (!r.needsGet || slices.Contains(names, "get_employment")) && ((r.name != "replace_employment" && r.name != "create_employment") || slices.Contains(names, "list_person_employments")) && (len(r.properties) == 0 || mcpRequestPropertiesPresent(capabilities, r.id, r.properties...)) {
			names = append(names, r.name)
		}
	}
	return names
}

type employmentIDArgs struct {
	EmploymentID int64  `json:"employment_id"`
	ETag         string `json:"etag,omitempty"`
}
type employmentBodyArgs struct {
	EmploymentID int64                     `json:"employment_id,omitempty"`
	ETag         string                    `json:"etag,omitempty"`
	Employment   *generated.EmploymentBody `json:"employment"`
}
type employmentEndArgs struct {
	EmploymentID int64  `json:"employment_id"`
	ETag         string `json:"etag"`
	EndDate      string `json:"end_date"`
}
type employmentListArgs struct {
	PersonID       int64  `json:"person_id,omitempty"`
	OrganizationID int64  `json:"organization_id,omitempty"`
	CurrentOnly    *bool  `json:"current_only,omitempty"`
	Limit          *int64 `json:"limit,omitempty"`
	Offset         *int64 `json:"offset,omitempty"`
}

func (b *daemonMCPOperations) executeEmploymentOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure(invalidArguments, false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "get_employment", "delete_employment":
		a, e := decodeMCPOperationArguments[employmentIDArgs](args)
		if e != nil {
			return invalid()
		}
		switch name {
		case "get_employment":
			result, err = readMCPOperationJSON[generated.Employment](ctx, b.client, http.MethodGet, "/api/v1/employments/{id}", &generated.GetEmploymentRequestOptions{PathParams: &generated.GetEmploymentPath{ID: a.EmploymentID}}, http.StatusOK)
		case "delete_employment":
			_, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodDelete, "/api/v1/employments/{id}", &generated.DeleteEmploymentRequestOptions{PathParams: &generated.DeleteEmploymentPath{ID: a.EmploymentID}, Header: &generated.DeleteEmploymentHeaders{IfMatch: a.ETag}}, 1<<20, http.StatusNoContent)
			if failure != nil {
				return failure, true, readErr
			}
			result = &mcpserver.OperationResult{Output: struct{}{}}
		}
	case "create_employment":
		a, e := decodeMCPOperationArguments[employmentBodyArgs](args)
		if e != nil || a.Employment == nil || a.Employment.IsPrimary == nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.Employment](ctx, b.client, http.MethodPost, "/api/v1/employments", &generated.CreateEmploymentRequestOptions{Body: a.Employment}, http.StatusCreated)
	case "replace_employment":
		a, e := decodeMCPOperationArguments[employmentBodyArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.Employment](ctx, b.client, http.MethodPatch, "/api/v1/employments/{id}", &generated.PatchEmploymentRequestOptions{PathParams: &generated.PatchEmploymentPath{ID: a.EmploymentID}, Header: &generated.PatchEmploymentHeaders{IfMatch: a.ETag}, Body: a.Employment}, http.StatusOK)
	case "end_employment":
		a, e := decodeMCPOperationArguments[employmentEndArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.Employment](ctx, b.client, http.MethodPost, "/api/v1/employments/{id}/end", &generated.EndEmploymentRequestOptions{PathParams: &generated.EndEmploymentPath{ID: a.EmploymentID}, Header: &generated.EndEmploymentHeaders{IfMatch: a.ETag}, Body: &generated.EndEmploymentBody{EndDate: a.EndDate}}, http.StatusOK)
	case "list_person_employments", "list_organization_employments":
		a, e := decodeMCPOperationArguments[employmentListArgs](args)
		if e != nil {
			return invalid()
		}
		if name == "list_person_employments" {
			result, err = readMCPOperationJSON[generated.EmploymentsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/employments", &generated.ListPersonEmploymentsRequestOptions{PathParams: &generated.ListPersonEmploymentsPath{ID: a.PersonID}, Query: &generated.ListPersonEmploymentsQuery{CurrentOnly: a.CurrentOnly, Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
		} else {
			result, err = readMCPOperationJSON[generated.EmploymentsResponse](ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}/employments", &generated.ListOrganizationEmploymentsRequestOptions{PathParams: &generated.ListOrganizationEmploymentsPath{ID: a.OrganizationID}, Query: &generated.ListOrganizationEmploymentsQuery{CurrentOnly: a.CurrentOnly, Limit: a.Limit, Offset: a.Offset}}, http.StatusOK)
		}
	default:
		return nil, false, nil
	}
	if name != "delete_employment" && name != "list_person_employments" && name != "list_organization_employments" {
		result = etagProjection(result, name != "get_employment", func(etag string, employment generated.Employment) mcpserver.EmploymentRecord {
			return mcpserver.EmploymentRecord{ETag: etag, Employment: employment}
		})
	}
	return result, true, err
}

func (b *daemonMCPOperations) employmentOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	switch name {
	case "create_employment", "replace_employment", "end_employment", "delete_employment":
	default:
		return "", false, nil
	}
	invalid := func() (string, bool, error) {
		return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
	}
	var id int64
	var input *generated.EmploymentBody
	switch name {
	case "create_employment", "replace_employment":
		a, e := decodeMCPOperationArguments[employmentBodyArgs](args)
		if e != nil || a.Employment == nil || (name == "create_employment" && a.Employment.IsPrimary == nil) {
			return invalid()
		}
		id, input = a.EmploymentID, a.Employment
	case "end_employment":
		a, e := decodeMCPOperationArguments[employmentEndArgs](args)
		if e != nil {
			return invalid()
		}
		id = a.EmploymentID
	default:
		a, e := decodeMCPOperationArguments[employmentIDArgs](args)
		if e != nil {
			return invalid()
		}
		id = a.EmploymentID
	}
	var current *mcpserver.EmploymentRecord
	var err error
	if name != "create_employment" {
		current, err = b.employmentDisclosureContext(ctx, name, id)
		if err != nil {
			return "", true, err
		}
	}
	var primary *generated.Employment
	if input != nil {
		willBePrimary := input.IsPrimary != nil && *input.IsPrimary
		if input.IsPrimary == nil && current != nil && current.Employment.IsPrimary && input.EndDate == nil && (input.IsCurrent == nil || *input.IsCurrent) {
			willBePrimary = true
		}
		if willBePrimary {
			listed, _, e := b.executeEmploymentOperation(ctx, "list_person_employments", map[string]any{"person_id": input.PersonID, "current_only": true, "limit": 1})
			if e != nil {
				return "", true, e
			}
			if e := operationReadFailure(listed, nativeDefiniteRefusal); e != nil {
				return "", true, e
			}
			rows, ok := listed.Output.(generated.EmploymentsResponse)
			if !ok {
				return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
			}
			guard := map[string]any{"id": int64(0), "revision": int64(0)}
			if len(rows.Employments) > 0 && rows.Employments[0].IsPrimary {
				primary = &rows.Employments[0]
				primary.Description = nil
				if name == "create_employment" {
					return "", true, &mcpserver.OperationRefusalError{Code: "employment_primary_conflict"}
				}
				guard = map[string]any{"id": primary.ID, "revision": primary.Revision}
			}
			body, ok := args["employment"].(map[string]any)
			if !ok {
				return invalid()
			}
			if _, exists := body["expected_primary"]; !exists {
				body["expected_primary"] = guard
			}
		}
	}
	effect := map[string]string{
		"create_employment":  "Create this employment record; it may become the person's primary current employment.",
		"replace_employment": "Replace mutable fields; omitted ones clear. Omitted is_current is kept only without end_date; omitted is_primary only while the record stays current.",
		"end_employment":     "End this employment on the given date; it is no longer current or primary.",
		"delete_employment":  "Permanently delete this employment record.",
	}[name]
	data, err := json.Marshal(struct {
		Operation string                      `json:"operation"`
		Current   *mcpserver.EmploymentRecord `json:"current,omitempty"`
		Primary   *generated.Employment       `json:"primary,omitempty"`
		Changes   map[string]any              `json:"changes"`
		Effect    string                      `json:"effect"`
	}{name, current, primary, args, effect}, json.Deterministic(true))
	return string(data), true, err
}

// employmentDisclosureContext reads the record an operation acts on.
func (b *daemonMCPOperations) employmentDisclosureContext(ctx context.Context, name string, id int64) (*mcpserver.EmploymentRecord, error) {
	result, _, err := b.executeEmploymentOperation(ctx, "get_employment", map[string]any{"employment_id": id})
	if err != nil {
		return nil, err
	}
	if err := operationReadFailure(result, func(code string) bool {
		return code == "operation_response_limit_exceeded" || nativeDefiniteRefusal(code)
	}); err != nil {
		return nil, err
	}
	current, ok := result.Output.(mcpserver.EmploymentRecord)
	if !ok {
		return nil, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if name != "replace_employment" && name != "delete_employment" {
		current.Employment.Description = nil
	}
	return &current, nil
}
