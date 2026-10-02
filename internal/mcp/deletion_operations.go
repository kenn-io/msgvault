package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/pkg/client/generated"
)

// DeletionSelectionPreview retains the exact daemon selection beside its
// preflight receipt. Clients must pass both selection and token unchanged.
type DeletionSelectionPreview struct {
	Selection generated.ExploreSelection         `json:"selection"`
	Preflight generated.ExplorePreflightResponse `json:"preflight"`
}

func deletionOperationalDefinitions() []operationalDefinition {
	selection := recordSchema[generated.ExploreSelection]()
	preview := closedObject(map[string]*jsonschema.Schema{
		"query":      stringSchema("Full-text search query; mutually exclusive with selection"),
		"source_id":  safeIDSchema("Restrict query to one exact source; mutually exclusive with account and collection"),
		"account":    accountProperty(),
		"collection": stringSchema("Exact collection containing one source; mutually exclusive with account and source_id"),
		"selection":  selection,
	})
	ids := &jsonschema.Schema{Type: mcpSchemaArray, Items: safeIDSchema("Internal message ID"), UniqueItems: true, MinItems: func() *int { v := 1; return &v }()}
	stage := closedObject(map[string]*jsonschema.Schema{
		"message_ids":     ids,
		"selection":       selection,
		"operation_token": stringSchema("Exact operation_token returned by preview_deletion_selection for this unchanged selection"),
		"description":     stringSchema("Optional batch description"),
		"dry_run":         booleanSchema("Return native eligibility counts without creating a batch; default false"),
	})
	return []operationalDefinition{
		newOperationalDefinition("preview_deletion_selection", "Preview active full-text matches with the daemon's index-readiness guard and exact revision-pinned preflight. Use query with optional one-source scope, or an existing selection including exclusions. Returns the exact selection and operation token; never stages or performs remote deletion.", OperationFamilyDeletion, preview, recordSchema[DeletionSelectionPreview](), false, false),
		newOperationalDefinition(ToolStageDeletion, "Stage the daemon's eligible subset using EITHER explicit message_ids OR the unchanged selection and operation_token from preview_deletion_selection. Query staging requires API schema 2.18.0 or newer. dry_run creates no batch. Never performs remote deletion. Review the batch before running msgvault delete-staged; execution requires '[deletion] remote_enabled = true' in the invoking CLI's config.toml or MSGVAULT_ENABLE_REMOTE_DELETE=1 for one command.", OperationFamilyDeletion, stage, recordSchema[generated.StageDeletionResponse](), true, false),
	}
}
