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

type enrichmentProvider struct {
	Name                  string                        `json:"name"`
	Kind                  *string                       `json:"kind"`
	Enabled               *bool                         `json:"enabled"`
	Endpoint              *string                       `json:"endpoint"`
	PollEndpoint          *string                       `json:"poll_endpoint"`
	Mode                  *string                       `json:"mode"`
	Tier                  *string                       `json:"tier"`
	AllowedIdentifiers    *[]string                     `json:"allowed_identifiers"`
	TargetKeys            *[]string                     `json:"target_keys"`
	AllowSensitiveTargets *bool                         `json:"allow_sensitive_targets"`
	RetentionPosture      *string                       `json:"retention_posture"`
	TrainingPosture       *string                       `json:"training_posture"`
	RefreshInterval       *string                       `json:"refresh_interval"`
	RequestTimeout        *string                       `json:"request_timeout"`
	PollInterval          *string                       `json:"poll_interval"`
	MaxJobAge             *string                       `json:"max_job_age"`
	MaxRetries            *int64                        `json:"max_retries"`
	MaxRequestsPerRun     *int64                        `json:"max_requests_per_run"`
	MaxRequestsPerDay     *int64                        `json:"max_requests_per_day"`
	Credential            *generated.SecretSettingState `json:"credential"`
}
type enrichmentSettings struct {
	Providers      []enrichmentProvider `json:"person_enrichment_providers"`
	PendingRestart *bool                `json:"pending_restart"`
}
type enrichmentArguments struct {
	Name              string `json:"name"`
	ETag              string `json:"etag"`
	Enabled           *bool  `json:"enabled,omitempty"`
	MaxRetries        *int64 `json:"max_retries,omitempty"`
	MaxRequestsPerRun *int64 `json:"max_requests_per_run,omitempty"`
	MaxRequestsPerDay *int64 `json:"max_requests_per_day,omitempty"`
}

func enrichmentMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	if !hasRoute("getSettings", http.MethodGet, "/api/v1/settings") {
		return nil
	}
	names := []string{"get_enrichment_policy"}
	if hasRoute("putSettingsPersonEnrichmentProvider", http.MethodPut, "/api/v1/settings/person-enrichment/providers/{name}") && mcpEnrichmentPropertiesExact(capabilities, "kind", "enabled", "endpoint", "poll_endpoint", "mode", "tier", "num_results", "allowed_identifiers", "target_keys", "allow_sensitive_targets", "retention_posture", "training_posture", "refresh_interval", "request_timeout", "poll_interval", "max_job_age", "max_retries", "max_requests_per_run", "max_requests_per_day") {
		names = append(names, "update_enrichment_policy")
	}
	return names
}

// Full replacement is safe only for the exact request property set we reconstruct.
func mcpEnrichmentPropertiesExact(capabilities *apiprotocol.MCPCapabilities, properties ...string) bool {
	if capabilities == nil {
		return false
	}
	for _, route := range capabilities.Routes {
		if route.OperationID == "putSettingsPersonEnrichmentProvider" {
			return len(route.RequestProperties) == len(properties) && !slices.ContainsFunc(properties, func(property string) bool { return !slices.Contains(route.RequestProperties, property) })
		}
	}
	return false
}

func enrichmentReplacement(p enrichmentProvider) (*generated.PersonEnrichmentProviderUpdate, bool) {
	// A missing or empty primary endpoint may be redacted and cannot be reconstructed safely.
	if p.Credential == nil || p.Kind == nil || p.Enabled == nil || p.Endpoint == nil || *p.Endpoint == "" || p.AllowedIdentifiers == nil || p.TargetKeys == nil || p.AllowSensitiveTargets == nil || p.RetentionPosture == nil || p.TrainingPosture == nil || p.RefreshInterval == nil || p.RequestTimeout == nil || p.PollInterval == nil || p.MaxJobAge == nil || p.MaxRetries == nil || p.MaxRequestsPerRun == nil || p.MaxRequestsPerDay == nil {
		return nil, false
	}
	// Native PUT trims these hidden fields, so padded values cannot be preserved.
	for _, value := range []*string{p.Endpoint, p.PollEndpoint, p.Mode, p.Tier} {
		if value != nil && *value != strings.TrimSpace(*value) {
			return nil, false
		}
	}
	pollEndpoint := p.PollEndpoint
	if *p.Kind == string(generated.PersonEnrichmentProviderUpdateKindExa) {
		// Successful native GETs omit Exa poll_endpoint; nonnil credential metadata proves CredentialEndpoint accepted its required empty value.
		if pollEndpoint != nil && *pollEndpoint != "" {
			return nil, false
		}
		empty := ""
		pollEndpoint = &empty
	} else if pollEndpoint == nil || *pollEndpoint == "" {
		return nil, false
	}
	return &generated.PersonEnrichmentProviderUpdate{Kind: generated.PersonEnrichmentProviderUpdateKind(*p.Kind), Enabled: *p.Enabled, Endpoint: *p.Endpoint, PollEndpoint: pollEndpoint, Mode: p.Mode, Tier: p.Tier, NumResults: nil, AllowedIdentifiers: *p.AllowedIdentifiers, TargetKeys: *p.TargetKeys, AllowSensitiveTargets: *p.AllowSensitiveTargets, RetentionPosture: *p.RetentionPosture, TrainingPosture: *p.TrainingPosture, RefreshInterval: *p.RefreshInterval, RequestTimeout: *p.RequestTimeout, PollInterval: p.PollInterval, MaxJobAge: p.MaxJobAge, MaxRetries: *p.MaxRetries, MaxRequestsPerRun: *p.MaxRequestsPerRun, MaxRequestsPerDay: *p.MaxRequestsPerDay}, true
}

func enrichmentProjection(result *mcpserver.OperationResult, writes bool) *mcpserver.OperationResult {
	if result == nil || result.IsError {
		return result
	}
	settings, ok := result.Output.(enrichmentSettings)
	if !ok || result.ETag == "" || settings.PendingRestart == nil {
		return operationFailure("invalid_operation_response", writes)
	}
	output := mcpserver.EnrichmentPolicy{ETag: result.ETag, PendingRestart: *settings.PendingRestart, Providers: []mcpserver.EnrichmentProviderPolicy{}}
	for _, p := range settings.Providers {
		if p.Name == "" || p.Enabled == nil || p.MaxRetries == nil || p.MaxRequestsPerRun == nil || p.MaxRequestsPerDay == nil {
			return operationFailure("invalid_operation_response", writes)
		}
		output.Providers = append(output.Providers, mcpserver.EnrichmentProviderPolicy{Name: p.Name, Enabled: *p.Enabled, MaxRetries: *p.MaxRetries, MaxRequestsPerRun: *p.MaxRequestsPerRun, MaxRequestsPerDay: *p.MaxRequestsPerDay})
	}
	result.Output = output
	return result
}

func validEnrichmentArguments(a enrichmentArguments) bool {
	if a.Name == "" || a.ETag == "" {
		return false
	}
	if a.Enabled == nil && a.MaxRetries == nil && a.MaxRequestsPerRun == nil && a.MaxRequestsPerDay == nil {
		return false
	}
	return true
}

func (b *daemonMCPOperations) enrichmentContext(ctx context.Context, a enrichmentArguments) (*mcpserver.OperationResult, *generated.PersonEnrichmentProviderUpdate, string, error) {
	result, err := readMCPOperationJSON[enrichmentSettings](ctx, b.client, http.MethodGet, "/api/v1/settings", nil, http.StatusOK)
	if err != nil || result == nil || result.IsError {
		return result, nil, "operation_context_unavailable", err
	}
	if result.ETag != a.ETag {
		return result, nil, "settings_conflict", nil
	}
	settings, ok := result.Output.(enrichmentSettings)
	if !ok {
		return result, nil, "invalid_operation_response", nil
	}
	for _, p := range settings.Providers {
		if p.Name != a.Name {
			continue
		}
		replacement, ok := enrichmentReplacement(p)
		if !ok {
			return result, nil, "provider_policy_unreconstructable", nil
		}
		if a.Enabled != nil {
			replacement.Enabled = *a.Enabled
		}
		if a.MaxRetries != nil {
			replacement.MaxRetries = *a.MaxRetries
		}
		if a.MaxRequestsPerRun != nil {
			replacement.MaxRequestsPerRun = *a.MaxRequestsPerRun
		}
		if a.MaxRequestsPerDay != nil {
			replacement.MaxRequestsPerDay = *a.MaxRequestsPerDay
		}
		return result, replacement, "", nil
	}
	return result, nil, "provider_not_found", nil
}

func (b *daemonMCPOperations) executeEnrichmentOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case "get_enrichment_policy":
		if _, err := decodeMCPOperationArguments[struct{}](args); err != nil {
			return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		result, err := readMCPOperationJSON[enrichmentSettings](ctx, b.client, http.MethodGet, "/api/v1/settings", nil, http.StatusOK)
		return enrichmentProjection(result, false), true, err
	case "update_enrichment_policy":
		a, err := decodeMCPOperationArguments[enrichmentArguments](args)
		if err != nil || !validEnrichmentArguments(a) {
			return operationFailure("invalid_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		_, replacement, refusal, err := b.enrichmentContext(ctx, a)
		if err != nil {
			return operationFailure("operation_context_unavailable", false), true, err
		}
		if refusal != "" {
			return operationFailure(refusal, false), true, nil
		}
		result, err := readMCPOperationJSONWithClassifier[enrichmentSettings](ctx, b.client, http.MethodPut, "/api/v1/settings/person-enrichment/providers/{name}", &generated.PutSettingsPersonEnrichmentProviderRequestOptions{PathParams: &generated.PutSettingsPersonEnrichmentProviderPath{Name: a.Name}, Header: &generated.PutSettingsPersonEnrichmentProviderHeaders{IfMatch: a.ETag}, Body: replacement}, func(code string) bool { return code == "provider_kind_conflict" || code == "invalid_provider_name" }, http.StatusOK)
		return enrichmentProjection(result, true), true, err
	default:
		return nil, false, nil
	}
}

func (b *daemonMCPOperations) enrichmentOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != "update_enrichment_policy" {
		return "", false, nil
	}
	a, err := decodeMCPOperationArguments[enrichmentArguments](args)
	if err != nil || !validEnrichmentArguments(a) {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
	}
	current, _, refusal, err := b.enrichmentContext(ctx, a)
	if err != nil {
		return "", true, err
	}
	if refusal != "" {
		return "", true, &mcpserver.OperationRefusalError{Code: refusal}
	}
	current = enrichmentProjection(current, false)
	if current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	data, err := json.Marshal(struct {
		Operation string              `json:"operation"`
		Current   any                 `json:"current"`
		Changes   enrichmentArguments `json:"changes"`
		Effect    string              `json:"effect"`
	}{name, current.Output, a, "Update the configured provider policy; changing request caps changes the consent fingerprint, so after restart enrichment waits for operator consent to the new fingerprint; native settings cleanup may remove a stored credential already bound to a different origin."}, json.Deterministic(true))
	return string(data), true, err
}
