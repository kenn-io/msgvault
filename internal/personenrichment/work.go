package personenrichment

import (
	"context"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// ConsentChecker is the exact-purpose authorization boundary used before
// person-enrichment egress. Inference or embedding consent cannot implement
// this method accidentally because each purpose has a distinct method name.
type ConsentChecker interface {
	HasActivePersonEnrichmentConsent(ctx context.Context, fingerprint string) (bool, error)
}

// SuppressionLookup is one provider- and normalization-scoped keyed digest.
// It deliberately contains neither a raw nor a normalized identifier.
type SuppressionLookup = SuppressionDigest

// SuppressionChecker is the durable privacy boundary used before enrichment
// egress. Callers first compare configured key IDs with all durable key IDs,
// then query every exact digest they may disclose.
type SuppressionChecker interface {
	HasPersonEnrichmentSuppressionContext(
		ctx context.Context,
		lookup SuppressionLookup,
	) (bool, error)
	ListPersonEnrichmentSuppressionKeyIDsContext(ctx context.Context) ([]string, error)
}

// WorkStore is the durable boundary used by the enrichment worker. Durable
// storage owns every run, lease, active-attempt binding, provider job, retry,
// and pre-egress budget reservation.
type WorkStore interface {
	StartRun(ctx context.Context, start RunStart) (*DurableRun, bool, error)
	ListRunningRuns(ctx context.Context, filter RunningRunFilter) ([]DurableRun, error)
	CompleteRun(ctx context.Context, runID int64, completion RunCompletion) error
	ClaimWork(ctx context.Context, options ClaimOptions) (*WorkLease, error)
	LoadProviderProfile(ctx context.Context, fingerprint string) (ProviderProfile, error)
	LoadRequestInput(ctx context.Context, lease WorkLease) (RequestInput, error)
	LoadProviderPersonIDs(ctx context.Context, personID int64, providerNamespace string) ([]string, error)
	RenewLease(ctx context.Context, token LeaseToken, until time.Time) error
	ReleaseWork(ctx context.Context, token LeaseToken, release WorkRelease) error
	BeginAttempt(ctx context.Context, token LeaseToken, start AttemptStart) (*DurableAttempt, bool, error)
	AuthorizeAttemptDispatch(ctx context.Context, token LeaseToken) error
	RecordProviderStarted(ctx context.Context, token LeaseToken, attempt Attempt) error
	SchedulePoll(ctx context.Context, token LeaseToken, result Result) error
	ScheduleRetry(ctx context.Context, token LeaseToken, retry RetryUpdate) error
	MarkUncertainStart(ctx context.Context, token LeaseToken, failure SafeFailure) error
	MarkTerminal(ctx context.Context, token LeaseToken, failure SafeFailure) error
}

type ClaimOptions struct {
	RunID         int64
	Owner         string
	ProviderName  string
	Now           time.Time
	LeaseDuration time.Duration
}

type LeaseToken struct {
	RunID              int64
	WorkPersonID       int64
	ProfileFingerprint string
	AttemptID          int64
	Owner              string
	Fence              int64
}

type WorkLease struct {
	Token              LeaseToken
	ProviderName       string
	ProfileFingerprint string
	PersonID           int64
	RunID              int64
	ActiveAttempt      *DurableAttempt
	Trigger            Trigger
	LeaseUntil         time.Time
}

type AttemptStart struct {
	RunID                int64
	PersonID             int64
	ProfileFingerprint   string
	PayloadHash          string
	RequestHash          string
	PersonRevision       int64
	Trigger              Trigger
	HardCostCap          bool
	GuaranteedMaxCost    Cost
	DisclosedIdentifiers []SuppressionDigest
}

type DurableAttempt struct {
	ID                  int64
	RunID               int64
	Token               LeaseToken
	State               string
	PayloadHash         string
	RequestHash         string
	PersonRevision      int64
	Trigger             Trigger
	RequestID           string
	JobID               string
	AdapterVersion      string
	SchemaVersion       string
	GeneratedSchemaHash string
	ProgramFingerprint  string
	GeneratedSchema     bool
	Targets             []personfacts.TargetDescriptor
	NextActionAt        time.Time
	StartedAt           time.Time
	AttemptCount        int64
	HardCostCap         bool
	ReservedCostMicros  int64
}

type RunStart struct {
	Kind        string
	RequestedBy string
	RequestedAt time.Time
}

type DurableRun struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"`
	RequestedBy string    `json:"requested_by"`
	State       string    `json:"state"`
	RequestedAt time.Time `json:"requested_at"`
}

type RunCompletion struct {
	State       string
	Failure     *SafeFailure
	CompletedAt time.Time
}

type RunningRunFilter struct {
	AfterRequestedAt time.Time
	AfterID          int64
	Limit            int
}

type SafeFailure struct {
	Class             FailureClass
	HTTPStatus        int
	ProviderRequestID string
	Message           string
}

type RetryUpdate struct {
	Failure      SafeFailure
	NextActionAt time.Time
}

type WorkRelease struct {
	Outcome        string
	Failure        *SafeFailure
	NextActionAt   *time.Time
	PersonRevision int64
	PayloadHash    string
	RequestHash    string
}
