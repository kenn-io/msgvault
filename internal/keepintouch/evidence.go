package keepintouch

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/taskclient"
)

const EvidenceMetadataKey = "msgvault.outreach_evidence.v1"
const DraftModeMetadataKey = "msgvault.outreach_mode"

var ErrEvidenceChanged = errors.New("outreach evidence changed; defer publication")

// EvidenceSnapshot is the bounded, non-text proof retained with the Kata
// issue so notification retries can validate the original draft without AI.
type EvidenceSnapshot struct {
	MessageID       int64     `json:"message_id"`
	SourceID        int64     `json:"source_id"`
	SourceMessageID string    `json:"source_message_id"`
	SourceVersion   string    `json:"source_version"`
	ContentSHA256   string    `json:"content_sha256"`
	EventTime       time.Time `json:"event_time"`
}

type EvidenceReader interface {
	HydratePersonSweepMessages(ctx context.Context, personID int64, messageIDs []int64, throughSequence int64) ([]peoplesweep.EvidenceItem, error)
}
type EvidenceValidator interface {
	ValidateEvidence(ctx context.Context, candidate Candidate, proof []EvidenceSnapshot) error
}

func snapshotEvidence(items []peoplesweep.EvidenceItem) []EvidenceSnapshot {
	proof := make([]EvidenceSnapshot, 0, len(items))
	for _, item := range items {
		proof = append(proof, EvidenceSnapshot{MessageID: item.Ref.MessageID, SourceID: item.Ref.SourceID, SourceMessageID: item.Ref.SourceMessageID, SourceVersion: item.SourceVersion, ContentSHA256: item.ContentSHA256, EventTime: item.EventTime.UTC()})
	}
	return proof
}

// ValidateEvidence rehydrates the exact saved inputs under current person
// scope. A reconciled deletion, edit, or identity reassignment cannot hide
// behind unchanged cadence timestamps. It performs no provider I/O.
func ValidateEvidence(ctx context.Context, archive EvidenceReader, personID int64, proof []EvidenceSnapshot) error {
	if len(proof) == 0 {
		return nil
	}
	if len(proof) > 6 {
		return errors.New("invalid outreach evidence proof")
	}
	ids := make([]int64, 0, len(proof))
	seen := map[int64]bool{}
	for _, item := range proof {
		if item.MessageID <= 0 || item.SourceID <= 0 || item.SourceMessageID == "" || item.SourceVersion == "" || len(item.ContentSHA256) != 64 || item.EventTime.IsZero() || seen[item.MessageID] {
			return errors.New("invalid outreach evidence proof")
		}
		seen[item.MessageID] = true
		ids = append(ids, item.MessageID)
	}
	items, err := archive.HydratePersonSweepMessages(ctx, personID, ids, 0)
	if errors.Is(err, peoplesweep.ErrPersonSweepMessageUnavailable) || errors.Is(err, peoplesweep.ErrSourceTextUnavailable) {
		return ErrEvidenceChanged
	}
	if err != nil {
		return err
	}
	current := map[int64]peoplesweep.EvidenceItem{}
	for _, item := range items {
		current[item.Ref.MessageID] = item
	}
	for _, saved := range proof {
		item, ok := current[saved.MessageID]
		if !ok || item.Tombstone || item.PersonID != personID || item.SubjectPersonID == nil || *item.SubjectPersonID != personID || item.Ref.SourceID != saved.SourceID || item.Ref.SourceMessageID != saved.SourceMessageID || item.SourceVersion != saved.SourceVersion || item.ContentSHA256 != saved.ContentSHA256 || !item.EventTime.Equal(saved.EventTime) {
			return ErrEvidenceChanged
		}
	}
	return nil
}
func (b *ProductionDrafts) ValidateEvidence(ctx context.Context, c Candidate, proof []EvidenceSnapshot) error {
	return ValidateEvidence(ctx, b.Archive, c.Person.ID, proof)
}

func (s *Service) validateTaskEvidence(ctx context.Context, c Candidate, task taskclient.KataTask) error {
	raw, present := task.Metadata[EvidenceMetadataKey]
	if !present {
		return errors.New("outreach issue has no evidence proof")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	var proof []EvidenceSnapshot
	if err := json.Unmarshal(encoded, &proof, json.RejectUnknownMembers(true)); err != nil {
		return fmt.Errorf("read outreach evidence proof: %w", err)
	}
	mode, _ := task.Metadata[DraftModeMetadataKey].(string)
	if mode != "grounded" && mode != "neutral_no_evidence" || mode == "grounded" && len(proof) == 0 {
		return errors.New("outreach issue has an invalid draft mode or evidence proof")
	}
	if len(proof) == 0 {
		return nil
	}
	validator, ok := s.Drafts.(EvidenceValidator)
	if !ok {
		return errors.New("outreach evidence validator is unavailable")
	}
	return validator.ValidateEvidence(ctx, c, proof)
}
