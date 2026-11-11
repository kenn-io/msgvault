package mcp

import (
	"time"

	"github.com/google/jsonschema-go/jsonschema"
)

type SweepUsage struct {
	Requests              int   `json:"requests"`
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	EstimatedCostMicroUSD int64 `json:"estimated_cost_microusd"`
}
type SweepStatus struct {
	Enabled             bool       `json:"enabled"`
	Schedule            string     `json:"schedule"`
	DirtyCount          int        `json:"dirty_count"`
	LeasedCount         int        `json:"leased_count"`
	RetryCount          int        `json:"retry_count"`
	OldestDirtyAt       *time.Time `json:"oldest_dirty_at,omitempty"`
	JournalHighWater    int64      `json:"journal_high_water"`
	CursorHighWater     int64      `json:"cursor_high_water"`
	ProgramFingerprint  string     `json:"program_fingerprint,omitempty"`
	CatalogFingerprint  string     `json:"catalog_fingerprint,omitempty"`
	ProviderFingerprint string     `json:"provider_fingerprint,omitempty"`
	LastFailure         string     `json:"last_failure,omitempty"`
}
type SweepRun struct {
	RunID           string     `json:"run_id"`
	PeopleAttempted int        `json:"people_attempted"`
	PeopleSucceeded int        `json:"people_succeeded"`
	ProjectedWrites int        `json:"projected_writes"`
	Usage           SweepUsage `json:"usage"`
}
type SweepHistoryRun struct {
	Kind            string     `json:"kind"`
	Mode            string     `json:"mode"`
	Status          string     `json:"status"`
	Attempts        int        `json:"attempts"`
	Successes       int        `json:"successes"`
	Failures        int        `json:"failures"`
	ProjectedWrites int        `json:"projected_writes"`
	Usage           SweepUsage `json:"usage"`
}
type SweepHistoryAttempt struct {
	PersonID        int64      `json:"person_id"`
	Status          string     `json:"status"`
	FailureClass    string     `json:"failure_class,omitempty"`
	SeedCount       int        `json:"seed_count"`
	ContextCount    int        `json:"context_count"`
	ClaimCount      int        `json:"claim_count"`
	DecisionCount   int        `json:"decision_count"`
	ProjectedWrites int        `json:"projected_writes"`
	Usage           SweepUsage `json:"usage"`
	LatencyMS       int64      `json:"latency_ms"`
}
type SweepHistory struct {
	Runs     []SweepHistoryRun     `json:"runs"`
	Attempts []SweepHistoryAttempt `json:"attempts"`
}

func peopleSweepDefinitions() []operationalDefinition {
	person := func() *jsonschema.Schema { return safeIDSchema("Tracked durable person ID") }
	return []operationalDefinition{
		newOperationalDefinition("get_people_sweep_status", "Read redacted people sweep status. Maintenance may return operation_in_progress; retry later.", OperationFamilySweep, closedObject(map[string]*jsonschema.Schema{}), outputSchemaFor[SweepStatus](), false),
		newOperationalDefinition("list_people_sweep_history", "Read redacted sweep history, optionally for a configured provider. Maintenance may return operation_in_progress; retry later.", OperationFamilySweep, closedObject(map[string]*jsonschema.Schema{"person_id": person(), "limit": boundedIntegerSchema("Maximum runs and attempts", 1, 200), "provider": stringSchema("Configured provider profile name")}), outputSchemaFor[SweepHistory](), false),
		newOperationalDefinition("run_people_sweep", "Run bounded people maintenance using the daemon's configured provider and existing consent. Requires approval; limit must fit the configured work batch size.", OperationFamilySweep, closedObject(map[string]*jsonschema.Schema{"person_id": person(), "limit": {Type: "integer", Minimum: new(float64(1))}, "backstop": {Type: "boolean"}}), outputSchemaFor[SweepRun](), true),
	}
}
