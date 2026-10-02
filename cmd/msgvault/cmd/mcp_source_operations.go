package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"net/http"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/pkg/client/generated"
)

type daemonMCPNamedPeopleBrowser struct {
	*daemonclient.PeopleBrowser

	client *daemonclient.Client
}

func (b daemonMCPNamedPeopleBrowser) PromoteWithDisplayName(ctx context.Context, participantID int64, name *string) (*mcpserver.OperationResult, error) {
	return readMCPOperationJSON[generated.Person](ctx, b.client, http.MethodPost, "/api/v1/people", &generated.CreatePersonRequestOptions{Body: &generated.CreatePersonBody{ParticipantID: participantID, DisplayName: name}}, http.StatusOK, http.StatusCreated)
}

func supportsNamedPromotion(capabilities *apiprotocol.MCPCapabilities) bool {
	if capabilities == nil || capabilities.Delegated {
		return false
	}
	for _, route := range capabilities.Routes {
		if route.OperationID == "createPerson" && route.Method == http.MethodPost && route.Path == "/api/v1/people" {
			return slices.Contains(route.RequestProperties, "display_name") && slices.Contains(route.RequestProperties, "participant_id")
		}
	}
	return false
}

type mcpRouteCheck func(string, string, string, ...string) bool

func sourceMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	var names []string
	for _, route := range []struct{ name, id, method, path string }{
		{"list_source_status", "listSourceStatus", http.MethodGet, "/api/v1/sources/status"},
		{"get_source_identities", "listSourceIdentities", http.MethodGet, "/api/v1/sources/{source_id}/identities"},
		{"get_participant_identity", "getParticipant", http.MethodGet, "/api/v1/participants/{id}"},
		{"get_cache_build_status", "getCacheBuildStatus", http.MethodGet, "/api/v1/cache-builds/{job_id}"},
		{"get_person_edit_context", "getPersonProfile", http.MethodGet, "/api/v1/people/{id}"},
		{"get_slack_sync_policy", "getSettings", http.MethodGet, "/api/v1/settings"},
	} {
		query := []string{}
		if route.name == "list_source_status" {
			query = append(query, "source_type")
		}
		if hasRoute(route.id, route.method, route.path, query...) {
			names = append(names, route.name)
		}
	}
	if slices.Contains(names, "get_person_edit_context") && hasRoute("patchPerson", http.MethodPatch, "/api/v1/people/{id}") && mcpRequestPropertiesPresent(capabilities, "patchPerson", "display_name") {
		names = append(names, "set_person_display_name")
	}
	if slices.Contains(names, "get_slack_sync_policy") && hasRoute("patchSettings", http.MethodPatch, "/api/v1/settings") && mcpRequestPropertiesPresent(capabilities, "patchSettings", "updates") {
		names = append(names, "update_slack_sync_policy")
	}
	return names
}

func mcpRequestPropertiesPresent(capabilities *apiprotocol.MCPCapabilities, id string, properties ...string) bool {
	if capabilities == nil {
		return false
	}
	for _, route := range capabilities.Routes {
		if route.OperationID != id {
			continue
		}
		for _, property := range properties {
			if !slices.Contains(route.RequestProperties, property) {
				return false
			}
		}
		return true
	}
	return false
}

type sourceStatusArguments struct {
	SourceType *string `json:"source_type,omitempty"`
}
type sourceIdentityArguments struct {
	SourceID int64 `json:"source_id"`
}
type participantIdentityArguments struct {
	ParticipantID int64 `json:"participant_id"`
}
type cacheBuildArguments struct {
	JobID string `json:"job_id"`
}
type personEditArguments struct {
	PersonID int64 `json:"person_id"`
}
type personNameArguments struct {
	PersonID    int64   `json:"person_id"`
	ETag        string  `json:"etag"`
	DisplayName *string `json:"display_name"`
}
type slackPolicyArguments struct {
	ETag            string    `json:"etag"`
	DMs             *bool     `json:"dms,omitempty"`
	GroupDMs        *bool     `json:"group_dms,omitempty"`
	Channels        *[]string `json:"channels,omitempty"`
	ExcludeChannels *[]string `json:"exclude_channels,omitempty"`
}

func decodeMCPOperationArguments[T any](args map[string]any) (T, error) {
	var value T
	if args == nil {
		args = map[string]any{}
	}
	data, err := json.Marshal(args)
	if err == nil {
		err = json.Unmarshal(data, &value, json.RejectUnknownMembers(true))
	}
	return value, err
}

func (b *daemonMCPOperations) executeSourceOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	invalid := func() (*mcpserver.OperationResult, bool, error) {
		return operationFailure("invalid_arguments", false), true, nil
	}
	var result *mcpserver.OperationResult
	var err error
	switch name {
	case "list_source_status":
		input, decodeErr := decodeMCPOperationArguments[sourceStatusArguments](args)
		if decodeErr != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.SourceStatusResponse](ctx, b.client, http.MethodGet, "/api/v1/sources/status", &generated.ListSourceStatusRequestOptions{Query: &generated.ListSourceStatusQuery{SourceType: input.SourceType}}, http.StatusOK)
	case "get_source_identities":
		input, decodeErr := decodeMCPOperationArguments[sourceIdentityArguments](args)
		if decodeErr != nil || input.SourceID <= 0 {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.SourceIdentitiesResponse](ctx, b.client, http.MethodGet, "/api/v1/sources/{source_id}/identities", &generated.ListSourceIdentitiesRequestOptions{PathParams: &generated.ListSourceIdentitiesPath{SourceID: input.SourceID}}, http.StatusOK)
	case "get_participant_identity":
		input, decodeErr := decodeMCPOperationArguments[participantIdentityArguments](args)
		if decodeErr != nil || input.ParticipantID <= 0 {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.PersonSummary](ctx, b.client, http.MethodGet, "/api/v1/participants/{id}", &generated.GetParticipantRequestOptions{PathParams: &generated.GetParticipantPath{ID: input.ParticipantID}}, http.StatusOK)
	case "get_cache_build_status":
		input, decodeErr := decodeMCPOperationArguments[cacheBuildArguments](args)
		if decodeErr != nil || strings.TrimSpace(input.JobID) == "" {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.CacheBuildStatus](ctx, b.client, http.MethodGet, "/api/v1/cache-builds/{job_id}", &generated.GetCacheBuildStatusRequestOptions{PathParams: &generated.GetCacheBuildStatusPath{JobID: input.JobID}}, http.StatusOK)
	case "get_person_edit_context":
		input, decodeErr := decodeMCPOperationArguments[personEditArguments](args)
		if decodeErr != nil || input.PersonID <= 0 {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.Person](ctx, b.client, http.MethodGet, "/api/v1/people/{id}", &generated.GetPersonProfileRequestOptions{PathParams: &generated.GetPersonProfilePath{ID: input.PersonID}}, http.StatusOK)
		result = personEditProjection(result, false)
	case "set_person_display_name":
		input, decodeErr := decodeMCPOperationArguments[personNameArguments](args)
		_, present := args["display_name"]
		if decodeErr != nil || input.PersonID <= 0 || input.ETag == "" || !present {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.Person](ctx, b.client, http.MethodPatch, "/api/v1/people/{id}", &generated.PatchPersonRequestOptions{PathParams: &generated.PatchPersonPath{ID: input.PersonID}, Header: &generated.PatchPersonHeaders{IfMatch: input.ETag}, Body: &generated.PatchPersonBody{DisplayName: input.DisplayName}}, http.StatusOK)
		result = personEditProjection(result, true)
	case "get_slack_sync_policy":
		if _, decodeErr := decodeMCPOperationArguments[struct{}](args); decodeErr != nil {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodGet, "/api/v1/settings", nil, http.StatusOK)
		result = slackPolicyProjection(result, false)
	case "update_slack_sync_policy":
		input, decodeErr := decodeMCPOperationArguments[slackPolicyArguments](args)
		if decodeErr != nil || input.ETag == "" {
			return invalid()
		}
		updates, updateErr := slackSettingUpdates(input)
		if updateErr != nil || len(updates) == 0 {
			return invalid()
		}
		result, err = readMCPOperationJSON[generated.SettingsResponse](ctx, b.client, http.MethodPatch, "/api/v1/settings", &generated.PatchSettingsRequestOptions{Header: &generated.PatchSettingsHeaders{IfMatch: input.ETag}, Body: &generated.PatchSettingsBody{Updates: updates}}, http.StatusOK)
		result = slackPolicyProjection(result, true)
	default:
		return nil, false, nil
	}
	return result, true, err
}

func personEditProjection(result *mcpserver.OperationResult, writes bool) *mcpserver.OperationResult {
	if result == nil || result.IsError {
		return result
	}
	person, ok := result.Output.(generated.Person)
	if !ok || result.ETag == "" {
		return operationFailure("invalid_operation_response", writes)
	}
	result.Output = mcpserver.PersonEditContext{ETag: result.ETag, Person: person}
	return result
}

var slackSelectionKeys = []string{"slack.dms", "slack.group_dms", "slack.channels", "slack.exclude_channels"}

func slackPolicyProjection(result *mcpserver.OperationResult, writes bool) *mcpserver.OperationResult {
	if result == nil || result.IsError {
		return result
	}
	settings, ok := result.Output.(generated.SettingsResponse)
	if !ok || result.ETag == "" {
		return operationFailure("invalid_operation_response", writes)
	}
	policy := mcpserver.SlackSyncPolicy{ETag: result.ETag, PendingRestart: settings.PendingRestart, Settings: []generated.Setting{}}
	for _, setting := range settings.Settings {
		if slices.Contains(slackSelectionKeys, setting.Key) {
			setting.Secret = nil
			setting.CredentialID = nil
			policy.Settings = append(policy.Settings, setting)
		}
	}
	result.Output = policy
	return result
}

func slackSettingUpdates(input slackPolicyArguments) ([]generated.SettingUpdate, error) {
	var updates []generated.SettingUpdate
	appendValue := func(key, kind string, value any) error {
		data, err := json.Marshal(map[string]any{kind: value})
		if err != nil {
			return err
		}
		var settingValue generated.SettingValue
		if err := json.Unmarshal(data, &settingValue); err != nil {
			return err
		}
		updates = append(updates, generated.SettingUpdate{Key: key, Value: &settingValue})
		return nil
	}
	for _, field := range []struct {
		key   string
		value *bool
	}{{"slack.dms", input.DMs}, {"slack.group_dms", input.GroupDMs}} {
		if field.value != nil {
			if err := appendValue(field.key, "boolean", *field.value); err != nil {
				return nil, err
			}
		}
	}
	for _, field := range []struct {
		key   string
		value *[]string
	}{{"slack.channels", input.Channels}, {"slack.exclude_channels", input.ExcludeChannels}} {
		if field.value != nil {
			if err := appendValue(field.key, "strings", *field.value); err != nil {
				return nil, err
			}
		}
	}
	return updates, nil
}

func (b *daemonMCPOperations) sourceOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	var readName string
	var readArgs map[string]any
	var expected string
	var conflictCode string
	switch name {
	case "update_slack_sync_policy":
		input, err := decodeMCPOperationArguments[slackPolicyArguments](args)
		if err != nil || input.ETag == "" {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		updates, err := slackSettingUpdates(input)
		if err != nil || len(updates) == 0 {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		readName, expected, conflictCode = "get_slack_sync_policy", input.ETag, "settings_conflict"
	case "set_person_display_name":
		input, err := decodeMCPOperationArguments[personNameArguments](args)
		_, present := args["display_name"]
		if err != nil || input.PersonID <= 0 || input.ETag == "" || !present {
			return "", true, &mcpserver.OperationRefusalError{Code: "invalid_arguments"}
		}
		readName, readArgs, expected, conflictCode = "get_person_edit_context", map[string]any{"person_id": input.PersonID}, input.ETag, "person_revision_conflict"
	default:
		return "", false, nil
	}
	current, _, err := b.executeSourceOperation(ctx, readName, readArgs)
	if err != nil {
		return "", true, err
	}
	if current == nil || current.IsError {
		return "", true, &mcpserver.OperationRefusalError{Code: "operation_context_unavailable"}
	}
	if current.ETag != expected {
		return "", true, &mcpserver.OperationRefusalError{Code: conflictCode}
	}
	effect := "persist the explicit display name; an empty name clears the saved override"
	if name == "update_slack_sync_policy" {
		effect = "persist the explicit source selection; existing archived data is retained; daemon restart may be required"
	}
	data, err := json.Marshal(struct {
		Operation string         `json:"operation"`
		Current   any            `json:"current"`
		Changes   map[string]any `json:"changes"`
		Effect    string         `json:"effect"`
	}{name, current.Output, args, effect}, json.Deterministic(true))
	if len(data) == 0 && err == nil {
		err = errors.New("empty operation disclosure")
	}
	return string(data), true, err
}
