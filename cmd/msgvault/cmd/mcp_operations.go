package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpAccountArgumentKey = "account"

type daemonMCPOperations struct {
	client           *daemonclient.Client
	supported        []string
	commands         []apiprotocol.MCPCommandDescriptor
	documentManifest string
	cardDAVNamed     bool
}

func newDaemonMCPOperations(client *daemonclient.Client, capabilities *apiprotocol.MCPCapabilities) *daemonMCPOperations {
	backend := &daemonMCPOperations{client: client}
	if capabilities == nil {
		return backend
	}
	backend.commands = slices.Clone(capabilities.Commands)
	for i := range backend.commands {
		backend.commands[i].Flags = slices.Clone(backend.commands[i].Flags)
	}
	backend.supported = draftMCPCapabilities(capabilities)
	if capabilities.Delegated {
		return backend
	}
	backend.supported = append(backend.supported, embeddingMCPCapabilities(backend.commands)...)

	hasRoute := func(id, method, path string, query ...string) bool {
		for _, route := range capabilities.Routes {
			if route.OperationID != id || route.Method != method || route.Path != path {
				continue
			}
			for _, name := range query {
				if !slices.Contains(route.QueryParameters, name) {
					return false
				}
			}
			return true
		}
		return false
	}
	if hasRoute("getSchedulerStatus", http.MethodGet, "/api/v1/scheduler/status") {
		backend.supported = append(backend.supported, "get_source_scheduler_status")
	}
	if hasRoute("triggerSync", http.MethodPost, "/api/v1/sync/{account}", "source_type") && hasRoute("listSourceStatus", http.MethodGet, "/api/v1/sources/status", "source_type") {
		backend.supported = append(backend.supported, "sync_source")
	}
	backend.supported = append(backend.supported, sourceMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, importMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, briefMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, recordMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, relationshipMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, factMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, sweepMCPCapabilities(hasRoute, capabilities)...)
	for _, name := range settingsMCPCapabilities(hasRoute) {
		if (name == "update_operational_settings" || name == "update_enrichment_controls") && !mcpRequestPropertiesPresent(capabilities, "patchSettings", "updates") {
			continue
		}
		if name == "update_enrichment_policy" && !mcpRequestPropertiesPresent(capabilities, "putSettingsPersonEnrichmentProvider", "kind", "endpoint", "poll_endpoint", "enabled", "mode", "tier", "num_results", "allowed_identifiers", "target_keys", "allow_sensitive_targets", "retention_posture", "training_posture", "refresh_interval", "request_timeout", "poll_interval", "max_job_age", "max_retries", "max_requests_per_run", "max_requests_per_day") {
			continue
		}
		backend.supported = append(backend.supported, name)
	}
	backend.supported = append(backend.supported, providerMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, documentMCPCapabilities(hasRoute, capabilities)...)
	backend.supported = append(backend.supported, historyMCPCapabilities(hasRoute)...)
	if hasRoute("getLaneReadiness", http.MethodGet, "/api/v1/lanes/readiness") {
		backend.supported = append(backend.supported, "get_lane_readiness")
	}
	backend.supported = append(backend.supported, visualMCPCapabilities(hasRoute, capabilities)...)
	backend.cardDAVNamed = hasRoute("listCardDAVConnections", http.MethodGet, mcpCardDAVPrefix+"/connections") && hasRoute("getCardDAVStatus", http.MethodGet, mcpCardDAVPrefix+"/status", "connection") && hasRoute("listCardDAVBooks", http.MethodGet, mcpCardDAVPrefix+"/books", "connection") && hasRoute("listCardDAVRuns", http.MethodGet, mcpCardDAVPrefix+"/runs", "connection") && mcpRequestPropertiesPresent(capabilities, "syncCardDAV", "connection")
	for _, route := range mcpCardDAVRoutes {
		if route.contextID != "" && !hasRoute(route.contextID, http.MethodGet, route.contextPath) {
			continue
		}
		if hasRoute(route.id, route.method, route.path, route.query...) && (len(route.properties) == 0 || mcpRequestPropertiesPresent(capabilities, route.id, route.properties...)) {
			backend.supported = append(backend.supported, route.name)
		}
	}
	return backend
}

func (b *daemonMCPOperations) capabilities() []string { return slices.Clone(b.supported) }

func (b *daemonMCPOperations) ExecuteOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, error) {
	if !slices.Contains(b.supported, name) {
		return operationFailure("operation_not_supported", false), nil
	}
	switch name {
	case "get_source_scheduler_status":
		return readMCPOperationJSON[generated.SchedulerStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/scheduler/status", nil, http.StatusOK)
	case "sync_source":
		account, sourceType, err := mcpSyncArguments(args)
		if err != nil {
			return operationFailure("invalid_source", false), nil //nolint:nilerr // Invalid arguments become a fixed structured tool refusal.
		}
		return readMCPOperationJSON[generated.StatusMessageResponse](ctx, b.client, http.MethodPost, "/api/v1/sync/{account}", &generated.TriggerSyncRequestOptions{PathParams: &generated.TriggerSyncPath{Account: account}, Query: &generated.TriggerSyncQuery{SourceType: &sourceType}}, http.StatusAccepted)
	default:
		if result, handled, err := b.executeFactOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeRelationshipOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeRecordOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeSweepOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeBriefOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeImportOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeSettingsOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeLaneReadinessOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeEmbeddingOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeVisualOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeHistoryOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeCardDAVOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeDocumentOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeDraftOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeProviderOperation(ctx, name, args); handled {
			return result, err
		}
		if result, handled, err := b.executeSourceOperation(ctx, name, args); handled {
			return result, err
		}
		return operationFailure("operation_not_supported", false), nil
	}
}

func (b *daemonMCPOperations) OperationDisclosure(ctx context.Context, name string, args map[string]any) (string, error) {
	if slices.Contains(b.supported, name) {
		if disclosure, handled, err := b.factOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.relationshipOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.recordOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.sweepOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.briefOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.importOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.settingsOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.visualOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.cardDAVOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.documentOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.draftOperationDisclosure(name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.providerOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
		if disclosure, handled, err := b.sourceOperationDisclosure(ctx, name, args); handled {
			return disclosure, err
		}
	}
	if name != "sync_source" || !slices.Contains(b.supported, name) {
		return "", &mcpserver.OperationRefusalError{Code: "operation_not_supported"}
	}
	account, sourceType, err := mcpSyncArguments(args)
	if err != nil {
		return "", &mcpserver.OperationRefusalError{Code: "invalid_source"}
	}
	result, err := readMCPOperationJSON[generated.SourceStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/sources/status", &generated.ListSourceStatusRequestOptions{Query: &generated.ListSourceStatusQuery{SourceType: &sourceType}}, http.StatusOK)
	if err != nil {
		return "", err
	}
	if result.IsError {
		return "", &mcpserver.OperationRefusalError{Code: "source_status_unavailable"}
	}
	status, ok := result.Output.(generated.SourceStatusResponse)
	if !ok {
		return "", errors.New("unexpected source status result")
	}
	for _, source := range status.Sources {
		if source.Identifier != account || source.SourceType != sourceType {
			continue
		}
		if !source.CanSync {
			return "", &mcpserver.OperationRefusalError{Code: "source_not_schedulable"}
		}
		disclosure := struct {
			Operation  string `json:"operation"`
			SourceID   int64  `json:"source_id"`
			SourceType string `json:"source_type"`
			Identifier string `json:"identifier"`
			Scope      string `json:"scope"`
			Acceptance string `json:"acceptance"`
		}{"sync_source", source.ID, sourceType, account, "configured scheduler scope; generic source jobs synchronize the shared configured sources of this type", "acceptance schedules work; it is not completion"}
		data, err := json.Marshal(disclosure, json.Deterministic(true))
		return string(data), err
	}
	return "", &mcpserver.OperationRefusalError{Code: "not_found"}
}

func mcpSyncArguments(args map[string]any) (string, string, error) {
	account, ok := args[mcpAccountArgumentKey].(string)
	if !ok || strings.TrimSpace(account) == "" {
		return "", "", errors.New("missing source identifier")
	}
	sourceType, ok := args["source_type"].(string)
	if !ok || strings.TrimSpace(sourceType) == "" {
		return "", "", errors.New("missing source type")
	}
	return account, sourceType, nil
}

func operationFailure(code string, uncertain bool) *mcpserver.OperationResult {
	return &mcpserver.OperationResult{IsError: true, Output: struct {
		Error                     string `json:"error"`
		OperationMayHaveCompleted bool   `json:"operation_may_have_completed"`
	}{code, uncertain}}
}

// The fixed route/request chosen by an adapter uses existing authentication and
// cancellation without the daemon client's busy retry loop. Mutations with an
// unclassified/interrupted response retain uncertain completion explicitly.
func readMCPOperationJSON[T any](ctx context.Context, client *daemonclient.Client, method, path string, options runtime.RequestOptions, success ...int) (*mcpserver.OperationResult, error) {
	writes := method != http.MethodGet
	response, err := client.DoGeneratedRequestWithContext(ctx, method, path, options)
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
	if !slices.Contains(success, response.StatusCode) {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &failure)
		code := failure.Error
		switch code {
		case "person_facts_unavailable", "person_fact_conflict", "tracking_unavailable", "invalid_target", "invalid_fact_target", "invalid_target_kind", "invalid_target_key", "invalid_limit", "invalid_offset", "person_merge_not_found", "person_merge_revision_conflict", "person_merge_idempotency_conflict", "person_split_idempotency_conflict", "person_merge_already_split", "person_split_reviewed_candidates", "person_split_merge_not_owned", "person_carddav_published", "person_merge_candidate_not_found", "person_merge_candidate_state_changed", "person_split_invalid_participants", "person_merge_invalid", "person_merge_snapshot_corrupt":
			return operationFailure(code, false), nil
		case "person_fact_failed", "person_merge_failed":
			return operationFailure(code, writes), nil
		case "organizations_unavailable", "organization_not_found", "organization_revision_conflict", "organization_has_employments", "organization_merge_conflict", "organization_profile_too_large", "invalid_organization", "invalid_source", "invalid_attribute_definition", "invalid_attribute_value", "invalid_organization_id", "invalid_employment_id", "employments_unavailable", "employment_not_found", "employment_revision_conflict", "employment_primary_conflict", "employment_duplicate_active", "invalid_employment", "invalid_partial_date", "invalid_person_id", "person_relationships_unavailable", "relationship_type_not_found", "person_relationship_not_found", "relationship_review_not_found", "relationship_revision_conflict", "relationship_type_slug_conflict", "relationship_type_related_type_conflict", "relationship_type_not_deletable", "relationship_type_in_use", "person_relationship_duplicate", "relationship_review_not_pending", "invalid_relationship", "invalid_relationship_type", "person_network_unavailable":
			return operationFailure(code, false), nil
		case "organization_failed", "organization_profile_media_failed", "employment_failed", "person_relationship_failed", "person_network_failed":
			return operationFailure(code, writes), nil
		case "attribute_value_conflict":
			var conflict generated.PersonAttributeConflictResponse
			if err := json.Unmarshal(data, &conflict); err != nil {
				return operationFailure("invalid_operation_response", false), err
			}
			return &mcpserver.OperationResult{IsError: true, Output: mcpserver.AttributeConflict{Error: code, CurrentValueID: conflict.CurrentValueID, CurrentValue: conflict.CurrentValue}}, nil
		case "profile_values_unavailable", "person_enrichment_dispatch_in_progress", "service_alias_conflict", "person_category_duplicate", "profile_patch_too_large", "invalid_profile_value", "profile_value_not_found", "profile_media_not_found", "profile_media_content_unavailable", "attribute_uniqueness_unsupported", "attribute_invalid", "attribute_value_invalid", "attribute_definition_not_found", "attribute_value_not_found", "attribute_definition_slug_conflict", "attribute_definition_universal_id_conflict", "attribute_definition_revision_conflict", "attribute_definition_not_deletable", "attribute_definition_has_values", "attribute_definition_not_writable", "attribute_definition_inactive", "invalid_attribute_slug", "invalid_expected_value_id", "invalid_ordinal", "invalid_attribute_definition_id", "invalid_if_match", "invalid_object_type":
			return operationFailure(code, false), nil
		case "person_profile_failed", "person_profile_media_failed", "attribute_failed":
			return operationFailure(code, writes), nil
		case "brief_generation_unavailable", "briefs_unavailable", "person_brief_not_found", "person_brief_not_tracked", "person_brief_not_enrolled", "person_brief_lane_disabled", "person_brief_policy_refused", "person_brief_no_supported_lane", "person_brief_busy":
			return operationFailure(code, false), nil
		case "person_brief_failed":
			return operationFailure(code, writes), nil
		case "ambiguous_account", "account_not_syncable", "sync_already_active", "service_unavailable":
			return operationFailure(code, false), nil
		case "unauthorized", "not_found", "scheduler_unavailable", "source_not_schedulable", "store_unavailable", "missing_account", "sync_error", "operation_in_progress", "settings_conflict", "settings_edit_rejected", "if_match_required", "person_revision_conflict", "person_profile_not_found", "persons_unavailable", "cache_build_not_found", "cache_build_unavailable", "validation_failed", "bad_request", "invalid_participant_id", "participant_not_found", "analytical_cache_unavailable", "query_resource_exhausted", "engine_unavailable", "document_status_scope_unavailable", "document_status_unavailable", "carddav_unavailable", "carddav_connection_unavailable", "google_authorization_required", "carddav_conflict_stale", "carddav_conflict_pending", "carddav_publication_pending", "carddav_inference_review_required", "carddav_review_stale", "conflict", "carddav_retry_after":
			return operationFailure(code, false), nil
		case "invalid_cursor", "operation_history_conflict", "invalid_operation_run_id", "operation_run_not_found", "operation_history_unavailable", "operation_history_failed":
			return operationFailure(code, false), nil
		case "lane_readiness_unavailable", "visual_policy_changed", "visual_policy_unavailable", "visual_search_not_ready", "visual_consent_required", "visual_operation_active", "visual_generation_changed", "invalid_visual_owner", "invalid_visual_generation", "invalid_visual_guard", "visual_coverage_busy", "rate_limit_exceeded", "cross_origin_loopback", "document_vector_status_unavailable":
			return operationFailure(code, false), nil
		case "carddav_upstream_failed", "carddav_storage_failed", "carddav_failed":
			return operationFailure(code, writes), nil
		default:
			return operationFailure("operation_refused", writes && response.StatusCode >= 500), nil
		}
	}
	var output T
	if response.StatusCode == http.StatusNoContent && len(data) == 0 {
		return &mcpserver.OperationResult{Output: output}, nil
	}
	if err := json.Unmarshal(data, &output); err != nil {
		return operationFailure("invalid_operation_response", writes), err
	}
	return &mcpserver.OperationResult{Output: output, ETag: response.Header.Get("ETag"), NewPersonETag: response.Header.Get("X-New-Person-Etag")}, nil
}
