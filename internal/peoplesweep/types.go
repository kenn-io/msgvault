package peoplesweep

import (
	"errors"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personscope"
)

// ChangeKind classifies why a tracked person's archive scope became dirty.
type ChangeKind string

// EvidenceChangeEffect is the closed lifecycle effect known by the mutation
// publisher while both sides of a source or identity change are available.
type EvidenceChangeEffect string

const (
	ChangeUpsert      ChangeKind = "upsert"
	ChangeDelete      ChangeKind = "delete"
	ChangeScope       ChangeKind = "scope"
	ChangeTracking    ChangeKind = "tracking"
	ChangePublication ChangeKind = "publication"

	EvidenceEffectNone               EvidenceChangeEffect = ""
	EvidenceEffectSourceDeleted      EvidenceChangeEffect = "source-deleted"
	EvidenceEffectSourceEdited       EvidenceChangeEffect = "source-edited"
	EvidenceEffectScopeUnlinked      EvidenceChangeEffect = "scope-unlinked"
	EvidenceEffectIdentityReassigned EvidenceChangeEffect = "identity-reassigned"
	EvidenceEffectSourceReimported   EvidenceChangeEffect = "source-reimported"
	EvidenceEffectScopeRelinked      EvidenceChangeEffect = "scope-relinked"
)

// ArchiveChange is one immutable person-linked archive mutation. It carries
// only durable coordinates and classifications; archive text never enters the
// journal.
type ArchiveChange struct {
	Sequence       int64
	PersonID       int64
	SourceLane     SourceClass
	Kind           ChangeKind
	EvidenceEffect EvidenceChangeEffect
	SourceID       int64
	MessageID      int64
	AttachmentID   int64
	OccurrenceKey  string
	RecordedAt     time.Time
}

// CursorKey identifies one independent source/program/catalog progress lane
// for a durable person.
type CursorKey struct {
	PersonID           int64       `json:"person_id"`
	SourceLane         SourceClass `json:"source_lane"`
	ProgramFingerprint string      `json:"program_fingerprint"`
	CatalogFingerprint string      `json:"catalog_fingerprint"`
}

// Cursor is the durable progress for one fingerprinted source lane.
type Cursor struct {
	Key                    CursorKey
	OptimisticSequence     int64
	OptimisticDocumentKey  string
	ReconcileUpperKey      string
	ReconcileAfterKey      string
	ReconcileDocumentKey   string
	ReconciliationComplete bool
	BackstopUpperKey       string
	BackstopAfterKey       string
	BackstopDocumentKey    string
	LastBackstopAt         *time.Time
}

// GenerationCursorMode identifies which independent cursor a generation
// mutation advances.
type GenerationCursorMode string

const (
	GenerationCursorOptimistic     GenerationCursorMode = "optimistic"
	GenerationCursorReconciliation GenerationCursorMode = "reconciliation"
	GenerationCursorBackstop       GenerationCursorMode = "backstop"
)

// GenerationCursor is the compare-and-set input for one leased sweep pass.
type GenerationCursor struct {
	Key              CursorKey            `json:"key"`
	Mode             GenerationCursorMode `json:"mode"`
	CursorFrom       int64                `json:"cursor_from"`
	CursorThrough    int64                `json:"cursor_through"`
	ReconcileFromKey string               `json:"reconcile_from_key"`
	ReconcileToKey   string               `json:"reconcile_to_key"`
	DocumentFromKey  string               `json:"document_from_key"`
	DocumentToKey    string               `json:"document_to_key"`
	BackstopUpperKey string               `json:"backstop_upper_key"`
}

// GapRequest bounds one ascending tracked-person reconciliation pass.
type GapRequest struct {
	ProgramFingerprint string
	CatalogFingerprint string
	SourceLanes        []SourceClass
	AfterPersonID      int64
	Limit              int
	Now                time.Time
	BackstopInterval   time.Duration
	ForceBackstop      bool
}

// GapResult reports the durable work produced by one bounded pass.
type GapResult struct {
	PeopleScanned int
	WorkCreated   int
	NextPersonID  int64
}

// ClaimRequest identifies a worker and its requested lease duration.
// AvailableAt is retained for orchestration metadata; retry eligibility is
// always evaluated against the database clock.
type ClaimRequest struct {
	WorkerID      string
	LeaseDuration time.Duration
	AvailableAt   time.Time
	PersonID      int64
}

// Lease is the fenced ownership token required by sweep mutations.
type Lease struct {
	PersonID     int64
	WorkerID     string
	Fence        int64
	ExpiresAt    time.Time
	AttemptCount int
}

// WorkFailure records a retry decision without inference usage accounting.
type WorkFailure struct {
	Lease     Lease
	AttemptID string
	Class     FailureClass
	RetryAt   time.Time
}

// FailureClass is the closed reason a leased person sweep did not complete.
type FailureClass string

const (
	FailurePolicy        FailureClass = "policy"
	FailureBudget        FailureClass = "budget"
	FailureLeaseLost     FailureClass = "lease_lost"
	FailureRateLimited   FailureClass = "rate_limited"
	FailureTimeout       FailureClass = "timeout"
	FailureProviderHTTP  FailureClass = "provider_http"
	FailureInvalidOutput FailureClass = "invalid_output"
	FailureArchiveGap    FailureClass = "archive_gap"
	FailureInternal      FailureClass = "internal"
)

var ErrLeaseLost = errors.New("person sweep lease fence is no longer current")

type Usage struct {
	Requests              int
	InputTokens           int64
	OutputTokens          int64
	EstimatedCostMicroUSD int64
}

var (
	ErrBudgetExceeded = errors.New("person sweep budget exceeded")
	ErrBudgetOverflow = errors.New("person sweep budget calculation overflow")
)

type RunMode string

const (
	RunIncremental RunMode = "incremental"
	RunBackstop    RunMode = "backstop"
)

type RunKind string

const (
	RunScheduled RunKind = "scheduled"
	RunManual    RunKind = "manual"
)

type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "succeeded"
	RunPartial   RunStatus = "partial"
	RunFailed    RunStatus = "failed"
)

type AttemptStatus string

const (
	AttemptRunning   AttemptStatus = "running"
	AttemptSucceeded AttemptStatus = "succeeded"
	AttemptFailed    AttemptStatus = "failed"
	AttemptCancelled AttemptStatus = "cancelled"
)

type StartRun struct {
	ID                  string
	Kind                RunKind
	Mode                RunMode
	ProgramFingerprint  string
	CatalogFingerprint  string
	ProviderFingerprint string
	StartedAt           time.Time
}

type Run struct {
	StartRun

	Status      RunStatus
	CompletedAt *time.Time
}

type StartAttempt struct {
	ID             string
	RunID          string
	PersonID       int64
	LeaseFence     int64
	Mode           RunMode
	CursorEnvelope []GenerationCursor
	EnvelopeHash   string
	StartedAt      time.Time
}

const (
	ProviderCallPurposePrimary = "primary"
	ProviderCallPurposeRepair  = "repair"
)

type ProviderCallCoordinate struct {
	BatchOrdinal int    `json:"batch_ordinal"`
	CallOrdinal  int    `json:"call_ordinal"`
	Purpose      string `json:"purpose"`
}

type BudgetReservationRequest struct {
	RunID                 string
	AttemptID             string
	BatchOrdinal          int
	CallOrdinal           int
	Purpose               string
	PersonID              int64
	ProviderFingerprint   string
	UTCDate               string
	InputHash             string
	ItemCount             int
	EstimatedRequests     int
	EstimatedInputTokens  int64
	EstimatedOutputTokens int64
	EstimatedCostMicroUSD int64
	Budget                BudgetConfig
}

type BudgetReservation struct {
	ID       string
	Request  BudgetReservationRequest
	Released bool
}

type CompletedUsage struct {
	BatchOrdinal      int
	CallOrdinal       int
	Purpose           string
	ProviderRequestID string
	Usage             TokenUsage
	UsageKnown        bool
	Latency           time.Duration
}

type FailureFinalization struct {
	Lease        Lease
	AttemptID    string
	Class        FailureClass
	RetryAt      time.Time
	Reservations []BudgetReservation
	Completed    []CompletedUsage
	FinalizedAt  time.Time
}

type RunFilter struct {
	PersonID            int64
	ProviderFingerprint string
	Limit               int
}

type AttemptFilter struct {
	RunID               string
	PersonID            int64
	ProviderFingerprint string
	Limit               int
}

type RunSummary struct {
	ID                  string
	Kind                RunKind
	Mode                RunMode
	Status              RunStatus
	ProgramFingerprint  string
	CatalogFingerprint  string
	ProviderFingerprint string
	Attempts            int
	Successes           int
	Failures            int
	ProjectedWrites     int
	Usage               Usage
	StartedAt           time.Time
	CompletedAt         *time.Time
}

type AttemptSummary struct {
	ID                  string
	RunID               string
	PersonID            int64
	Status              AttemptStatus
	FailureClass        FailureClass
	CursorEnvelope      []GenerationCursor
	EnvelopeHash        string
	ProgramFingerprint  string
	CatalogFingerprint  string
	ProviderFingerprint string
	GenerationID        *int64
	GenerationKey       string
	SeedCount           int
	ContextCount        int
	ClaimCount          int
	DecisionCount       int
	ProjectedWrites     int
	ProviderRequestID   string
	Usage               Usage
	Latency             time.Duration
}

// OperationalStatus is the safe, aggregate state exposed to operators. It
// intentionally excludes evidence, request bodies, credentials, and retry
// headers.
type OperationalStatus struct {
	DirtyCount       int
	LeasedCount      int
	RetryCount       int
	OldestDirtyAt    *time.Time
	JournalHighWater int64
	CursorHighWater  int64
	LastFailure      FailureClass
}

// EvidenceRef is a durable, versioned coordinate into an archive source.
type EvidenceRef struct {
	SourceLane      SourceClass `json:"source_lane"`
	SourceID        int64       `json:"source_id"`
	MessageID       int64       `json:"message_id"`
	AttachmentID    int64       `json:"attachment_id"`
	SourceMessageID string      `json:"source_message_id"`
	OccurrenceKey   string      `json:"occurrence_key"`
	ChunkKey        string      `json:"chunk_key"`
	SpanStart       int         `json:"span_start"`
	SpanEnd         int         `json:"span_end"`
}

type TextSpan struct {
	Start int
	End   int
}

type EvidenceItem struct {
	Ref                 EvidenceRef
	EvidenceKey         string
	PersonID            int64
	SubjectPersonID     *int64
	SourceClass         SourceClass
	SourceVersion       string
	ContentSHA256       string
	EventTime           time.Time
	RecordedTime        time.Time
	Excerpt             string
	Highlight           TextSpan
	Provenance          personscope.Provenance
	IdentityBasisPoints int
	Directness          personfacts.EvidenceDirectness
	Authority           personfacts.EvidenceAuthority
	Tombstone           bool
}

type WindowRequest struct {
	PersonID        int64
	Lane            SourceClass
	Mode            GenerationCursorMode
	AfterSequence   int64
	ThroughSequence int64
	ReconcileAfter  string
	ReconcileUpper  string
	BackstopUpper   string
	DocumentAfter   string
	Limit           int
}

type PersonWindow struct {
	Changes            []ArchiveChange
	Seeds              []EvidenceItem
	NextSequence       int64
	NextReconcileKey   string
	NextDocumentKey    string
	CapturedUpperKey   string
	ReconciliationDone bool
}

type DocumentContextRequest struct {
	PersonID            int64
	CandidateMessageIDs []int64
	Query               string
	After               *time.Time
	Before              *time.Time
	Limit               int
}

type ContextRequest struct {
	PersonID                 int64
	Target                   personfacts.TargetDescriptor
	CandidateMessageIDs      []int64
	SourceClasses            []SourceClass
	SourceSince              string
	SourceUntil              string
	HistoricalCandidateLimit int
	Limit                    int
}

type HistoricalCandidateRequest struct {
	PersonID      int64
	SourceClasses []SourceClass
	SourceSince   string
	SourceUntil   string
	Limit         int
}

var ErrSourceTextUnavailable = errors.New("person sweep source has no durable text")
