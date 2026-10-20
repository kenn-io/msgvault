package api

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"go.kenn.io/msgvault/internal/personcard"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

const personCardFactReadPageSize = 200

// Facts are joined locally; neither raw evidence excerpts nor sensitive targets
// enter the pure card inputs. Each target is read once with a bounded page.
type personCardFactEvidence struct {
	fact     personcard.FactChange
	evidence []personfacts.Evidence
}

func (s *Server) personCardFacts(ctx context.Context, id int64, in *personcard.Inputs, opts personcard.Options) {
	b, ok := s.store.(PersonFactStore)
	attrs, hasAttrs := s.store.(PersonAttributeStore)
	if !ok || !hasAttrs {
		s.personCardGap(in, "facts_unavailable", nil)
		return
	}
	catalog, err := b.BuildPersonFactCatalogContext(ctx, false)
	if err != nil {
		s.personCardGap(in, "facts_unavailable", err)
		return
	}
	values, err := attrs.ListPersonAttributeValuesContext(ctx, id, store.PersonAttributeQuery{})
	if err != nil {
		s.personCardGap(in, "facts_unavailable", err)
		return
	}
	for _, v := range values {
		if v.Source != store.ProvenanceUser || v.SupersededAt != nil || v.CreatedAt.Before(opts.AsOf.AddDate(0, 0, -90)) || v.CreatedAt.After(opts.AsOf) {
			continue
		}
		definition, err := attrs.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, v.DefinitionSlug)
		if err != nil {
			s.personCardGap(in, "facts_unavailable", err)
			return
		}
		if definition.IsSensitive || definition.Slug == store.AttributeSlugNotes {
			continue
		}
		text, err := v.Value.CanonicalString()
		if err != nil {
			continue
		}
		in.Facts = append(in.Facts, personcard.FactChange{ID: v.ID, User: true, ChangedAt: v.CreatedAt, Label: definition.Label, Text: text})
	}
	decisions, err := b.ListPersonFactDecisionsContext(ctx, id, personfacts.DecisionFilter{
		ProjectionTransitionsOnly: true,
		Limit:                     50,
	})
	if err != nil {
		s.personCardGap(in, "facts_unavailable", err)
		return
	}
	relevant := map[string]personfacts.Decision{}
	for _, d := range decisions {
		if d.Action == personfacts.DecisionApplied && d.Projection != nil {
			if _, ok := relevant[d.ClaimKey]; !ok {
				relevant[d.ClaimKey] = d
			}
		}
	}
	if len(relevant) == 0 {
		return
	}
	claimKeys := make([]string, 0, len(relevant))
	for claimKey := range relevant {
		claimKeys = append(claimKeys, claimKey)
	}
	slices.Sort(claimKeys)
	pendingFacts := make([]personCardFactEvidence, 0, len(relevant))
	messageIDs := make([]int64, 0)
	seenMessageIDs := make(map[int64]struct{})
	flushPending := func() error {
		sourceByMessageID := make(map[int64]int64, len(messageIDs))
		for start := 0; start < len(messageIDs); start += personCardFactReadPageSize {
			end := min(start+personCardFactReadPageSize, len(messageIDs))
			summaries, err := s.getMessagesSummariesByIDs(ctx, messageIDs[start:end])
			if err != nil {
				return err
			}
			for _, message := range summaries {
				sourceByMessageID[message.ID] = message.SourceID
			}
		}
		for _, pending := range pendingFacts {
			for _, evidence := range pending.evidence {
				if mid, ok := personCardEvidenceMessageID(evidence.Input.SourceRef); ok {
					if sourceID, found := sourceByMessageID[mid]; found {
						pending.fact.SourceIDs = append(pending.fact.SourceIDs, sourceID)
					}
				}
			}
			in.Facts = append(in.Facts, pending.fact)
		}
		pendingFacts = nil
		messageIDs = nil
		seenMessageIDs = make(map[int64]struct{})
		return nil
	}
	gapWithPending := func(err error) {
		if flushErr := flushPending(); flushErr != nil {
			err = errors.Join(err, flushErr)
		}
		s.personCardGap(in, "facts_unavailable", err)
	}
	for _, target := range catalog.Targets {
		if target.Sensitive || target.Slug == store.AttributeSlugNotes {
			continue
		}
		ref := personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}
		claims, err := b.ListPersonFactClaimsContext(ctx, id, personfacts.ClaimFilter{
			Target: &ref, ClaimKeys: claimKeys, Limit: personCardFactReadPageSize, AnyRevision: true,
		})
		if err != nil {
			gapWithPending(err)
			return
		}
		selected := []personfacts.Claim{}
		for _, claim := range claims {
			if _, ok := relevant[claim.ClaimKey]; ok {
				selected = append(selected, claim)
			}
		}
		if len(selected) == 0 {
			continue
		}
		// The store hydrates Supported and LatestStatus from the newest status event.
		byID := map[int64]personfacts.Evidence{}
		evidenceIDs := make([]int64, 0)
		seenEvidenceIDs := make(map[int64]struct{})
		for _, claim := range selected {
			for _, evidenceID := range claim.EvidenceIDs {
				if _, seen := seenEvidenceIDs[evidenceID]; seen {
					continue
				}
				seenEvidenceIDs[evidenceID] = struct{}{}
				evidenceIDs = append(evidenceIDs, evidenceID)
			}
		}
		for start := 0; start < len(evidenceIDs); start += personCardFactReadPageSize {
			end := min(start+personCardFactReadPageSize, len(evidenceIDs))
			evidence, err := b.ListPersonFactEvidenceContext(ctx, id, personfacts.EvidenceFilter{
				EvidenceIDs: evidenceIDs[start:end], Limit: personCardFactReadPageSize,
			})
			if err != nil {
				gapWithPending(err)
				return
			}
			for _, e := range evidence {
				byID[e.ID] = e
			}
		}
		for _, claim := range selected {
			decision := relevant[claim.ClaimKey]
			changedAt := decision.ResolvedAt
			if claim.ValidFrom != nil {
				changedAt = *claim.ValidFrom
			}
			if changedAt.Before(opts.AsOf.AddDate(0, 0, -90)) || changedAt.After(opts.AsOf) {
				continue
			}
			supported := []personfacts.Evidence{}
			for _, eid := range claim.EvidenceIDs {
				e, ok := byID[eid]
				if ok && e.Supported {
					supported = append(supported, e)
				}
			}
			if len(supported) == 0 {
				continue
			}
			slices.SortFunc(supported, func(a, b personfacts.Evidence) int {
				if c := b.Input.EventTime.Compare(a.Input.EventTime); c != 0 {
					return c
				}
				if a.ID > b.ID {
					return -1
				}
				if a.ID < b.ID {
					return 1
				}
				return 0
			})
			text := string(claim.SubmittedValue)
			if claim.Normalized != nil {
				text = string(claim.Normalized.JSON)
			}
			var scalar string
			if json.Unmarshal([]byte(text), &scalar) == nil {
				text = scalar
			}
			if claim.Target.Kind == personfacts.TargetEmployment {
				text, err = personCardEmploymentFact(claim)
				if err != nil {
					gapWithPending(err)
					return
				}
				if text == "" {
					continue
				}
			}
			label := target.Slug
			if definition, err := attrs.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, target.Slug); err == nil {
				label = definition.Label
			}
			fact := personcard.FactChange{ID: decision.ID, Supported: true, Relation: string(claim.Relation), ChangedAt: changedAt, Label: label, Text: text, EvidenceRef: ""}
			for _, evidence := range supported {
				if personcard.ValidCitationRef(evidence.Input.SourceRef) && (strings.HasPrefix(evidence.Input.SourceRef, "message:") || strings.HasPrefix(evidence.Input.SourceRef, "msg:") || strings.HasPrefix(evidence.Input.SourceRef, "evt:")) {
					fact.EvidenceRef = evidence.Input.SourceRef
					break
				}
			}
			pendingFacts = append(pendingFacts, personCardFactEvidence{fact: fact, evidence: supported})
			for _, e := range supported {
				if mid, ok := personCardEvidenceMessageID(e.Input.SourceRef); ok {
					if _, seen := seenMessageIDs[mid]; !seen {
						seenMessageIDs[mid] = struct{}{}
						messageIDs = append(messageIDs, mid)
					}
				}
			}
		}
	}
	if err := flushPending(); err != nil {
		s.personCardGap(in, "facts_unavailable", err)
	}
}

func personCardEvidenceMessageID(ref string) (int64, bool) {
	if messageID, ok := strings.CutPrefix(ref, "message:"); ok {
		id, err := strconv.ParseInt(messageID, 10, 64)
		return id, err == nil && id > 0
	}
	_, occurrence, id, err := store.ParseMeetingRef(ref)
	if err != nil {
		return 0, false
	}
	if occurrence.MessageID > 0 {
		id = occurrence.MessageID
	}
	return id, id > 0
}

func personCardEmploymentFact(claim personfacts.Claim) (string, error) {
	if claim.Normalized == nil {
		return "", nil
	}
	var value personfacts.EmploymentValue
	if err := json.Unmarshal(claim.Normalized.JSON, &value); err != nil {
		return "", fmt.Errorf("decode normalized employment claim: %w", err)
	}
	organization := value.Organization.Name
	if organization == "" {
		return "", nil
	}
	if value.Title == "" {
		return organization, nil
	}
	return value.Title + " @ " + organization, nil
}
