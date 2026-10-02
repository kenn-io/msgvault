package cmd

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/api"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpEnrichmentProviderPath = "/api/v1/settings/person-enrichment/providers/{name}"

func settingsMCPCapabilities(hasRoute func(string, string, string, ...string) bool) []string {
	if !hasRoute("getSettings", http.MethodGet, "/api/v1/settings") {
		return nil
	}
	names := []string{"get_operational_settings", "get_enrichment_policy"}
	if hasRoute("patchSettings", http.MethodPatch, "/api/v1/settings") {
		names = append(names, "update_operational_settings", "update_enrichment_controls")
	}
	if hasRoute("putSettingsPersonEnrichmentProvider", http.MethodPut, mcpEnrichmentProviderPath) {
		names = append(names, "update_enrichment_policy")
	}
	return names
}

type mcpSettingUpdate struct {
	Key   string           `json:"key"`
	Value api.SettingValue `json:"value"`
}

type mcpSettingsArguments struct {
	ETag    string             `json:"etag"`
	Updates []mcpSettingUpdate `json:"updates"`
}

type mcpEnrichmentArguments struct {
	ETag    string                            `json:"etag"`
	Name    string                            `json:"name"`
	Changes mcpserver.EnrichmentPolicyChanges `json:"changes"`
}

func noNullSettingsArguments(args map[string]any) error {
	data, err := json.Marshal(args)
	if err != nil {
		return err
	}
	decoder := jsontext.NewDecoder(strings.NewReader(string(data)))
	for {
		token, err := decoder.ReadToken()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if token.Kind() == 'n' {
			return errors.New("null setting argument")
		}
	}
}

func validSettingsETag(etag string) bool {
	return mcpBoundedOpaqueValue(etag, 256) && len(etag) >= 3 && strings.HasPrefix(etag, `"`) && strings.HasSuffix(etag, `"`)
}

func typedSettingUpdates(name string, args map[string]any) (mcpSettingsArguments, error) {
	input, err := decodeMCPOperationArguments[mcpSettingsArguments](args)
	if err != nil {
		return input, err
	}
	if err = noNullSettingsArguments(args); err != nil {
		return input, err
	}
	keys := mcpserver.OperationalSettingKeys()
	if name == "update_enrichment_controls" {
		keys = mcpserver.EnrichmentControlKeys()
	}
	if !validSettingsETag(input.ETag) || len(input.Updates) == 0 || len(input.Updates) > len(keys) {
		return input, errors.New("missing setting updates or ETag")
	}
	seen := map[string]bool{}
	for _, update := range input.Updates {
		if !slices.Contains(keys, update.Key) || seen[update.Key] {
			return input, errors.New("unsupported or repeated setting key")
		}
		seen[update.Key] = true
		count := 0
		for _, present := range []bool{update.Value.String != nil, update.Value.Integer != nil, update.Value.Number != nil, update.Value.Boolean != nil, update.Value.Strings != nil} {
			if present {
				count++
			}
		}
		if count != 1 {
			return input, errors.New("setting value requires exactly one typed member")
		}
	}
	return input, nil
}

func enrichmentPolicyArguments(args map[string]any) (mcpEnrichmentArguments, error) {
	input, err := decodeMCPOperationArguments[mcpEnrichmentArguments](args)
	if err != nil {
		return input, err
	}
	if err = noNullSettingsArguments(args); err != nil {
		return input, err
	}
	if !validSettingsETag(input.ETag) || !mcpBoundedOpaqueValue(input.Name, 128) || providercredentials.ValidateID(providercredentials.PersonEnrichmentID(input.Name)) != nil {
		return input, errors.New("invalid provider name or ETag")
	}
	data, err := json.Marshal(input.Changes)
	if err != nil || string(data) == "{}" {
		return input, errors.New("missing policy changes")
	}
	return input, nil
}

func settingsProjection(result *mcpserver.OperationResult, enrichment bool, writes bool) *mcpserver.OperationResult {
	if result == nil || result.IsError {
		return result
	}
	current, ok := result.Output.(generated.SettingsResponse)
	if !ok || !validSettingsETag(result.ETag) {
		return operationFailure("invalid_operation_response", writes)
	}
	for i := range current.Settings {
		setting := &current.Settings[i]
		if setting.Secret != nil {
			safe := *setting.Secret
			safe.Hint = nil
			setting.Secret = &safe
		}
		if !slices.Contains(mcpserver.OperationalSettingKeys(), setting.Key) && (!enrichment || !slices.Contains(mcpserver.EnrichmentControlKeys(), setting.Key)) {
			setting.Value = nil
		}
		setting.ReadOnly = new(setting.ReadOnly != nil && *setting.ReadOnly || !slices.Contains(mcpserver.OperationalSettingKeys(), setting.Key))
	}
	if !enrichment {
		result.Output = mcpserver.OperationalSettings{ETag: result.ETag, PendingRestart: current.PendingRestart, Groups: current.Groups, Settings: current.Settings}
		return result
	}
	output := mcpserver.EnrichmentPolicy{ETag: result.ETag, PendingRestart: current.PendingRestart, Controls: []generated.Setting{}, Providers: current.PersonEnrichmentProviders}
	if output.Providers == nil {
		output.Providers = []generated.PersonEnrichmentProviderSetting{}
	}
	for _, setting := range current.Settings {
		if slices.Contains(mcpserver.EnrichmentControlKeys(), setting.Key) {
			setting.ReadOnly = nil
			output.Controls = append(output.Controls, setting)
		}
	}
	for i := range output.Providers {
		if output.Providers[i].Credential != nil {
			safe := *output.Providers[i].Credential
			safe.Hint = nil
			output.Providers[i].Credential = &safe
		}
	}
	result.Output = output
	return result
}

func (b *daemonMCPOperations) readSettingsOperation(ctx context.Context, enrichment bool) (*mcpserver.OperationResult, error) {
	result, err := readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodGet, "/api/v1/settings", nil, http.StatusOK)
	return settingsProjection(result, enrichment, false), err
}

func (b *daemonMCPOperations) executeSettingsOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if !slices.Contains([]string{"get_operational_settings", "update_operational_settings", "get_enrichment_policy", "update_enrichment_controls", "update_enrichment_policy"}, name) {
		return nil, false, nil
	}
	switch name {
	case "get_operational_settings", "get_enrichment_policy":
		if _, err := decodeMCPOperationArguments[struct{}](args); err != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
		}
		result, err := b.readSettingsOperation(ctx, name == "get_enrichment_policy")
		return result, true, err
	case "update_operational_settings", "update_enrichment_controls":
		input, err := typedSettingUpdates(name, args)
		if err != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
		}
		updates := make([]generated.SettingUpdate, 0, len(input.Updates))
		for _, update := range input.Updates {
			data, err := json.Marshal(update.Value)
			if err != nil {
				return operationFailure("invalid_operation_arguments", false), true, err
			}
			var value generated.SettingValue
			if err := json.Unmarshal(data, &value); err != nil {
				return operationFailure("invalid_operation_arguments", false), true, err
			}
			updates = append(updates, generated.SettingUpdate{Key: update.Key, Value: &value})
		}
		result, err := readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodPatch, "/api/v1/settings", &generated.PatchSettingsRequestOptions{Header: &generated.PatchSettingsHeaders{IfMatch: input.ETag}, Body: &generated.PatchSettingsBody{Updates: updates}}, http.StatusOK)
		return settingsProjection(result, name == "update_enrichment_controls", true), true, err
	default:
		input, err := enrichmentPolicyArguments(args)
		if err != nil {
			return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
		}
		current, err := b.readSettingsOperation(ctx, true)
		if err != nil || current == nil || current.IsError {
			return current, true, err
		}
		if current.ETag != input.ETag {
			return operationFailure("settings_conflict", false), true, nil
		}
		policy, ok := current.Output.(mcpserver.EnrichmentPolicy)
		if !ok {
			return operationFailure("invalid_operation_response", false), true, nil
		}
		for _, provider := range policy.Providers {
			if provider.Name != input.Name {
				continue
			}
			if provider.Credential == nil {
				return operationFailure("enrichment_policy_unavailable", false), true, nil
			}
			body, err := mergeEnrichmentPolicy(provider, input.Changes)
			if err != nil {
				return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed refusal.
			}
			result, err := readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodPut, mcpEnrichmentProviderPath, &generated.PutSettingsPersonEnrichmentProviderRequestOptions{PathParams: &generated.PutSettingsPersonEnrichmentProviderPath{Name: input.Name}, Header: &generated.PutSettingsPersonEnrichmentProviderHeaders{IfMatch: input.ETag}, Body: &body}, http.StatusOK)
			return settingsProjection(result, true, true), true, err
		}
		return operationFailure("enrichment_provider_not_found", false), true, nil
	}
}

// Build the owner's full replacement from its current effective policy, then
// overlay only closed typed changes. No endpoint/kind/env/credential member is
// present in EnrichmentPolicyChanges.
func mergeEnrichmentPolicy(provider generated.PersonEnrichmentProviderSetting, changes mcpserver.EnrichmentPolicyChanges) (generated.PutSettingsPersonEnrichmentProviderBody, error) {
	body := generated.PutSettingsPersonEnrichmentProviderBody{Kind: generated.PersonEnrichmentProviderUpdateKind(provider.Kind), Enabled: provider.Enabled, Endpoint: provider.Endpoint, PollEndpoint: provider.PollEndpoint, Mode: provider.Mode, Tier: provider.Tier, NumResults: provider.NumResults, AllowedIdentifiers: provider.AllowedIdentifiers, TargetKeys: provider.TargetKeys, AllowSensitiveTargets: provider.AllowSensitiveTargets, RetentionPosture: provider.RetentionPosture, TrainingPosture: provider.TrainingPosture, RefreshInterval: provider.RefreshInterval, RequestTimeout: provider.RequestTimeout, PollInterval: new(provider.PollInterval), MaxJobAge: new(provider.MaxJobAge), MaxRetries: provider.MaxRetries, MaxRequestsPerRun: provider.MaxRequestsPerRun, MaxRequestsPerDay: provider.MaxRequestsPerDay}
	original, err := json.Marshal(body)
	if err != nil {
		return body, err
	}
	update, err := json.Marshal(changes)
	if err != nil {
		return body, err
	}
	var merged, overlay map[string]jsontext.Value
	if err = json.Unmarshal(original, &merged); err != nil {
		return body, err
	}
	if err = json.Unmarshal(update, &overlay); err != nil {
		return body, err
	}
	maps.Copy(merged, overlay)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return body, err
	}
	err = json.Unmarshal(encoded, &body, json.RejectUnknownMembers(true))
	return body, err
}

func (b *daemonMCPOperations) settingsOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "update_operational_settings" && name != "update_enrichment_controls" && name != "update_enrichment_policy" {
		return "", false, nil
	}
	var expected string
	if name == "update_enrichment_policy" {
		input, err := enrichmentPolicyArguments(args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
		}
		expected = input.ETag
	} else {
		input, err := typedSettingUpdates(name, args)
		if err != nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_arguments"}
		}
		expected = input.ETag
	}
	current, err := b.readSettingsOperation(ctx, name != "update_operational_settings")
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "settings_unavailable"}
	}
	if current.ETag != expected {
		return "", true, &mcpserver.OperationRefusalError{Code: "settings_conflict"}
	}
	if name == "update_enrichment_policy" {
		input, _ := enrichmentPolicyArguments(args)
		policy, ok := current.Output.(mcpserver.EnrichmentPolicy)
		if !ok {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_response"}
		}
		index := slices.IndexFunc(policy.Providers, func(provider generated.PersonEnrichmentProviderSetting) bool { return provider.Name == input.Name })
		if index < 0 {
			return "", true, &mcpserver.OperationRefusalError{Code: "enrichment_provider_not_found"}
		}
		// The owner always emits credential state for a valid destination,
		// including when no key is configured. A nil state means it could
		// not validate a disabled provider's host-owned destination; its
		// public endpoint projection may hide fields we cannot preserve.
		if policy.Providers[index].Credential == nil {
			return "", true, &mcpserver.OperationRefusalError{Code: "enrichment_policy_unavailable"}
		}
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Changes   map[string]any `json:"changes"`
		Current   any            `json:"current"`
		Effect    string         `json:"effect"`
	}{name, args, current.Output, "Config update preserves exact caller ETag and reports pending restart; enabling enrichment may schedule provider work after restart"}, json.Deterministic(true))
	return string(data), true, err
}
