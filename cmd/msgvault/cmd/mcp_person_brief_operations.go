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
	if has("listPersonBriefVersions", http.MethodGet, "/api/v1/people/{id}/brief/versions", "limit", "before_version") &&
		has("getPersonBriefEnrollment", http.MethodGet, "/api/v1/people/{id}/brief-enrollment") {
		names = append(names, "list_person_brief_versions")
	}
	if has("setPersonBriefEnrollment", http.MethodPut, "/api/v1/people/{id}/brief-enrollment") &&
		mcpRequestPropertiesPresent(caps, "setPersonBriefEnrollment", "enrolled", "track") {
		names = append(names, "set_person_brief_enrollment")
	}
	return names
}

type briefArgs struct {
	PersonID      int64  `json:"person_id"`
	Limit         *int64 `json:"limit,omitempty"`
	Enrolled      *bool  `json:"enrolled,omitempty"`
	Track         *bool  `json:"track,omitempty"`
	BeforeVersion *int64 `json:"before_version,omitempty"`
}

func parseBriefArgs(name string, args map[string]any) (briefArgs, bool) {
	input, err := decodeMCPOperationArguments[briefArgs](args)
	return input, err == nil && (name != "set_person_brief_enrollment" || input.Enrolled != nil)
}

func isBriefOperation(name string) bool {
	switch name {
	case "list_person_brief_versions", "set_person_brief_enrollment":
		return true
	}
	return false
}

func briefRefusal(code string) bool {
	switch code {
	case "server_busy", "invalid_request", "person_brief_not_found", "person_brief_not_tracked", "briefs_unavailable":
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
	case "set_person_brief_enrollment":
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBriefEnrollment](ctx, b.client, http.MethodPut, "/api/v1/people/{id}/brief-enrollment", &generated.SetPersonBriefEnrollmentRequestOptions{PathParams: &generated.SetPersonBriefEnrollmentPath{ID: in.PersonID}, Body: &generated.SetPersonBriefEnrollmentBody{Enrolled: *in.Enrolled, Track: in.Track}}, briefRefusal, http.StatusOK)
		return result, true, err
	case "list_person_brief_versions":
		enrollment, err := readMCPOperationJSONWithClassifier[generated.PersonBriefEnrollment](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/brief-enrollment", &generated.GetPersonBriefEnrollmentRequestOptions{PathParams: &generated.GetPersonBriefEnrollmentPath{ID: in.PersonID}}, briefRefusal, http.StatusOK)
		if err != nil || enrollment.IsError {
			return enrollment, true, err
		}
		limit := int64(5)
		if in.Limit != nil {
			limit = *in.Limit
		}
		result, err := readMCPOperationJSONWithClassifier[generated.PersonBriefVersionsResponse](ctx, b.client, http.MethodGet, "/api/v1/people/{id}/brief/versions", &generated.ListPersonBriefVersionsRequestOptions{PathParams: &generated.ListPersonBriefVersionsPath{ID: in.PersonID}, Query: &generated.ListPersonBriefVersionsQuery{Limit: &limit, BeforeVersion: in.BeforeVersion}}, briefRefusal, http.StatusOK)
		if err == nil && !result.IsError {
			history := result.Output.(generated.PersonBriefVersionsResponse) //nolint:forcetypeassert // The generic reader returns this type on success.
			rows := history.Versions
			var next *int64
			if history.HasMore {
				v := rows[len(rows)-1].Version
				next = &v
			}
			output := mcpserver.PersonBriefVersionsFromGenerated(rows)
			output.Enrollment = enrollment.Output.(generated.PersonBriefEnrollment) //nolint:forcetypeassert // The generic reader returns this type on success.
			output.NextBeforeVersion = next
			result.Output = output
		}
		return result, true, err
	}
	return nil, false, nil
}

func (b *daemonMCPOperations) personBriefDisclosure(name string, args map[string]any) (string, bool, error) {
	if name != "set_person_brief_enrollment" {
		return "", false, nil
	}
	in, ok := parseBriefArgs(name, args)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	effect := "replace enrollment; when enrolling an untracked person, track=true also tracks them; omitted or false refuses; enrollment lets scheduled sweeps generate briefs under existing consent and provider budget"
	data, err := json.Marshal(struct {
		Operation string    `json:"operation"`
		Arguments briefArgs `json:"arguments"`
		Effect    string    `json:"effect"`
	}{name, in, effect}, json.Deterministic(true))
	return string(data), true, err
}
