package personobs

import (
	"slices"
	"strings"
	"unicode/utf8"

	"go.kenn.io/msgvault/internal/personfacts"
)

func invalid(field, reason string) *ValidationError {
	return &ValidationError{Field: field, Reason: reason}
}

// ValidateProposal checks the pure contract. Store preparation additionally
// aligns evidence and binds it to the generation before any transaction opens.
func ValidateProposal(p ProposedObservation, premises map[int64]Observation) *ValidationError {
	if !slices.Contains([]Level{LevelExplicit, LevelDeductive, LevelInductive, LevelContradiction}, p.Level) {
		return invalid("level", "level")
	}
	if !slices.Contains([]Origin{OriginExtraction, OriginOwner, OriginReasoning}, p.Origin) {
		return invalid("origin", "origin")
	}
	if (p.Level == LevelExplicit && p.Origin == OriginReasoning) || (p.Level != LevelExplicit && p.Origin != OriginReasoning) {
		return invalid("origin", "origin-level")
	}
	if !slices.Contains([]Kind{KindState, KindEvent, KindPlan, KindPreference, KindRelationship, KindPattern, KindOther}, p.Kind) {
		return invalid("kind", "kind")
	}
	if p.Level == LevelInductive && p.Kind != KindPattern {
		return invalid("kind", "inductive-kind")
	}
	content := CanonicalizePlaceholders(p.Content)
	if hasObservationControl(content) {
		return invalid("content", "content-control-character")
	}
	if hasObservationControl(p.SubjectRef) {
		return invalid("subject_ref", "subject-control-character")
	}
	if n := utf8.RuneCountInString(content); n < 1 || n > 500 {
		return invalid("content", "content-length")
	}
	if !strings.Contains(content, "[PERSON]") {
		return invalid("content", "missing-person-token")
	}
	if utf8.RuneCountInString(p.SubjectRef) > 64 {
		return invalid("subject_ref", "subject-length")
	}
	if p.ConfidenceBP < 0 || p.ConfidenceBP > 1000 {
		return invalid("confidence_bp", "confidence-range")
	}
	if p.ValidFrom != nil && p.ValidUntil != nil && p.ValidUntil.Before(*p.ValidFrom) {
		return invalid("valid_until", "validity-order")
	}
	if personfacts.ValidatePartialDate(p.OccursOn) != nil {
		return invalid("occurs_on", "occurs-on")
	}
	if (p.TargetKind == "") != (p.TargetKey == "") {
		return invalid("target_key", "target-pair")
	}
	if p.Stability != "" && !slices.Contains([]Stability{StabilityFixed, StabilityDurable, StabilityVolatile, StabilityEpisodic}, p.Stability) {
		return invalid("stability", "stability")
	}
	for _, c := range p.ModelCategories {
		if !c.Valid() {
			return invalid("categories", "category")
		}
	}
	if p.ContextCount > 0 || p.Attribution == AttributionAssent || p.Attribution == AttributionReported {
		return invalid("context", "context-citations-unavailable")
	}
	if p.Level == LevelExplicit {
		if len(p.Premises) > 0 {
			return invalid("premises", "explicit-has-premises")
		}
		if p.Origin != OriginOwner && len(p.Support) == 0 {
			return invalid("support", "explicit-requires-support")
		}
		if p.Origin != OriginOwner && p.Attribution != AttributionSelf {
			return invalid("attribution", "attribution")
		}
	} else {
		if len(p.Support) > 0 {
			return invalid("support", "derived-has-support")
		}
		seen := map[int64]bool{}
		support := 0
		supported := 0
		for _, edge := range p.Premises {
			if seen[edge.ID] {
				return invalid("premises", "premise-duplicate")
			}
			seen[edge.ID] = true
			premise, ok := premises[edge.ID]
			if !ok {
				return invalid("premises", "premise-missing")
			}
			if e := validateEdge(Observation{PersonID: p.PersonID, Level: p.Level}, premise, edge.Role, false); e != nil {
				return e
			}
			if edge.Role == PremiseRoleSupport {
				support++
			}
			if PremiseSupported(premise) {
				supported++
			}
		}
		count := support
		minCount := 1
		if p.Level == LevelInductive {
			minCount = 2
		}
		if p.Level == LevelContradiction {
			count = supported
			minCount = 2
		}
		if count < minCount {
			return invalid("premises", "premise-count")
		}
	}
	for _, s := range p.Support {
		if hasObservationControl(s.Quote) {
			return invalid("support", "quote-control-character")
		}
		if hasObservationControl(s.Evidence.Excerpt) {
			return invalid("support", "excerpt-control-character")
		}
		excerpt, quote := []rune(s.Evidence.Excerpt), []rune(s.Quote)
		if s.QuoteStart < 0 || len(quote) == 0 || s.QuoteStart > len(excerpt) || len(quote) > len(excerpt)-s.QuoteStart {
			return invalid("support", "quote-out-of-range")
		}
		if string(excerpt[s.QuoteStart:s.QuoteStart+len(quote)]) != s.Quote {
			return invalid("support", "quote-out-of-range")
		}
	}
	return nil
}
func ValidateAppend(derived, premise Observation, role PremiseRole) *ValidationError {
	return validateEdge(derived, premise, role, true)
}
func validateEdge(derived, premise Observation, role PremiseRole, checkOrder bool) *ValidationError {
	if derived.PersonID != premise.PersonID {
		return invalid("premises", "premise-person")
	}
	if derived.Level == LevelExplicit {
		return invalid("premises", "explicit-has-premises")
	}
	if role != PremiseRoleSupport && role != PremiseRoleCounter {
		return invalid("premises", "premise-role")
	}
	if premise.Level == LevelContradiction || (derived.Level != LevelContradiction && premise.Level == LevelInductive) {
		return invalid("premises", "premise-level")
	}
	if checkOrder && (derived.ID == premise.ID || (derived.Level == LevelDeductive && premise.Level == LevelDeductive && premise.ID >= derived.ID)) {
		return invalid("premises", "premise-order")
	}
	if role == PremiseRoleSupport && !PremiseSupported(premise) {
		return invalid("premises", "premise-not-supported")
	}
	if role == PremiseRoleCounter && premise.Status != StatusActive && premise.Status != StatusUnsupported {
		return invalid("premises", "premise-not-live")
	}
	return nil
}

func hasObservationControl(text string) bool {
	return strings.ContainsFunc(text, func(r rune) bool {
		return r < 32 && r != '\t' && r != '\n'
	})
}
