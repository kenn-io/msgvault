package meetingarchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/store"
)

// evidenceStableProviderID names identity-match evidence that two participants
// carry the same stable provider anchor.
const evidenceStableProviderID = "stable_provider_id"

// Anchor builds a stable, provider-scoped identifier for one human. The parts
// are length-prefixed before hashing, so ("a:b","c") and ("a","b:c") differ,
// and hashing keeps device-local identifiers such as an Apple Contacts ID out
// of the archive. The namespace prefix stays readable: identities asserted by
// two different anchors of one namespace are treated as contradictory.
func Anchor(namespace string, parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		_, _ = hash.Write([]byte(strconv.Itoa(len(part)) + ":" + part))
	}
	return namespace + ":" + hex.EncodeToString(hash.Sum(nil))
}

func anchorNamespace(anchor string) string {
	namespace, _, _ := strings.Cut(anchor, ":")
	return namespace
}

// LinkResult counts what one LinkIdentities call did.
type LinkResult struct {
	// Linked counts identity pairs linked or resumed by this call.
	Linked int
	// Conflicts counts people or pairs left for user review instead of being
	// linked: two different curated persons, or an address that another
	// anchor of the same provider already claims.
	Conflicts int
	// Settled counts anchored people that needed no writes.
	Settled int
}

// LinkIdentities records every anchored person's emails and phones as
// observations of the person's stable anchor and links the participants that
// share it. This is msgvault's stable-provider-ID policy (see
// internal/beeper/matching.go): only a shared stable ID links automatically,
// a link that would join two different persons becomes a conflict, and a user
// rejection is kept. Names never match anything.
func (a *Archiver) LinkIdentities(ctx context.Context, sourceID int64, people []Person) (LinkResult, error) {
	var result LinkResult
	if a == nil || a.store == nil {
		return result, ErrUnavailable
	}
	for _, raw := range people {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		person := raw.Normalized()
		identities := person.identities()
		if person.Anchor == "" || len(identities) == 0 {
			continue
		}
		if err := a.linkPerson(ctx, sourceID, person, identities, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (a *Archiver) linkPerson(
	ctx context.Context, sourceID int64, person Person, identities []identity, result *LinkResult,
) error {
	anchor := person.Anchor
	// The archive owner's own contact card or record can list a shared number
	// or inbox. Linking it would put the owner into someone else's person.
	for _, id := range identities {
		owner, err := a.store.IsAccountIdentityAddressContext(ctx, id.value)
		if err != nil {
			return err
		}
		if owner {
			return nil
		}
	}
	settled, err := a.store.StableAnchorSettledContext(ctx, sourceID, anchor, anchorIdentities(identities))
	if err != nil {
		return err
	}
	if settled {
		result.Settled++
		return nil
	}

	participantIDs := make([]int64, len(identities))
	for i, id := range identities {
		participantID, err := a.ensureIdentityParticipant(ctx, person.Name, id)
		if err != nil {
			return err
		}
		participantIDs[i] = participantID
	}

	contradicted, err := a.contradictedParticipants(ctx, sourceID, anchor, identities, participantIDs)
	if err != nil {
		return err
	}
	if len(contradicted) > 0 {
		return a.recordContradiction(ctx, sourceID, anchor, contradicted, participantIDs, result)
	}

	sourceRef := "meeting-person:" + anchor
	observedAt := time.Now().UTC()
	for i, id := range identities {
		kind := store.ContactAddressEmail
		if id.kind == identityPhone {
			kind = store.ContactAddressPhone
		}
		if _, err := a.store.RecordContactObservationContext(ctx, participantIDs[i],
			store.ParticipantContactObservationInput{
				SourceID: &sourceID, AddressKind: kind, ProviderUserID: &anchor,
				OriginalValue: id.value, ObservedAt: &observedAt,
				Envelope: store.ValueEnvelopeInput{
					Source: store.ProvenanceArchiveObservation, SourceRef: &sourceRef,
				},
			}); err != nil {
			return fmt.Errorf("record meeting attendee observation: %w", err)
		}
	}

	observations, err := a.store.FindObservationsByProviderUserIDContext(ctx, anchor, 0)
	if err != nil {
		return err
	}
	primary := participantIDs[0]
	seen := map[int64]bool{primary: true}
	for _, observation := range observations {
		other := observation.ParticipantID
		if seen[other] {
			continue
		}
		seen[other] = true
		if err := a.linkPair(ctx, sourceID, anchor, sourceRef, primary, other, result); err != nil {
			return err
		}
	}
	return nil
}

func (a *Archiver) ensureIdentityParticipant(ctx context.Context, name string, id identity) (int64, error) {
	if id.kind == identityPhone {
		participantID, err := a.store.EnsurePhoneParticipantContext(ctx, id.value, name)
		if err != nil {
			return 0, fmt.Errorf("ensure attendee phone participant: %w", err)
		}
		return participantID, nil
	}
	participantID, err := a.store.EnsureParticipantContext(ctx, id.value, name, emailDomain(id.value))
	if err != nil {
		return 0, fmt.Errorf("ensure attendee email participant: %w", err)
	}
	return participantID, nil
}

// contradictedParticipants returns the participants whose address another
// anchor of the same namespace already claims in the same source. Two
// Contacts cards, or two import ids, sharing one household phone must not
// expand each other's person automatically. Another source's anchor is
// independent evidence, like another provider's.
func (a *Archiver) contradictedParticipants(
	ctx context.Context, sourceID int64, anchor string, identities []identity, participantIDs []int64,
) ([]int64, error) {
	namespace := anchorNamespace(anchor)
	var contradicted []int64
	for i, id := range identities {
		observations, err := a.store.ListParticipantObservationsContext(ctx, participantIDs[i], true)
		if err != nil {
			return nil, err
		}
		for _, observation := range observations {
			if observation.ProviderUserID == nil || *observation.ProviderUserID == anchor ||
				observation.SourceID == nil || *observation.SourceID != sourceID ||
				anchorNamespace(*observation.ProviderUserID) != namespace ||
				observation.NormalizedValue != id.value {
				continue
			}
			contradicted = append(contradicted, participantIDs[i])
			break
		}
	}
	return contradicted, nil
}

func (a *Archiver) recordContradiction(
	ctx context.Context, sourceID int64, anchor string,
	contradicted, participantIDs []int64, result *LinkResult,
) error {
	sourceRef := "meeting-person:" + anchor
	for _, conflicted := range contradicted {
		for _, other := range participantIDs {
			if other == conflicted {
				continue
			}
			if _, _, err := a.store.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: conflicted,
				RightKind: store.IdentityMatchParticipant, RightID: other,
				Basis: store.IdentityMatchStableProviderID, NormalizedValue: &anchor,
				State: store.IdentityMatchStateConflict, Source: store.ProvenanceArchiveObservation,
				SourceRef: &sourceRef, SourceID: &sourceID,
			}); err != nil && !errors.Is(err, store.ErrIdentityMatchSelfLink) {
				return fmt.Errorf("record contradictory attendee identity: %w", err)
			}
		}
	}
	result.Conflicts++
	return nil
}

func (a *Archiver) linkPair(
	ctx context.Context, sourceID int64, anchor, sourceRef string, left, right int64, result *LinkResult,
) error {
	candidate, _, err := a.store.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
		LeftKind: store.IdentityMatchParticipant, LeftID: left,
		RightKind: store.IdentityMatchParticipant, RightID: right,
		Basis: store.IdentityMatchStableProviderID, NormalizedValue: &anchor,
		State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation,
		SourceRef: &sourceRef, SourceID: &sourceID,
	})
	if err != nil {
		return fmt.Errorf("record attendee identity match: %w", err)
	}
	if candidate.State == store.IdentityMatchStateRejected {
		return nil
	}
	if candidate.State == store.IdentityMatchStateConflict {
		result.Conflicts++
		return nil
	}
	if err := a.store.AttachIdentityMatchCandidateSourceContext(ctx, candidate.ID, sourceID); err != nil {
		return err
	}
	if _, err := a.store.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
		EvidenceKind: evidenceStableProviderID, Detail: &anchor,
		Source: store.ProvenanceArchiveObservation, SourceID: &sourceID,
	}); err != nil {
		return err
	}

	linked := true
	if candidate.State == store.IdentityMatchStateAccepted {
		_, _, linked, err = a.store.ResumeAcceptedIdentityMatchCandidateContext(ctx, candidate.ID)
	} else {
		_, _, err = a.store.AcceptIdentityMatchCandidateContext(ctx, candidate.ID, "system", nil)
	}
	switch {
	case err == nil:
		if linked {
			result.Linked++
		}
	case errors.Is(err, store.ErrPersonBindingConflict):
		slog.Warn("meeting attendee identities belong to different persons; left for review",
			"candidate_id", candidate.ID)
		result.Conflicts++
	case errors.Is(err, store.ErrIdentityMatchRejected),
		errors.Is(err, store.ErrIdentityMatchNotAccepted),
		errors.Is(err, store.ErrIdentityMatchNotFound):
		// A concurrent user decision or participant merge won; it is durable.
	default:
		return fmt.Errorf("link attendee identities: %w", err)
	}
	return nil
}

func anchorIdentities(identities []identity) []store.AnchorIdentity {
	out := make([]store.AnchorIdentity, len(identities))
	for i, id := range identities {
		kind := store.ContactAddressEmail
		if id.kind == identityPhone {
			kind = store.ContactAddressPhone
		}
		out[i] = store.AnchorIdentity{Kind: kind, Value: id.value}
	}
	return out
}
