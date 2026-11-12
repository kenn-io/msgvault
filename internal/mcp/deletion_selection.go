package mcp

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func selectionInputSchema() *jsonschema.Schema {
	selection := outputSchemaFor[generated.ExploreSelection]()
	selection.Schema = ""
	input := closedObject(map[string]*jsonschema.Schema{"selection": selection}, "selection")
	input.Defs, selection.Defs = selection.Defs, nil
	input.Definitions, selection.Definitions = selection.Definitions, nil
	return input
}

type deletionExploreResponse struct {
	generated.ExploreHTTPResponse
	Predicate generated.ExploreHTTPRequest `json:"predicate"`
}

func (h *handlers) exploreDeletionSelection(ctx context.Context, req toolRequest) (*toolResult, error) {
	args := req.arguments
	if _, present := args["collection"]; present {
		filters, _ := args["filters"].([]any)
		for _, item := range filters {
			filter, _ := item.(map[string]any)
			if filter["dimension"] == "account" || filter["dimension"] == "source" {
				return toolErrorResult("collection cannot be combined with account or source filters"), nil
			}
		}
		selection, err := h.resolveReadSelection(ctx, "", args)
		if err != nil {
			return dependencyError("resolve deletion collection", err)
		}
		if len(selection.sourceIDs) == 0 {
			return toolErrorResult("collection has no sources"), nil
		}
		values := make([]string, 0, len(selection.sourceIDs))
		for _, id := range selection.sourceIDs {
			values = append(values, strconv.FormatInt(id, 10))
		}
		forwarded := make(map[string]any, len(args))
		for key, value := range args {
			if key != "collection" {
				forwarded[key] = value
			}
		}
		forwarded["filters"] = append(slices.Clone(filters), map[string]any{"dimension": "source", "values": values})
		args = forwarded
	}
	result, err := h.operations.ExecuteOperation(ctx, "explore_deletion_selection", args)
	if err != nil || result == nil || result.IsError {
		return operationalToolResponse("explore_deletion_selection", result, err)
	}
	response, ok := result.Output.(generated.ExploreHTTPResponse)
	if !ok {
		return nil, newInternalError("explore deletion selection", fmt.Errorf("unexpected response"))
	}
	predicate := make(map[string]any, len(args))
	for key, value := range args {
		if key != "cursor" {
			predicate[key] = value
		}
	}
	return jsonResult(struct {
		generated.ExploreHTTPResponse
		Predicate map[string]any `json:"predicate"`
	}{response, predicate})
}

func deletionSelectionDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("explore_deletion_selection", "Explore native rows and obtain the cache revision, search provenance and candidate snapshot needed for an exact deletion selection. Use the returned predicate unchanged in the selection. A saturated candidate pool cannot be staged.", OperationFamilyRecords, outputSchemaFor[generated.ExploreHTTPRequest](), outputSchemaFor[deletionExploreResponse](), false),
		newOperationalDefinition("preflight_deletion_selection", "Preflight an exact native selection. Preserve the selection unchanged and pass it with the returned operation token to stage_deletion. No remote deletion occurs.", OperationFamilyRecords, selectionInputSchema(), outputSchemaFor[generated.ExplorePreflightResponse](), false),
	}
}

func (h *handlers) stageDeletionSelection(ctx context.Context, req toolRequest) (*toolResult, error) {
	for key := range req.arguments {
		if key != "selection" && key != "operation_token" {
			return toolErrorResult("selection cannot be combined with other filters"), nil
		}
	}
	dryRun, err := h.operations.ExecuteOperation(ctx, "stage_deletion_selection", map[string]any{"selection": req.arguments["selection"], "operation_token": req.arguments["operation_token"], "dry_run": true})
	if err != nil || dryRun == nil || dryRun.IsError {
		return operationalToolResponse("stage_deletion_selection", dryRun, err)
	}
	counts, ok := dryRun.Output.(generated.StageDeletionResponse)
	if !ok || counts.Account == nil || counts.MatchedCount == nil {
		return nil, newInternalError("dry run deletion selection", fmt.Errorf("unexpected response"))
	}
	skipped := int64(0)
	if counts.SkippedCount != nil {
		skipped = *counts.SkippedCount
	}
	disclosure := fmt.Sprintf("Stage a local deletion manifest for account %s. Matched: %d; staged: %d; skipped: %d. No remote deletion occurs.", *counts.Account, *counts.MatchedCount, counts.MessageCount, skipped)
	if err := req.confirmUserAction(ctx, disclosure); err != nil {
		return confirmationToolError(err)
	}
	selection, _ := req.arguments["selection"].(map[string]any)
	predicate, _ := selection["predicate"].(map[string]any)
	query, _ := predicate["query"].(string)
	description := "selection: " + stringArgument(selection, "mode")
	if strings.TrimSpace(query) != "" && stringArgument(selection, "mode") != "explicit" {
		description = "query: " + query
		description = capDeletionDescription(description)
	}
	result, err := h.operations.ExecuteOperation(ctx, "stage_deletion_selection", map[string]any{"selection": req.arguments["selection"], "operation_token": req.arguments["operation_token"], "description": description})
	if err != nil || result == nil || result.IsError {
		return operationalToolResponse("stage_deletion_selection", result, err)
	}
	staged, ok := result.Output.(generated.StageDeletionResponse)
	if !ok || staged.ID == nil || staged.Status == nil {
		return operationalToolResponse("stage_deletion_selection", &OperationResult{IsError: true, Output: map[string]any{"error": "invalid_operation_response", "operation_may_have_completed": true}}, nil)
	}
	response := stageDeletionResponse{BatchID: *staged.ID, Status: *staged.Status, MessageCount: int(staged.MessageCount)}
	if staged.SkippedCount != nil {
		response.SkippedCount = int(*staged.SkippedCount)
	}
	response.NextStep = deletionNextStep(response.BatchID, response.SkippedCount, response.MessageCount, "")
	return jsonResult(response)
}
