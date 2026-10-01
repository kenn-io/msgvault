package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	mcpPersonBriefPath           = "/api/v1/people/{id}/brief"
	mcpPersonBriefEnrollmentPath = "/api/v1/people/{id}/brief-enrollment"
)

func briefMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	names := []string{}
	if hasRoute("listPersonBriefVersions", http.MethodGet, mcpPersonBriefPath+"/versions", "limit") {
		names = append(names, "list_person_brief_versions")
	}
	if hasRoute("getPersonBriefEnrollment", http.MethodGet, mcpPersonBriefEnrollmentPath) {
		names = append(names, "get_person_brief_enrollment")
		if hasRoute("setPersonBriefEnrollment", http.MethodPut, mcpPersonBriefEnrollmentPath) && mcpRequestPropertiesPresent(capabilities, "setPersonBriefEnrollment", "enrolled", "track") {
			names = append(names, "set_person_brief_enrollment")
		}
		if hasRoute("generatePersonBrief", http.MethodPost, mcpPersonBriefPath+"/generate") && hasRoute("getSettingsPeopleInference", http.MethodGet, peopleProviderSettingsPath) {
			names = append(names, "generate_person_brief")
		}
	}
	if hasRoute("getPersonBrief", http.MethodGet, mcpPersonBriefPath) && hasRoute("rejectPersonBrief", http.MethodPost, mcpPersonBriefPath+"/reject") && mcpRequestPropertiesPresent(capabilities, "rejectPersonBrief", "reason") {
		names = append(names, "reject_person_brief")
	}
	return names
}

type mcpBriefInput struct {
	PersonID int64   `json:"person_id"`
	Limit    *int64  `json:"limit,omitempty"`
	Enrolled *bool   `json:"enrolled,omitzero"`
	Track    *bool   `json:"track,omitzero"`
	Reason   *string `json:"reason,omitzero"`
}

func mcpBriefArguments(name string, args map[string]any) (mcpBriefInput, error) {
	allowed := []string{"person_id"}
	switch name {
	case "list_person_brief_versions":
		allowed = append(allowed, "limit")
	case "set_person_brief_enrollment":
		allowed = append(allowed, "enrolled", "track")
	case "reject_person_brief":
		allowed = append(allowed, "reason")
	}
	for key, value := range args {
		if !slices.Contains(allowed, key) || value == nil {
			return mcpBriefInput{}, errors.New("invalid brief argument")
		}
	}
	input, err := decodeMCPOperationArguments[mcpBriefInput](args)
	if err != nil || !mcpPositiveSafeID(input.PersonID) || input.Limit != nil && (*input.Limit < 1 || *input.Limit > 200) || name == "set_person_brief_enrollment" && input.Enrolled == nil {
		return mcpBriefInput{}, errors.New("invalid brief arguments")
	}
	return input, nil
}

func projectMCPBriefResult(result *mcpserver.OperationResult, err error) (*mcpserver.OperationResult, error) {
	if err != nil || result == nil || result.IsError {
		return result, err
	}
	brief, ok := result.Output.(generated.PersonBrief)
	if !ok {
		return operationFailure("invalid_operation_response", false), nil
	}
	projected, err := mcpserver.ProjectBriefVersion(brief)
	if err != nil {
		return operationFailure("invalid_operation_response", false), err
	}
	result.Output = projected
	return result, nil
}

func (b *daemonMCPOperations) executeBriefOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !slices.Contains([]string{"list_person_brief_versions", "get_person_brief_enrollment", "set_person_brief_enrollment", "reject_person_brief", "generate_person_brief"}, name) {
		return nil, false, nil
	}
	input, err := mcpBriefArguments(name, args)
	if err != nil {
		return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
	}
	switch name {
	case "list_person_brief_versions":
		result, err := readMCPOperationJSON[generated.PersonBriefVersionsResponse](ctx, b.client, http.MethodGet, mcpPersonBriefPath+"/versions", &generated.ListPersonBriefVersionsRequestOptions{PathParams: &generated.ListPersonBriefVersionsPath{ID: input.PersonID}, Query: &generated.ListPersonBriefVersionsQuery{Limit: input.Limit}}, http.StatusOK)
		if err != nil || result == nil || result.IsError {
			return result, true, err
		}
		versions, ok := result.Output.(generated.PersonBriefVersionsResponse)
		if !ok || versions.Versions == nil {
			return operationFailure("invalid_operation_response", false), true, nil
		}
		output := mcpserver.BriefVersions{Versions: make([]mcpserver.BriefVersion, 0, len(versions.Versions))}
		for _, brief := range versions.Versions {
			projected, err := mcpserver.ProjectBriefVersion(brief)
			if err != nil {
				return operationFailure("invalid_operation_response", false), true, err
			}
			output.Versions = append(output.Versions, projected)
		}
		result.Output = output
		return result, true, nil
	case "get_person_brief_enrollment":
		result, err := readMCPOperationJSON[generated.PersonBriefEnrollment](ctx, b.client, http.MethodGet, mcpPersonBriefEnrollmentPath, &generated.GetPersonBriefEnrollmentRequestOptions{PathParams: &generated.GetPersonBriefEnrollmentPath{ID: input.PersonID}}, http.StatusOK)
		return result, true, err
	case "set_person_brief_enrollment":
		result, err := readMCPOperationJSON[generated.PersonBriefEnrollment](ctx, b.client, http.MethodPut, mcpPersonBriefEnrollmentPath, &generated.SetPersonBriefEnrollmentRequestOptions{PathParams: &generated.SetPersonBriefEnrollmentPath{ID: input.PersonID}, Body: &generated.PutPersonBriefEnrollmentRequest{Enrolled: *input.Enrolled, Track: input.Track}}, http.StatusOK)
		return result, true, err
	case "reject_person_brief":
		result, err := readMCPOperationJSON[generated.PersonBrief](ctx, b.client, http.MethodPost, mcpPersonBriefPath+"/reject", &generated.RejectPersonBriefRequestOptions{PathParams: &generated.RejectPersonBriefPath{ID: input.PersonID}, Body: &generated.RejectPersonBriefRequest{Reason: input.Reason}}, http.StatusOK)
		result, err = projectMCPBriefResult(result, err)
		return result, true, err
	case "generate_person_brief":
		result, err := readMCPOperationJSON[generated.PersonBriefRun](ctx, b.client, http.MethodPost, mcpPersonBriefPath+"/generate", &generated.GeneratePersonBriefRequestOptions{PathParams: &generated.GeneratePersonBriefPath{ID: input.PersonID}}, http.StatusOK)
		return result, true, err
	default:
		return nil, false, nil
	}
}

func (b *daemonMCPOperations) briefOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if !slices.Contains([]string{"set_person_brief_enrollment", "reject_person_brief", "generate_person_brief"}, name) {
		return "", false, nil
	}
	input, err := mcpBriefArguments(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
	}
	var current *mcpserver.OperationResult
	effect := "Replace enrollment; optional track uses the owning atomic tracking transaction. No caller revision guard."
	if name == "reject_person_brief" {
		current, err = readMCPOperationJSON[generated.PersonBrief](ctx, b.client, http.MethodGet, mcpPersonBriefPath, &generated.GetPersonBriefRequestOptions{PathParams: &generated.GetPersonBriefPath{ID: input.PersonID}}, http.StatusOK)
		current, err = projectMCPBriefResult(current, err)
		effect = "Reject the current brief at execution. The owning route is revision-free; the version shown is a disclosure, not a compare-and-swap guard. Owner reason is untrusted prose."
	} else {
		current, _, err = b.executeBriefOperation(ctx, "get_person_brief_enrollment", map[string]any{"person_id": input.PersonID})
	}
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "briefs_unavailable"}
	}
	var provider *mcpserver.PeopleProviderProfile
	if name == "generate_person_brief" {
		provider, err = b.runningBriefProvider(ctx)
		if err != nil {
			return "", true, err
		}
		effect = "Generate for one enrolled tracked person using the running daemon worker and its existing consent and budgets. May contact the disclosed provider and incur cost. Only permitted person-authored conversation text is eligible; email, meetings, documents and owner replies are excluded. Version0 or brief_failure_class means no stored brief."
	}
	data, err := json.Marshal(struct {
		Operation       string                           `json:"operation"`
		Request         mcpBriefInput                    `json:"request"`
		Current         any                              `json:"current"`
		RunningProvider *mcpserver.PeopleProviderProfile `json:"running_provider,omitzero"`
		Effect          string                           `json:"effect"`
	}{name, input, current.Output, provider, effect}, json.Deterministic(true))
	return string(data), true, err
}

func (b *daemonMCPOperations) runningBriefProvider(ctx context.Context) (*mcpserver.PeopleProviderProfile, error) {
	result, err := b.readProviderHTTP(ctx, mcpProviderRoutes[0], nil)
	if err != nil {
		return nil, err
	}
	if result == nil || result.IsError {
		return nil, &mcpserver.OperationRefusalError{Code: "people_inference_unavailable"}
	}
	settings, ok := result.Output.(mcpserver.PeopleProviderSettings)
	if !ok || !settings.RunningEnabled || settings.RunningFingerprint == "" {
		return nil, &mcpserver.OperationRefusalError{Code: "people_inference_unavailable"}
	}
	for _, profile := range settings.Profiles {
		if profile.Name == settings.RunningName && profile.Fingerprint == settings.RunningFingerprint {
			if !profile.Checked || !profile.ConsentActive {
				return nil, &mcpserver.OperationRefusalError{Code: "consent_required"}
			}
			return &profile, nil
		}
	}
	return nil, &mcpserver.OperationRefusalError{Code: "running_provider_invalid"}
}
