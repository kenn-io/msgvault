package mcp

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

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

func deletionSelectionDefinitions() []operationalDefinition {
	return []operationalDefinition{
		newOperationalDefinition("explore_deletion_selection", "Explore native rows and obtain the cache revision, search provenance and candidate snapshot needed for an exact deletion selection. A saturated candidate pool cannot be staged.", OperationFamilyRecords, outputSchemaFor[generated.ExploreHTTPRequest](), outputSchemaFor[generated.ExploreHTTPResponse](), false),
		newOperationalDefinition("preflight_deletion_selection", "Preflight an exact native selection. Preserve the selection unchanged and pass it with the returned operation token to stage_deletion. No remote deletion occurs.", OperationFamilyRecords, selectionInputSchema(), outputSchemaFor[generated.ExplorePreflightResponse](), false),
	}
}

func (h *handlers) stageDeletionSelection(ctx context.Context, req toolRequest) (*toolResult, error) {
	if h.delegatedOnly || h.operations == nil || !slices.Contains(h.operationCapabilities, "stage_deletion_selection") {
		return toolErrorResult("exact selection staging is unavailable"), nil
	}
	for key := range req.arguments {
		if key != "selection" && key != "operation_token" {
			return toolErrorResult("selection cannot be combined with other filters"), nil
		}
	}
	if token, ok := req.arguments["operation_token"].(string); !ok || token == "" {
		return toolErrorResult("operation_token is required"), nil
	}
	preflight, err := h.operations.ExecuteOperation(ctx, "preflight_deletion_selection", map[string]any{"selection": req.arguments["selection"]})
	if err != nil || preflight == nil || preflight.IsError {
		return operationalToolResponse("preflight_deletion_selection", preflight, err)
	}
	counts, ok := preflight.Output.(generated.ExplorePreflightResponse)
	if !ok {
		return nil, newInternalError("preflight deletion selection", fmt.Errorf("unexpected response"))
	}
	selection, err := json.Marshal(req.arguments["selection"], json.Deterministic(true))
	if err != nil {
		return toolErrorResult("invalid selection"), nil
	}
	disclosure := fmt.Sprintf("Stage a local deletion manifest for this exact selection: %s. Matched: %d; deletable: %d; estimated bytes: %d. No remote deletion occurs.", selection, counts.Count, counts.DeletableCount, counts.EstimatedBytes)
	if err := req.confirmUserAction(ctx, disclosure); err != nil {
		return confirmationToolError(err)
	}
	result, err := h.operations.ExecuteOperation(ctx, "stage_deletion_selection", req.arguments)
	if err != nil || result == nil || result.IsError {
		return operationalToolResponse("stage_deletion_selection", result, err)
	}
	staged, ok := result.Output.(generated.StageDeletionResponse)
	if !ok || staged.ID == nil || staged.Status == nil {
		return operationalToolResponse("stage_deletion_selection", &OperationResult{IsError: true, Output: map[string]any{"error": "invalid_operation_response", "operation_may_have_completed": true}}, nil)
	}
	response := stageDeletionResponse{BatchID: *staged.ID, Status: *staged.Status, MessageCount: int(staged.MessageCount), NextStep: "Review the staged manifest; remote deletion requires a separate CLI action."}
	if staged.SkippedCount != nil {
		response.SkippedCount = int(*staged.SkippedCount)
	}
	return jsonResult(response)
}
