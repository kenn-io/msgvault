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

var operationalSettingKeys = []string{
	"analytics.auto_build_cache", "analytics.min_rebuild_interval", "analytics.builder_memory_limit", "analytics.builder_threads", "analytics.builder_temp_limit",
	"sync.rate_limit_qps", "activity.timezone", "activity.max_direct_counterparts", "activity.batch_size", "activity.schedule", "backup.zstd_level",
	"vector.search.rrf_k", "vector.search.k_per_signal", "vector.search.subject_boost", "vector.search.max_page_size_hybrid", "vector.search.sqlite_accelerator", "vector.search.ann_nprobe", "vector.search.ann_oversample", "vector.search.ann_threads",
}

type operationalSettingArguments struct {
	ETag    string `json:"etag"`
	Updates []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	} `json:"updates"`
}

func operationalSettingsCapabilities(hasRoute mcpRouteCheck, caps *apiprotocol.MCPCapabilities) []string {
	if !hasRoute("getSettings", http.MethodGet, "/api/v1/settings") {
		return nil
	}
	names := []string{"get_operational_settings"}
	if hasRoute("patchSettings", http.MethodPatch, "/api/v1/settings") && mcpRequestPropertiesPresent(caps, "patchSettings", "updates") {
		names = append(names, "update_operational_settings")
	}
	return names
}

func operationalUpdates(args map[string]any) (operationalSettingArguments, []generated.SettingUpdate, error) {
	input, err := decodeMCPOperationArguments[operationalSettingArguments](args)
	if err != nil || input.ETag == "" || len(input.Updates) == 0 {
		return input, nil, &mcpserver.OperationRefusalError{Code: mcpInvalidArguments}
	}
	updates := make([]generated.SettingUpdate, 0, len(input.Updates))
	for _, update := range input.Updates {
		if !slices.Contains(operationalSettingKeys, update.Key) {
			return input, nil, &mcpserver.OperationRefusalError{Code: mcpInvalidArguments}
		}
		value, err := mcpSettingValue(update.Value)
		if err != nil {
			return input, nil, &mcpserver.OperationRefusalError{Code: mcpInvalidArguments}
		}
		updates = append(updates, generated.SettingUpdate{Key: update.Key, Value: &value})
	}
	return input, updates, nil
}

func (b *daemonMCPOperations) executeOperationalSettings(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "get_operational_settings":
		if _, err := decodeMCPOperationArguments[struct{}](args); err != nil {
			return operationFailure(mcpInvalidArguments, false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		result, err = readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodGet, "/api/v1/settings", nil, http.StatusOK)
	case "update_operational_settings":
		input, updates, decodeErr := operationalUpdates(args)
		if decodeErr != nil {
			return operationFailure(mcpInvalidArguments, false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		result, err = readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodPatch, "/api/v1/settings", &generated.PatchSettingsRequestOptions{Header: &generated.PatchSettingsHeaders{IfMatch: input.ETag}, Body: &generated.PatchSettingsBody{Updates: updates}}, http.StatusOK)
	default:
		return nil, false, nil
	}
	if result == nil || result.IsError {
		return result, true, err
	}
	settings, ok := result.Output.(generated.SettingsResponse)
	if !ok || result.ETag == "" {
		return operationFailure("invalid_operation_response", name == "update_operational_settings"), true, err
	}
	projected := mcpserver.OperationalSettings{ETag: result.ETag, PendingRestart: settings.PendingRestart, Settings: []generated.Setting{}}
	for _, setting := range settings.Settings {
		if slices.Contains(operationalSettingKeys, setting.Key) {
			projected.Settings = append(projected.Settings, setting)
		}
	}
	result.Output = projected
	return result, true, err
}

func (b *daemonMCPOperations) operationalSettingsDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "update_operational_settings" {
		return "", false, nil
	}
	input, _, err := operationalUpdates(args)
	if err != nil {
		return "", true, err
	}
	current, _, err := b.executeOperationalSettings(ctx, "get_operational_settings", nil)
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		if current != nil {
			if failure, ok := current.Output.(operationFailureOutput); ok && failure.Error == "settings_read_failed" {
				return "", true, &mcpserver.OperationRefusalError{Code: failure.Error}
			}
		}
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if current.ETag != input.ETag {
		return "", true, &mcpserver.OperationRefusalError{Code: "settings_conflict"}
	}
	settings, ok := current.Output.(mcpserver.OperationalSettings)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	selected := make([]generated.Setting, 0, len(input.Updates))
	for _, update := range input.Updates {
		index := slices.IndexFunc(settings.Settings, func(s generated.Setting) bool { return s.Key == update.Key })
		if index < 0 {
			return "", true, &mcpserver.OperationRefusalError{Code: "settings_edit_rejected"}
		}
		selected = append(selected, settings.Settings[index])
	}
	data, err := json.Marshal(struct {
		Operation string `json:"operation"`
		Current   any    `json:"current"`
		Changes   any    `json:"changes"`
		Effect    string `json:"effect"`
	}{name, selected, input.Updates, "Persist the requested settings; daemon restart may be required. The daemon may also delete stored provider credentials whose configured destination no longer matches."}, json.Deterministic(true))
	return string(data), true, err
}
