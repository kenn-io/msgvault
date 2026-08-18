package beeper

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/store"
)

// Identity matching.
//
// The matching policy is deliberately asymmetric:
//
//  1. the same stable provider or Beeper user ID may resolve automatically;
//  2. a service-and-scope username match creates a candidate;
//  3. matching phone, email, display name, or conversation membership may add
//     evidence but cannot confirm a username-based link;
//  4. only the same stable provider/Beeper ID or explicit user confirmation
//     may accept a candidate;
//  5. the same username under different stable IDs is a conflict, never an
//     automatic merge.
//
// All five rungs are implemented here. The weak-evidence rungs only create or
// explain reviewable candidates; they never accept or link them.

const (
	evidenceProviderID = "stable_provider_id"
	evidenceUsername   = "service_scope_username"
	evidencePhone      = "phone"
	evidenceEmail      = "email"
	evidenceName       = "display_name"
	evidenceMembership = "conversation_membership"
)

// participantPair is canonically ordered, so A-B and B-A share one run-local
// memo the way they share one candidate row.
type participantPair struct{ lo, hi int64 }

func newParticipantPair(a, b int64) participantPair {
	if a > b {
		return participantPair{lo: b, hi: a}
	}
	return participantPair{lo: a, hi: b}
}

type optionalMatchValue struct {
	value string
	set   bool
}

type identityMatchMemoKey struct {
	pair            participantPair
	basis           store.IdentityMatchBasis
	serviceSlug     optionalMatchValue
	scopeKind       optionalMatchValue
	scopeValue      optionalMatchValue
	normalizedValue optionalMatchValue
}

func newIdentityMatchMemoKey(
	pair participantPair,
	basis store.IdentityMatchBasis,
	serviceSlug, scopeKind, scopeValue, normalizedValue *string,
) identityMatchMemoKey {
	return identityMatchMemoKey{
		pair:            pair,
		basis:           basis,
		serviceSlug:     newOptionalMatchValue(serviceSlug),
		scopeKind:       newOptionalMatchValue(scopeKind),
		scopeValue:      newOptionalMatchValue(scopeValue),
		normalizedValue: newOptionalMatchValue(normalizedValue),
	}
}

func newOptionalMatchValue(value *string) optionalMatchValue {
	if value == nil {
		return optionalMatchValue{}
	}
	return optionalMatchValue{value: *value, set: true}
}

// matchOutcome reports the candidate IDs one observation produced, split by
// what happened to them.
type matchOutcome struct {
	AutoResolved []int64
	Suggested    []int64
	Conflicts    []int64
}

type identityMatcher struct {
	store *store.Store
	// resolved memoizes candidate identities already handled this run.
	resolved map[identityMatchMemoKey]struct{}
}

func newIdentityMatcher(s *store.Store) *identityMatcher {
	return &identityMatcher{
		store:    s,
		resolved: map[identityMatchMemoKey]struct{}{},
	}
}

// match evaluates one recorded observation against the ladder.
func (m *identityMatcher) match(
	ctx context.Context, participantID int64, result *store.RecordContactObservationResult,
) (matchOutcome, error) {
	var outcome matchOutcome
	if result == nil || result.Observation == nil || participantID == 0 {
		return outcome, nil
	}
	observation := result.Observation

	if result.Conflicting {
		candidateIDs := result.CandidateIDs
		if len(candidateIDs) == 0 && result.CandidateID != nil {
			candidateIDs = []int64{*result.CandidateID}
		}
		for _, candidateID := range candidateIDs {
			outcome.Conflicts = append(outcome.Conflicts, candidateID)
			if err := m.explainConflict(
				ctx, participantID, candidateID, observation.SourceID,
			); err != nil {
				return outcome, err
			}
		}
	}
	if observation.ProviderUserID != nil {
		if err := m.matchStableProviderID(ctx, participantID, observation, &outcome); err != nil {
			return outcome, err
		}
	}
	if err := m.matchServiceScopeUsername(ctx, participantID, observation, &outcome); err != nil {
		return outcome, err
	}
	return outcome, nil
}

// matchServiceScopeUsername is rung 2: it suggests, never confirms.
//
// A username collision inside one service and scope is already a conflict, and
// a shared stable ID already went through rung 1. What remains is the same
// username on the same service under a different scope. That exact but weak
// match becomes a candidate a user must accept.
func (m *identityMatcher) matchServiceScopeUsername(
	ctx context.Context,
	participantID int64,
	observation *store.ParticipantContactObservation,
	outcome *matchOutcome,
) error {
	if observation.AddressKind != store.ContactAddressUsername ||
		observation.ServiceSlug == nil {
		return nil
	}
	others, err := m.store.FindObservationsByServiceValueContext(
		ctx, store.ContactAddressUsername, *observation.ServiceSlug,
		observation.NormalizedValue, 0)
	if err != nil {
		return err
	}
	seen := map[int64]struct{}{participantID: {}}
	for i := range others {
		other := &others[i]
		if _, duplicate := seen[other.ParticipantID]; duplicate {
			continue
		}

		// Same scope belongs to the collision/corroboration path.
		if equalStringPtr(other.ScopeKind, observation.ScopeKind) &&
			equalStringPtr(other.ScopeValue, observation.ScopeValue) {
			continue
		}
		// Same stable ID belongs to rung 1.
		if other.ProviderUserID != nil && observation.ProviderUserID != nil &&
			*other.ProviderUserID == *observation.ProviderUserID {
			continue
		}
		seen[other.ParticipantID] = struct{}{}
		pair := newParticipantPair(participantID, other.ParticipantID)
		memoKey := newIdentityMatchMemoKey(
			pair, store.IdentityMatchServiceScopeUsername,
			observation.ServiceSlug, nil, nil, &observation.NormalizedValue,
		)
		if _, done := m.resolved[memoKey]; done {
			continue
		}

		candidate, _, err := m.store.UpsertIdentityMatchCandidateContext(
			ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: participantID,
				RightKind: store.IdentityMatchParticipant, RightID: other.ParticipantID,
				Basis:           store.IdentityMatchServiceScopeUsername,
				ServiceSlug:     observation.ServiceSlug,
				CrossScope:      true,
				NormalizedValue: &observation.NormalizedValue,
				State:           store.IdentityMatchStateCandidate,
				Source:          store.ProvenanceArchiveObservation,
				SourceRef:       observation.Envelope.SourceRef,
				SourceID:        observation.SourceID,
			})
		if err != nil {
			return err
		}
		if err := m.attachCandidateSources(
			ctx, candidate.ID, observation.SourceID,
			observationSourceIDsForParticipants(
				others, participantID, other.ParticipantID,
			)...,
		); err != nil {
			return err
		}
		switch candidate.State {
		case store.IdentityMatchStateRejected:
			m.resolved[memoKey] = struct{}{}
			continue
		case store.IdentityMatchStateConflict:
			outcome.Conflicts = append(outcome.Conflicts, candidate.ID)
			m.resolved[memoKey] = struct{}{}
			continue
		case store.IdentityMatchStateCandidate, store.IdentityMatchStateAccepted:
		}

		detail := *observation.ServiceSlug + "/" + observation.NormalizedValue +
			" across " + derefString(observation.ScopeValue) +
			" and " + derefString(other.ScopeValue)
		if err := m.addEvidence(
			ctx, candidate, evidenceUsername, detail, observation.SourceID,
			observationSourceIDsForParticipants(
				others, participantID, other.ParticipantID,
			)...,
		); err != nil {
			return err
		}
		if err := m.gatherEvidence(
			ctx, candidate, participantID, other.ParticipantID, observation.SourceID,
		); err != nil {
			return err
		}
		if candidate.State == store.IdentityMatchStateCandidate {
			outcome.Suggested = append(outcome.Suggested, candidate.ID)
		}
		m.resolved[memoKey] = struct{}{}
	}
	return nil
}

// explainConflict attaches evidence to a collision candidate without changing
// its conflict state.
func (m *identityMatcher) explainConflict(
	ctx context.Context, participantID, candidateID int64, sourceID *int64,
) error {
	candidate, err := m.store.GetIdentityMatchCandidateContext(ctx, candidateID)
	if err != nil {
		return err
	}
	other := candidate.LeftID
	if other == participantID {
		other = candidate.RightID
	}
	if other == participantID {
		return nil
	}
	return m.gatherEvidence(ctx, candidate, participantID, other, sourceID)
}

// gatherEvidence is rung 3: phone, email, display name, and shared
// conversations explain an existing candidate. It creates and accepts none.
func (m *identityMatcher) gatherEvidence(
	ctx context.Context, candidate *store.IdentityMatchCandidate, a, b int64,
	sourceID *int64,
) error {
	if candidate == nil || a == b {
		return nil
	}
	left, err := m.store.ListParticipantObservationsContext(ctx, a, true)
	if err != nil {
		return err
	}
	right, err := m.store.ListParticipantObservationsContext(ctx, b, true)
	if err != nil {
		return err
	}
	contributingSources := observationSourceIDs(left, right)
	for _, pairing := range []struct {
		kind         store.ContactAddressKind
		evidenceKind string
	}{
		{kind: store.ContactAddressPhone, evidenceKind: evidencePhone},
		{kind: store.ContactAddressEmail, evidenceKind: evidenceEmail},
	} {
		for _, shared := range sharedNormalizedValues(left, right, pairing.kind) {
			supportingSources := observationSourceIDsForValue(
				left, right, pairing.kind, shared,
			)
			if err := m.addEvidence(
				ctx, candidate, pairing.evidenceKind, shared, sourceID,
				supportingSources...,
			); err != nil {
				return err
			}
		}
	}

	names, err := m.store.ParticipantDisplayNamesContext(ctx, []int64{a, b})
	if err != nil {
		return err
	}
	if nameA, nameB := names[a], names[b]; nameA != "" && strings.EqualFold(nameA, nameB) {
		if err := m.addEvidence(
			ctx, candidate, evidenceName, nameA, sourceID, contributingSources...,
		); err != nil {
			return err
		}
	}

	shared, err := m.store.SharedConversationsContext(ctx, a, b)
	if err != nil {
		return err
	}
	for _, conversation := range shared {
		reference := "conversation:" + strconv.FormatInt(conversation.ConversationID, 10)
		detail := "Shared conversation"
		if conversation.SourceConversationID != "" {
			detail += " " + conversation.SourceConversationID
		}
		conversationSourceID := conversation.SourceID
		if err := m.addEvidenceWithReference(
			ctx, candidate, evidenceMembership, reference, detail,
			&conversationSourceID,
		); err != nil {
			return err
		}
	}
	return nil
}

// sharedNormalizedValues returns the normalized values both participants
// currently show for one address kind, sorted deterministically.
func sharedNormalizedValues(
	left, right []store.ParticipantContactObservation, kind store.ContactAddressKind,
) []string {
	rightValues := make(map[string]struct{}, len(right))
	for i := range right {
		if right[i].AddressKind == kind {
			rightValues[right[i].NormalizedValue] = struct{}{}
		}
	}
	var shared []string
	for i := range left {
		if left[i].AddressKind != kind {
			continue
		}
		if _, ok := rightValues[left[i].NormalizedValue]; ok {
			shared = append(shared, left[i].NormalizedValue)
		}
	}
	slices.Sort(shared)
	return slices.Compact(shared)
}

// matchStableProviderID is rung 1: the only rung that may link automatically.
//
// The lookup is exact and namespaced, the fan-out is per participant rather
// than per row, and a rejected or already-decided candidate is left exactly as
// the user left it.
func (m *identityMatcher) matchStableProviderID(
	ctx context.Context,
	participantID int64,
	observation *store.ParticipantContactObservation,
	outcome *matchOutcome,
) error {
	others, err := m.store.FindObservationsByProviderUserIDContext(
		ctx, *observation.ProviderUserID, 0)
	if err != nil {
		return err
	}
	seen := map[int64]struct{}{participantID: {}}
	for i := range others {
		other := others[i].ParticipantID
		if _, duplicate := seen[other]; duplicate {
			continue
		}
		seen[other] = struct{}{}

		pair := newParticipantPair(participantID, other)
		memoKey := newIdentityMatchMemoKey(
			pair, store.IdentityMatchStableProviderID,
			observation.ServiceSlug, observation.ScopeKind, observation.ScopeValue,
			observation.ProviderUserID,
		)
		if _, done := m.resolved[memoKey]; done {
			continue
		}

		candidate, _, err := m.store.UpsertIdentityMatchCandidateContext(
			ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: participantID,
				RightKind: store.IdentityMatchParticipant, RightID: other,
				Basis:           store.IdentityMatchStableProviderID,
				ServiceSlug:     observation.ServiceSlug,
				ScopeKind:       observation.ScopeKind,
				ScopeValue:      observation.ScopeValue,
				NormalizedValue: observation.ProviderUserID,
				State:           store.IdentityMatchStateCandidate,
				Source:          store.ProvenanceArchiveObservation,
				SourceRef:       observation.Envelope.SourceRef,
				SourceID:        observation.SourceID,
			})
		if err != nil {
			return err
		}
		if err := m.attachCandidateSources(
			ctx, candidate.ID, observation.SourceID,
			observationSourceIDsForParticipants(others, participantID, other)...,
		); err != nil {
			return err
		}
		switch candidate.State {
		case store.IdentityMatchStateRejected:
			m.resolved[memoKey] = struct{}{}
			continue
		case store.IdentityMatchStateConflict:
			outcome.Conflicts = append(outcome.Conflicts, candidate.ID)
			m.resolved[memoKey] = struct{}{}
			continue
		case store.IdentityMatchStateCandidate, store.IdentityMatchStateAccepted:
			if err := m.addEvidence(
				ctx, candidate, evidenceProviderID, *observation.ProviderUserID,
				observation.SourceID,
				observationSourceIDsForParticipants(others, participantID, other)...,
			); err != nil {
				return err
			}
		}

		var accepted *store.IdentityMatchCandidate
		linked := true
		if candidate.State == store.IdentityMatchStateAccepted {
			accepted, _, linked, err = m.store.ResumeAcceptedIdentityMatchCandidateContext(
				ctx, candidate.ID)
		} else {
			accepted, _, err = m.store.AcceptIdentityMatchCandidateContext(
				ctx, candidate.ID, "system", nil)
		}
		switch {
		case err == nil:
			m.resolved[memoKey] = struct{}{}
			if linked {
				outcome.AutoResolved = append(outcome.AutoResolved, accepted.ID)
			}
		case errors.Is(err, store.ErrPersonBindingConflict):
			slog.Warn("skipping automatic identity match across durable persons",
				"candidate_id", candidate.ID,
				"left_participant_id", candidate.LeftID,
				"right_participant_id", candidate.RightID)
			outcome.Conflicts = append(outcome.Conflicts, candidate.ID)
			m.resolved[memoKey] = struct{}{}
		case errors.Is(err, store.ErrIdentityMatchNotAccepted),
			errors.Is(err, store.ErrIdentityMatchNotFound):
			if staleErr := m.handleStaleAcceptedMatch(
				ctx, candidate, memoKey, outcome,
			); staleErr != nil {
				return staleErr
			}
		case errors.Is(err, store.ErrIdentityMatchRejected):
			// A user rejection won the identity lock after this importer
			// observed the candidate. It is a durable decision, not an
			// import error, and must not be retried by another observation in
			// this run.
			m.resolved[memoKey] = struct{}{}
		default:
			return err
		}
	}
	return nil
}

// handleStaleAcceptedMatch converts a concurrent review or participant merge
// into the current durable outcome. It must not fail the surrounding import
// merely because the accepted snapshot changed while recovery waited for the
// shared identity lock.
func (m *identityMatcher) handleStaleAcceptedMatch(
	ctx context.Context,
	candidate *store.IdentityMatchCandidate,
	memoKey identityMatchMemoKey,
	outcome *matchOutcome,
) error {
	current, err := m.store.GetIdentityMatchCandidateContext(ctx, candidate.ID)
	if errors.Is(err, store.ErrIdentityMatchNotFound) {
		m.resolved[memoKey] = struct{}{}
		return nil
	}
	if err != nil {
		return err
	}
	if current.State == store.IdentityMatchStateAccepted {
		// The decision changed again after the guarded application returned.
		// Leave the pair retryable for another observation in this run.
		return nil
	}
	if current.State == store.IdentityMatchStateConflict {
		outcome.Conflicts = append(outcome.Conflicts, current.ID)
	}
	m.resolved[memoKey] = struct{}{}
	return nil
}

func (m *identityMatcher) attachCandidateSources(
	ctx context.Context, candidateID int64, sourceID *int64, additional ...int64,
) error {
	for _, supportingSourceID := range uniqueSourceIDs(sourceID, additional...) {
		if err := m.store.AttachIdentityMatchCandidateSourceContext(
			ctx, candidateID, supportingSourceID,
		); err != nil {
			return err
		}
	}
	return nil
}

func uniqueSourceIDs(primary *int64, additional ...int64) []int64 {
	seen := make(map[int64]struct{}, len(additional)+1)
	ids := make([]int64, 0, len(additional)+1)
	add := func(id int64) {
		if id == 0 {
			return
		}
		if _, exists := seen[id]; exists {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if primary != nil {
		add(*primary)
	}
	for _, id := range additional {
		add(id)
	}
	return ids
}

func observationSourceIDs(
	groups ...[]store.ParticipantContactObservation,
) []int64 {
	var ids []int64
	for _, group := range groups {
		for i := range group {
			if group[i].SourceID != nil {
				ids = append(ids, *group[i].SourceID)
			}
		}
	}
	return uniqueSourceIDs(nil, ids...)
}

func observationSourceIDsForParticipants(
	observations []store.ParticipantContactObservation, participantIDs ...int64,
) []int64 {
	wanted := make(map[int64]struct{}, len(participantIDs))
	for _, participantID := range participantIDs {
		wanted[participantID] = struct{}{}
	}
	var ids []int64
	for i := range observations {
		if _, ok := wanted[observations[i].ParticipantID]; ok && observations[i].SourceID != nil {
			ids = append(ids, *observations[i].SourceID)
		}
	}
	return uniqueSourceIDs(nil, ids...)
}

func observationSourceIDsForValue(
	left, right []store.ParticipantContactObservation,
	kind store.ContactAddressKind, value string,
) []int64 {
	var ids []int64
	for _, group := range [][]store.ParticipantContactObservation{left, right} {
		for i := range group {
			if group[i].AddressKind == kind &&
				group[i].NormalizedValue == value && group[i].SourceID != nil {
				ids = append(ids, *group[i].SourceID)
			}
		}
	}
	return uniqueSourceIDs(nil, ids...)
}

// addEvidence attaches one evidence row unless it is already present.
func (m *identityMatcher) addEvidence(
	ctx context.Context, candidate *store.IdentityMatchCandidate, kind, detail string,
	sourceID *int64, additionalSourceIDs ...int64,
) error {
	return m.addEvidenceWithReference(
		ctx, candidate, kind, "", detail, sourceID, additionalSourceIDs...,
	)
}

func (m *identityMatcher) addEvidenceWithReference(
	ctx context.Context, candidate *store.IdentityMatchCandidate, kind, reference, detail string,
	sourceID *int64, additionalSourceIDs ...int64,
) error {
	if candidate == nil {
		return nil
	}
	sourceIDs := uniqueSourceIDs(sourceID, additionalSourceIDs...)
	value := detail
	var evidenceReference *string
	if reference != "" {
		evidenceReference = &reference
	}
	input := store.IdentityMatchEvidenceInput{
		EvidenceKind: kind,
		EvidenceRef:  evidenceReference,
		Detail:       &value,
		Source:       store.ProvenanceArchiveObservation,
	}
	if len(sourceIDs) == 0 {
		_, err := m.store.AddIdentityMatchEvidenceContext(ctx, candidate.ID, input)
		return err
	}
	for _, supportingSourceID := range sourceIDs {
		input.SourceID = &supportingSourceID
		if _, err := m.store.AddIdentityMatchEvidenceContext(
			ctx, candidate.ID, input,
		); err != nil {
			return err
		}
	}
	return nil
}
