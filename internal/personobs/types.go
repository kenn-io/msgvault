// Package personobs defines provider-neutral person observation contracts.
package personobs

import (
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

const NormalizationVersion = "obs-norm-v1"

type Ref = string
type Level string

const (
	LevelExplicit      Level = "explicit"
	LevelDeductive     Level = "deductive"
	LevelInductive     Level = "inductive"
	LevelContradiction Level = "contradiction"
)

type Origin string

const (
	OriginExtraction Origin = "extraction"
	OriginOwner      Origin = "owner"
	OriginReasoning  Origin = "reasoning"
)

type Kind string

const (
	KindState        Kind = "state"
	KindEvent        Kind = "event"
	KindPlan         Kind = "plan"
	KindPreference   Kind = "preference"
	KindRelationship Kind = "relationship"
	KindPattern      Kind = "pattern"
	KindOther        Kind = "other"
)

type Status string

const (
	StatusActive      Status = "active"
	StatusUnsupported Status = "unsupported"
	StatusSuperseded  Status = "superseded"
	StatusRejected    Status = "rejected"
)

type StatusReason string

const (
	StatusReasonNone                 StatusReason = ""
	StatusReasonEvidenceUnsupported  StatusReason = "evidence-unsupported"
	StatusReasonPremisesWithdrawn    StatusReason = "premises-withdrawn"
	StatusReasonDecayed              StatusReason = "decayed"
	StatusReasonKnowledgeUpdate      StatusReason = "knowledge-update"
	StatusReasonDuplicateMerge       StatusReason = "duplicate-merge"
	StatusReasonPersonMergeCollision StatusReason = "person-merge-collision"
	StatusReasonOwnerRejected        StatusReason = "owner-rejected"
	StatusReasonOwnerSuperseded      StatusReason = "owner-superseded"
)

type OwnerState string

const (
	OwnerStateNone      OwnerState = ""
	OwnerStateConfirmed OwnerState = "confirmed"
	OwnerStateUnsure    OwnerState = "unsure"
)

type SupportFlag string

const (
	SupportFlagNone      SupportFlag = ""
	SupportFlagLapsed    SupportFlag = "support_lapsed"
	SupportFlagContested SupportFlag = "contested"
)

type Attribution string

const (
	AttributionSelf     Attribution = "self"
	AttributionAssent   Attribution = "assent"
	AttributionReported Attribution = "reported"
)

type PremiseRole string

const (
	PremiseRoleSupport PremiseRole = "support"
	PremiseRoleCounter PremiseRole = "counter"
)

type Stability string

const (
	StabilityFixed    Stability = "fixed"
	StabilityDurable  Stability = "durable"
	StabilityVolatile Stability = "volatile"
	StabilityEpisodic Stability = "episodic"
)

type StabilitySource string

const (
	StabilitySourceProgram    StabilitySource = "program"
	StabilitySourceDefinition StabilitySource = "definition"
	StabilitySourceOwner      StabilitySource = "owner"
)

type ExclusionReason string

const (
	ExclusionReasonNone       ExclusionReason = ""
	ExclusionReasonDraft      ExclusionReason = "draft"
	ExclusionReasonHiddenText ExclusionReason = "hidden_text"
	ExclusionReasonList       ExclusionReason = "list"
	ExclusionReasonBot        ExclusionReason = "bot"
	ExclusionReasonColdRoom   ExclusionReason = "cold_room"
	ExclusionReasonQuarantine ExclusionReason = "quarantine"
)

type Category string

const (
	CategoryHealth                     Category = "health"
	CategorySexualOrientationOrSexLife Category = "sexual_orientation_or_sex_life"
	CategoryGenderIdentity             Category = "gender_identity"
	CategoryReligionOrBelief           Category = "religion_or_belief"
	CategoryPolitics                   Category = "politics"
	CategoryEthnicityOrRace            Category = "ethnicity_or_race"
	CategoryTradeUnion                 Category = "trade_union"
	CategoryGeneticOrBiometric         Category = "genetic_or_biometric"
	CategoryCriminalRecord             Category = "criminal_record"
	CategoryImmigrationStatus          Category = "immigration_status"
	CategoryFinancialHardship          Category = "financial_hardship"
	CategoryMinors                     Category = "minors"
	CategoryFinances                   Category = "finances"
	CategoryLegalProceedings           Category = "legal_proceedings"
)

type DecisionAction string

const (
	DecisionActionConfirm DecisionAction = "confirm"
	DecisionActionUnsure  DecisionAction = "unsure"
	DecisionActionClear   DecisionAction = "clear"
	DecisionActionReject  DecisionAction = "reject"
	DecisionActionPin     DecisionAction = "pin"
	DecisionActionUnpin   DecisionAction = "unpin"
	DecisionActionEnd     DecisionAction = "end"
	DecisionActionForget  DecisionAction = "forget"
)

type Observation struct {
	ID, PersonID, Sequence                    int64
	GenerationID                              *int64
	Level                                     Level
	Origin                                    Origin
	Kind                                      Kind
	Content, ContentKey, SubjectRef           string
	SubjectPersonID                           *int64
	Directness                                personfacts.EvidenceDirectness
	Sensitive                                 bool
	ConfidenceBP                              int
	ValidFrom, ValidUntil                     *time.Time
	OccursOn                                  *personfacts.PartialDateValue
	Status                                    Status
	StatusReason                              StatusReason
	SupersededByID                            *int64
	OwnerState                                OwnerState
	Pinned                                    bool
	SupportFlag                               SupportFlag
	Stability                                 Stability
	StabilitySource                           StabilitySource
	TargetKind, TargetKey                     string
	Categories                                []Category
	MentionCount, SupportedCount              int
	FirstSeenAt, LastSeenAt, PremiseChangedAt *time.Time
	ProgramFingerprint                        string
	CreatedAt, UpdatedAt                      time.Time
}
type Citation struct {
	EvidenceID           int64
	QuoteStart, QuoteEnd int
	QuoteSHA256          string
	Exclusion            ExclusionReason
	Leaf                 Leaf
}
type Leaf struct {
	MessageID, ConversationID       int64
	ConversationKind                string
	AuthoredByPerson, AuthorIsOwner bool
	EventTime                       time.Time
}
type PremiseInput struct {
	ID   int64
	Role PremiseRole
}
type SupportInput struct {
	Evidence   personfacts.EvidenceInput
	Quote      string
	QuoteStart int
}
type ProposedObservation struct {
	PersonID              int64
	Level                 Level
	Origin                Origin
	Kind                  Kind
	Content, SubjectRef   string
	Attribution           Attribution
	Sensitive             bool
	ConfidenceBP          int
	ValidFrom, ValidUntil *time.Time
	OccursOn              *personfacts.PartialDateValue
	Support               []SupportInput
	ContextCount          int
	Premises              []PremiseInput
	ModelCategories       []Category
	Stability             Stability
	TargetKind, TargetKey string
	ReinforcesID          *int64
	SupersedesIDs         []int64
}
type BatchInput struct {
	PersonID           int64
	Generation         *personfacts.GenerationInput
	ProgramFingerprint string
	Proposals          []ProposedObservation
	Actor              string
}
type Filter struct {
	Statuses         []Status
	Levels           []Level
	Kinds            []Kind
	IncludeSensitive bool
	AsOf             *time.Time
	Limit, Offset    int
}
type Page struct {
	Items []Observation
	Total int
}
type Event struct {
	ID, ObservationID    int64
	Event, Actor, Detail string
	CreatedAt            time.Time
}
type Detail struct {
	Observation

	Evidence   []Citation
	Premises   []PremiseInput
	Dependents []int64
	Events     []Event
}
type Layout struct {
	Total, Pinned, Sensitive, NeedsRecheck int
	ByLevel                                map[Level]int
	ByStatus                               map[Status]int
}
type BatchItemResult struct {
	Index         int
	Outcome       string
	ObservationID int64
	AffectedIDs   []int64
	Failure       *ValidationError
}
type BatchResult struct{ Items []BatchItemResult }
type Decision struct {
	ObservationID     int64
	Action            DecisionAction
	Actor             string
	ExpectedUpdatedAt time.Time
	EndAt             *time.Time
	Reason            string
}
type OwnerInput struct {
	PersonID              int64
	Content, SubjectRef   string
	Kind                  Kind
	Categories            []Category
	Stability             Stability
	ValidFrom, ValidUntil *time.Time
	OccursOn              *personfacts.PartialDateValue
	Actor                 string
}

type ValidationError struct{ Field, Reason string }

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }
