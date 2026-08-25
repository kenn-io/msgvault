package personfacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"sort"
	"strings"
	"time"
)

const ResolverVersionV1 = "person-fact-resolver-v1"

type resolverCandidate struct {
	claim             ResolvedClaim
	supportedEvidence []Evidence
	score             ScoreBreakdown
	canonicalKey      string
	decision          Decision
	eligible          bool
}

type resolverDirection struct {
	fingerprint    string
	negative       bool
	indexes        []int
	representative int
	score          ScoreBreakdown
	canonicalKey   string
}

type resolverDirectionKey struct {
	fingerprint string
	negative    bool
}

type effectiveEvidenceStatus struct {
	personID      int64
	key           string
	sourceVersion string
	supported     bool
	eventID       int64
	reason        EvidenceStatusReason
}

func DefaultPolicyV1() Policy {
	return Policy{
		Version: ResolverVersionV1, ApplyThreshold: 750, ReplacementMargin: 100,
		HysteresisMargin: 75, MinimumIdentityScore: 900, MaxCorroborationBonus: 150,
		SourceClassWeights: map[EvidenceSourceClass]int{
			EvidenceArchive: 300, EvidencePublic: 250, EvidenceSystem: 500,
			EvidenceProviderAssertion: 150,
		},
		DirectnessWeights: map[EvidenceDirectness]int{
			DirectSelf: 400, DirectOther: 200, Indirect: 0,
		},
		AuthorityWeights: map[EvidenceAuthority]int{
			AuthorityAuthoritative: 300, AuthorityOrdinary: 100, AuthorityAggregator: 0,
		},
	}
}

func NewResolver(policy Policy) (Resolver, error) {
	if err := validateResolverPolicy(policy); err != nil {
		return Resolver{}, err
	}
	return Resolver{Policy: copyResolverPolicy(policy)}, nil
}

func (r Resolver) Resolve(input ResolutionInput) (Resolution, error) {
	if err := validateResolverPolicy(r.Policy); err != nil {
		return Resolution{}, fmt.Errorf("resolver policy: %w", err)
	}
	if err := validateResolutionInput(input); err != nil {
		return Resolution{}, err
	}
	input = copyResolutionInput(input)
	statuses, err := collectEffectiveEvidenceStatuses(input)
	if err != nil {
		return Resolution{}, err
	}
	inputFingerprint, err := resolverInputFingerprint(input, r.Policy, statuses)
	if err != nil {
		return Resolution{}, err
	}

	candidates := make([]resolverCandidate, len(input.Claims))
	seenClaimKeys := make(map[string]struct{}, len(input.Claims))
	for index := range input.Claims {
		claim := input.Claims[index]
		if claim.ClaimKey == "" {
			return Resolution{}, errors.New("resolver claim key is required")
		}
		if _, exists := seenClaimKeys[claim.ClaimKey]; exists {
			return Resolution{}, fmt.Errorf("duplicate resolver claim key %q", claim.ClaimKey)
		}
		seenClaimKeys[claim.ClaimKey] = struct{}{}
		candidate, candidateErr := r.prepareCandidate(input, claim, statuses)
		if candidateErr != nil {
			return Resolution{}, candidateErr
		}
		candidates[index] = candidate
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].canonicalKey < candidates[j].canonicalKey })

	pinned := input.Pin.Pinned || bootstrapDeclaredPin(input)
	if input.Pin.EventID != nil {
		pinned = input.Pin.Pinned
	}
	if pinned {
		for index := range candidates {
			if candidates[index].eligible {
				candidates[index].decision = resolverDecision(input.PersonID, candidates[index],
					DecisionRetained, ReasonPinRetained, "")
			}
		}
	} else {
		switch input.Target.Cardinality {
		case CardinalitySingle:
			r.resolveSingle(input, candidates)
		case CardinalityMulti:
			r.resolveMulti(input, candidates)
		}
	}

	resolution := Resolution{
		Target: resolverTargetRef(input.Target), ResolverVersion: r.Policy.Version,
		InputFingerprint: inputFingerprint, ResolvedAt: input.ResolvedAt,
		Decisions: make([]Decision, 0, len(candidates)), Projections: []ProjectionPlan{},
	}
	for index := range candidates {
		decision := candidates[index].decision
		if decision.Action == "" {
			decision = resolverDecision(input.PersonID, candidates[index],
				DecisionRetained, ReasonBelowThreshold, "")
		}
		decisionKey, keyErr := DecisionKey(inputFingerprint, decision.ClaimKey, decision.Action)
		if keyErr != nil {
			return Resolution{}, keyErr
		}
		decision.DecisionKey = decisionKey
		resolution.Decisions = append(resolution.Decisions, decision)
		resolution.Projections = append(resolution.Projections, candidates[index].projectionPlans(input)...)
	}
	return resolution, nil
}

func (r Resolver) prepareCandidate(
	input ResolutionInput, claim ResolvedClaim, statuses map[string]effectiveEvidenceStatus,
) (resolverCandidate, error) {
	candidate := resolverCandidate{claim: claim}
	candidate.canonicalKey = resolverClaimSortKey(claim)
	failure := claim.Failure
	if failure != nil {
		if !validResolverRejectionFailure(failure) {
			return resolverCandidate{}, fmt.Errorf("claim %q has an invalid durable rejection", claim.ClaimKey)
		}
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: failure.Action, Reason: failure.Reason,
		}
		return candidate, nil
	}
	if claim.Claim.Target.Kind != input.Target.Kind || claim.Claim.Target.Key != input.Target.Key {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionInvalid, Reason: ReasonUnsupportedTarget,
		}
		return candidate, nil
	}
	if claim.Claim.Target.Revision != input.Target.Revision {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionInvalid, Reason: ReasonStaleTargetRevision,
		}
		return candidate, nil
	}
	if claim.Claim.Relation != RelationSupport && claim.Claim.Relation != RelationContradict &&
		claim.Claim.Relation != RelationSupersede {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionInvalid, Reason: ReasonMalformedValue,
		}
		return candidate, nil
	}
	if claim.Claim.Origin != OriginExtraction && claim.Claim.Origin != OriginEnrichment &&
		claim.Claim.Origin != OriginSystem {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionInvalid, Reason: ReasonMalformedValue,
		}
		return candidate, nil
	}
	if claim.Claim.Confidence.ReportedScore < 0 || claim.Claim.Confidence.ReportedScore > 1000 ||
		claim.Normalized == nil || claim.Normalized.Fingerprint == "" {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionInvalid, Reason: ReasonMalformedValue,
		}
		return candidate, nil
	}
	if (claim.Claim.ValidFrom != nil &&
		(claim.Claim.ValidFrom.IsZero() || !isUTC(*claim.Claim.ValidFrom))) ||
		(claim.Claim.ValidUntil != nil &&
			(claim.Claim.ValidUntil.IsZero() || !isUTC(*claim.Claim.ValidUntil))) ||
		(claim.Claim.ValidFrom != nil && claim.Claim.ValidUntil != nil &&
			claim.Claim.ValidUntil.Before(*claim.Claim.ValidFrom)) {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionInvalid, Reason: ReasonMalformedValue,
		}
		return candidate, nil
	}
	// Claim validity is a half-open interval: valid_from is inclusive and
	// valid_until is exclusive at the resolution's world time.
	if (claim.Claim.ValidFrom != nil && input.ResolvedAt.Before(*claim.Claim.ValidFrom)) ||
		(claim.Claim.ValidUntil != nil && !input.ResolvedAt.Before(*claim.Claim.ValidUntil)) {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionRetained, Reason: ReasonOutsideValidity,
		}
		return candidate, nil
	}
	if input.Target.Sensitive && !input.Policy.AllowSensitive {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionPolicyRejected, Reason: ReasonSensitivePolicy,
		}
		return candidate, nil
	}

	for _, evidence := range claim.Evidence {
		status := statuses[effectiveEvidenceTupleKey(evidence)]
		if !status.supported {
			continue
		}
		if evidence.PersonID != input.PersonID || evidence.Input.PersonID != input.PersonID ||
			evidence.Input.SubjectPersonID == nil || *evidence.Input.SubjectPersonID != input.PersonID ||
			evidence.Input.IdentityScore < r.Policy.MinimumIdentityScore {
			candidate.decision = Decision{
				PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
				Action: DecisionIdentityRejected, Reason: ReasonIdentityMismatch,
			}
			return candidate, nil
		}
		if validationErr := validateEvidenceInput(evidence.Input); validationErr != nil {
			reason := ReasonMalformedValue
			action := DecisionInvalid
			if typed, ok := errors.AsType[*evidenceValidationError](validationErr); ok {
				reason = typed.reason
				if reason == ReasonIdentityMismatch {
					action = DecisionIdentityRejected
				}
			}
			candidate.decision = Decision{
				PersonID: input.PersonID, ClaimKey: claim.ClaimKey, Action: action, Reason: reason,
			}
			return candidate, nil
		}
		candidate.supportedEvidence = append(candidate.supportedEvidence, evidence)
	}
	if len(candidate.supportedEvidence) == 0 {
		candidate.decision = Decision{
			PersonID: input.PersonID, ClaimKey: claim.ClaimKey,
			Action: DecisionRetained, Reason: ReasonEvidenceUnsupported,
		}
		return candidate, nil
	}
	candidate.score = r.scoreClaim(input.ResolvedAt, claim, candidate.supportedEvidence)
	candidate.eligible = true
	return candidate, nil
}

func (r Resolver) scoreClaim(
	resolvedAt time.Time, claim ResolvedClaim, evidence []Evidence,
) ScoreBreakdown {
	return r.scoreEvidence(resolvedAt, claim.Claim.Confidence.ReportedScore, evidence)
}

func (r Resolver) scoreEvidence(
	resolvedAt time.Time, reportedConfidence int, evidence []Evidence,
) ScoreBreakdown {
	score := ScoreBreakdown{Confidence: reportedConfidence / 10}
	independent := make(map[string]struct{}, len(evidence))
	for _, item := range evidence {
		input := item.Input
		score.SourceClass = max(score.SourceClass, r.Policy.SourceClassWeights[input.SourceClass])
		score.Directness = max(score.Directness, r.Policy.DirectnessWeights[input.Directness])
		score.Authority = max(score.Authority, r.Policy.AuthorityWeights[input.Authority])
		score.Freshness = max(score.Freshness, resolverFreshness(resolvedAt, input.EventTime))
		independent[resolverIndependentSource(input)] = struct{}{}
	}
	if len(independent) > 1 {
		score.Corroboration = min((len(independent)-1)*50, r.Policy.MaxCorroborationBonus)
	}
	score.Total = score.SourceClass + score.Directness + score.Authority +
		score.Confidence + score.Freshness + score.Corroboration
	return score
}

func (r Resolver) resolveSingle(input ResolutionInput, candidates []resolverCandidate) {
	activeCurrent := activeResolverCurrent(input)
	if len(activeCurrent) > 1 {
		for index := range candidates {
			if candidates[index].eligible {
				candidates[index].decision = resolverDecision(input.PersonID, candidates[index],
					DecisionConflictRejected, ReasonInsufficientMargin, "")
			}
		}
		return
	}
	directions := r.singleValueDirections(input.ResolvedAt, candidates)
	directions = r.scopeSingleValueNegativeDirections(input, candidates, directions)
	if len(directions) == 0 {
		return
	}
	sort.Slice(directions, func(i, j int) bool {
		if directions[i].score.Total != directions[j].score.Total {
			return directions[i].score.Total > directions[j].score.Total
		}
		return directions[i].canonicalKey < directions[j].canonicalKey
	})
	winner := directions[0]
	if winner.score.Total < r.Policy.ApplyThreshold {
		for _, direction := range directions {
			setDirectionDecision(input.PersonID, candidates, direction,
				DecisionRetained, ReasonBelowThreshold, "")
		}
		return
	}
	if len(directions) > 1 && winner.score.Total == directions[1].score.Total {
		tied := make([]resolverDirection, 0, len(directions))
		for _, direction := range directions {
			if direction.score.Total == winner.score.Total {
				tied = append(tied, direction)
			}
		}
		for tiedIndex, direction := range tied {
			competing := candidates[tied[(tiedIndex+1)%len(tied)].representative].claim.ClaimKey
			setDirectionDecision(input.PersonID, candidates, direction,
				DecisionAmbiguousRetained, ReasonCompetingTie, competing)
		}
		for _, direction := range directions[len(tied):] {
			setDirectionDecision(input.PersonID, candidates, direction,
				DecisionConflictRejected, ReasonInsufficientMargin,
				candidates[winner.representative].claim.ClaimKey)
		}
		return
	}
	if len(directions) > 1 && winner.score.Total-directions[1].score.Total < r.Policy.ReplacementMargin {
		setDirectionDecision(input.PersonID, candidates, winner,
			DecisionRetained, ReasonInsufficientMargin,
			candidates[directions[1].representative].claim.ClaimKey)
		for _, direction := range directions[1:] {
			setDirectionDecision(input.PersonID, candidates, direction,
				DecisionConflictRejected, ReasonInsufficientMargin,
				candidates[winner.representative].claim.ClaimKey)
		}
		return
	}
	if len(activeCurrent) == 1 && directionChangesCurrent(winner, activeCurrent[0]) {
		required := r.Policy.ApplyThreshold + r.Policy.ReplacementMargin + r.Policy.HysteresisMargin
		if winner.score.Total < required {
			setDirectionDecision(input.PersonID, candidates, winner,
				DecisionRetained, ReasonInsufficientMargin, "")
			for _, direction := range directions[1:] {
				setDirectionDecision(input.PersonID, candidates, direction,
					DecisionConflictRejected, ReasonInsufficientMargin,
					candidates[winner.representative].claim.ClaimKey)
			}
			return
		}
	}
	winnerReason := ReasonAppliedProjection
	if winner.negative {
		winnerReason = negativeDecisionReason(candidates[winner.representative].claim.Claim.Relation)
	}
	setWinningDirection(input.PersonID, candidates, winner, winnerReason)
	for _, direction := range directions[1:] {
		losingReason := ReasonAppliedProjection
		if winner.negative {
			losingReason = winnerReason
		}
		setDirectionDecision(input.PersonID, candidates, direction,
			DecisionSuperseded, losingReason, candidates[winner.representative].claim.ClaimKey)
	}
}

func (r Resolver) scopeSingleValueNegativeDirections(
	input ResolutionInput, candidates []resolverCandidate, directions []resolverDirection,
) []resolverDirection {
	currentFingerprint := ""
	if current := activeResolverCurrent(input); len(current) == 1 {
		currentFingerprint = current[0].Normalized.Fingerprint
	}
	byFingerprint := make(map[string][]resolverDirection)
	for _, direction := range directions {
		byFingerprint[direction.fingerprint] = append(
			byFingerprint[direction.fingerprint], direction)
	}
	remaining := make([]resolverDirection, 0, len(directions))
	for fingerprint, scoped := range byFingerprint {
		hasNegative := false
		for _, direction := range scoped {
			hasNegative = hasNegative || direction.negative
		}
		if !hasNegative || fingerprint == currentFingerprint {
			remaining = append(remaining, scoped...)
			continue
		}
		if positive := r.resolveAbsentSingleValueDirections(input, candidates, scoped); positive != nil {
			remaining = append(remaining, *positive)
		}
	}
	return remaining
}

func (r Resolver) resolveAbsentSingleValueDirections(
	input ResolutionInput, candidates []resolverCandidate, directions []resolverDirection,
) *resolverDirection {
	sort.Slice(directions, func(i, j int) bool {
		if directions[i].score.Total != directions[j].score.Total {
			return directions[i].score.Total > directions[j].score.Total
		}
		return directions[i].canonicalKey < directions[j].canonicalKey
	})
	winner := directions[0]
	if winner.score.Total < r.Policy.ApplyThreshold {
		for _, direction := range directions {
			setMultiDirectionDecision(input.PersonID, candidates, direction,
				DecisionRetained, ReasonBelowThreshold, "")
		}
		return nil
	}
	if len(directions) > 1 && winner.score.Total == directions[1].score.Total {
		for index, direction := range directions {
			competing := candidates[directions[(index+1)%len(directions)].representative].claim.ClaimKey
			setMultiDirectionDecision(input.PersonID, candidates, direction,
				DecisionAmbiguousRetained, ReasonCompetingTie, competing)
		}
		return nil
	}
	if len(directions) > 1 && winner.score.Total-directions[1].score.Total < r.Policy.ReplacementMargin {
		setMultiDirectionDecision(input.PersonID, candidates, winner,
			DecisionRetained, ReasonInsufficientMargin,
			candidates[directions[1].representative].claim.ClaimKey)
		for _, direction := range directions[1:] {
			setMultiDirectionDecision(input.PersonID, candidates, direction,
				DecisionConflictRejected, ReasonInsufficientMargin,
				candidates[winner.representative].claim.ClaimKey)
		}
		return nil
	}
	if winner.negative {
		winnerReason := negativeDecisionReason(candidates[winner.representative].claim.Claim.Relation)
		setWinningDirection(input.PersonID, candidates, winner, winnerReason)
		for _, direction := range directions[1:] {
			setDirectionDecision(input.PersonID, candidates, direction,
				DecisionSuperseded, winnerReason,
				candidates[winner.representative].claim.ClaimKey)
		}
		return nil
	}
	for _, direction := range directions[1:] {
		setDirectionDecision(input.PersonID, candidates, direction,
			DecisionSuperseded, ReasonAppliedProjection,
			candidates[winner.representative].claim.ClaimKey)
	}
	return &winner
}

func (r Resolver) singleValueDirections(
	resolvedAt time.Time, candidates []resolverCandidate,
) []resolverDirection {
	grouped := make(map[resolverDirectionKey][]int)
	for index := range candidates {
		if !candidates[index].eligible {
			continue
		}
		negative := candidates[index].claim.Claim.Relation != RelationSupport
		key := resolverDirectionKey{
			fingerprint: candidates[index].claim.Normalized.Fingerprint,
			negative:    negative,
		}
		grouped[key] = append(grouped[key], index)
	}
	directions := make([]resolverDirection, 0, len(grouped))
	for _, indexes := range grouped {
		direction := resolverDirection{
			fingerprint: candidates[indexes[0]].claim.Normalized.Fingerprint,
			negative:    candidates[indexes[0]].claim.Claim.Relation != RelationSupport,
			indexes:     append([]int(nil), indexes...),
		}
		sort.Slice(direction.indexes, func(i, j int) bool {
			left, right := candidates[direction.indexes[i]], candidates[direction.indexes[j]]
			if left.score.Total != right.score.Total {
				return left.score.Total > right.score.Total
			}
			return left.canonicalKey < right.canonicalKey
		})
		direction.representative = direction.indexes[0]
		direction.canonicalKey = direction.fingerprint + fmt.Sprintf("\x00%t\x00", direction.negative) +
			candidates[direction.representative].canonicalKey
		reportedConfidence := 0
		evidence := make([]Evidence, 0)
		for _, index := range direction.indexes {
			reportedConfidence = max(reportedConfidence,
				candidates[index].claim.Claim.Confidence.ReportedScore)
			evidence = append(evidence, candidates[index].supportedEvidence...)
		}
		direction.score = r.scoreEvidence(resolvedAt, reportedConfidence, evidence)
		for _, index := range direction.indexes {
			candidates[index].score = direction.score
		}
		directions = append(directions, direction)
	}
	return directions
}

func directionChangesCurrent(direction resolverDirection, current CurrentProjection) bool {
	if direction.negative {
		return direction.fingerprint == current.Normalized.Fingerprint
	}
	return direction.fingerprint != current.Normalized.Fingerprint
}

func setWinningDirection(
	personID int64, candidates []resolverCandidate, direction resolverDirection, reason DecisionReason,
) {
	representative := direction.representative
	representativeReason := directionClaimReason(direction, candidates[representative], reason)
	candidates[representative].decision = resolverDecision(personID, candidates[representative],
		DecisionApplied, representativeReason, "")
	for _, index := range direction.indexes {
		if index == representative {
			continue
		}
		claimReason := directionClaimReason(direction, candidates[index], reason)
		candidates[index].decision = resolverDecision(personID, candidates[index],
			DecisionSuperseded, claimReason, candidates[representative].claim.ClaimKey)
	}
}

func directionClaimReason(
	direction resolverDirection, candidate resolverCandidate, fallback DecisionReason,
) DecisionReason {
	if direction.negative {
		return negativeDecisionReason(candidate.claim.Claim.Relation)
	}
	return fallback
}

func setDirectionDecision(
	personID int64, candidates []resolverCandidate, direction resolverDirection,
	action DecisionAction, reason DecisionReason, competing string,
) {
	for _, index := range direction.indexes {
		claimReason := directionClaimReason(direction, candidates[index], reason)
		candidates[index].decision = resolverDecision(personID, candidates[index], action, claimReason, competing)
	}
}

func negativeDecisionReason(relation ClaimRelation) DecisionReason {
	if relation == RelationSupersede {
		return ReasonExplicitSupersession
	}
	return ReasonExplicitContradiction
}

func (r Resolver) resolveMulti(input ResolutionInput, candidates []resolverCandidate) {
	byFingerprint := make(map[string][]resolverDirection)
	for _, direction := range r.singleValueDirections(input.ResolvedAt, candidates) {
		byFingerprint[direction.fingerprint] = append(byFingerprint[direction.fingerprint], direction)
	}
	for _, directions := range byFingerprint {
		sort.Slice(directions, func(i, j int) bool {
			if directions[i].score.Total != directions[j].score.Total {
				return directions[i].score.Total > directions[j].score.Total
			}
			return directions[i].canonicalKey < directions[j].canonicalKey
		})
		winner := directions[0]
		if winner.score.Total < r.Policy.ApplyThreshold {
			for _, direction := range directions {
				setMultiDirectionDecision(input.PersonID, candidates, direction,
					DecisionRetained, ReasonBelowThreshold, "")
			}
			continue
		}
		if len(directions) > 1 && winner.score.Total == directions[1].score.Total {
			for index, direction := range directions {
				competing := candidates[directions[(index+1)%len(directions)].representative].claim.ClaimKey
				setMultiDirectionDecision(input.PersonID, candidates, direction,
					DecisionAmbiguousRetained, ReasonCompetingTie, competing)
			}
			continue
		}
		if len(directions) > 1 && winner.score.Total-directions[1].score.Total < r.Policy.ReplacementMargin {
			setMultiDirectionDecision(input.PersonID, candidates, winner,
				DecisionRetained, ReasonInsufficientMargin,
				candidates[directions[1].representative].claim.ClaimKey)
			for _, direction := range directions[1:] {
				setMultiDirectionDecision(input.PersonID, candidates, direction,
					DecisionConflictRejected, ReasonInsufficientMargin,
					candidates[winner.representative].claim.ClaimKey)
			}
			continue
		}
		winnerReason := ReasonAppliedProjection
		if winner.negative {
			winnerReason = negativeDecisionReason(candidates[winner.representative].claim.Claim.Relation)
		}
		setWinningDirection(input.PersonID, candidates, winner, winnerReason)
		for _, direction := range directions[1:] {
			setDirectionDecision(input.PersonID, candidates, direction,
				DecisionSuperseded, winnerReason, candidates[winner.representative].claim.ClaimKey)
		}
	}
}

func setMultiDirectionDecision(
	personID int64, candidates []resolverCandidate, direction resolverDirection,
	action DecisionAction, reason DecisionReason, competing string,
) {
	for _, index := range direction.indexes {
		candidates[index].decision = resolverDecision(
			personID, candidates[index], action, reason, competing)
	}
}

func (c resolverCandidate) projectionPlans(input ResolutionInput) []ProjectionPlan {
	if c.decision.Action != DecisionApplied {
		return nil
	}
	activeFrom := input.ResolvedAt
	if c.claim.Claim.ValidFrom != nil {
		activeFrom = *c.claim.Claim.ValidFrom
	}
	current := activeResolverCurrent(input)
	if c.claim.Claim.Relation == RelationSupport {
		for _, item := range current {
			if item.Normalized.Fingerprint == c.claim.Normalized.Fingerprint {
				return nil
			}
		}
		plan := ProjectionPlan{
			Operation: ProjectionSet, Target: resolverTargetRef(input.Target),
			ClaimKey: c.claim.ClaimKey, ActiveFrom: activeFrom,
		}
		if input.Target.Cardinality == CardinalitySingle && len(current) == 1 {
			ref := current[0].Ref
			plan.CurrentRef = &ref
		}
		return []ProjectionPlan{plan}
	}
	for _, item := range current {
		if item.Normalized.Fingerprint == c.claim.Normalized.Fingerprint {
			ref := item.Ref
			return []ProjectionPlan{{
				Operation: ProjectionRetire, Target: resolverTargetRef(input.Target),
				ClaimKey: c.claim.ClaimKey, CurrentRef: &ref, ActiveFrom: activeFrom,
			}}
		}
	}
	return nil
}

func resolverDecision(
	personID int64, candidate resolverCandidate, action DecisionAction,
	reason DecisionReason, competing string,
) Decision {
	return Decision{
		PersonID: personID, ClaimKey: candidate.claim.ClaimKey, Action: action,
		Reason: reason, Score: candidate.score, CompetingClaimKey: competing,
	}
}

func resolverFreshness(resolvedAt, eventTime time.Time) int {
	if eventTime.After(resolvedAt) {
		return 0
	}
	age := resolvedAt.Sub(eventTime)
	switch {
	case age <= 30*24*time.Hour:
		return 100
	case age <= 180*24*time.Hour:
		return 60
	case age <= 730*24*time.Hour:
		return 20
	default:
		return 0
	}
}

func resolverIndependentSource(input EvidenceInput) string {
	identity := strings.TrimSpace(input.SourceRef)
	if input.SourceClass == EvidencePublic {
		identity = canonicalResolverURL(input.SourceURL)
	}
	return string(input.SourceClass) + "\x00" + identity
}

func canonicalResolverURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String()
}

func collectEffectiveEvidenceStatuses(
	input ResolutionInput,
) (map[string]effectiveEvidenceStatus, error) {
	statuses := make(map[string]effectiveEvidenceStatus)
	for _, claim := range input.Claims {
		for _, evidence := range claim.Evidence {
			tuple := effectiveEvidenceTupleKey(evidence)
			if _, exists := statuses[tuple]; !exists {
				statuses[tuple] = effectiveEvidenceStatus{
					personID: evidence.PersonID, key: resolverEvidenceKey(evidence),
					sourceVersion: evidence.Input.SourceVersion,
					supported:     true,
				}
			}
			if evidence.LatestStatus == nil {
				continue
			}
			event := *evidence.LatestStatus
			if event.ID <= 0 || event.PersonID != evidence.PersonID ||
				event.EvidenceKey != resolverEvidenceKey(evidence) ||
				event.SourceVersion != evidence.Input.SourceVersion ||
				!validEvidenceStatusTransition(event.Supported, event.Reason) {
				return nil, fmt.Errorf("evidence %q has an invalid latest status event", resolverEvidenceKey(evidence))
			}
			current := statuses[tuple]
			if event.ID == current.eventID && current.eventID != 0 &&
				(event.Supported != current.supported || event.Reason != current.reason) {
				return nil, fmt.Errorf("evidence %q has conflicting status event %d", event.EvidenceKey, event.ID)
			}
			if event.ID > current.eventID {
				current.supported = event.Supported
				current.eventID = event.ID
				current.reason = event.Reason
				statuses[tuple] = current
			}
		}
	}
	return statuses, nil
}

func effectiveEvidenceTupleKey(evidence Evidence) string {
	return fmt.Sprintf("%d\x00%s\x00%s", evidence.PersonID, resolverEvidenceKey(evidence), evidence.Input.SourceVersion)
}

func resolverEvidenceKey(evidence Evidence) string {
	if evidence.Key != "" {
		return evidence.Key
	}
	if key, err := EvidenceKey(evidence.Input); err == nil {
		return key
	}
	encoded, err := json.Marshal(evidenceKeyView(evidence.Input))
	if err != nil {
		return ""
	}
	return fingerprint(encoded)
}

func bootstrapDeclaredPin(input ResolutionInput) bool {
	for _, current := range activeResolverCurrent(input) {
		if current.Declared {
			return true
		}
	}
	return false
}

func activeResolverCurrent(input ResolutionInput) []CurrentProjection {
	current := make([]CurrentProjection, 0, len(input.Current))
	for _, item := range input.Current {
		if item.ActiveUntil == nil || input.ResolvedAt.Before(*item.ActiveUntil) {
			current = append(current, item)
		}
	}
	sort.Slice(current, func(i, j int) bool {
		if current[i].Normalized.Fingerprint != current[j].Normalized.Fingerprint {
			return current[i].Normalized.Fingerprint < current[j].Normalized.Fingerprint
		}
		if current[i].Ref.Kind != current[j].Ref.Kind {
			return current[i].Ref.Kind < current[j].Ref.Kind
		}
		return current[i].Ref.RowID < current[j].Ref.RowID
	})
	return current
}

func resolverClaimSortKey(claim ResolvedClaim) string {
	evidenceKeys := make([]string, len(claim.Evidence))
	for index := range claim.Evidence {
		evidenceKeys[index] = resolverEvidenceKey(claim.Evidence[index])
	}
	sort.Strings(evidenceKeys)
	return claim.Claim.Target.Key + "\x00" + normalizedFingerprint(claim.Normalized) + "\x00" +
		strings.Join(evidenceKeys, "\x00") + "\x00" + claim.ClaimKey
}

func resolverTargetRef(target TargetDescriptor) TargetRef {
	return TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
}

func validateResolverPolicy(policy Policy) error {
	if policy.Version == "" {
		return errors.New("resolver policy version is required")
	}
	if policy.ApplyThreshold < 0 || policy.ReplacementMargin < 0 || policy.HysteresisMargin < 0 ||
		policy.MinimumIdentityScore < 0 || policy.MinimumIdentityScore > 1000 ||
		policy.MaxCorroborationBonus < 0 {
		return errors.New("resolver policy contains an out-of-range value")
	}
	if err := validatePolicyMap(policy.SourceClassWeights,
		[]EvidenceSourceClass{EvidenceArchive, EvidencePublic, EvidenceSystem, EvidenceProviderAssertion}); err != nil {
		return fmt.Errorf("source class weights: %w", err)
	}
	if err := validatePolicyMap(policy.DirectnessWeights,
		[]EvidenceDirectness{DirectSelf, DirectOther, Indirect}); err != nil {
		return fmt.Errorf("directness weights: %w", err)
	}
	if err := validatePolicyMap(policy.AuthorityWeights,
		[]EvidenceAuthority{AuthorityAuthoritative, AuthorityOrdinary, AuthorityAggregator}); err != nil {
		return fmt.Errorf("authority weights: %w", err)
	}
	if policy.Version == ResolverVersionV1 && !sameResolverPolicy(policy, DefaultPolicyV1()) {
		return errors.New("resolver-v1 policy must match its frozen definition")
	}
	return nil
}

func sameResolverPolicy(left, right Policy) bool {
	return left.Version == right.Version &&
		left.ApplyThreshold == right.ApplyThreshold &&
		left.ReplacementMargin == right.ReplacementMargin &&
		left.HysteresisMargin == right.HysteresisMargin &&
		left.MinimumIdentityScore == right.MinimumIdentityScore &&
		left.MaxCorroborationBonus == right.MaxCorroborationBonus &&
		samePolicyMap(left.SourceClassWeights, right.SourceClassWeights) &&
		samePolicyMap(left.DirectnessWeights, right.DirectnessWeights) &&
		samePolicyMap(left.AuthorityWeights, right.AuthorityWeights)
}

func samePolicyMap[K comparable](left, right map[K]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func validResolverRejectionFailure(failure *ValidationFailure) bool {
	if !validValidationFailure(failure) {
		return false
	}
	switch failure.Action {
	case DecisionInvalid:
		return failure.Reason == ReasonMalformedValue ||
			failure.Reason == ReasonUnsupportedTarget ||
			failure.Reason == ReasonStaleTargetRevision ||
			failure.Reason == ReasonUnalignedEvidence
	case DecisionIdentityRejected:
		return failure.Reason == ReasonIdentityMismatch
	case DecisionPolicyRejected:
		return failure.Reason == ReasonSensitivePolicy
	default:
		return false
	}
}

func validatePolicyMap[K comparable](weights map[K]int, allowed []K) error {
	if len(weights) != len(allowed) {
		return errors.New("weight map must contain exactly the closed vocabulary")
	}
	for _, key := range allowed {
		weight, exists := weights[key]
		if !exists || weight < 0 {
			return errors.New("weight map is missing a key or contains a negative weight")
		}
	}
	return nil
}

func validateResolutionInput(input ResolutionInput) error {
	if input.PersonID <= 0 {
		return errors.New("resolution person id must be positive")
	}
	if input.ResolvedAt.IsZero() || !isUTC(input.ResolvedAt) {
		return errors.New("resolution time must be nonzero UTC")
	}
	if input.Target.Key == "" || input.Target.Revision == "" {
		return errors.New("resolution target key and revision are required")
	}
	if validUnsupportedTargetRejectionInput(input) {
		return nil
	}
	if input.Target.Kind != TargetAttribute && input.Target.Kind != TargetEmployment {
		return fmt.Errorf("unsupported resolution target kind %q", input.Target.Kind)
	}
	if input.Target.Cardinality != CardinalitySingle && input.Target.Cardinality != CardinalityMulti {
		return fmt.Errorf("unsupported resolution cardinality %q", input.Target.Cardinality)
	}
	if input.Target.Kind == TargetEmployment {
		if input.Target.ValueType != ValueEmployment || input.Target.Cardinality != CardinalityMulti {
			return errors.New("employment resolution target must be a multi-value employment target")
		}
	} else {
		switch input.Target.ValueType {
		case ValueText, ValueInteger, ValueReal, ValueBoolean, ValueDate, ValueTimestamp:
		default:
			return fmt.Errorf("unsupported attribute resolution value type %q", input.Target.ValueType)
		}
	}
	if input.Pin.Target != (TargetRef{}) && input.Pin.Target != resolverTargetRef(input.Target) {
		return errors.New("pin target does not match resolution target")
	}
	if input.Pin.EventID != nil && *input.Pin.EventID <= 0 {
		return errors.New("pin event id must be positive")
	}
	return nil
}

func validUnsupportedTargetRejectionInput(input ResolutionInput) bool {
	if len(input.Current) != 0 || input.Pin != (PinState{}) || len(input.Claims) == 0 {
		return false
	}
	for _, claim := range input.Claims {
		if claim.Failure == nil || claim.Failure.Action != DecisionInvalid ||
			claim.Failure.Reason != ReasonUnsupportedTarget {
			return false
		}
	}
	return true
}

func copyResolverPolicy(policy Policy) Policy {
	cloned := policy
	cloned.SourceClassWeights = copyPolicyMap(policy.SourceClassWeights)
	cloned.DirectnessWeights = copyPolicyMap(policy.DirectnessWeights)
	cloned.AuthorityWeights = copyPolicyMap(policy.AuthorityWeights)
	return cloned
}

func copyPolicyMap[K comparable](input map[K]int) map[K]int {
	cloned := make(map[K]int, len(input))
	maps.Copy(cloned, input)
	return cloned
}

type resolverFingerprintStatus struct {
	PersonID      int64                `json:"person_id"`
	EvidenceKey   string               `json:"evidence_key"`
	SourceVersion string               `json:"source_version"`
	Supported     bool                 `json:"supported"`
	EventID       int64                `json:"latest_status_event_id"`
	Reason        EvidenceStatusReason `json:"reason"`
}

type resolverFingerprintEvidence struct {
	Key    string                    `json:"key"`
	Input  canonicalEvidence         `json:"input"`
	Status resolverFingerprintStatus `json:"effective_status"`
}

type resolverFingerprintClaim struct {
	ClaimKey   string                        `json:"claim_key"`
	Target     TargetRef                     `json:"target"`
	Relation   ClaimRelation                 `json:"relation"`
	Submitted  string                        `json:"submitted"`
	Normalized *canonicalNormalized          `json:"normalized"`
	Evidence   []resolverFingerprintEvidence `json:"evidence"`
	ValidFrom  *string                       `json:"valid_from"`
	ValidUntil *string                       `json:"valid_until"`
	Origin     ClaimOrigin                   `json:"origin"`
	Confidence ConfidenceInputs              `json:"confidence"`
	Failure    *canonicalFailure             `json:"failure"`
	sortKey    string
}

type resolverFingerprintCurrent struct {
	Ref             ProjectionRef       `json:"ref"`
	Normalized      canonicalNormalized `json:"normalized"`
	ActiveFrom      string              `json:"active_from"`
	ActiveUntil     *string             `json:"active_until"`
	TransactionTime string              `json:"transaction_time"`
	Declared        bool                `json:"declared"`
}

func resolverInputFingerprint(
	input ResolutionInput, policy Policy, statuses map[string]effectiveEvidenceStatus,
) (string, error) {
	current := make([]resolverFingerprintCurrent, len(input.Current))
	for index, item := range input.Current {
		current[index] = resolverFingerprintCurrent{
			Ref: item.Ref,
			Normalized: canonicalNormalized{JSON: append(json.RawMessage(nil), item.Normalized.JSON...),
				Fingerprint: item.Normalized.Fingerprint},
			ActiveFrom:      portableFactTime(item.ActiveFrom).Format(time.RFC3339Nano),
			ActiveUntil:     canonicalTimePointer(item.ActiveUntil),
			TransactionTime: portableFactTime(item.TransactionTime).Format(time.RFC3339Nano), Declared: item.Declared,
		}
	}
	sort.Slice(current, func(i, j int) bool {
		left, _ := json.Marshal(current[i])
		right, _ := json.Marshal(current[j])
		return string(left) < string(right)
	})

	claims := make([]resolverFingerprintClaim, len(input.Claims))
	for index, item := range input.Claims {
		view := resolverFingerprintClaim{
			ClaimKey: item.ClaimKey, Target: resolverTargetRef(item.Claim.Target),
			Relation: item.Claim.Relation, Submitted: resolverSubmittedValue(item.Claim.SubmittedValue),
			ValidFrom: canonicalTimePointer(item.Claim.ValidFrom), ValidUntil: canonicalTimePointer(item.Claim.ValidUntil),
			Origin: item.Claim.Origin, Confidence: item.Claim.Confidence,
			Evidence: []resolverFingerprintEvidence{},
			sortKey:  resolverClaimSortKey(item),
		}
		if item.Normalized != nil {
			view.Normalized = &canonicalNormalized{
				JSON: append(json.RawMessage(nil), item.Normalized.JSON...), Fingerprint: item.Normalized.Fingerprint,
			}
		}
		if item.Failure != nil {
			view.Failure = &canonicalFailure{
				Action: item.Failure.Action, Reason: item.Failure.Reason, Detail: item.Failure.Detail,
			}
		}
		for _, evidence := range item.Evidence {
			status := statuses[effectiveEvidenceTupleKey(evidence)]
			view.Evidence = append(view.Evidence, resolverFingerprintEvidence{
				Key: resolverEvidenceKey(evidence), Input: evidenceKeyView(evidence.Input),
				Status: resolverFingerprintStatus{
					PersonID: status.personID, EvidenceKey: status.key, SourceVersion: status.sourceVersion,
					Supported: status.supported, EventID: status.eventID, Reason: status.reason,
				},
			})
		}
		sort.Slice(view.Evidence, func(i, j int) bool {
			left, _ := json.Marshal(view.Evidence[i])
			right, _ := json.Marshal(view.Evidence[j])
			return string(left) < string(right)
		})
		claims[index] = view
	}
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].sortKey < claims[j].sortKey
	})

	encoded, err := json.Marshal(struct {
		PersonID                     int64                        `json:"person_id"`
		Target                       TargetDescriptor             `json:"target"`
		ResolvedAt                   string                       `json:"resolved_at"`
		Policy                       Policy                       `json:"resolver_policy"`
		PolicyContext                PolicyContext                `json:"policy_context"`
		ProjectionContextFingerprint string                       `json:"projection_context_fingerprint"`
		Current                      []resolverFingerprintCurrent `json:"current"`
		Claims                       []resolverFingerprintClaim   `json:"claims"`
		Pin                          PinState                     `json:"pin"`
	}{
		PersonID: input.PersonID, Target: canonicalTarget(input.Target),
		ResolvedAt: portableFactTime(input.ResolvedAt).Format(time.RFC3339Nano), Policy: policy,
		PolicyContext: input.Policy, ProjectionContextFingerprint: input.ProjectionContextFingerprint,
		Current: current, Claims: claims, Pin: input.Pin,
	})
	if err != nil {
		return "", fmt.Errorf("encode resolver input fingerprint: %w", err)
	}
	return fingerprint(encoded), nil
}

func resolverSubmittedValue(value json.RawMessage) string {
	canonical, err := canonicalizeRawJSON(value)
	if err != nil {
		return string(value)
	}
	return string(canonical)
}
