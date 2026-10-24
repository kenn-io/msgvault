package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

type PreparedObservation struct {
	Proposal   personobs.ProposedObservation
	ContentKey string
	Citations  []personobs.Citation
	Categories []personobs.Category
	Stability  personobs.Stability
	Failure    *personobs.ValidationError
}
type PreparedObservationBatch struct {
	PersonID           int64
	Generation         *personfacts.GenerationInput
	ProgramFingerprint string
	Items              []PreparedObservation
	Digest             string
}

func observationBatchDigest(b PreparedObservationBatch) (string, error) {
	canonical, err := json.Marshal(struct {
		PersonID           int64
		Generation         *personfacts.GenerationInput
		ProgramFingerprint string
		Items              []PreparedObservation
	}{b.PersonID, b.Generation, b.ProgramFingerprint, b.Items}, json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encode prepared observation batch: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
func verifyPreparedObservationBatch(b PreparedObservationBatch) error {
	digest, err := observationBatchDigest(b)
	if err != nil {
		return err
	}
	if b.Digest != digest {
		return errors.New("prepared observation batch failed integrity verification")
	}
	return nil
}

func (s *Store) PreparePersonObservationBatch(ctx context.Context, in personobs.BatchInput) (PreparedObservationBatch, error) {
	// Own every pointer and slice before freezing the prepared digest.
	encoded, err := json.Marshal(in)
	if err != nil {
		return PreparedObservationBatch{}, err
	}
	var input personobs.BatchInput
	if err := json.Unmarshal(encoded, &input); err != nil {
		return PreparedObservationBatch{}, err
	}
	if input.PersonID <= 0 {
		return PreparedObservationBatch{}, errors.New("observation batch requires a person")
	}
	if input.Generation != nil && (input.Generation.PersonID != input.PersonID || input.Generation.ProgramFingerprint != input.ProgramFingerprint) {
		return PreparedObservationBatch{}, errors.New("observation generation does not match person and program")
	}
	batch := PreparedObservationBatch{PersonID: input.PersonID, Generation: input.Generation, ProgramFingerprint: input.ProgramFingerprint, Items: make([]PreparedObservation, 0, len(input.Proposals))}
	for _, proposal := range input.Proposals {
		if input.Generation == nil && proposal.Origin != personobs.OriginOwner {
			return PreparedObservationBatch{}, errors.New("machine observation requires a generation")
		}
		if proposal.Origin == personobs.OriginOwner && (input.Generation != nil || input.ProgramFingerprint != "") {
			return PreparedObservationBatch{}, errors.New("owner observation cannot carry machine provenance")
		}
		if proposal.PersonID != 0 && proposal.PersonID != input.PersonID {
			return PreparedObservationBatch{}, errors.New("observation proposal belongs to another person")
		}
		proposal.PersonID = input.PersonID
		proposal.Content = personobs.CanonicalizePlaceholders(proposal.Content)
		item := PreparedObservation{Proposal: proposal, ContentKey: personobs.ContentKey(proposal.Level, proposal.SubjectRef, proposal.Content)}
		item.Proposal.ValidFrom = personFactPortableTimePointer(item.Proposal.ValidFrom)
		item.Proposal.ValidUntil = personFactPortableTimePointer(item.Proposal.ValidUntil)
		premises := make(map[int64]personobs.Observation, len(proposal.Premises))
		for _, edge := range proposal.Premises {
			detail, err := s.GetPersonObservation(ctx, edge.ID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					item.Failure = &personobs.ValidationError{Field: "premises", Reason: "premise-missing"}
					break
				}
				return PreparedObservationBatch{}, err
			}
			premises[edge.ID] = detail.Observation
		}
		if item.Failure == nil {
			item.Failure = personobs.ValidateProposal(proposal, premises)
			if item.Failure != nil && item.Failure.Field == "premises" && item.Failure.Reason == "premise-not-supported" {
				// Citation exclusions can change before the locked write refreshes them.
				item.Failure = nil
			}
		}
		if item.Failure == nil {
			for _, support := range proposal.Support {
				if _, err := personfacts.EvidenceKey(support.Evidence); err != nil {
					item.Failure = &personobs.ValidationError{Field: "support", Reason: "invalid-evidence"}
					break
				}
				alignment, err := (PersonSweepEvidenceAligner{Store: s}).Align(ctx, support.Evidence)
				if err != nil {
					return PreparedObservationBatch{}, fmt.Errorf("align observation support: %w", err)
				}
				if !alignment.Accepted || support.Evidence.PersonID != input.PersonID || support.Evidence.SubjectPersonID == nil || *support.Evidence.SubjectPersonID != input.PersonID || support.Evidence.Directness != personfacts.DirectSelf {
					item.Failure = &personobs.ValidationError{Field: "support", Reason: "unaligned-evidence"}
					break
				}
				ref, err := peoplesweep.DecodePersonSweepEvidenceRef(support.Evidence.SourceRef)
				if err != nil {
					return PreparedObservationBatch{}, err
				}
				leaf, exclusion, err := s.observationLeaf(ctx, ref.MessageID)
				if err != nil {
					return PreparedObservationBatch{}, err
				}
				hash := sha256.Sum256([]byte(support.Quote))
				item.Citations = append(item.Citations, personobs.Citation{QuoteStart: support.QuoteStart, QuoteEnd: support.QuoteStart + len([]rune(support.Quote)), QuoteSHA256: hex.EncodeToString(hash[:]), Exclusion: exclusion, Leaf: leaf})
			}
		}
		categories := append([]personobs.Category(nil), proposal.ModelCategories...)
		categories = append(categories, personobs.DetectCategories(proposal.Content)...)
		for _, edge := range proposal.Premises {
			categories = append(categories, premises[edge.ID].Categories...)
		}
		slices.Sort(categories)
		item.Categories = slices.Compact(categories)
		item.Proposal.Sensitive = item.Proposal.Sensitive || len(item.Categories) > 0
		item.Stability = personobs.ClampStability(proposal.Level, proposal.Stability, definitionStability(proposal.TargetKind, proposal.TargetKey), item.Proposal.ValidFrom, item.Proposal.ValidUntil, proposal.OccursOn)
		batch.Items = append(batch.Items, item)
	}
	batch.Digest, err = observationBatchDigest(batch)
	return batch, err
}

// definitionStability is replaced when definition metadata is available.
func definitionStability(_, _ string) *personobs.Stability { return nil }

// observationLeaf never decodes evidence during a later support recheck. The
// projection is retained even if the source message subsequently disappears.
func (s *Store) observationLeaf(ctx context.Context, messageID int64) (personobs.Leaf, personobs.ExclusionReason, error) {
	var leaf personobs.Leaf
	var event requiredTimestamp
	var listID string
	var draft bool
	err := s.db.QueryRowContext(ctx, `SELECT m.id,m.conversation_id,c.conversation_type,m.is_from_me,
  COALESCE(m.sent_at,m.received_at,m.internal_date,m.archived_at),COALESCE(m.list_id,''),m.draft_authored
  FROM messages m JOIN conversations c ON c.id=m.conversation_id WHERE m.id=?`, messageID).Scan(&leaf.MessageID, &leaf.ConversationID, &leaf.ConversationKind, &leaf.AuthorIsOwner, &event, &listID, &draft)
	if err != nil {
		return leaf, "", fmt.Errorf("project observation evidence leaf: %w", err)
	}
	leaf.EventTime = event.Time
	leaf.AuthoredByPerson = true
	exclusion := personobs.ExclusionReasonNone
	if strings.TrimSpace(listID) != "" {
		exclusion = personobs.ExclusionReasonList
	}
	if draft {
		exclusion = personobs.ExclusionReasonDraft
	}
	return leaf, exclusion, nil
}
