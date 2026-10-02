package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrollment"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpProviderNameArgumentKey = "name"

const peopleProviderSettingsPath = "/api/v1/settings/people-inference"

type mcpProviderRoute struct {
	name, id, method, path string
	fields                 []string
}

var mcpProviderRoutes = []mcpProviderRoute{
	{"get_people_provider_settings", "getSettingsPeopleInference", http.MethodGet, peopleProviderSettingsPath, nil},
	{"create_people_provider_preset", "putSettingsPeopleInferencePreset", http.MethodPut, peopleProviderSettingsPath + "/providers/{name}", []string{"preset_id", "model", "retention_posture", "training_posture", "allowed_sources", "source_since", "allow_sensitive"}},
	{"check_people_provider", "checkSettingsPeopleInferenceProvider", http.MethodPost, peopleProviderSettingsPath + "/providers/{name}/check", nil},
	{"consent_people_provider", "consentSettingsPeopleInferenceProvider", http.MethodPost, peopleProviderSettingsPath + "/providers/{name}/consent", []string{"fingerprint", "confirmed"}},
	{"select_people_provider", "selectSettingsPeopleInference", http.MethodPost, peopleProviderSettingsPath + "/select", []string{mcpProviderNameArgumentKey}},
	{"revoke_people_provider_consent", "revokeSettingsPeopleInferenceProvider", http.MethodPost, peopleProviderSettingsPath + "/providers/{name}/revoke", nil},
	{"disable_people_inference", "disableSettingsPeopleInference", http.MethodPost, peopleProviderSettingsPath + "/disable", nil},
	{"update_people_provider_policy", "patchSettingsPeopleInferencePolicy", http.MethodPatch, peopleProviderSettingsPath + "/providers/{name}/policy", []string{"model", "retention_posture", "training_posture", "allowed_sources", "source_since", "source_until", "allow_sensitive", "reasoning_effort", "reasoning_mode", "request_timeout"}},
	{"remove_people_provider", "deleteSettingsPeopleInferenceProvider", http.MethodDelete, peopleProviderSettingsPath + "/providers/{name}", nil},
}

func providerMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	if capabilities == nil || capabilities.Delegated || !hasRoute("getSettingsPeopleInference", http.MethodGet, peopleProviderSettingsPath) {
		return nil
	}
	var names []string
	for _, route := range mcpProviderRoutes {
		if hasRoute(route.id, route.method, route.path) && mcpRequestPropertiesPresent(capabilities, route.id, route.fields...) {
			names = append(names, route.name)
		}
	}
	for _, d := range capabilities.Commands {
		if !slices.Contains(d.Flags, "json") {
			continue
		}
		switch d.Name {
		case "person provider status":
			names = append(names, "get_people_provider_status")
		case "person provider history":
			if slices.Contains(d.Flags, "person") && slices.Contains(d.Flags, "limit") {
				names = append(names, "list_people_provider_history")
			}
		}
	}
	return names
}

type mcpProviderInput struct {
	personenrollment.PolicyUpdate

	Name        string `json:"name,omitempty"`
	ETag        string `json:"etag,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	PresetID    string `json:"preset_id,omitempty"`
	PersonID    *int64 `json:"person_id,omitempty"`
	Limit       *int   `json:"limit,omitempty"`
}

func mcpProviderArguments(name string, args map[string]any) (mcpProviderInput, error) {
	allowed := []string{mcpProviderNameArgumentKey, "etag"}
	required := []string{mcpProviderNameArgumentKey, "etag"}
	switch name {
	case "get_people_provider_settings":
		allowed = nil
		required = nil
	case "get_people_provider_status":
		allowed = []string{mcpProviderNameArgumentKey}
		required = nil
	case "list_people_provider_history":
		allowed = []string{mcpProviderNameArgumentKey, "person_id", "limit"}
		required = nil
	case "disable_people_inference":
		allowed = []string{"etag"}
		required = allowed
	case "create_people_provider_preset":
		allowed = append(allowed, "preset_id", "model", "retention_posture", "training_posture", "allowed_sources", "source_since", "source_until", "allow_sensitive")
		required = append(required, "preset_id", "model", "retention_posture", "training_posture", "allowed_sources", "source_since", "allow_sensitive")
	case "update_people_provider_policy":
		allowed = append(allowed, "model", "retention_posture", "training_posture", "allowed_sources", "source_since", "source_until", "allow_sensitive", "reasoning_effort", "reasoning_mode", "request_timeout")
	case "consent_people_provider":
		allowed = append(allowed, "fingerprint")
		required = append(required, "fingerprint")
	case "check_people_provider", "select_people_provider", "revoke_people_provider_consent", "remove_people_provider":
	default:
		return mcpProviderInput{}, errors.New("unsupported provider operation")
	}
	for key, value := range args {
		if !slices.Contains(allowed, key) || value == nil {
			return mcpProviderInput{}, errors.New("invalid provider field")
		}
	}
	for _, key := range required {
		if _, ok := args[key]; !ok {
			return mcpProviderInput{}, errors.New("missing provider field")
		}
	}
	input, err := decodeMCPOperationArguments[mcpProviderInput](args)
	if err != nil {
		return input, err
	}
	if _, ok := args[mcpProviderNameArgumentKey]; ok {
		if err := peoplesweep.ValidateProviderProfileName(input.Name); err != nil {
			return input, err
		}
	}
	if _, ok := args["etag"]; ok {
		if len(input.ETag) < 3 || !strings.HasPrefix(input.ETag, `"`) || !strings.HasSuffix(input.ETag, `"`) || strings.ContainsAny(input.ETag, "\r\n") {
			return input, errors.New("invalid config ETag")
		}
	}
	if input.PersonID != nil && (*input.PersonID <= 0 || *input.PersonID > 9007199254740991) {
		return input, errors.New("invalid person ID")
	}
	if input.Limit != nil && (*input.Limit < 1 || *input.Limit > maxPersonSweepHistoryLimit) {
		return input, errors.New("invalid history limit")
	}
	if input.AllowedSources != nil && len(*input.AllowedSources) == 0 {
		return input, errors.New("empty source policy")
	}
	if name == "update_people_provider_policy" && !input.HasChanges() {
		return input, errors.New("empty policy update")
	}
	if name == "create_people_provider_preset" && (!slices.Contains([]string{"openai", "openrouter", "venice"}, input.PresetID) || input.Model == nil || *input.Model == "" || input.RetentionPosture == nil || input.TrainingPosture == nil || input.AllowedSources == nil || len(*input.AllowedSources) == 0 || input.SourceSince == nil || input.AllowSensitive == nil) {
		return input, errors.New("invalid preset policy")
	}
	if name == "consent_people_provider" && strings.TrimSpace(input.Fingerprint) == "" {
		return input, errors.New("missing fingerprint")
	}
	return input, nil
}

func (b *daemonMCPOperations) executeProviderOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !slices.ContainsFunc(mcpProviderRoutes, func(r mcpProviderRoute) bool { return r.name == name }) && name != "get_people_provider_status" && name != "list_people_provider_history" {
		return nil, false, nil
	}
	input, err := mcpProviderArguments(name, args)
	if err != nil {
		return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
	}
	if name == "get_people_provider_status" || name == "list_people_provider_history" {
		result, err := b.readProviderCLI(ctx, name, input)
		return result, true, err
	}
	var options runtime.RequestOptions
	switch name {
	case "create_people_provider_preset":
		options = &generated.PutSettingsPeopleInferencePresetRequestOptions{PathParams: &generated.PutSettingsPeopleInferencePresetPath{Name: input.Name}, Header: &generated.PutSettingsPeopleInferencePresetHeaders{IfMatch: input.ETag}, Body: &generated.PutSettingsPeopleInferencePresetBody{PresetID: generated.PeopleInferencePresetCreateRequestPresetID(input.PresetID), Model: *input.Model, RetentionPosture: *input.RetentionPosture, TrainingPosture: *input.TrainingPosture, AllowedSources: *input.AllowedSources, SourceSince: *input.SourceSince, SourceUntil: input.SourceUntil, AllowSensitive: *input.AllowSensitive}}
	case "check_people_provider":
		options = &generated.CheckSettingsPeopleInferenceProviderRequestOptions{PathParams: &generated.CheckSettingsPeopleInferenceProviderPath{Name: input.Name}, Header: &generated.CheckSettingsPeopleInferenceProviderHeaders{IfMatch: input.ETag}}
	case "consent_people_provider":
		options = &generated.ConsentSettingsPeopleInferenceProviderRequestOptions{PathParams: &generated.ConsentSettingsPeopleInferenceProviderPath{Name: input.Name}, Header: &generated.ConsentSettingsPeopleInferenceProviderHeaders{IfMatch: input.ETag}, Body: &generated.ConsentSettingsPeopleInferenceProviderBody{Fingerprint: input.Fingerprint, Confirmed: true}}
	case "select_people_provider":
		options = &generated.SelectSettingsPeopleInferenceRequestOptions{Header: &generated.SelectSettingsPeopleInferenceHeaders{IfMatch: input.ETag}, Body: &generated.SelectSettingsPeopleInferenceBody{Name: input.Name}}
	case "revoke_people_provider_consent":
		options = &generated.RevokeSettingsPeopleInferenceProviderRequestOptions{PathParams: &generated.RevokeSettingsPeopleInferenceProviderPath{Name: input.Name}, Header: &generated.RevokeSettingsPeopleInferenceProviderHeaders{IfMatch: input.ETag}}
	case "disable_people_inference":
		options = &generated.DisableSettingsPeopleInferenceRequestOptions{Header: &generated.DisableSettingsPeopleInferenceHeaders{IfMatch: input.ETag}}
	case "remove_people_provider":
		options = &generated.DeleteSettingsPeopleInferenceProviderRequestOptions{PathParams: &generated.DeleteSettingsPeopleInferenceProviderPath{Name: input.Name}, Header: &generated.DeleteSettingsPeopleInferenceProviderHeaders{IfMatch: input.ETag}}
	case "update_people_provider_policy":
		body := generated.PatchSettingsPeopleInferencePolicyBody{Model: input.Model, RetentionPosture: input.RetentionPosture, TrainingPosture: input.TrainingPosture, SourceSince: input.SourceSince, SourceUntil: input.SourceUntil, AllowSensitive: input.AllowSensitive, ReasoningEffort: input.ReasoningEffort, ReasoningMode: input.ReasoningMode, RequestTimeout: input.RequestTimeout}
		if input.AllowedSources != nil {
			body.AllowedSources = *input.AllowedSources
		}
		options = &generated.PatchSettingsPeopleInferencePolicyRequestOptions{PathParams: &generated.PatchSettingsPeopleInferencePolicyPath{Name: input.Name}, Header: &generated.PatchSettingsPeopleInferencePolicyHeaders{IfMatch: input.ETag}, Body: &body}
	}
	for _, route := range mcpProviderRoutes {
		if route.name == name {
			result, err := b.readProviderHTTP(ctx, route, options)
			return result, true, err
		}
	}
	return operationFailure("operation_not_supported", false), true, nil
}

func (b *daemonMCPOperations) readProviderHTTP(ctx context.Context, route mcpProviderRoute, options runtime.RequestOptions) (*mcpserver.OperationResult, error) {
	writes := route.method != http.MethodGet
	response, err := b.client.DoGeneratedRequestWithContext(ctx, route.method, route.path, options)
	if err != nil {
		return operationFailure("operation_failed", writes), err
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return operationFailure("operation_response_failed", writes), err
	}
	if len(data) > 1<<20 {
		return operationFailure("operation_response_limit_exceeded", writes), nil
	}
	if response.StatusCode != http.StatusOK {
		var failure generated.PeopleInferencePolicyUpdateError
		if err := json.Unmarshal(data, &failure); err != nil {
			return operationFailure("invalid_operation_response", writes), err
		}
		if !providerSafeError(failure.ErrorData) {
			return operationFailure("operation_refused", writes && response.StatusCode >= 500), nil
		}
		// Only the owning policy-update route declares these rollback facts.
		if route.name == "update_people_provider_policy" && failure.ConsentRemainsRevoked != nil && failure.RolledBack != nil && failure.OperationMayHaveCompleted != nil {
			return &mcpserver.OperationResult{IsError: true, Output: struct {
				Error                     string `json:"error"`
				ConsentRemainsRevoked     bool   `json:"consent_remains_revoked"`
				RolledBack                bool   `json:"rolled_back"`
				OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
			}{failure.ErrorData, *failure.ConsentRemainsRevoked, *failure.RolledBack, *failure.OperationMayHaveCompleted}}, nil
		}
		return operationFailure(failure.ErrorData, writes && response.StatusCode >= 500 && failure.ErrorData != "provider_negotiation_failed"), nil
	}
	if route.name == "check_people_provider" {
		var output generated.PeopleInferenceCheckResponse
		if err := json.Unmarshal(data, &output); err != nil {
			return operationFailure("invalid_operation_response", writes), err
		}
		if !output.Ok || output.Fingerprint == "" || output.Model == "" {
			return operationFailure("invalid_operation_response", writes), nil
		}
		return &mcpserver.OperationResult{Output: output}, nil
	}
	var output generated.PeopleInferenceSettingsResponse
	if err := json.Unmarshal(data, &output); err != nil {
		return operationFailure("invalid_operation_response", writes), err
	}
	if output.Profiles == nil || response.Header.Get("ETag") == "" {
		return operationFailure("invalid_operation_response", writes), nil
	}
	return &mcpserver.OperationResult{Output: projectProviderSettings(output, response.Header.Get("ETag")), ETag: response.Header.Get("ETag")}, nil
}

func providerSafeError(code string) bool {
	return slices.Contains([]string{"unauthorized", "not_found", "provider_not_found", "provider_invalid", "provider_unavailable", "provider_check_failed", "provider_negotiation_failed", "provider_policy_update_failed", "people_inference_unavailable", "credential_conflict", "credential_store_unavailable", "codex_unavailable", "settings_conflict", "settings_edit_rejected", "if_match_required", "invalid_if_match", "invalid_json", "invalid_policy_update", "operation_in_progress", "consent_required", "check_required", "fingerprint_conflict", "provider_fingerprint_mismatch", "provider_not_checked", "provider_exists", "invalid_provider", "invalid_provider_name", "provider_in_use", "credential_store_unsupported", "consent_disclosure_changed", "provider_check_store_failed", "consent_store_failed", "consent_revoke_failed", "settings_read_failed", "running_provider_invalid"}, code)
}
func providerString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
func projectProviderSettings(input generated.PeopleInferenceSettingsResponse, etag string) mcpserver.PeopleProviderSettings {
	output := mcpserver.PeopleProviderSettings{ETag: etag, StoredCredentialsSupported: input.StoredCredentialsSupported, Profiles: []mcpserver.PeopleProviderProfile{}, ConfiguredName: providerString(input.ConfiguredName), ConfiguredEnabled: input.ConfiguredEnabled, ConfiguredFingerprint: providerString(input.ConfiguredFingerprint), RunningName: providerString(input.RunningName), RunningEnabled: input.RunningEnabled, RunningFingerprint: providerString(input.RunningFingerprint), PendingRestart: input.PendingRestart}
	for _, p := range input.Profiles {
		output.Profiles = append(output.Profiles, mcpserver.PeopleProviderProfile{Name: p.Name, Selected: p.Selected, PresetID: providerString(p.PresetID), Protocol: p.Protocol, Endpoint: providerString(p.Endpoint), Model: p.Model, CredentialSource: p.CredentialSource, CredentialConfigured: p.CredentialConfigured, Checked: p.Checked, ConsentActive: p.ConsentActive, OutputMode: p.OutputMode, RetentionPosture: p.RetentionPosture, TrainingPosture: p.TrainingPosture, AllowedSources: p.AllowedSources, SourceSince: p.SourceSince, SourceUntil: providerString(p.SourceUntil), AllowSensitive: p.AllowSensitive, Fingerprint: providerString(p.Fingerprint), ReasoningEffort: providerString(p.ReasoningEffort), ReasoningMode: providerString(p.ReasoningMode), RequestTimeout: p.RequestTimeout})
	}
	return output
}

func (b *daemonMCPOperations) readProviderCLI(ctx context.Context, name string, input mcpProviderInput) (*mcpserver.OperationResult, error) {
	subcommand := multimodalStatusSubcommand
	if name == "list_people_provider_history" {
		subcommand = "history"
	}
	argv := []string{"person", "provider", subcommand}
	if input.Name != "" {
		argv = append(argv, input.Name)
	}
	argv = append(argv, "--json")
	if input.PersonID != nil {
		argv = append(argv, "--person="+strconv.FormatInt(*input.PersonID, 10))
	}
	if input.Limit != nil {
		argv = append(argv, "--limit="+strconv.Itoa(*input.Limit))
	}
	stream, err := b.client.RunMCPCLICommand(ctx, argv, "")
	if err != nil {
		return operationFailure("provider_observation_failed", false), err
	}
	if stream == nil || stream.Failed {
		return operationFailure("provider_observation_failed", false), nil
	}
	if name == "list_people_provider_history" {
		var history mcpserver.PeopleProviderHistory
		if err := json.Unmarshal([]byte(stream.Stdout), &history, json.RejectUnknownMembers(true)); err != nil {
			return operationFailure("invalid_operation_response", false), err
		}
		if history.Runs == nil || history.Attempts == nil {
			return operationFailure("invalid_operation_response", false), nil
		}
		return &mcpserver.OperationResult{Output: history}, nil
	}
	var status personProviderStatusOutput
	if err := json.Unmarshal([]byte(stream.Stdout), &status, json.RejectUnknownMembers(true)); err != nil {
		return operationFailure("invalid_operation_response", false), err
	}
	if status.Profile.Fingerprint == "" || status.Name == "" {
		return operationFailure("invalid_operation_response", false), nil
	}
	result, err := b.readProviderHTTP(ctx, mcpProviderRoutes[0], nil)
	if err != nil || result.IsError {
		return result, err
	}
	settings, ok := result.Output.(mcpserver.PeopleProviderSettings)
	if !ok {
		return operationFailure("invalid_operation_response", false), nil
	}
	var policy mcpserver.PeopleProviderProfile
	for _, p := range settings.Profiles {
		if p.Name == status.Name {
			policy = p
			break
		}
	}
	if policy.Fingerprint != status.Profile.Fingerprint {
		return operationFailure("settings_conflict", false), nil
	}
	p := status.Profile
	output := mcpserver.PeopleProviderStatus{Settings: settings, Policy: mcpserver.PeopleProviderDisclosure{PeopleProviderProfile: policy, Auth: string(p.Auth), DriverVersion: p.DriverVersion, TokenLimitParameter: p.TokenLimitParameter, ExecutionBoundary: p.ExecutionBoundary, PacketRendererPolicy: p.PacketRendererPolicy, ProgramFingerprint: p.ProgramFingerprint, DisclosedPacketFields: p.DisclosedPacketFields}, Check: status.Check, Consent: status.Consent, StaleProgramCheck: status.StaleProgramCheck, StaleProgramConsent: status.StaleProgramConsent}
	if status.CodexIsolation != nil {
		output.Isolation = &mcpserver.PeopleProviderIsolation{Available: status.CodexIsolation.Available, ExecutionBoundary: status.CodexIsolation.ExecutionBoundary}
	}
	return &mcpserver.OperationResult{Output: output, ETag: settings.ETag}, nil
}

func (b *daemonMCPOperations) providerOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if !slices.ContainsFunc(mcpProviderRoutes, func(r mcpProviderRoute) bool { return r.name == name && r.method != http.MethodGet }) {
		return "", false, nil
	}
	input, err := mcpProviderArguments(name, args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	current, err := b.readProviderHTTP(ctx, mcpProviderRoutes[0], nil)
	if err != nil {
		return "", true, err
	}
	if current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "provider_settings_unavailable"}
	}
	settings, ok := current.Output.(mcpserver.PeopleProviderSettings)
	if !ok {
		return "", true, errors.New("unexpected provider settings result")
	}
	if settings.ETag != input.ETag {
		return "", true, &mcpserver.OperationRefusalError{Code: "settings_conflict"}
	}
	var policy *mcpserver.PeopleProviderProfile
	for i := range settings.Profiles {
		if settings.Profiles[i].Name == input.Name {
			policy = &settings.Profiles[i]
			break
		}
	}
	if name == "create_people_provider_preset" {
		if policy != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "provider_exists"}
		}
		preset, err := peoplesweep.PresetProviderConfig(input.PresetID, *input.Model)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		candidate, err := input.Apply(preset)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		cfg := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: input.Name}, Providers: map[string]peoplesweep.ProviderConfig{input.Name: candidate}}
		cfg.ApplyDefaults()
		p, err := cfg.Profile()
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "provider_invalid"}
		}
		policy = &mcpserver.PeopleProviderProfile{Name: input.Name, PresetID: input.PresetID, Protocol: string(p.Protocol), Endpoint: p.Endpoint, Model: p.Model, CredentialSource: string(p.Credential), RetentionPosture: p.RetentionPosture, TrainingPosture: p.TrainingPosture, AllowedSources: *input.AllowedSources, SourceSince: p.SourceSince, SourceUntil: p.SourceUntil, AllowSensitive: p.AllowSensitive, Fingerprint: p.Fingerprint}
	} else if name != "disable_people_inference" && policy == nil {
		return "", true, &mcpserver.OperationRefusalError{Code: "provider_not_found"}
	}
	if name == "consent_people_provider" && (policy.Fingerprint != input.Fingerprint || !policy.Checked) {
		return "", true, &mcpserver.OperationRefusalError{Code: "provider_not_checked"}
	}
	effects := map[string]string{
		"create_people_provider_preset":  "Save a new fixed-endpoint policy. Credentials still require trusted host/Web enrollment; this call does not contact the provider.",
		"check_people_provider":          "Send only fixed synthetic input to this provider; this may incur cost and records a successful check.",
		"consent_people_provider":        "Permit archive inference under this exact checked provider fingerprint and its disclosed source/date/sensitive policy.",
		"select_people_provider":         "Select this already checked and consented policy for the next daemon startup; no restart is performed.",
		"revoke_people_provider_consent": "Revoke configured and applicable running-policy consent. New dispatches lose authority; already in-flight requests can finish.",
		"disable_people_inference":       "Disable saved inference and revoke configured and running authority. Already in-flight requests can finish; the daemon is not restarted.",
		"update_people_provider_policy":  "Negotiate and synthetically check the disclosed provider, which may incur cost. Preserve its credential source and revoke consent. Failed checks attempt exact-file rollback while consent remains revoked. A saved change may require daemon restart.",
		"remove_people_provider":         "Remove the eligible named policy and its stored credential, revoke consent and invalidate its check. The daemon is not restarted.",
	}[name]
	data, err := json.Marshal(struct {
		Operation     string                           `json:"operation"`
		Arguments     map[string]any                   `json:"arguments"`
		CurrentPolicy *mcpserver.PeopleProviderProfile `json:"current_policy,omitempty"`
		Settings      mcpserver.PeopleProviderSettings `json:"settings"`
		Effects       string                           `json:"effects"`
	}{name, args, policy, settings, effects}, json.Deterministic(true))
	return string(data), true, err
}
