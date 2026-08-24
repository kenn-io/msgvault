// Package personenrichment defines provider-neutral contracts for external
// person enrichment. It deliberately has no dependency on durable storage.
package personenrichment

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

type Provider interface {
	Start(ctx context.Context, request Request) (Attempt, error)
	Poll(ctx context.Context, attempt Attempt) (Result, error)
}

// GuaranteedChargeProvider is optional. Its bound must be a provider guarantee
// for this exact request, not an operator estimate or historical average. The
// caller obtains it after authorization but before reservation and Start; a
// Result's observed Cost is never used to infer this pre-start guarantee.
type GuaranteedChargeProvider interface {
	GuaranteedMaxCharge(ctx context.Context, request Request) (Cost, error)
}

type ProviderFactory func(ProviderConfig, string) (Provider, error)

type Request struct {
	RequestHash string                         `json:"request_hash"`
	Identity    Identity                       `json:"identity"`
	Targets     []personfacts.TargetDescriptor `json:"targets"`
}

type Identity struct {
	Name              string   `json:"name,omitempty"`
	Email             string   `json:"email,omitempty"`
	Phone             string   `json:"phone,omitempty"`
	CurrentCompany    string   `json:"current_company,omitempty"`
	PublicProfileURLs []string `json:"public_profile_urls,omitempty"`
}

type IdentifierClass string

const (
	IdentifierName             IdentifierClass = "name"
	IdentifierEmail            IdentifierClass = "email"
	IdentifierPhone            IdentifierClass = "phone"
	IdentifierCurrentCompany   IdentifierClass = "current_company"
	IdentifierPublicProfileURL IdentifierClass = "public_profile_url"
)

type SuppressionIdentifierClass string

const (
	SuppressionEmail            SuppressionIdentifierClass = "email"
	SuppressionPhone            SuppressionIdentifierClass = "phone"
	SuppressionPublicProfileURL SuppressionIdentifierClass = "public_profile_url"
	SuppressionProviderPersonID SuppressionIdentifierClass = "provider_person_id"
	SuppressionNameCompany      SuppressionIdentifierClass = "name_company"
)

type SuppressionDigest struct {
	ProviderNamespace    string
	NormalizationVersion string
	IdentifierClass      SuppressionIdentifierClass
	KeyID                string
	Digest               []byte
}

type AttemptState string

const (
	AttemptComplete AttemptState = "complete"
	AttemptPending  AttemptState = "pending"
)

type Attempt struct {
	State               AttemptState
	RequestID           string
	JobID               string
	PollAfter           time.Duration
	StartedAt           time.Time
	AdapterVersion      string
	SchemaVersion       string
	GeneratedSchema     bool
	GeneratedSchemaHash string
	ProgramFingerprint  string
	// Targets is the exact immutable target set used to generate an asynchronous
	// provider schema. Poll validates it against GeneratedSchemaHash before
	// decoding provider values after a worker restart.
	Targets []personfacts.TargetDescriptor
	Result  *Result
}

// Validate checks the mutually exclusive synchronous and asynchronous
// provider outcomes without rewriting provider-issued opaque identifiers.
func (a Attempt) Validate() error {
	if err := validatePresentOpaqueID("request ID", a.RequestID); err != nil {
		return err
	}
	if err := validatePresentOpaqueID("job ID", a.JobID); err != nil {
		return err
	}
	switch a.State {
	case AttemptComplete:
		if a.Result == nil || a.Result.State != ResultComplete {
			return errors.New("complete attempt requires exactly one complete result")
		}
		if err := a.Result.Validate(); err != nil {
			return fmt.Errorf("validate complete result: %w", err)
		}
	case AttemptPending:
		if strings.TrimSpace(a.JobID) == "" {
			return errors.New("pending attempt requires a non-empty job ID")
		}
		if a.Result != nil {
			return errors.New("pending attempt requires a nil result")
		}
	default:
		return fmt.Errorf("invalid attempt state %q", a.State)
	}
	return nil
}

type ResultState string

const (
	ResultComplete ResultState = "complete"
	ResultPending  ResultState = "pending"
)

type Result struct {
	State               ResultState
	RequestID           string
	JobID               string
	PollAfter           time.Duration
	Claims              []personfacts.ProposedClaim
	Citations           []Citation
	ProviderPersonIDs   []ProviderPersonID
	CanonicalPublicURLs []string
	IdentityMatches     []IdentityMatch
	IdentityConfidence  int
	FreshAsOf           time.Time
	SourceAttempts      []SourceAttempt
	Cost                Cost
	AdapterVersion      string
	SchemaVersion       string
	GeneratedSchema     bool
	GeneratedSchemaHash string
	ProviderVersion     string
	Model               string
	ModelVersion        string
}

// Validate ensures provider output is safe to pass toward durable resolver
// inputs. IdentityMatch.Value remains transient and is never normalized here.
func (r Result) Validate() error {
	if err := validatePresentOpaqueID("request ID", r.RequestID); err != nil {
		return err
	}
	if err := validatePresentOpaqueID("job ID", r.JobID); err != nil {
		return err
	}
	switch r.State {
	case ResultComplete:
		if strings.TrimSpace(r.ProviderVersion) == "" {
			return errors.New("complete result requires a provider version")
		}
	case ResultPending:
		if strings.TrimSpace(r.JobID) == "" {
			return errors.New("pending result requires a non-empty job ID")
		}
		if len(r.Claims) != 0 {
			return errors.New("pending result must not contain claims")
		}
	default:
		return fmt.Errorf("invalid result state %q", r.State)
	}
	if err := validateConfidence("identity", r.IdentityConfidence); err != nil {
		return err
	}
	for i, match := range r.IdentityMatches {
		if !validIdentifierClass(match.Class) {
			return fmt.Errorf("identity match %d has invalid identifier class %q", i, match.Class)
		}
		if err := validateConfidence(fmt.Sprintf("identity match %d", i), match.Confidence); err != nil {
			return err
		}
	}
	for i, personID := range r.ProviderPersonIDs {
		if strings.TrimSpace(personID.ID) == "" {
			return fmt.Errorf("provider person ID %d must be non-empty", i)
		}
		if err := validateConfidence(fmt.Sprintf("provider person ID %d", i), personID.Confidence); err != nil {
			return err
		}
	}
	for i, claim := range r.Claims {
		if err := validateConfidence(fmt.Sprintf("claim %d", i), claim.Confidence.ReportedScore); err != nil {
			return err
		}
		for j, evidence := range claim.Evidence {
			if err := validateConfidence(
				fmt.Sprintf("claim %d evidence %d identity", i, j),
				evidence.IdentityScore,
			); err != nil {
				return err
			}
		}
	}
	if err := r.Cost.Validate(); err != nil {
		return err
	}
	return nil
}

type IdentityMatch struct {
	Class      IdentifierClass
	Value      string // Transient: normalize and compare, then discard before the sink.
	Confidence int
}

type Citation struct {
	Key         string
	URL         string
	Title       string
	Publisher   string
	Excerpt     string
	PublishedAt time.Time
	RetrievedAt time.Time
}

type ProviderPersonID struct {
	ID         string
	Confidence int
}

type SourceAttempt struct {
	URL        string
	Outcome    string
	ObservedAt time.Time
}

type Cost struct {
	Currency     string
	AmountMicros int64
	Estimated    bool
}

func (c Cost) Validate() error {
	if c.AmountMicros < 0 {
		return errors.New("cost amount must be non-negative")
	}
	if c.Currency == "" && c.AmountMicros == 0 {
		return nil
	}
	if c.Currency != "USD" {
		return fmt.Errorf("cost currency must be USD, got %q", c.Currency)
	}
	return nil
}

// ValidateGuaranteed applies the stricter contract required before reserving
// a hard monetary budget.
func (c Cost) ValidateGuaranteed() error {
	if err := c.Validate(); err != nil {
		return err
	}
	if c.Currency != "USD" {
		return errors.New("guaranteed maximum charge must be expressed in USD micros")
	}
	if c.Estimated {
		return errors.New("guaranteed maximum charge must not be estimated")
	}
	return nil
}

type ClaimSink interface {
	CommitEnrichmentClaims(ctx context.Context, commit ClaimCommit) (*ClaimOutcome, error)
}

type ClaimOutcomeStatus string

const (
	ClaimApplied          ClaimOutcomeStatus = "applied"
	ClaimPolicyRejected   ClaimOutcomeStatus = "policy_rejected"
	ClaimIdentityRejected ClaimOutcomeStatus = "identity_rejected"
	ClaimSuppressed       ClaimOutcomeStatus = "suppressed"
)

type ClaimOutcome struct {
	Status     ClaimOutcomeStatus
	Generation *personfacts.GenerationResult
}

type ClaimCommit struct {
	AttemptID           int64
	RunID               int64
	LeaseFence          int64
	PersonID            int64
	ProfileFingerprint  string
	ProviderNamespace   string
	RequestHash         string
	IdentityAssessment  IdentityAssessment
	result              Result
	returnedIdentifiers verifiedReturnedIdentifierManifest
}

type ClaimCommitInput struct {
	AttemptID          int64
	RunID              int64
	PersonID           int64
	LeaseFence         int64
	ProfileFingerprint string
	ProviderNamespace  string
	RequestHash        string
	IdentityAssessment IdentityAssessment
}

type verifiedReturnedIdentifierManifest struct {
	verificationVersion string
	providerNamespace   string
	coverageHash        string
	digests             []SuppressionDigest
}

type IdentityAssessment struct {
	Accepted       bool
	Score          int
	Reason         string
	MatchedClasses []IdentifierClass
}

func (a IdentityAssessment) Validate() error {
	if err := validateConfidence("identity assessment", a.Score); err != nil {
		return err
	}
	seen := make(map[IdentifierClass]struct{}, len(a.MatchedClasses))
	for _, class := range a.MatchedClasses {
		if !validIdentifierClass(class) {
			return fmt.Errorf("identity assessment contains invalid identifier class %q", class)
		}
		if _, exists := seen[class]; exists {
			return fmt.Errorf("identity assessment contains duplicate identifier class %q", class)
		}
		seen[class] = struct{}{}
	}
	return nil
}

func validIdentifierClass(value IdentifierClass) bool {
	switch value {
	case IdentifierName, IdentifierEmail, IdentifierPhone,
		IdentifierCurrentCompany, IdentifierPublicProfileURL:
		return true
	default:
		return false
	}
}

func validateConfidence(name string, score int) error {
	if score < 0 || score > 1000 {
		return fmt.Errorf("%s confidence %d must be in [0,1000]", name, score)
	}
	return nil
}

func validatePresentOpaqueID(name, value string) error {
	if value != "" && strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must be non-empty when present", name)
	}
	return nil
}
