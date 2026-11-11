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

type VisualOperationStatus struct {
	Generation             VisualGenerationStatus  `json:"generation"`
	Eligible               int64                   `json:"eligible"`
	Current                int64                   `json:"current"`
	Stale                  int64                   `json:"stale"`
	Tombstoned             int64                   `json:"tombstoned"`
	Retryable              int64                   `json:"retryable"`
	Terminal               int64                   `json:"terminal"`
	UnknownRole            int64                   `json:"unknown_role"`
	Unavailable            int64                   `json:"unavailable"`
	ActiveLeases           int64                   `json:"active_leases"`
	JournalHighWater       int64                   `json:"journal_high_water"`
	ReconciliationComplete bool                    `json:"reconciliation_complete"`
	JournalCursor          int64                   `json:"journal_cursor"`
	JournalLag             int64                   `json:"journal_lag"`
	Converged              int64                   `json:"converged"`
	ConvergenceTotal       int64                   `json:"convergence_total"`
	ConvergenceRatio       float64                 `json:"convergence_ratio"`
	Formats                []visual.FormatCoverage `json:"formats"`
	Usage                  visual.ProviderUsage    `json:"usage"`
}

type VisualRetireReceipt struct {
	Retired bool `json:"retired"`
}

func visualOperationalDefinitions() []operationalDefinition {
	status := outputSchemaFor[VisualOperationStatus]()
	return []operationalDefinition{
		newOperationalDefinition("get_visual_attachment_status", "Read visual attachment indexing progress. Coverage performs an expensive archive-wide blob scan only when explicitly requested.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"coverage": booleanSchema("Scan all candidate blobs for per-format coverage")}), status, false),
		newOperationalDefinition("start_visual_attachment_build", "Grant hosted visual processing consent and run one bounded pass. Requires explicit consent and approval; provider charges may apply.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"consent": booleanSchema("Explicit consent to hosted processing")}, "consent"), status, true),
		newOperationalDefinition("resume_visual_attachment_build", "Resume one bounded visual indexing pass under existing consent. Requires approval; provider charges may apply.", OperationFamilyVisual, closedObject(nil), status, true),
		newOperationalDefinition("retry_visual_attachment_owner", "Retry the exact message and blob owner under existing consent. Requires approval; provider charges may apply.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"message_id": safeIDSchema("Exact message ID"), "blob_hash": stringSchema("Exact attachment blob hash")}, "message_id", "blob_hash"), status, true),
		newOperationalDefinition("retire_visual_attachment_generation", "Retire the exact current visual generation. Requires approval.", OperationFamilyVisual, closedObject(map[string]*jsonschema.Schema{"generation_id": safeIDSchema("Current generation ID from status")}, "generation_id"), outputSchemaFor[VisualRetireReceipt](), true),
	}
}
