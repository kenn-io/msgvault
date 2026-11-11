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

func peopleProviderMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	const path = "/api/v1/settings/people-inference"
	if !hasRoute("getSettingsPeopleInference", http.MethodGet, path) {
		return nil
	}
	names := []string{"get_people_provider_settings"}
	if hasRoute("selectSettingsPeopleInference", http.MethodPost, path+"/select") && mcpRequestPropertiesPresent(capabilities, "selectSettingsPeopleInference", "name") {
		names = append(names, "select_people_provider")
	}
	if hasRoute("disableSettingsPeopleInference", http.MethodPost, path+"/disable") {
		names = append(names, "disable_people_inference")
	}
	return names
}

type peopleProviderArgs struct {
	ETag string `json:"etag"`
	Name string `json:"name,omitempty"`
}

func decodePeopleProviderWrite(name string, args map[string]any) (peopleProviderArgs, bool) {
	input, err := decodeMCPOperationArguments[peopleProviderArgs](args)
	valid := err == nil && input.ETag != "" && ((name == "select_people_provider" && strings.TrimSpace(input.Name) != "") || (name == "disable_people_inference" && input.Name == ""))
	return input, valid
}

func peopleProviderDefinite(code string) bool {
	return slices.Contains([]string{"check_required", "consent_required", "provider_not_found", "invalid_provider", "people_inference_unavailable", "server_busy"}, code)
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
		input, valid := decodePeopleProviderWrite(name, args)
		if !valid {
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
	// Native selection can publish before a conflict and disable revokes consent before editing.
	if name != "get_people_provider_settings" && result != nil && result.IsError {
		if failure, ok := result.Output.(operationFailureOutput); ok && (failure.Error == "settings_conflict" || failure.Error == "settings_edit_rejected") {
			failure.OperationMayHaveCompleted = true
			result.Output = failure
		}
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
	input, valid := decodePeopleProviderWrite(name, args)
	if !valid {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	current, _, err := b.executePeopleProviderOperation(ctx, "get_people_provider_settings", nil)
	if err != nil || current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if current.ETag != input.ETag {
		return "", true, &mcpserver.OperationRefusalError{Code: "settings_conflict"}
	}
	effect := "Enable people inference with the selected provider; native checks and consent remain required. Restart may be required for the new policy to run."
	if name == "disable_people_inference" {
		effect = "Disable inference and revoke active consent for both configured and running providers shown in the current policy. Operator re-consent is required to enable later. Restart may be required."
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current.Output, args, effect}, json.Deterministic(true))
	return string(data), true, err
}
