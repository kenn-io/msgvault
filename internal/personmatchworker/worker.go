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

var ErrConsentRequired = errors.New("identity scoring consent is required")

const QuestionVersion = personmatch.QuestionVersion

type Store interface {
	HasPersonMatchConsentContext(ctx context.Context, fingerprint string) (bool, error)
	PersonMatchConsentEgressContext(ctx context.Context, fingerprint string, dispatch func() error) (bool, error)
	EnsurePersonMatchScoringCandidatesContext(ctx context.Context, limit int) (int, error)
	ClaimNextIdentityMatchJudgmentContext(ctx context.Context, owner string, lease time.Duration, scoringVersion ...string) (*store.IdentityMatchJudgmentLease, error)
	RecordIdentityMatchJudgmentContext(ctx context.Context, lease store.IdentityMatchJudgmentLease, input store.IdentityMatchJudgmentInput) (*store.IdentityMatchJudgment, error)
}

type Worker struct {
	Store      Store
	Config     personmatch.Config
	Endpoint   string // Empty uses the pinned direct endpoint; tests can supply a local fixture.
	HTTPClient *http.Client
}

type Result struct {
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

// Run preflights exact disclosure consent and the named daemon credential
// before any claim or provider request. It journals judgments but never links,
// merges, promotes, or publishes identities.
func (w Worker) Run(ctx context.Context, limit int) ([]Result, error) {
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
		return nil, errors.New("scoring limit exceeds configured batch size")
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
	results := make([]Result, 0, limit)
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
		lease, err := w.Store.ClaimNextIdentityMatchJudgmentContext(ctx, "daemon_scoring", 2*time.Minute, scoringVersion)
		if err != nil {
			return results, err
		}
		if lease == nil {
			break
		}
		result := Result{CandidateID: lease.CandidateID, ReviewToken: lease.Candidate.ReviewToken,
			ModelID: cfg.ModelID, PacketSchema: personmatch.PacketSchema,
			PolicyVersion: personmatchpolicy.Version, ProposedAction: personmatchpolicy.NeedsReview,
			Blockers: []string{}, EvidenceClasses: []string{}, Status: "needs_review"}
		packet, signals, packetErr := BuildPairPacket(&lease.Candidate)
		for _, evidence := range packet.Evidence {
			result.EvidenceClasses = append(result.EvidenceClasses, evidence.Class)
		}
		input := localPolicyInput(lease.Candidate, lease.Summaries, signals)
		input.Probability = 1
		input.MinimumProbability = cfg.MinimumProbability
		switch {
		case lease.Candidate.ScoringEvidenceIncomplete:
			result.Blockers = []string{"evidence_incomplete"}
		case packetErr != nil:
			result.Blockers = []string{"unsupported_pair"}
		case len(packet.Evidence) == 0:
			result.Blockers = []string{"identity_evidence_missing"}
		default:
			// No provider score can overcome a local blocker. Keep the review
			// and its reason locally without disclosing the participants.
			result.Blockers = personmatchpolicy.Evaluate(input).Blockers
		}
		journalInput := store.IdentityMatchJudgmentInput{
			Status: "retryable_error", ModelID: cfg.ModelID, QuestionVersion: QuestionVersion,
			PolicyVersion: personmatchpolicy.Version, Outcome: "needs_review",
		}
		consentRevoked := false
		if len(result.Blockers) > 0 {
			journalInput.Status = "terminal_error"
			journalInput.Blockers = result.Blockers
			journalInput.ErrorClass = "local_guard_blocked"
			result.Status = "local_guard_blocked"
		} else {
			packet.Left, packet.Right = lease.Summaries.Left, lease.Summaries.Right
			// A batch may spend time loading and validating a pair after its initial
			// preflight. Check the exact disclosure again at the provider boundary.
			consented, err = w.Store.HasPersonMatchConsentContext(ctx, fingerprint)
			if err != nil {
				return results, err
			}
			if !consented {
				consentRevoked = true
				journalInput.ErrorClass = "consent_revoked"
			} else {
				judgment, scoreErr := judge.Score(ctx, packet)
				if scoreErr != nil {
					journalInput.ErrorClass = "provider_error"
					consentRevoked = errors.Is(scoreErr, personmatch.ErrConsentRevoked)
					if consentRevoked {
						journalInput.ErrorClass = "consent_revoked"
					}
					result.Status = "provider_error"
				} else {
					input.Probability = judgment.Probability
					decision := personmatchpolicy.Evaluate(input)
					result.Probability = &judgment.Probability
					result.ProposedAction = decision.Action
					result.Blockers = decision.Blockers
					result.Status = "scored"
					journalInput.Status = "scored"
					journalInput.Probability = &judgment.Probability
					journalInput.Blockers = decision.Blockers
					journalInput.Outcome = string(decision.Action)
				}
			}
		}
		_, err = w.Store.RecordIdentityMatchJudgmentContext(ctx, *lease, journalInput)
		stale := errors.Is(err, store.ErrIdentityMatchJudgmentFingerprintStale)
		if stale {
			result.Status = "stale"
			result.Probability = nil
			result.ProposedAction = personmatchpolicy.NeedsReview
			result.Blockers = []string{"stale_review_snapshot"}
		} else if err != nil {
			return results, err
		}
		if !consentRevoked || stale {
			results = append(results, result)
		}
		if consentRevoked {
			return results, ErrConsentRequired
		}
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
		case "phone":
			input.SharedContactPoint = true
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
