// Package personfacts defines provider-neutral person fact contracts.
package personfacts

import (
	"context"
	"encoding/json"
	"time"
)

type TargetKind string
type ValueType string
type Cardinality string

const (
	TargetAttribute   TargetKind  = "attribute"
	TargetEmployment  TargetKind  = "employment"
	ValueText         ValueType   = "text"
	ValueInteger      ValueType   = "integer"
	ValueReal         ValueType   = "real"
	ValueBoolean      ValueType   = "boolean"
	ValueDate         ValueType   = "date"
	ValueTimestamp    ValueType   = "timestamp"
	ValueEmployment   ValueType   = "employment"
	ValueOrganization ValueType   = "organization"
	ValuePartialDate  ValueType   = "partial-date"
	CardinalitySingle Cardinality = "single"
	CardinalityMulti  Cardinality = "multi"
)

type ChoiceDescriptor struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type FieldDescriptor struct {
	Name        string      `json:"name"`
	ValueType   ValueType   `json:"value_type"`
	Cardinality Cardinality `json:"cardinality"`
	Required    bool        `json:"required"`
}

type TargetDescriptor struct {
	Kind         TargetKind         `json:"kind"`
	Key          string             `json:"key"`
	Revision     string             `json:"revision"`
	UniversalID  string             `json:"universal_id"`
	Slug         string             `json:"slug"`
	Description  string             `json:"description"`
	ValueType    ValueType          `json:"value_type"`
	Cardinality  Cardinality        `json:"cardinality"`
	RecordTarget string             `json:"record_target,omitempty"`
	MaxLength    int                `json:"max_length,omitempty"`
	Choices      []ChoiceDescriptor `json:"choices"`
	Fields       []FieldDescriptor  `json:"fields"`
	Sensitive    bool               `json:"sensitive"`
}

type Catalog struct {
	Version     string             `json:"version"`
	Fingerprint string             `json:"fingerprint"`
	Targets     []TargetDescriptor `json:"targets"`
}

type CatalogOptions struct{ IncludeSensitive bool }

type Definition struct {
	UniversalID  string
	Slug         string
	Description  string
	ValueType    ValueType
	Cardinality  Cardinality
	RecordTarget string
	MaxLength    int
	Choices      []ChoiceDescriptor
	Sensitive    bool
	Active       bool
	APIMutable   bool
	Derived      bool
}

type TargetRef struct {
	Kind     TargetKind `json:"kind"`
	Key      string     `json:"key"`
	Revision string     `json:"revision"`
}

type EvidenceFilter struct {
	Target *TargetRef
	Limit  int
	Offset int
}

type EvidenceStatusFilter struct {
	EvidenceKey string
	Supported   *bool
	Limit       int
	Offset      int
}

type ClaimFilter struct {
	Target *TargetRef
	Limit  int
	Offset int
}

type DecisionFilter struct {
	Target *TargetRef
	Limit  int
	Offset int
}

type ProjectionRef struct {
	Kind  string `json:"kind"`
	RowID int64  `json:"row_id"`
}

const MaxEvidenceExcerptRunes = 2000

type EvidenceSourceClass string
type EvidenceDirectness string
type EvidenceAuthority string
type ClaimRelation string
type ClaimOrigin string
type DecisionAction string
type DecisionReason string
type EvidenceStatusReason string

const (
	EvidenceArchive                  EvidenceSourceClass  = "archive"
	EvidencePublic                   EvidenceSourceClass  = "public"
	EvidenceSystem                   EvidenceSourceClass  = "system"
	EvidenceProviderAssertion        EvidenceSourceClass  = "provider_assertion"
	DirectSelf                       EvidenceDirectness   = "direct-self"
	DirectOther                      EvidenceDirectness   = "direct-other"
	Indirect                         EvidenceDirectness   = "indirect"
	AuthorityAuthoritative           EvidenceAuthority    = "authoritative"
	AuthorityOrdinary                EvidenceAuthority    = "ordinary"
	AuthorityAggregator              EvidenceAuthority    = "aggregator"
	RelationSupport                  ClaimRelation        = "support"
	RelationContradict               ClaimRelation        = "contradict"
	RelationSupersede                ClaimRelation        = "supersede"
	RelationInvalid                  ClaimRelation        = "invalid"
	OriginExtraction                 ClaimOrigin          = "extraction"
	OriginEnrichment                 ClaimOrigin          = "enrichment"
	OriginSystem                     ClaimOrigin          = "system"
	OriginInvalid                    ClaimOrigin          = "invalid"
	DecisionApplied                  DecisionAction       = "applied"
	DecisionRetained                 DecisionAction       = "retained"
	DecisionSuperseded               DecisionAction       = "superseded"
	DecisionInvalid                  DecisionAction       = "invalid"
	DecisionIdentityRejected         DecisionAction       = "identity-rejected"
	DecisionPolicyRejected           DecisionAction       = "policy-rejected"
	DecisionConflictRejected         DecisionAction       = "conflict-rejected"
	DecisionAmbiguousRetained        DecisionAction       = "ambiguous-retained"
	ReasonMalformedValue             DecisionReason       = "malformed-value"
	ReasonUnsupportedTarget          DecisionReason       = "unsupported-target"
	ReasonStaleTargetRevision        DecisionReason       = "stale-target-revision"
	ReasonUnalignedEvidence          DecisionReason       = "unaligned-evidence"
	ReasonIdentityMismatch           DecisionReason       = "identity-mismatch"
	ReasonSensitivePolicy            DecisionReason       = "sensitive-policy"
	ReasonPinRetained                DecisionReason       = "pin-retained"
	ReasonBelowThreshold             DecisionReason       = "below-threshold"
	ReasonInsufficientMargin         DecisionReason       = "insufficient-margin"
	ReasonCompetingTie               DecisionReason       = "competing-tie"
	ReasonExplicitContradiction      DecisionReason       = "explicit-contradiction"
	ReasonExplicitSupersession       DecisionReason       = "explicit-supersession"
	ReasonOrganizationAmbiguous      DecisionReason       = "organization-ambiguous"
	ReasonAppliedProjection          DecisionReason       = "applied-projection"
	ReasonEvidenceUnsupported        DecisionReason       = "evidence-unsupported"
	ReasonOutsideValidity            DecisionReason       = "outside-validity"
	EvidenceStatusSourceDeleted      EvidenceStatusReason = "source-deleted"
	EvidenceStatusSourceEdited       EvidenceStatusReason = "source-edited"
	EvidenceStatusScopeUnlinked      EvidenceStatusReason = "scope-unlinked"
	EvidenceStatusIdentityReassigned EvidenceStatusReason = "identity-reassigned"
	EvidenceStatusSourceReimported   EvidenceStatusReason = "source-reimported"
	EvidenceStatusScopeRelinked      EvidenceStatusReason = "scope-relinked"
)

type EvidenceInput struct {
	PersonID        int64               `json:"PersonID"`
	SourceClass     EvidenceSourceClass `json:"SourceClass"`
	Directness      EvidenceDirectness  `json:"Directness"`
	Authority       EvidenceAuthority   `json:"Authority"`
	SourceRef       string              `json:"SourceRef"`
	SourceURL       string              `json:"SourceURL"`
	SubjectPersonID *int64              `json:"SubjectPersonID"`
	SubjectRef      string              `json:"SubjectRef"`
	SpanStart       *int64              `json:"SpanStart"`
	SpanEnd         *int64              `json:"SpanEnd"`
	Excerpt         string              `json:"Excerpt"`
	ContentSHA256   string              `json:"ContentSHA256"`
	SourceVersion   string              `json:"SourceVersion"`
	EventTime       time.Time           `json:"EventTime"`
	RecordedTime    time.Time           `json:"RecordedTime"`
	IdentityScore   int                 `json:"IdentityScore"`
}

type OrganizationReference struct {
	ID     *int64 `json:"id,omitempty"`
	Name   string `json:"name"`
	Domain string `json:"domain,omitempty"`
}

type PartialDateValue struct {
	Year  int `json:"year"`
	Month int `json:"month,omitempty"`
	Day   int `json:"day,omitempty"`
}

type EmploymentValue struct {
	Organization OrganizationReference `json:"organization"`
	Title        string                `json:"title,omitempty"`
	Role         string                `json:"role,omitempty"`
	Department   string                `json:"department,omitempty"`
	Location     string                `json:"location,omitempty"`
	StartDate    *PartialDateValue     `json:"start_date,omitempty"`
	EndDate      *PartialDateValue     `json:"end_date,omitempty"`
}

type ConfidenceInputs struct {
	ReportedScore int `json:"reported_score"`
}

type PolicyContext struct {
	AllowSensitive            bool   `json:"allow_sensitive"`
	ProviderPolicyFingerprint string `json:"provider_policy_fingerprint"`
}

type ProposedClaim struct {
	Target         TargetDescriptor `json:"Target"`
	Relation       ClaimRelation    `json:"Relation"`
	SubmittedValue json.RawMessage  `json:"SubmittedValue"`
	Evidence       []EvidenceInput  `json:"Evidence"`
	ValidFrom      *time.Time       `json:"ValidFrom"`
	ValidUntil     *time.Time       `json:"ValidUntil"`
	Origin         ClaimOrigin      `json:"Origin"`
	Confidence     ConfidenceInputs `json:"Confidence"`
}

type SourceCursor struct {
	Lane  string `json:"lane"`
	Start string `json:"start"`
	End   string `json:"end"`
}

type GenerationInput struct {
	PersonID              int64
	SourceCursors         []SourceCursor
	ProgramID             string
	ProgramVersion        string
	ProgramFingerprint    string
	CatalogFingerprint    string
	Provider              string
	ProviderVersion       string
	Model                 string
	ModelVersion          string
	ResolvedAt            time.Time
	Policy                PolicyContext
	Claims                []ProposedClaim
	EvidenceStatusChanges []EvidenceStatusChange
}

type EvidenceStatusChange struct {
	EvidenceKey   string               `json:"evidence_key"`
	SourceVersion string               `json:"source_version"`
	Supported     bool                 `json:"supported"`
	Reason        EvidenceStatusReason `json:"reason"`
}

type NormalizedValue struct {
	JSON        json.RawMessage `json:"JSON"`
	Fingerprint string          `json:"Fingerprint"`
}

type ValidationFailure struct {
	Action DecisionAction `json:"Action"`
	Reason DecisionReason `json:"Reason"`
	Detail string         `json:"Detail"`
}

type AlignmentResult struct {
	Accepted      bool
	SourceVersion string
	ContentSHA256 string
	Failure       *ValidationFailure
}

type EvidenceAligner interface {
	Align(ctx context.Context, input EvidenceInput) (AlignmentResult, error)
}

type PreparedClaim struct {
	Target                        TargetDescriptor
	Relation                      ClaimRelation
	SubmittedValue                json.RawMessage
	SubmittedFingerprint          string
	SubmittedEvidenceFingerprints []string
	Normalized                    *NormalizedValue
	Evidence                      []EvidenceInput
	EvidenceKeys                  []string
	ValidFrom                     *time.Time
	ValidUntil                    *time.Time
	Origin                        ClaimOrigin
	Confidence                    ConfidenceInputs
	Failure                       *ValidationFailure
}

type PreparedEvidenceStatusChange struct {
	EvidenceKey   string
	SourceVersion string
	Supported     bool
	Reason        EvidenceStatusReason
}

type PreparedGeneration struct {
	canonicalJSON         []byte
	generationKey         string
	programFingerprint    string
	input                 GenerationInput
	claims                []PreparedClaim
	evidenceStatusChanges []PreparedEvidenceStatusChange
}

type Evidence struct {
	ID           int64                `json:"ID"`
	PersonID     int64                `json:"PersonID"`
	Key          string               `json:"Key"`
	CreatedAt    time.Time            `json:"CreatedAt"`
	Input        EvidenceInput        `json:"Input"`
	Supported    bool                 `json:"Supported"`
	LatestStatus *EvidenceStatusEvent `json:"LatestStatus"`
}

type EvidenceStatusEvent struct {
	ID            int64                `json:"ID"`
	PersonID      int64                `json:"PersonID"`
	GenerationID  int64                `json:"GenerationID"`
	EvidenceID    int64                `json:"EvidenceID"`
	EvidenceKey   string               `json:"EvidenceKey"`
	SourceVersion string               `json:"SourceVersion"`
	Supported     bool                 `json:"Supported"`
	Reason        EvidenceStatusReason `json:"Reason"`
	CreatedAt     time.Time            `json:"CreatedAt"`
}

type Generation struct {
	ID                 int64
	PersonID           int64
	GenerationKey      string
	SourceCursors      []SourceCursor
	ProgramID          string
	ProgramVersion     string
	ProgramFingerprint string
	CatalogFingerprint string
	Provider           string
	ProviderVersion    string
	Model              string
	ModelVersion       string
	Policy             PolicyContext
	ResolvedAt         time.Time
	CreatedAt          time.Time
}

type Claim struct {
	ID             int64
	PersonID       int64
	GenerationID   int64
	ClaimKey       string
	Generation     Generation
	Target         TargetRef
	Relation       ClaimRelation
	SubmittedValue json.RawMessage
	Normalized     *NormalizedValue
	EvidenceIDs    []int64
	ValidFrom      *time.Time
	ValidUntil     *time.Time
	Origin         ClaimOrigin
	Confidence     ConfidenceInputs
	Failure        *ValidationFailure
	CreatedAt      time.Time
}

// CurrentProjection describes one value visible to fact resolution.
type CurrentProjection struct {
	Ref             ProjectionRef   `json:"Ref"`
	Normalized      NormalizedValue `json:"Normalized"`
	ActiveFrom      time.Time       `json:"ActiveFrom"`
	ActiveUntil     *time.Time      `json:"ActiveUntil"`
	TransactionTime time.Time       `json:"TransactionTime"`
	Declared        bool            `json:"Declared"`
}

type ResolvedClaim struct {
	ClaimKey   string             `json:"ClaimKey"`
	Claim      ProposedClaim      `json:"Claim"`
	Normalized *NormalizedValue   `json:"Normalized"`
	Evidence   []Evidence         `json:"Evidence"`
	Failure    *ValidationFailure `json:"Failure"`
}

type PinState struct {
	Target  TargetRef `json:"target"`
	Pinned  bool      `json:"pinned"`
	Actor   string    `json:"actor,omitempty"`
	EventID *int64    `json:"event_id,omitempty"`
}

type ResolutionInput struct {
	PersonID                     int64               `json:"PersonID"`
	Target                       TargetDescriptor    `json:"Target"`
	ResolvedAt                   time.Time           `json:"ResolvedAt"`
	Policy                       PolicyContext       `json:"Policy"`
	ProjectionContextFingerprint string              `json:"ProjectionContextFingerprint"`
	Current                      []CurrentProjection `json:"Current"`
	Claims                       []ResolvedClaim     `json:"Claims"`
	Pin                          PinState            `json:"Pin"`
}

type ScoreBreakdown struct {
	SourceClass   int `json:"source_class"`
	Directness    int `json:"directness"`
	Authority     int `json:"authority"`
	Confidence    int `json:"confidence"`
	Freshness     int `json:"freshness"`
	Corroboration int `json:"corroboration"`
	Total         int `json:"total"`
}

type ProjectionOperation string

const (
	ProjectionSet    ProjectionOperation = "set"
	ProjectionRetire ProjectionOperation = "retire"
)

type ProjectionPlan struct {
	Operation  ProjectionOperation `json:"operation"`
	Target     TargetRef           `json:"target"`
	ClaimKey   string              `json:"claim_key"`
	CurrentRef *ProjectionRef      `json:"current_ref"`
	ActiveFrom time.Time           `json:"active_from"`
}

type Decision struct {
	ID                int64          `json:"id,omitempty"`
	PersonID          int64          `json:"person_id,omitempty"`
	ResolutionID      int64          `json:"resolution_id,omitempty"`
	DecisionKey       string         `json:"decision_key,omitempty"`
	ClaimKey          string         `json:"claim_key"`
	Action            DecisionAction `json:"action"`
	Reason            DecisionReason `json:"reason"`
	Score             ScoreBreakdown `json:"score"`
	CompetingClaimKey string         `json:"competing_claim_key,omitempty"`
	Projection        *ProjectionRef `json:"projection,omitempty"`
	CreatedAt         time.Time      `json:"created_at"`
}

type Resolution struct {
	ID               int64            `json:"id"`
	Target           TargetRef        `json:"target"`
	ResolverVersion  string           `json:"resolver_version"`
	InputFingerprint string           `json:"input_fingerprint"`
	ResolvedAt       time.Time        `json:"resolved_at"`
	Decisions        []Decision       `json:"decisions"`
	Projections      []ProjectionPlan `json:"projections"`
}

type ResolutionResult struct {
	ID               int64           `json:"id"`
	Target           TargetRef       `json:"target"`
	ResolverVersion  string          `json:"resolver_version"`
	InputFingerprint string          `json:"input_fingerprint"`
	ResolvedAt       time.Time       `json:"resolved_at"`
	Decisions        []Decision      `json:"decisions"`
	Projections      []ProjectionRef `json:"projections"`
}

// PinWrite describes the effective pin and any atomic re-resolution it caused.
type PinWrite struct {
	State       PinState           `json:"state"`
	Resolutions []ResolutionResult `json:"resolutions"`
	Projections []ProjectionRef    `json:"projections"`
}

// GenerationResult is the complete durable result of one generation envelope.
type GenerationResult struct {
	GenerationID         int64                 `json:"generation_id"`
	GenerationKey        string                `json:"generation_key"`
	EvidenceStatusEvents []EvidenceStatusEvent `json:"evidence_status_events"`
	Resolutions          []ResolutionResult    `json:"resolutions"`
	Decisions            []Decision            `json:"decisions"`
	Projections          []ProjectionRef       `json:"projections"`
}

type Policy struct {
	Version               string                      `json:"Version"`
	ApplyThreshold        int                         `json:"ApplyThreshold"`
	ReplacementMargin     int                         `json:"ReplacementMargin"`
	HysteresisMargin      int                         `json:"HysteresisMargin"`
	MinimumIdentityScore  int                         `json:"MinimumIdentityScore"`
	MaxCorroborationBonus int                         `json:"MaxCorroborationBonus"`
	SourceClassWeights    map[EvidenceSourceClass]int `json:"SourceClassWeights"`
	DirectnessWeights     map[EvidenceDirectness]int  `json:"DirectnessWeights"`
	AuthorityWeights      map[EvidenceAuthority]int   `json:"AuthorityWeights"`
}

type Resolver struct {
	Policy Policy
}
