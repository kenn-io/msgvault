package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"go.kenn.io/msgvault/internal/peoplesweep"
)

// personSweepDocumentEvidenceConsumer reads attachment replacements and
// deletions so stored facts quoting retired document text lose their support.
const personSweepDocumentEvidenceConsumer = "person-sweep-documents/v1"

type personSweepDocumentEvidence struct {
	personID int64
	ref      peoplesweep.EvidenceRef
}

// storedPersonSweepDocumentEvidence returns stored evidence that quotes
// document text, grouped by the attachment it quotes.
func (s *Store) storedPersonSweepDocumentEvidence(ctx context.Context) (map[int64][]personSweepDocumentEvidence, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT person_id, source_ref FROM person_fact_evidence
		WHERE source_class = 'archive' AND source_ref LIKE 'person-sweep/v1:%'
		ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list stored document evidence: %w", err)
	}
	defer func() { _ = rows.Close() }()
	evidence := map[int64][]personSweepDocumentEvidence{}
	for rows.Next() {
		var item personSweepDocumentEvidence
		var sourceRef string
		if err := rows.Scan(&item.personID, &sourceRef); err != nil {
			return nil, fmt.Errorf("scan stored document evidence: %w", err)
		}
		item.ref, err = peoplesweep.DecodePersonSweepEvidenceRef(sourceRef)
		if err != nil {
			return nil, fmt.Errorf("decode stored document evidence: %w", err)
		}
		if item.ref.SourceLane == peoplesweep.SourceDocumentText && item.ref.AttachmentID > 0 {
			evidence[item.ref.AttachmentID] = append(evidence[item.ref.AttachmentID], item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate stored document evidence: %w", err)
	}
	return evidence, nil
}

// registerPersonSweepDocumentEvidenceConsumer starts journaling attachment
// changes when stored evidence quotes document text. The retired reconciler
// covered every change before this point.
func (s *Store) registerPersonSweepDocumentEvidenceConsumer(ctx context.Context) error {
	evidence, err := s.storedPersonSweepDocumentEvidence(ctx)
	if err != nil || len(evidence) == 0 {
		return err
	}
	consumer, _, err := s.RegisterAttachmentChangeConsumer(ctx, personSweepDocumentEvidenceConsumer)
	if err != nil || consumer.ReconciliationComplete {
		return err
	}
	return s.CompleteAttachmentChangeReconciliation(ctx, personSweepDocumentEvidenceConsumer, consumer.BaselineSequence)
}

// publishPersonSweepDocumentEvidenceChanges turns journaled attachment
// replacements and deletions into person sweep changes for the people whose
// stored document evidence quotes those attachments.
func (s *Store) publishPersonSweepDocumentEvidenceChanges(ctx context.Context) error {
	if _, err := s.GetAttachmentChangeConsumer(ctx, personSweepDocumentEvidenceConsumer); err != nil {
		if errors.Is(err, ErrAttachmentChangeConsumerMissing) {
			return nil
		}
		return err
	}
	for {
		changes, err := s.ListAttachmentChanges(ctx, personSweepDocumentEvidenceConsumer, 1000)
		if err != nil || len(changes) == 0 {
			return err
		}
		effects := map[int64]peoplesweep.EvidenceChangeEffect{}
		for _, change := range changes {
			if change.OldAttachmentID == nil {
				continue
			}
			switch {
			case change.EventKind == "attachment_delete":
				effects[*change.OldAttachmentID] = peoplesweep.EvidenceEffectSourceDeleted
			case change.EventKind == "attachment_update" &&
				!equalStringPointers(change.OldContentHash, change.NewContentHash) &&
				effects[*change.OldAttachmentID] == peoplesweep.EvidenceEffectNone:
				effects[*change.OldAttachmentID] = peoplesweep.EvidenceEffectSourceEdited
			}
		}
		if len(effects) > 0 {
			if err := s.appendPersonSweepDocumentEvidenceChanges(ctx, effects); err != nil {
				return err
			}
		}
		if err := s.AdvanceAttachmentChangeConsumer(ctx, personSweepDocumentEvidenceConsumer, changes[len(changes)-1].Sequence); err != nil {
			return err
		}
	}
}

// publishPersonSweepDocumentEvidenceChangesBestEffort runs after a sync
// commits. A failure leaves the changes journaled for the next sync.
func (s *Store) publishPersonSweepDocumentEvidenceChangesBestEffort(ctx context.Context) {
	if err := s.publishPersonSweepDocumentEvidenceChanges(ctx); err != nil {
		slog.Warn("publish attachment changes for stored document evidence", "error", err)
	}
}

func (s *Store) appendPersonSweepDocumentEvidenceChanges(
	ctx context.Context, effects map[int64]peoplesweep.EvidenceChangeEffect,
) error {
	evidence, err := s.storedPersonSweepDocumentEvidence(ctx)
	if err != nil {
		return err
	}
	attachmentIDs := make([]int64, 0, len(effects))
	for attachmentID := range effects {
		attachmentIDs = append(attachmentIDs, attachmentID)
	}
	slices.Sort(attachmentIDs)
	return s.withTxContext(ctx, func(tx *loggedTx) error {
		type target struct{ personID, attachmentID int64 }
		seen := map[target]struct{}{}
		var personIDs []int64
		for _, attachmentID := range attachmentIDs {
			for _, item := range evidence[attachmentID] {
				if _, ok := seen[target{item.personID, attachmentID}]; ok {
					continue
				}
				seen[target{item.personID, attachmentID}] = struct{}{}
				// A deleted message already withdraws its evidence.
				var lane peoplesweep.SourceClass
				err := tx.QueryRowContext(ctx, s.Rebind(`
					SELECT CASE WHEN message_type = 'meeting_transcript'
					            THEN 'meeting_text' ELSE 'conversation_text' END
					FROM messages WHERE id = ?`), item.ref.MessageID).Scan(&lane)
				if errors.Is(err, sql.ErrNoRows) {
					continue
				}
				if err != nil {
					return fmt.Errorf("read document evidence message lane: %w", err)
				}
				kind := peoplesweep.ChangeUpsert
				if effects[attachmentID] == peoplesweep.EvidenceEffectSourceDeleted {
					kind = peoplesweep.ChangeDelete
				}
				if err := s.appendPersonSweepChangeTx(ctx, tx, peoplesweep.ArchiveChange{
					PersonID: item.personID, SourceLane: lane, Kind: kind,
					EvidenceEffect: effects[attachmentID], SourceID: item.ref.SourceID,
					MessageID: item.ref.MessageID, AttachmentID: attachmentID,
				}); err != nil {
					return err
				}
				personIDs = append(personIDs, item.personID)
			}
		}
		return s.coalescePersonSweepPeopleTx(ctx, tx, personIDs)
	})
}

func equalStringPointers(left, right *string) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}
