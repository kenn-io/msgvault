package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func personBriefMCPCapabilities(has mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, r := range []struct {
		name, id, method, path string
		query, props           []string
	}{
		{"list_person_brief_versions", "listPersonBriefVersions", http.MethodGet, "/api/v1/people/{id}/brief/versions", []string{"limit"}, nil},
		{"get_person_brief_enrollment", "getPersonBriefEnrollment", http.MethodGet, "/api/v1/people/{id}/brief-enrollment", nil, nil},
		{"set_person_brief_enrollment", "setPersonBriefEnrollment", http.MethodPut, "/api/v1/people/{id}/brief-enrollment", nil, []string{"enrolled", "track"}},
		{"generate_person_brief", "generatePersonBrief", http.MethodPost, "/api/v1/people/{id}/brief/generate", nil, nil},
		{"reject_person_brief", "rejectPersonBrief", http.MethodPost, "/api/v1/people/{id}/brief/reject", nil, []string{"reason"}},
	} {
		if has(r.id, r.method, r.path, r.query...) && mcpRequestPropertiesPresent(caps, r.id, r.props...) {
			names = append(names, r.name)
		}
	}
	return names
}

type briefArgs struct {
	PersonID int64   `json:"person_id"`
	Limit    *int64  `json:"limit,omitempty"`
	Enrolled *bool   `json:"enrolled,omitempty"`
	Track    *bool   `json:"track,omitempty"`
	Reason   *string `json:"reason,omitempty"`
}

func parseBriefArgs(name string, args map[string]any) (briefArgs, bool) {
	input, err := decodeMCPOperationArguments[briefArgs](args)
	return input, err == nil && (name != "set_person_brief_enrollment" || input.Enrolled != nil && input.Track != nil)
}

func isBriefOperation(name string) bool {
	switch name {
	case "list_person_brief_versions", "get_person_brief_enrollment", "set_person_brief_enrollment", "generate_person_brief", "reject_person_brief":
		return true
	}
	return false
}

func briefRefusal(code string) bool {
	switch code {
	case "server_busy", "invalid_request", "person_brief_not_found", "person_brief_not_tracked", "person_brief_not_enrolled", "person_brief_lane_disabled", "person_brief_policy_refused", "person_brief_no_supported_lane", "person_brief_busy", "brief_generation_unavailable", "briefs_unavailable":
		return true
	}
	return false
}

func (b *daemonMCPOperations) executePersonBriefOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !isBriefOperation(name) {
		return nil, false, nil
	}
	in, ok := parseBriefArgs(name, args)
	if !ok {
		return operationFailure("invalid_arguments", false), true, nil
	}
	switch name {
	case "get_person_brief_enrollment":
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBriefEnrollment](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/brief-enrollment", &generated.GetPersonBriefEnrollmentRequestOptions{PathParams: &generated.GetPersonBriefEnrollmentPath{ID: in.PersonID}}, briefRefusal, http.StatusOK)
		return result, true, err
	case "set_person_brief_enrollment":
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBriefEnrollment](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/brief-enrollment", &generated.SetPersonBriefEnrollmentRequestOptions{PathParams: &generated.SetPersonBriefEnrollmentPath{ID: in.PersonID}, Body: &generated.SetPersonBriefEnrollmentBody{Enrolled: *in.Enrolled, Track: in.Track}}, briefRefusal, http.StatusOK)
		return result, true, err
	case "generate_person_brief":
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBriefRun](ctx, b.client, http.MethodPost, "/api/v1/people/{id}/brief/generate", &generated.GeneratePersonBriefRequestOptions{PathParams: &generated.GeneratePersonBriefPath{ID: in.PersonID}}, briefRefusal, http.StatusOK)
		return result, true, err
	case "list_person_brief_versions":
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBriefVersionsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/brief/versions", &generated.ListPersonBriefVersionsRequestOptions{PathParams: &generated.ListPersonBriefVersionsPath{ID: in.PersonID}, Query: &generated.ListPersonBriefVersionsQuery{Limit: in.Limit}}, briefRefusal, http.StatusOK)
		if err == nil && !result.IsError {
			output, valid := result.Output.(generated.PersonBriefVersionsResponse)
			if !valid {
				return operationFailure("invalid_operation_response", false), true, nil
			}
			result.Output = mcpserver.PersonBriefVersionsFromGenerated(output.Versions)
		}
		return result, true, err
	case "reject_person_brief":
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBrief](ctx, b.client, http.MethodPost, "/api/v1/people/{id}/brief/reject", &generated.RejectPersonBriefRequestOptions{PathParams: &generated.RejectPersonBriefPath{ID: in.PersonID}, Body: &generated.RejectPersonBriefBody{Reason: in.Reason}}, briefRefusal, http.StatusOK)
		if err == nil && !result.IsError {
			output, valid := result.Output.(generated.PersonBrief)
			if !valid {
				return operationFailure("invalid_operation_response", true), true, nil
			}
			result.Output = mcpserver.PersonBriefVersionFromGenerated(output)
		}
		return result, true, err
	}
	return nil, false, nil
}

func (b *daemonMCPOperations) personBriefDisclosure(name string, args map[string]any) (string, bool, error) {
	if name != "set_person_brief_enrollment" && name != "generate_person_brief" && name != "reject_person_brief" {
		return "", false, nil
	}
	in, ok := parseBriefArgs(name, args)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	effect := map[string]string{"set_person_brief_enrollment": "replace enrollment; when enrolling an untracked person, track=true also tracks them and track=false refuses; enrollment lets scheduled sweeps generate briefs under existing consent and provider budget", "generate_person_brief": "run one person sweep now: may extract and save profile facts and spend provider budget, even if brief generation fails", "reject_person_brief": "reject the version current at execution (no version precondition); the next eligible scheduled sweep may generate a replacement"}[name]
	data, err := json.Marshal(struct {
		Operation string    `json:"operation"`
		Arguments briefArgs `json:"arguments"`
		Effect    string    `json:"effect"`
	}{name, in, effect}, json.Deterministic(true))
	return string(data), true, err
}
