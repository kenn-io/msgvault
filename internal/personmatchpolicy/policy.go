// Package personmatchpolicy holds the local, versioned guard for proposed
// identity matches. A provider probability cannot relax these guards.
package personmatchpolicy

import "math"

const Version = "person-match-policy-v1"

type SignalClass string

const (
	SignalStableID         SignalClass = "stable_provider_id"
	SignalEmail            SignalClass = "email"
	SignalPhone            SignalClass = "phone"
	SignalSelfDeclaration  SignalClass = "self_declaration"
	SignalDisplayName      SignalClass = "display_name"
	SignalVerifiedFullName SignalClass = "verified_full_name"
	SignalConversation     SignalClass = "conversation"
)

type Signal struct {
	Class    SignalClass `json:"class"`
	SourceID int64       `json:"source_id"`
}

type Input struct {
	Probability              float64
	MinimumProbability       float64
	Signals                  []Signal
	UserRejected             bool
	ConflictingStableID      bool
	SharedContactPoint       bool
	ActiveCardDAVPublication bool
	StaleRevision            bool
	ManualMergeReview        bool
	IncompleteGuardData      bool
}

type Action string

const (
	NeedsReview    Action = "needs_review"
	ProposedAccept Action = "proposed_accept"
)

type Decision struct {
	Action   Action   `json:"action"`
	Blockers []string `json:"blockers"`
}

// Evaluate describes a proposed action only. It never mutates identities.
func Evaluate(in Input) Decision {
	d := Decision{Action: NeedsReview, Blockers: []string{}}
	if math.IsNaN(in.Probability) || math.IsInf(in.Probability, 0) ||
		in.Probability < 0 || in.Probability > 1 || in.Probability <= in.MinimumProbability {
		d.Blockers = append(d.Blockers, "probability_below_threshold")
	}
	if in.UserRejected {
		d.Blockers = append(d.Blockers, "user_rejected")
	}
	if in.ConflictingStableID {
		d.Blockers = append(d.Blockers, "conflicting_stable_id")
	}
	if in.SharedContactPoint {
		d.Blockers = append(d.Blockers, "shared_contact_point")
	}
	if in.ActiveCardDAVPublication {
		d.Blockers = append(d.Blockers, "active_carddav_publication")
	}
	if in.StaleRevision {
		d.Blockers = append(d.Blockers, "stale_revision")
	}
	if in.ManualMergeReview {
		d.Blockers = append(d.Blockers, "manual_merge_review")
	}
	if in.IncompleteGuardData {
		d.Blockers = append(d.Blockers, "incomplete_guard_data")
	}
	classes := map[SignalClass]bool{}
	sources := map[int64]bool{}
	for _, signal := range in.Signals {
		if signal.SourceID <= 0 {
			continue
		}
		switch signal.Class {
		case SignalStableID, SignalEmail, SignalPhone, SignalSelfDeclaration, SignalVerifiedFullName:
			classes[signal.Class] = true
			sources[signal.SourceID] = true
		case SignalDisplayName, SignalConversation:
			// Neither class can independently corroborate identity.
		}
	}
	if classes[SignalVerifiedFullName] && !classes[SignalStableID] && !classes[SignalEmail] &&
		!classes[SignalPhone] && !classes[SignalSelfDeclaration] {
		delete(classes, SignalVerifiedFullName)
	}
	if len(classes) < 2 || len(sources) < 2 {
		d.Blockers = append(d.Blockers, "independent_identity_evidence_required")
	}
	if len(d.Blockers) == 0 {
		d.Action = ProposedAccept
	}
	return d
}
