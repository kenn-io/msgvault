package peoplesweep

import "time"

// RunOutput is the redacted wire contract of the owning sweep CLI.
// Consumers do not receive provider diagnostics, paths, excerpts or credentials.
type RunOutput struct {
	RunID           string      `json:"run_id"`
	PeopleAttempted int         `json:"people_attempted"`
	PeopleSucceeded int         `json:"people_succeeded"`
	ProjectedWrites int         `json:"projected_writes"`
	Usage           UsageOutput `json:"usage"`
}

type UsageOutput struct {
	Requests              int   `json:"requests"`
	InputTokens           int64 `json:"input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	EstimatedCostMicroUSD int64 `json:"estimated_cost_microusd"`
}

type StatusOutput struct {
	Enabled             bool             `json:"enabled"`
	WorkBatchSize       int              `json:"work_batch_size"`
	Budgets             BudgetDisclosure `json:"budgets"`
	Schedule            string           `json:"schedule"`
	DirtyCount          int              `json:"dirty_count"`
	LeasedCount         int              `json:"leased_count"`
	RetryCount          int              `json:"retry_count"`
	OldestDirtyAt       *time.Time       `json:"oldest_dirty_at,omitempty"`
	JournalHighWater    int64            `json:"journal_high_water"`
	CursorHighWater     int64            `json:"cursor_high_water"`
	ProgramFingerprint  string           `json:"program_fingerprint,omitempty"`
	CatalogFingerprint  string           `json:"catalog_fingerprint,omitempty"`
	ProviderFingerprint string           `json:"provider_fingerprint,omitempty"`
	LastFailure         FailureClass     `json:"last_failure,omitempty"`
}

// BudgetDisclosure names the safe caps and prices without changing the
// persisted encoding or fingerprints of BudgetConfig.
type BudgetDisclosure struct {
	MaxRequestsPerPerson               int   `json:"max_requests_per_person"`
	MaxInputTokensPerPerson            int64 `json:"max_input_tokens_per_person"`
	MaxOutputTokensPerPerson           int64 `json:"max_output_tokens_per_person"`
	MaxRequestsPerRun                  int   `json:"max_requests_per_run"`
	MaxInputTokensPerRun               int64 `json:"max_input_tokens_per_run"`
	MaxOutputTokensPerRun              int64 `json:"max_output_tokens_per_run"`
	MaxEstimatedCostMicroUSDPerRun     int64 `json:"max_estimated_cost_microusd_per_run"`
	MaxRequestsPerDay                  int   `json:"max_requests_per_day"`
	MaxInputTokensPerDay               int64 `json:"max_input_tokens_per_day"`
	MaxOutputTokensPerDay              int64 `json:"max_output_tokens_per_day"`
	MaxEstimatedCostMicroUSDPerDay     int64 `json:"max_estimated_cost_microusd_per_day"`
	InputCostMicroUSDPerMillionTokens  int64 `json:"input_cost_microusd_per_million_tokens"`
	OutputCostMicroUSDPerMillionTokens int64 `json:"output_cost_microusd_per_million_tokens"`
}

// DiscloseBudget projects all configured enforcement caps and token prices.
func DiscloseBudget(b BudgetConfig) BudgetDisclosure {
	return BudgetDisclosure(b)
}

type HistoryRunOutput struct {
	Kind            RunKind     `json:"kind"`
	Mode            RunMode     `json:"mode"`
	Status          RunStatus   `json:"status"`
	Attempts        int         `json:"attempts"`
	Successes       int         `json:"successes"`
	Failures        int         `json:"failures"`
	ProjectedWrites int         `json:"projected_writes"`
	Usage           UsageOutput `json:"usage"`
}

type HistoryAttemptOutput struct {
	PersonID        int64         `json:"person_id"`
	Status          AttemptStatus `json:"status"`
	FailureClass    FailureClass  `json:"failure_class,omitempty"`
	SeedCount       int           `json:"seed_count"`
	ContextCount    int           `json:"context_count"`
	ClaimCount      int           `json:"claim_count"`
	DecisionCount   int           `json:"decision_count"`
	ProjectedWrites int           `json:"projected_writes"`
	Usage           UsageOutput   `json:"usage"`
	LatencyMS       int64         `json:"latency_ms"`
}

type HistoryOutput struct {
	Runs     []HistoryRunOutput     `json:"runs"`
	Attempts []HistoryAttemptOutput `json:"attempts"`
}
