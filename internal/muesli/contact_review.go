package muesli

import (
	"context"
	"fmt"
	"slices"

	"go.kenn.io/msgvault/internal/meetingidentity"
	"go.kenn.io/msgvault/internal/store"
)

// recordContactReviewCandidates offers ambiguous Contacts evidence for explicit
// review. Only identities already in the archive can be suggested. It runs on
// unchanged upserts too, since a phone identity may have arrived since last sync.
func (imp *Importer) recordContactReviewCandidates(ctx context.Context, sourceID int64, m Meeting) error {
	if m.ContactsState != ContactsComplete {
		return nil
	}
	key, err := m.SourceMessageID()
	if err != nil {
		return err
	}
	for _, p := range dedupeParticipants(m.Participants) {
		if len(p.ContactReviewPhones) == 0 || p.Email == "" || p.Resolution == resolutionResolved || p.Anchor != "" || p.archivePerson().Email != p.Email {
			continue
		}
		owner, err := imp.store.IsAccountIdentityAddressContext(ctx, p.Email)
		if err != nil {
			return err
		}
		if owner {
			continue
		}
		emailID, err := imp.store.EmailParticipantContext(ctx, p.Email)
		if err != nil {
			return err
		}
		if emailID == 0 {
			continue
		}
		for _, phone := range p.ContactReviewPhones {
			owner, err := imp.store.IsAccountIdentityAddressContext(ctx, phone)
			if err != nil {
				return err
			}
			if owner {
				continue
			}
			phoneID, err := imp.store.PhoneParticipantContext(ctx, phone)
			if err != nil {
				return err
			}
			if phoneID == 0 {
				continue
			}
			if emailID == phoneID {
				continue
			}
			candidate, _, err := imp.store.UpsertIdentityMatchCandidateContext(ctx, store.IdentityMatchCandidateInput{
				LeftKind: store.IdentityMatchParticipant, LeftID: emailID,
				RightKind: store.IdentityMatchParticipant, RightID: phoneID,
				Basis: store.IdentityMatchEmail, NormalizedValue: &p.Email,
				State: store.IdentityMatchStateCandidate, Source: store.ProvenanceArchiveObservation, SourceID: &sourceID,
			})
			if err != nil {
				return fmt.Errorf("record Muesli identity review candidate: %w", err)
			}
			ref := key + ":" + p.stableRef()
			detail := fmt.Sprintf("Muesli historical Contacts observation: %s appeared on multiple unlinked cards. Review a possible match to existing phone %s; this evidence does not establish ownership.", p.Email, phone)
			_, err = imp.store.AddIdentityMatchEvidenceContext(ctx, candidate.ID, store.IdentityMatchEvidenceInput{
				EvidenceKind: "muesli_ambiguous_contact", EvidenceRef: &ref, Detail: &detail,
				Source: store.ProvenanceArchiveObservation, SourceID: &sourceID,
			})
			if err != nil {
				return fmt.Errorf("record Muesli identity review evidence: %w", err)
			}
		}
	}
	return nil
}

// validateContactReviewLimits checks effective evidence, including identities
// carried forward by the archive and combined duplicate attendee rows.
func (m Meeting) validateContactReviewLimits() error {
	if slices.ContainsFunc(dedupeParticipants(m.Participants), contactReviewOversized) {
		return remoteInvalid("participant contact review", "exceeds the effective identity or email limit")
	}
	return nil
}

// dropOversizedContactReview removes review phones from attendees whose
// evidence exceeds the transfer budget, checking each attendee row and each
// combined duplicate. A resolved duplicate hides another row's suggestions
// from the combined check, but the transfer still bounds every row.
// Suggestions are optional: an email shared by that many cards is too
// ambiguous to suggest a match, and the meeting itself must still import.
func dropOversizedContactReview(m *Meeting) {
	oversized := map[string]bool{}
	for _, p := range dedupeParticipants(m.Participants) {
		if contactReviewOversized(p) {
			oversized[p.Email] = true
		}
	}
	for i := range m.Participants {
		p := &m.Participants[i]
		if oversized[meetingidentity.Normalize(p.Email)] || contactReviewOversized(*p) {
			p.ContactReviewPhones = nil
		}
	}
}

func contactReviewOversized(p Participant) bool {
	if len(p.ContactReviewPhones) == 0 {
		return false
	}
	ids := map[string]bool{p.Email: true}
	for _, values := range [][]string{p.ContactEmails, p.ContactPhones, p.ContactReviewPhones} {
		for _, value := range values {
			ids[value] = true
		}
	}
	return len(ids) > 50 || len(p.ContactEmails)+len(p.ContactPhones)+len(p.ContactReviewPhones) > 50 || len(p.Email) > 320
}
