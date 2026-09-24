package personmatchworker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personmatch"
	"go.kenn.io/msgvault/internal/personmatchpolicy"
	"go.kenn.io/msgvault/internal/store"
)

var (
	ErrConsentRequired    = errors.New("identity scoring consent is required")
	ErrEvaluationRequired = errors.New("automatic_match_evaluation_required")
)

const QuestionVersion = personmatch.QuestionVersion

type Store interface {
	HasPersonMatchConsentContext(ctx context.Context, fingerprint string) (bool, error)
	PersonMatchConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error)
	EnsurePersonMatchScoringCandidatesContext(ctx context.Context, limit int) (int, error)
	PersonMatchPairSummariesContext(ctx context.Context, leftID, rightID int64) (store.PersonMatchPairSummaries, error)
	ClaimNextIdentityMatchJudgmentContext(ctx context.Context, owner string, lease time.Duration, scoringVersion ...string) (*store.IdentityMatchJudgmentLease, error)
	RecordIdentityMatchJudgmentContext(ctx context.Context, lease store.IdentityMatchJudgmentLease, input store.IdentityMatchJudgmentInput) (*store.IdentityMatchJudgment, error)
}

type Worker struct {
	Store      Store
	Config     personmatch.Config
	Endpoint   string // Empty uses the pinned direct endpoint; tests can supply a local fixture.
	HTTPClient *http.Client
}

type DryResult struct {
	CandidateID     int64                    `json:"candidate_id"`
	ReviewToken     string                   `json:"review_token"`
	ModelID         string                   `json:"model_id"`
	PacketSchema    string                   `json:"packet_schema"`
	PolicyVersion   string                   `json:"policy_version"`
	EvidenceClasses []string                 `json:"evidence_classes"`
	Probability     *float64                 `json:"probability,omitempty"`
	ProposedAction  personmatchpolicy.Action `json:"proposed_action"`
	Blockers        []string                 `json:"blockers"`
	Status          string                   `json:"status"`
}

func (w Worker) RunLive(context.Context, int) error { return ErrEvaluationRequired }

// RunDry preflights exact disclosure consent and the named daemon credential
// before any claim or provider request. It journals judgments but never links,
// merges, promotes, or publishes identities.
func (w Worker) RunDry(ctx context.Context, limit int) ([]DryResult, error) {
	if w.Store == nil {
		return nil, errors.New("identity scoring store unavailable")
	}
	cfg := w.Config
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	fingerprint, err := cfg.DisclosureFingerprint()
	if err != nil {
		return nil, err
	}
	consented, err := w.Store.HasPersonMatchConsentContext(ctx, fingerprint)
	if err != nil {
		return nil, err
	}
	if !consented {
		return nil, ErrConsentRequired
	}
	key, err := cfg.CredentialFromEnvironment()
	if err != nil {
		return nil, err
	}
	if limit < 1 || limit > cfg.BatchSize {
		return nil, errors.New("dry-run limit exceeds configured batch size")
	}
	if _, err := w.Store.EnsurePersonMatchScoringCandidatesContext(ctx, limit); err != nil {
		return nil, err
	}
	endpoint := w.Endpoint
	if endpoint == "" {
		endpoint = personmatch.Endpoint
	}
	judge := personmatch.JevClient{Endpoint: endpoint, ModelID: cfg.ModelID, Key: key, HTTPClient: w.HTTPClient,
		RequestGate: func(dispatchContext context.Context, dispatch func() error) (bool, error) {
			return w.Store.PersonMatchConsentEgressContext(dispatchContext, fingerprint, dispatch)
		}}
	versionBytes := sha256.Sum256([]byte(fmt.Sprintf("%s:%s:%s:%.17g", fingerprint, QuestionVersion,
		personmatchpolicy.Version, cfg.MinimumProbability)))
	scoringVersion := hex.EncodeToString(versionBytes[:])
	results := make([]DryResult, 0, limit)
	for len(results) < limit {
		// Consent can be revoked while a batch is running. Recheck before each
		// potential disclosure and stop immediately on revocation.
		consented, err = w.Store.HasPersonMatchConsentContext(ctx, fingerprint)
		if err != nil {
			return results, err
		}
		if !consented {
			return results, ErrConsentRequired
		}
		lease, err := w.Store.ClaimNextIdentityMatchJudgmentContext(ctx, "daemon_dry_run", 2*time.Minute, scoringVersion)
		if err != nil {
			return results, err
		}
		if lease == nil {
			break
		}
		result := DryResult{CandidateID: lease.CandidateID, ReviewToken: lease.Candidate.ReviewToken,
			ModelID: cfg.ModelID, PacketSchema: personmatch.PacketSchema,
			PolicyVersion: personmatchpolicy.Version, ProposedAction: personmatchpolicy.NeedsReview,
			Blockers: []string{}, EvidenceClasses: []string{}, Status: "needs_review"}
		if lease.Candidate.ScoringEvidenceIncomplete {
			result.Blockers = append(result.Blockers, "evidence_incomplete")
			_, err = w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, store.IdentityMatchJudgmentInput{
				Status: "terminal_error", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
				PolicyVersion: personmatchpolicy.Version, Blockers: result.Blockers,
				Outcome: "needs_review", ErrorClass: "evidence_incomplete",
			})
			if err != nil {
				return results, err
			}
			result.Status = "local_guard_blocked"
			results = append(results, result)
			continue
		}
		packet, signals, packetErr := BuildPairPacket(&lease.Candidate)
		if packetErr == nil {
			for _, evidence := range packet.Evidence {
				result.EvidenceClasses = append(result.EvidenceClasses, evidence.Class)
			}
		}
		if packetErr != nil || len(packet.Evidence) == 0 {
			if packetErr != nil {
				result.Blockers = append(result.Blockers, "unsupported_pair")
			}
			if len(packet.Evidence) == 0 {
				result.Blockers = append(result.Blockers, "identity_evidence_missing")
			}
			_, err = w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, store.IdentityMatchJudgmentInput{
				Status: "terminal_error", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
				PolicyVersion: personmatchpolicy.Version, Blockers: result.Blockers,
				Outcome: "needs_review", ErrorClass: "local_guard_blocked",
			})
			if err != nil {
				return results, err
			}
			result.Status = "local_guard_blocked"
			results = append(results, result)
			continue
		}
		summaries, summaryErr := w.Store.PersonMatchPairSummariesContext(ctx,
			lease.Candidate.LeftID, lease.Candidate.RightID)
		if summaryErr != nil {
			_, err = w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, store.IdentityMatchJudgmentInput{
				Status: "retryable_error", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
				PolicyVersion: personmatchpolicy.Version, Outcome: "needs_review", ErrorClass: "pair_read_error",
			})
			if err != nil {
				return results, err
			}
			result.Status = "pair_read_error"
			results = append(results, result)
			continue
		}
		packet.Left, packet.Right = summaries.Left, summaries.Right
		// A batch may spend time loading and validating a pair after its initial
		// preflight. Check the exact disclosure again at the provider boundary.
		consented, err = w.Store.HasPersonMatchConsentContext(ctx, fingerprint)
		if err != nil {
			return results, err
		}
		if !consented {
			_, recordErr := w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, store.IdentityMatchJudgmentInput{
				Status: "retryable_error", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
				PolicyVersion: personmatchpolicy.Version, Outcome: "needs_review", ErrorClass: "consent_revoked",
			})
			if recordErr != nil {
				return results, recordErr
			}
			return results, ErrConsentRequired
		}
		judgment, scoreErr := judge.Score(ctx, packet)
		if scoreErr != nil {
			errorClass := "provider_error"
			if errors.Is(scoreErr, personmatch.ErrConsentRevoked) {
				errorClass = "consent_revoked"
			}
			_, err = w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, store.IdentityMatchJudgmentInput{
				Status: "retryable_error", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
				PolicyVersion: personmatchpolicy.Version, Outcome: "needs_review", ErrorClass: errorClass,
			})
			if err != nil {
				return results, err
			}
			if errors.Is(scoreErr, personmatch.ErrConsentRevoked) {
				return results, ErrConsentRequired
			}
			result.Status = "provider_error"
			results = append(results, result)
			continue
		}
		input := localPolicyInput(lease.Candidate, summaries, signals)
		input.Probability = judgment.Probability
		input.MinimumProbability = cfg.MinimumProbability
		decision := personmatchpolicy.Evaluate(input)
		result.Probability = &judgment.Probability
		result.ProposedAction = decision.Action
		result.Blockers = decision.Blockers
		result.Status = "scored"
		_, err = w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, store.IdentityMatchJudgmentInput{
			Status: "scored", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
			PolicyVersion: personmatchpolicy.Version, Probability: &judgment.Probability,
			Blockers: decision.Blockers, Outcome: string(decision.Action),
		})
		if errors.Is(err, store.ErrIdentityMatchJudgmentFingerprintStale) {
			result.Status = "stale"
			result.Probability = nil
			result.ProposedAction = personmatchpolicy.NeedsReview
			result.Blockers = []string{"stale_review_snapshot"}
			results = append(results, result)
			continue
		}
		if err != nil {
			return results, err
		}
		results = append(results, result)
	}
	return results, nil
}

func localPolicyInput(candidate store.IdentityMatchCandidate, summaries store.PersonMatchPairSummaries, signals []personmatchpolicy.Signal) personmatchpolicy.Input {
	signals = append([]personmatchpolicy.Signal(nil), signals...)
	if exactFullName(summaries.Left.DisplayName, summaries.Right.DisplayName) {
		for i := range signals {
			if signals[i].Class == personmatchpolicy.SignalDisplayName {
				signals[i].Class = personmatchpolicy.SignalVerifiedFullName
			}
		}
	}
	activeCardDAVPublication := (candidate.LeftPerson != nil && candidate.LeftPerson.ActiveCardDAVPublication) ||
		(candidate.RightPerson != nil && candidate.RightPerson.ActiveCardDAVPublication)
	input := personmatchpolicy.Input{Signals: signals,
		UserRejected:             candidate.State == store.IdentityMatchStateRejected,
		ManualMergeReview:        candidate.Blocker == "person_merge_required",
		ActiveCardDAVPublication: activeCardDAVPublication,
		StaleRevision:            !candidate.Actionable && candidate.Blocker != "person_merge_required" && candidate.Blocker != "active_carddav_publication",
		IncompleteGuardData:      summaries.Truncated}
	for _, evidence := range candidate.Evidence {
		switch evidence.EvidenceKind {
		case "phone", "shared_phone", "recycled_phone", "shared_contact_point":
			input.SharedContactPoint = true
		case "conflicting_stable_id":
			input.ConflictingStableID = true
		}
	}
	leftStable := map[string]map[string]bool{}
	for _, identifier := range summaries.Left.Identifiers {
		switch identifier.Type {
		case "beeper", "apple_id", "whatsapp", "imessage":
			key := identifier.Type + "\x00" + identifier.ServiceSlug + "\x00" + identifier.ScopeKind + "\x00" + identifier.ScopeValue
			if leftStable[key] == nil {
				leftStable[key] = map[string]bool{}
			}
			leftStable[key][identifier.Value] = true
		}
	}
	for _, identifier := range summaries.Right.Identifiers {
		key := identifier.Type + "\x00" + identifier.ServiceSlug + "\x00" + identifier.ScopeKind + "\x00" + identifier.ScopeValue
		if values := leftStable[key]; len(values) > 0 && !values[identifier.Value] {
			input.ConflictingStableID = true
		}
	}
	return input
}

func exactFullName(left, right string) bool {
	left = strings.Join(strings.Fields(left), " ")
	right = strings.Join(strings.Fields(right), " ")
	return len(strings.Fields(left)) >= 2 && strings.EqualFold(left, right)
}
