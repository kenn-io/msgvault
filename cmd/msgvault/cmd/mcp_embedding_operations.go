package cmd

import (
	"context"
	"encoding/json/v2"
	"slices"

	"go.kenn.io/msgvault/internal/apiprotocol"
	mcpserver "go.kenn.io/msgvault/internal/mcp"
	"go.kenn.io/msgvault/internal/vector"
)

func embeddingMCPCapabilities(commands []apiprotocol.MCPCommandDescriptor) []string {
	for _, command := range commands {
		if command.Name == "embeddings list" && !command.Delegated && slices.Contains(command.Flags, flagJSON) {
			return []string{"list_embedding_generations"}
		}
	}
	return nil
}

func (b *daemonMCPOperations) executeEmbeddingOperation(ctx context.Context, name string, args map[string]any) (*mcpserver.OperationResult, bool, error) {
	if name != "list_embedding_generations" {
		return nil, false, nil
	}
	if _, err := decodeMCPOperationArguments[struct{}](args); err != nil {
		return operationFailure("invalid_operation_arguments", false), true, nil //nolint:nilerr // Invalid arguments become a fixed structured refusal.
	}
	stream, err := b.client.RunMCPCLICommand(ctx, []string{embeddingsCommandName, cmdUseList, "--json"}, "")
	if err != nil || stream == nil || stream.Failed {
		return operationFailure("embedding_status_unavailable", false), true, err
	}
	var report vector.GenerationStatusReport
	if err := json.Unmarshal([]byte(stream.Stdout), &report, json.RejectUnknownMembers(true)); err != nil {
		return operationFailure("invalid_embedding_status", false), true, err
	}
	if !mcpBoundedOpaqueValue(report.ConfiguredGenerationFingerprint, 1024) {
		return operationFailure("invalid_embedding_status", false), true, nil
	}
	for _, id := range report.SourceIDs {
		if !mcpPositiveSafeID(id) {
			return operationFailure("invalid_embedding_status", false), true, nil
		}
	}
	for _, row := range report.Generations {
		if !mcpPositiveSafeID(int64(row.ID)) || !mcpBoundedOpaqueValue(row.Model, 1024) || !mcpBoundedOpaqueValue(row.Fingerprint, 1024) || row.Dimension < 1 || row.StartedAt.IsZero() || !slices.Contains([]vector.GenerationState{vector.GenerationBuilding, vector.GenerationActive, vector.GenerationRetired}, row.State) || row.CoverageAvailable != (row.State != vector.GenerationRetired) {
			return operationFailure("invalid_embedding_status", false), true, nil
		}
		for _, count := range []int64{row.MessageCount, row.LiveCount, row.EmbeddedCount, row.BlankCount, row.MissingCount, row.Accelerator.IndexedCount} {
			if count < 0 {
				return operationFailure("invalid_embedding_status", false), true, nil
			}
		}
		if row.CoverageAvailable && row.LiveCount != row.EmbeddedCount+row.BlankCount+row.MissingCount {
			return operationFailure("invalid_embedding_status", false), true, nil
		}
		if !slices.Contains([]string{"", "exact", "building", "ready", "stale"}, row.Accelerator.State) {
			return operationFailure("invalid_embedding_status", false), true, nil
		}
	}
	return &mcpserver.OperationResult{Output: report}, true, nil
}
