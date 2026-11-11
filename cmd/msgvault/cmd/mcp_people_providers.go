package cmd

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func peopleProviderMCPCapabilities(c *apiprotocol.MCPCapabilities) []string {
	if c == nil {
		return nil
	}
	var names []string
	for _, want := range []struct {
		name, id, method, path string
		property               bool
	}{
		{"get_people_provider_settings", "getSettingsPeopleInference", http.MethodGet, "/api/v1/settings/people-inference", false},
		{"select_people_provider", "selectSettingsPeopleInference", http.MethodPost, "/api/v1/settings/people-inference/select", true},
		{"disable_people_inference", "disableSettingsPeopleInference", http.MethodPost, "/api/v1/settings/people-inference/disable", false},
	} {
		for _, route := range c.Routes {
			if route.OperationID == want.id && route.Method == want.method && route.Path == want.path && (!want.property || slices.Contains(route.RequestProperties, "name")) {
				names = append(names, want.name)
				break
			}
		}
	}
	// Mutations need a current safe view for disclosure and revision checks.
	if !slices.Contains(names, "get_people_provider_settings") {
		return nil
	}
	return names
}

type peopleProviderArgs struct {
	ETag string `json:"etag"`
	Name string `json:"name,omitempty"`
}

func peopleProviderDefinite(code string) bool {
	return slices.Contains([]string{"check_required", "consent_required", "provider_not_found", "invalid_provider", "people_inference_unavailable", "if_match_required", "settings_conflict"}, code)
}
func (b *daemonMCPOperations) executePeopleProviderOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "get_people_provider_settings":
		if _, e := decodeMCPOperationArguments[struct{}](args); e != nil {
			return operationFailure("invalid_arguments", false), true, nil
		}
		result, err = readMCPOperationJSON[generated.PeopleInferenceSettingsResponse](ctx, b.client, http.MethodGet, "/api/v1/settings/people-inference", nil, http.StatusOK)
	case "select_people_provider", "disable_people_inference":
		input, e := decodeMCPOperationArguments[peopleProviderArgs](args)
		if e != nil || input.ETag == "" || (name == "select_people_provider" && strings.TrimSpace(input.Name) == "") || (name == "disable_people_inference" && input.Name != "") {
			return operationFailure("invalid_arguments", false), true, nil
		}
		if name == "select_people_provider" {
			result, err = readMCPOperationJSONWithClassifier[generated.PeopleInferenceSettingsResponse](ctx, b.client, http.MethodPost, "/api/v1/settings/people-inference/select", &generated.SelectSettingsPeopleInferenceRequestOptions{Header: &generated.SelectSettingsPeopleInferenceHeaders{IfMatch: input.ETag}, Body: &generated.PeopleInferenceSelectionRequest{Name: input.Name}}, peopleProviderDefinite, http.StatusOK)
		} else {
			result, err = readMCPOperationJSONWithClassifier[generated.PeopleInferenceSettingsResponse](ctx, b.client, http.MethodPost, "/api/v1/settings/people-inference/disable", &generated.DisableSettingsPeopleInferenceRequestOptions{Header: &generated.DisableSettingsPeopleInferenceHeaders{IfMatch: input.ETag}}, peopleProviderDefinite, http.StatusOK)
		}
	default:
		return nil, false, nil
	}
	return projectPeopleProviderSettings(result, name != "get_people_provider_settings"), true, err
}
func projectPeopleProviderSettings(result *mcpserver.OperationResult, writes bool) *mcpserver.OperationResult {
	if result == nil || result.IsError {
		return result
	}
	settings, ok := result.Output.(generated.PeopleInferenceSettingsResponse)
	if !ok || result.ETag == "" {
		return operationFailure("invalid_operation_response", writes)
	}
	view := mcpserver.PeopleProviderSettings{ETag: result.ETag, ConfiguredEnabled: settings.ConfiguredEnabled, ConfiguredName: settings.ConfiguredName, RunningEnabled: settings.RunningEnabled, RunningName: settings.RunningName, PendingRestart: settings.PendingRestart, Profiles: []mcpserver.PeopleProviderProfile{}}
	for _, p := range settings.Profiles {
		view.Profiles = append(view.Profiles, mcpserver.PeopleProviderProfile{Name: p.Name, Selected: p.Selected, Protocol: p.Protocol, Model: p.Model, Checked: p.Checked, ConsentActive: p.ConsentActive, AllowSensitive: p.AllowSensitive, AllowedSources: p.AllowedSources, SourceSince: p.SourceSince, SourceUntil: p.SourceUntil, RetentionPosture: p.RetentionPosture, TrainingPosture: p.TrainingPosture})
	}
	result.Output = view
	return result
}
func (b *daemonMCPOperations) peopleProviderDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "select_people_provider" && name != "disable_people_inference" {
		return "", false, nil
	}
	input, err := decodeMCPOperationArguments[peopleProviderArgs](args)
	if err != nil || input.ETag == "" || (name == "select_people_provider" && strings.TrimSpace(input.Name) == "") || (name == "disable_people_inference" && input.Name != "") {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	current, _, err := b.executePeopleProviderOperation(ctx, "get_people_provider_settings", nil)
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if current.ETag != input.ETag {
		return "", true, &mcpserver.OperationRefusalError{Code: "settings_conflict"}
	}
	effect := "Select the configured provider; native checks and consent remain required. Restart may be required."
	if name == "disable_people_inference" {
		effect = "Disable inference and revoke active consent. Operator re-consent is required to enable later. Restart may be required."
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current.Output, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
