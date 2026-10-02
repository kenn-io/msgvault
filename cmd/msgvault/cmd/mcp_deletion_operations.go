package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/apiprotocol"
	"go.kenn.io/msgvault/internal/daemonclient"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/search"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const mcpDeletionPreview = "preview_deletion_selection"
const mcpDeletionStage = "stage_deletion"
const mcpDeletionPath = "/api/v1/deletions"
const mcpDeletionContextUnavailable = "operation_context_unavailable"

type mcpDeletionPreviewInput struct {
	Query      *string                     `json:"query,omitzero"`
	SourceID   *int64                      `json:"source_id,omitzero"`
	Account    *string                     `json:"account,omitzero"`
	Collection *string                     `json:"collection,omitzero"`
	Selection  *generated.ExploreSelection `json:"selection,omitzero"`
}

func deletionMCPCapabilities(hasRoute mcpRouteCheck, capabilities *apiprotocol.MCPCapabilities) []string {
	names := []string{}
	if hasRoute("stageDeletion", http.MethodPost, mcpDeletionPath) && mcpRequestPropertiesPresent(capabilities, "stageDeletion", "message_ids", "selection", "operation_token", "dry_run", "description") {
		names = append(names, mcpDeletionStage)
	}
	if hasRoute("searchCLI", http.MethodGet, "/api/v1/cli/search", "q", "limit") && hasRoute("explore", http.MethodPost, "/api/v1/explore") && hasRoute("preflightExploreSelection", http.MethodPost, "/api/v1/explore/preflight") && mcpRequestPropertiesPresent(capabilities, "explore", "query", "filters", "search_mode") && mcpRequestPropertiesPresent(capabilities, "preflightExploreSelection", "selection") {
		names = append(names, mcpDeletionPreview)
	}
	return names
}

func mcpDeletionStageInput(args map[string]any) (generated.StageDeletionBody, error) {
	input, err := decodeMCPOperationArguments[generated.StageDeletionBody](args)
	if err != nil {
		return input, err
	}
	for key, value := range args {
		if value == nil || key == "filter" {
			return input, errors.New("invalid staging argument")
		}
	}
	idsPresent := input.MessageIds != nil
	if idsPresent == (input.Selection != nil) || input.Filter != nil || input.Validate() != nil {
		return input, errors.New("use explicit IDs or reviewed selection")
	}
	if idsPresent {
		if len(input.MessageIds) == 0 || input.OperationToken != nil {
			return input, errors.New("invalid explicit ID selection")
		}
		seen := map[int64]bool{}
		for _, id := range input.MessageIds {
			if id <= 0 || id > 9007199254740991 || seen[id] {
				return input, errors.New("invalid message ID")
			}
			seen[id] = true
		}
	} else if input.OperationToken == nil || !mcpBoundedOpaqueValue(*input.OperationToken, 4096) {
		return input, errors.New("reviewed operation token required")
	}
	if input.Description != nil && len(*input.Description) > 4096 {
		return input, errors.New("description too long")
	}
	return input, nil
}

func (b *daemonMCPOperations) deletionSourceID(ctx context.Context, input mcpDeletionPreviewInput) (*int64, error) {
	count := 0
	for _, supplied := range []bool{input.SourceID != nil, input.Account != nil, input.Collection != nil} {
		if supplied {
			count++
		}
	}
	if count > 1 {
		return nil, &mcpserver.OperationRefusalError{Code: mcpFactInvalidArgumentCode}
	}
	if input.SourceID != nil {
		if *input.SourceID <= 0 || *input.SourceID > 9007199254740991 {
			return nil, &mcpserver.OperationRefusalError{Code: mcpFactInvalidArgumentCode}
		}
		return input.SourceID, nil
	}
	engine := daemonclient.NewEngineAdapter(b.client)
	if input.Collection != nil {
		if strings.TrimSpace(*input.Collection) == "" || *input.Collection == store.DefaultCollectionName {
			return nil, &mcpserver.OperationRefusalError{Code: "invalid_collection"}
		}
		scopes, err := engine.ListCollectionScopes(ctx)
		if err != nil {
			return nil, err
		}
		var found *int64
		for _, scope := range scopes {
			if scope.Name != *input.Collection {
				continue
			}
			if len(scope.SourceIDs) != 1 || found != nil {
				return nil, &mcpserver.OperationRefusalError{Code: "single_source_required"}
			}
			id := scope.SourceIDs[0]
			found = &id
		}
		if found == nil {
			return nil, &mcpserver.OperationRefusalError{Code: "collection_not_found"}
		}
		return found, nil
	}
	if input.Account != nil {
		if strings.TrimSpace(*input.Account) == "" {
			return nil, &mcpserver.OperationRefusalError{Code: mcpFactInvalidArgumentCode}
		}
		accounts, err := engine.ListAccounts(ctx)
		if err != nil {
			return nil, err
		}
		var found *int64
		for _, account := range accounts {
			if strings.EqualFold(account.Identifier, *input.Account) {
				if found != nil {
					return nil, &mcpserver.OperationRefusalError{Code: "ambiguous_account"}
				}
				id := account.ID
				found = &id
			}
		}
		if found == nil {
			return nil, &mcpserver.OperationRefusalError{Code: "not_found"}
		}
		return found, nil
	}
	return nil, nil //nolint:nilnil // No explicit source means the native unrestricted query scope.
}

func (b *daemonMCPOperations) deletionIndexReady(ctx context.Context, queryText string) error {
	parsed := search.Parse(queryText)
	if parsed.Err() != nil || parsed.IsEmpty() || len(queryText) > 4096 {
		return &mcpserver.OperationRefusalError{Code: "invalid_search_query"}
	}
	probe, err := b.client.GetCLISearch(ctx, daemonclient.CLISearchRequest{Query: queryText, Limit: 1})
	if err != nil {
		return err
	}
	switch probe.IndexState {
	case "checking":
		return &mcpserver.OperationRefusalError{Code: "search_index_checking"}
	case "building":
		return &mcpserver.OperationRefusalError{Code: "search_index_building"}
	}
	return nil
}

func (b *daemonMCPOperations) previewDeletion(ctx context.Context, args map[string]any) (*mcpserver.OperationResult, error) {
	input, err := decodeMCPOperationArguments[mcpDeletionPreviewInput](args)
	if err != nil {
		return operationFailure(mcpFactInvalidArgumentCode, false), nil //nolint:nilerr // Invalid input becomes a fixed structured tool refusal.
	}
	for _, value := range args {
		if value == nil {
			return operationFailure(mcpFactInvalidArgumentCode, false), nil
		}
	}
	if (input.Query != nil) == (input.Selection != nil) || input.Selection != nil && (input.SourceID != nil || input.Account != nil || input.Collection != nil) {
		return operationFailure(mcpFactInvalidArgumentCode, false), nil
	}
	supported, err := b.client.SupportsAPISchemaVersion(ctx, stageDeleteMinAPISchemaVersion)
	if err != nil {
		return nil, err
	}
	if !supported {
		return operationFailure("deletion_schema_upgrade_required", false), nil
	}
	var selection generated.ExploreSelection
	if input.Selection != nil {
		selection = *input.Selection
		if selection.Validate() != nil {
			return operationFailure(mcpFactInvalidArgumentCode, false), nil //nolint:nilerr // Invalid input becomes a fixed structured tool refusal.
		}
		if selection.Predicate.SearchMode == nil || *selection.Predicate.SearchMode != generated.ExploreHTTPRequestSearchModeFullText || selection.Predicate.Query == nil {
			return operationFailure("invalid_search_query", false), nil
		}
		if err := b.deletionIndexReady(ctx, *selection.Predicate.Query); err != nil {
			return nil, err
		}
	} else {
		parsed := search.Parse(*input.Query)
		if parsed.Err() != nil || parsed.IsEmpty() || len(*input.Query) > 4096 {
			return operationFailure("invalid_search_query", false), nil //nolint:nilerr // Invalid input becomes a fixed structured tool refusal.
		}
		sourceID, err := b.deletionSourceID(ctx, input)
		if err != nil {
			return nil, err
		}
		if err := b.deletionIndexReady(ctx, *input.Query); err != nil {
			return nil, err
		}
		mode := generated.ExploreHTTPRequestSearchModeFullText
		limit := int64(1)
		predicate := generated.ExploreHTTPRequest{Query: input.Query, SearchMode: &mode, Filters: []generated.ExploreFilter{{Dimension: generated.ExploreFilterDimensionDeletion, Values: []string{string(search.DeletionScopeActive)}}}}
		if sourceID != nil {
			predicate.Filters = append(predicate.Filters, generated.ExploreFilter{Dimension: generated.ExploreFilterDimensionSource, Values: []string{strconv.FormatInt(*sourceID, 10)}})
		}
		result, err := readMCPOperationJSON[generated.ExploreHTTPResponse](ctx, b.client, http.MethodPost, "/api/v1/explore", &generated.ExploreRequestOptions{Body: &generated.ExploreBody{Query: predicate.Query, SearchMode: predicate.SearchMode, Filters: predicate.Filters, Limit: &limit}}, http.StatusOK)
		if err != nil || result.IsError {
			return result, err
		}
		explored, ok := result.Output.(generated.ExploreHTTPResponse)
		if !ok {
			return operationFailure("invalid_operation_response", false), nil
		}
		selection = generated.ExploreSelection{CacheRevision: explored.CacheRevision, CandidateSnapshotID: explored.CandidateSnapshotID, Mode: generated.ExploreSelectionModeAllMatching, Predicate: predicate, SearchProvenance: explored.SearchProvenance}
	}
	result, err := readMCPOperationJSON[generated.ExplorePreflightResponse](ctx, b.client, http.MethodPost, "/api/v1/explore/preflight", &generated.PreflightExploreSelectionRequestOptions{Body: &generated.PreflightExploreSelectionBody{Selection: selection}}, http.StatusOK)
	if err != nil || result.IsError {
		return result, err
	}
	receipt, ok := result.Output.(generated.ExplorePreflightResponse)
	if !ok {
		return operationFailure("invalid_operation_response", false), nil
	}
	return &mcpserver.OperationResult{Output: mcpserver.DeletionSelectionPreview{Selection: selection, Preflight: receipt}}, nil
}

func (b *daemonMCPOperations) executeDeletionOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	switch name {
	case mcpDeletionPreview:
		result, err := b.previewDeletion(ctx, args)
		if refusal, ok := errors.AsType[*mcpserver.OperationRefusalError](err); ok {
			return operationFailure(refusal.Code, false), true, nil
		}
		return result, true, err
	case mcpDeletionStage:
		input, err := mcpDeletionStageInput(args)
		if err != nil {
			return operationFailure(mcpFactInvalidArgumentCode, false), true, nil //nolint:nilerr // Invalid input becomes a fixed structured tool refusal.
		}
		if input.Selection != nil {
			supported, err := b.client.SupportsAPISchemaVersion(ctx, stageDeleteMinAPISchemaVersion)
			if err != nil {
				return nil, true, err
			}
			if !supported {
				return operationFailure("deletion_schema_upgrade_required", false), true, nil
			}
		}
		result, err := readMCPOperationJSON[generated.StageDeletionResponse](ctx, b.client, http.MethodPost, mcpDeletionPath, &generated.StageDeletionRequestOptions{Body: &input}, http.StatusOK, http.StatusCreated)
		return result, true, err
	default:
		return nil, false, nil
	}
}

func (b *daemonMCPOperations) deletionOperationDisclosure(ctx context.Context, name string, args map[string]any) (string, bool, error) {
	if name != mcpDeletionStage {
		return "", false, nil
	}
	input, err := mcpDeletionStageInput(args)
	if err != nil {
		return "", true, &mcpserver.OperationRefusalError{Code: mcpFactInvalidArgumentCode}
	}
	if input.Selection != nil {
		supported, err := b.client.SupportsAPISchemaVersion(ctx, stageDeleteMinAPISchemaVersion)
		if err != nil {
			return "", true, err
		}
		if !supported {
			return "", true, &mcpserver.OperationRefusalError{Code: "deletion_schema_upgrade_required"}
		}
	}
	dryRun := true
	reviewedInput := input
	reviewedInput.DryRun = &dryRun
	reviewed, err := readMCPOperationJSON[generated.StageDeletionResponse](ctx, b.client, http.MethodPost, mcpDeletionPath, &generated.StageDeletionRequestOptions{Body: &reviewedInput}, http.StatusOK)
	if err != nil {
		return "", true, err
	}
	if reviewed == nil {
		return "", true, &mcpserver.OperationRefusalError{Code: mcpDeletionContextUnavailable}
	}
	if reviewed.IsError {
		data, err := json.Marshal(reviewed.Output)
		if err != nil {
			return "", true, err
		}
		var failure struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &failure) != nil || failure.Error == "" {
			return "", true, &mcpserver.OperationRefusalError{Code: mcpDeletionContextUnavailable}
		}
		return "", true, &mcpserver.OperationRefusalError{Code: failure.Error}
	}
	receipt, ok := reviewed.Output.(generated.StageDeletionResponse)
	if !ok {
		return "", true, &mcpserver.OperationRefusalError{Code: "invalid_operation_response"}
	}
	description := "Stage the unchanged reviewed selection and one-shot operation token"
	if input.MessageIds != nil {
		data, err := json.Marshal(input.MessageIds)
		if err != nil {
			return "", true, err
		}
		description = "Stage explicit internal message IDs " + string(data)
	} else {
		description += fmt.Sprintf(" at cache revision %q", input.Selection.CacheRevision)
	}
	if input.DryRun != nil && *input.DryRun {
		description += " as a dry run, without creating a batch"
	} else {
		description += " into a deletion batch"
	}
	description += fmt.Sprintf(". Current native eligibility: %d message(s) can be staged", receipt.MessageCount)
	if receipt.MatchedCount != nil {
		description += fmt.Sprintf(" from %d matches", *receipt.MatchedCount)
	}
	if receipt.SkippedCount != nil {
		description += fmt.Sprintf("; %d skipped", *receipt.SkippedCount)
	}
	return description + ". The daemon resolves eligibility and revision authority; this does not perform remote deletion.", true, nil
}
