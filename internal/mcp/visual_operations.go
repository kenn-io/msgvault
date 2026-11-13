package mcp

import (
	"github.com/google/jsonschema-go/jsonschema"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type VisualGenerationStatus struct {
	ID        int64  `json:"id"`
	State     string `json:"state"`
	Consented bool   `json:"consented"`
}

type VisualDuplicateCost struct {
	AtLeastOnce        bool `json:"at_least_once"`
	ProviderIdempotent bool `json:"provider_idempotent"`
}

type VisualOperationStatus struct {
	Generation             VisualGenerationStatus   `json:"generation"`
	Eligible               *int64                   `json:"eligible,omitempty"`
	Current                int64                    `json:"current"`
	Stale                  int64                    `json:"stale"`
	Tombstoned             int64                    `json:"tombstoned"`
	Retryable              int64                    `json:"retryable"`
	Terminal               int64                    `json:"terminal"`
	UnknownRole            *int64                   `json:"unknown_role,omitempty"`
	Unavailable            *int64                   `json:"unavailable,omitempty"`
	ActiveLeases           int64                    `json:"active_leases"`
	JournalHighWater       int64                    `json:"journal_high_water"`
	ReconciliationComplete bool                     `json:"reconciliation_complete"`
	JournalCursor          int64                    `json:"journal_cursor"`
	JournalLag             int64                    `json:"journal_lag"`
	Converged              int64                    `json:"converged"`
	ConvergenceTotal       int64                    `json:"convergence_total"`
	ConvergenceRatio       float64                  `json:"convergence_ratio"`
	Formats                *[]visual.FormatCoverage `json:"formats,omitempty"`
	Usage                  visual.ProviderUsage     `json:"usage"`
	DuplicateCost          VisualDuplicateCost      `json:"duplicate_cost"`
}

type VisualRetireReceipt struct {
	Retired bool `json:"retired"`
}

func visualOperationalDefinitions() []operationalDefinition {
	status := outputSchemaFor[VisualOperationStatus]()
	hash := stringSchema("Exact attachment SHA-256 blob hash")
	hash.Pattern = "^[a-fA-F0-9]{64}$"
	definitions := []operationalDefinition{
		newOperationalDefinition("get_visual_attachment_status", "Read visual attachment indexing progress. Coverage performs an expensive archive-wide blob scan only when explicitly requested.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"coverage": booleanSchema("Scan all candidate blobs for per-format coverage")}), status, false),
		newOperationalDefinition("start_visual_attachment_build", "Grant hosted visual processing consent and run one bounded pass. Requires explicit consent and approval; provider charges may apply.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"consent": booleanSchema("Explicit consent to hosted processing")}, "consent"), status, true),
		newOperationalDefinition("resume_visual_attachment_build", "Resume one bounded visual indexing pass under existing consent. Requires approval; provider charges may apply.", OperationFamilyVisual, closedObject(nil), status, true),
		newOperationalDefinition("retry_visual_attachment_owner", "Retry the exact message and blob owner under existing consent. Requires approval; provider charges may apply.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"message_id": safeIDSchema("Exact message ID"), "blob_hash": hash}, "message_id", "blob_hash"), status, true),
		newOperationalDefinition("retire_visual_attachment_generation", "Retire the exact current visual generation. Requires approval.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"generation_id": safeIDSchema("Current generation ID from status")}, "generation_id"), outputSchemaFor[VisualRetireReceipt](), true),
	}
	retire := definitions[len(definitions)-1].definition.annotations
	retire.OpenWorldHint = new(false)
	retire.DestructiveHint = new(true)
	return definitions
}
