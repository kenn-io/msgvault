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
	body := []string{"person_id", "organization_id", "source", "title", "role", "department", "location", "address_id", "description", "start_date", "end_date", "is_current", "is_primary", "source_ref", "confidence"}
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
		{"set_primary_employment", "setPrimaryEmployment", http.MethodPost, "/api/v1/employments/{id}/primary", nil, true},
		{"delete_employment", "deleteEmployment", http.MethodDelete, "/api/v1/employments/{id}", nil, true},
	} {
		if hasRoute(r.id, r.method, r.path) && (!r.needsGet || slices.Contains(names, "get_employment")) && (len(r.properties) == 0 || mcpRequestPropertiesPresent(capabilities, r.id, r.properties...)) {
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

func employmentDefiniteRefusal(code string) bool {
	switch code {
	case "employment_not_found", "employment_revision_conflict", "employment_primary_conflict", "employment_duplicate_active", "invalid_employment", "invalid_partial_date", "invalid_source", "invalid_person_id", "invalid_organization_id", "invalid_organization", "organization_not_found", "organizations_unavailable", "invalid_if_match", "employments_unavailable", "server_busy":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executeEmploymentOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure(invalidArguments, false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "get_employment", "set_primary_employment", "delete_employment":
		a, e := decodeMCPOperationArguments[employmentIDArgs](args)
		if e != nil {
			return invalid()
		}
		switch name {
		case "get_employment":
			result, err = readMCPOperationJSONWithClassifier[generated.Employment](ctx, b.client, http.MethodGet, "/api/v1/employments/{id}", &generated.GetEmploymentRequestOptions{PathParams: &generated.GetEmploymentPath{ID: a.EmploymentID}}, employmentDefiniteRefusal, http.StatusOK)
		case "set_primary_employment":
			result, err = readMCPOperationJSONWithClassifier[generated.Employment](ctx, b.client, http.MethodPost, "/api/v1/employments/{id}/primary", &generated.SetPrimaryEmploymentRequestOptions{PathParams: &generated.SetPrimaryEmploymentPath{ID: a.EmploymentID}, Header: &generated.SetPrimaryEmploymentHeaders{IfMatch: a.ETag}}, employmentDefiniteRefusal, http.StatusOK)
		case "delete_employment":
			_, _, failure, readErr := readMCPOperationBytes(ctx, b.client, http.MethodDelete, "/api/v1/employments/{id}", &generated.DeleteEmploymentRequestOptions{PathParams: &generated.DeleteEmploymentPath{ID: a.EmploymentID}, Header: &generated.DeleteEmploymentHeaders{IfMatch: a.ETag}}, 1<<20, employmentDefiniteRefusal, http.StatusNoContent)
			if failure != nil {
				return failure, true, readErr
			}
			result = &mcpserver.OperationResult{Output: struct{}{}}
		}
	case "create_employment", "replace_employment":
		a, e := decodeMCPOperationArguments[employmentBodyArgs](args)
		if e != nil {
			return invalid()
		}
		if name == "create_employment" {
			result, err = readMCPOperationJSONWithClassifier[generated.Employment](ctx, b.client, http.MethodPost, "/api/v1/employments", &generated.CreateEmploymentRequestOptions{Body: a.Employment}, employmentDefiniteRefusal, http.StatusCreated)
		} else {
			result, err = readMCPOperationJSONWithClassifier[generated.Employment](ctx, b.client, http.MethodPatch, "/api/v1/employments/{id}", &generated.PatchEmploymentRequestOptions{PathParams: &generated.PatchEmploymentPath{ID: a.EmploymentID}, Header: &generated.PatchEmploymentHeaders{IfMatch: a.ETag}, Body: a.Employment}, employmentDefiniteRefusal, http.StatusOK)
		}
	case "end_employment":
		a, e := decodeMCPOperationArguments[employmentEndArgs](args)
		if e != nil {
			return invalid()
		}
		result, err = readMCPOperationJSONWithClassifier[generated.Employment](ctx, b.client, http.MethodPost, "/api/v1/employments/{id}/end", &generated.EndEmploymentRequestOptions{PathParams: &generated.EndEmploymentPath{ID: a.EmploymentID}, Header: &generated.EndEmploymentHeaders{IfMatch: a.ETag}, Body: &generated.EndEmploymentBody{EndDate: a.EndDate}}, employmentDefiniteRefusal, http.StatusOK)
	case "list_person_employments", "list_organization_employments":
		a, e := decodeMCPOperationArguments[employmentListArgs](args)
		if e != nil {
			return invalid()
		}
		if name == "list_person_employments" {
			result, err = readMCPOperationJSONWithClassifier[generated.EmploymentsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/employments", &generated.ListPersonEmploymentsRequestOptions{PathParams: &generated.ListPersonEmploymentsPath{ID: a.PersonID}, Query: &generated.ListPersonEmploymentsQuery{CurrentOnly: a.CurrentOnly, Limit: a.Limit, Offset: a.Offset}}, employmentDefiniteRefusal, http.StatusOK)
		} else {
			result, err = readMCPOperationJSONWithClassifier[generated.EmploymentsResponse](ctx, b.client, http.MethodGet, "/api/v1/organizations/{id}/employments", &generated.ListOrganizationEmploymentsRequestOptions{PathParams: &generated.ListOrganizationEmploymentsPath{ID: a.OrganizationID}, Query: &generated.ListOrganizationEmploymentsQuery{CurrentOnly: a.CurrentOnly, Limit: a.Limit, Offset: a.Offset}}, employmentDefiniteRefusal, http.StatusOK)
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
	case "create_employment", "replace_employment", "end_employment", "set_primary_employment", "delete_employment":
	default:
		return "", false, nil
	}
	invalid := func() (string, bool, error) {
		return "", true, &mcpserver.OperationRefusalError{Code: invalidArguments}
	}
	var id int64
	switch name {
	case "create_employment", "replace_employment":
		a, e := decodeMCPOperationArguments[employmentBodyArgs](args)
		if e != nil {
			return invalid()
		}
		id = a.EmploymentID
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
	var current any
	if name != "create_employment" {
		result, _, err := b.executeEmploymentOperation(ctx, "get_employment", map[string]any{"employment_id": id})
		if err != nil {
			return "", true, err
		}
		if err := operationContext(result, result.ETag, "employment_revision_conflict", employmentDefiniteRefusal); err != nil {
			return "", true, err
		}
		current = result.Output
	}
	effect := map[string]string{
		"create_employment":      "Create this employment record; it may become the person's primary current employment.",
		"replace_employment":     "Replace mutable fields; omitted ones clear, except current/primary flags.",
		"end_employment":         "End this employment on the given date; it is no longer current or primary.",
		"set_primary_employment": "Make this employment primary; the person's other primary employment is demoted.",
		"delete_employment":      "Permanently delete this employment record.",
	}[name]
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current,omitempty"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
