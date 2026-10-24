package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

type SuppressionCandidate struct {
	Origin                      personobs.Origin
	PostRejectionSupport, Floor int
}
type SuppressionVerdict string

var errObservationSourceLockSetChanged = errors.New("observation source lock set changed")

const maxObservationSourceLockRestarts = 12

type observationSupportSourcePlan struct {
	premiseIDs     []int64
	messageIDs     []int64
	documentRefs   []peoplesweep.EvidenceRef
	attachmentIDs  []int64
	documentHashes []string
}

const (
	SuppressionAllow      SuppressionVerdict = "allow"
	SuppressionSuppressed SuppressionVerdict = "suppressed"
	SuppressionLifted     SuppressionVerdict = "lifted"
)

func (s *Store) lockPersonMutationTx(ctx context.Context, tx *loggedTx, personID int64) error {
	return s.lockProfileIdentityKeyTxContext(ctx, tx, "person-observations", personID)
}

func (s *Store) observationSupportSourceBaseTx(
	ctx context.Context,
	tx *loggedTx,
	batch PreparedObservationBatch,
) (observationSupportSourcePlan, error) {
	plan := observationSupportSourcePlan{}
	messageIDs := make([]int64, 0)
	documentRefs := make([]peoplesweep.EvidenceRef, 0)
	for _, item := range batch.Items {
		if item.Failure != nil {
			continue
		}
		for _, support := range item.Proposal.Support {
			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(support.Evidence.SourceRef)
			if err != nil || ref.MessageID <= 0 {
				continue // Invalid evidence is dropped when the proposal is validated.
			}
			messageIDs = append(messageIDs, ref.MessageID)
			if ref.SourceLane == peoplesweep.SourceDocumentText {
				documentRefs = append(documentRefs, ref)
			}
		}
	}
	premiseIDs, premiseMessageIDs, premiseDocumentRefs, err := s.observationBatchPremiseSourcesTx(ctx, tx, batch)
	if err != nil {
		return plan, err
	}
	messageIDs = append(messageIDs, premiseMessageIDs...)
	documentRefs = append(documentRefs, premiseDocumentRefs...)
	attachmentIDs := make([]int64, 0, len(documentRefs))
	for _, ref := range documentRefs {
		attachmentIDs = append(attachmentIDs, ref.AttachmentID)
	}
	slices.Sort(attachmentIDs)
	attachmentIDs = slices.Compact(attachmentIDs)
	slices.Sort(messageIDs)
	messageIDs = slices.Compact(messageIDs)
	return observationSupportSourcePlan{
		premiseIDs: premiseIDs, messageIDs: messageIDs, documentRefs: documentRefs,
		attachmentIDs: attachmentIDs,
	}, nil
}

func (s *Store) observationSupportSourcePlanTx(
	ctx context.Context,
	tx *loggedTx,
	batch PreparedObservationBatch,
) (observationSupportSourcePlan, error) {
	plan, err := s.observationSupportSourceBaseTx(ctx, tx, batch)
	if err != nil {
		return observationSupportSourcePlan{}, err
	}
	plan.documentHashes, err = s.observationDocumentHashesTx(ctx, tx, plan.documentRefs)
	return plan, err
}

func (s *Store) observationDocumentHashesTx(
	ctx context.Context,
	tx *loggedTx,
	documentRefs []peoplesweep.EvidenceRef,
) ([]string, error) {
	documentHashes := make([]string, 0, len(documentRefs))
	for _, ref := range documentRefs {
		var canonicalBlobHash string
		err := tx.QueryRowContext(ctx, `SELECT canonical_blob_hash FROM document_occurrences
			WHERE attachment_id=? AND message_id=? AND source_id=? AND occurrence_key=?`,
			ref.AttachmentID, ref.MessageID, ref.SourceID, ref.OccurrenceKey,
		).Scan(&canonicalBlobHash)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read observation document support hash: %w", err)
		}
		if canonicalBlobHash != "" {
			documentHashes = append(documentHashes, canonicalBlobHash)
		}
	}
	slices.Sort(documentHashes)
	return slices.Compact(documentHashes), nil
}

func (s *Store) lockObservationSupportSourcesTx(
	ctx context.Context,
	tx *loggedTx,
	batch PreparedObservationBatch,
) (observationSupportSourcePlan, error) {
	plan, err := s.observationSupportSourceBaseTx(ctx, tx, batch)
	if err != nil {
		return observationSupportSourcePlan{}, err
	}
	for _, attachmentID := range plan.attachmentIDs {
		if err := s.lockDocumentOccurrenceAttachmentTx(ctx, tx, attachmentID); err != nil {
			return observationSupportSourcePlan{}, err
		}
	}
	// Read publication hashes only after locking their occurrence attachments.
	// The same plan is re-read after the person lock; new source keys then force
	// a rollback and retry instead of acquiring source locks out of order.
	plan.documentHashes, err = s.observationDocumentHashesTx(ctx, tx, plan.documentRefs)
	if err != nil {
		return observationSupportSourcePlan{}, err
	}
	for _, canonicalBlobHash := range plan.documentHashes {
		if err := s.lockDocumentPublicationHashTx(ctx, tx, canonicalBlobHash); err != nil {
			return observationSupportSourcePlan{}, err
		}
	}
	for _, messageID := range plan.messageIDs {
		var lockedID int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE id=?`+s.dialect.SelectForUpdate(), messageID).Scan(&lockedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return observationSupportSourcePlan{}, fmt.Errorf("lock observation support message %d: %w", messageID, err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT message_id FROM message_bodies WHERE message_id=?`+s.dialect.SelectForUpdate(), messageID).Scan(&lockedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return observationSupportSourcePlan{}, fmt.Errorf("lock observation support body %d: %w", messageID, err)
		}
	}
	return plan, nil
}

func observationSourcePlanNeedsRestart(locked, current observationSupportSourcePlan) bool {
	for _, messageID := range current.messageIDs {
		if !slices.Contains(locked.messageIDs, messageID) {
			return true
		}
	}
	for _, attachmentID := range current.attachmentIDs {
		if !slices.Contains(locked.attachmentIDs, attachmentID) {
			return true
		}
	}
	for _, documentHash := range current.documentHashes {
		if !slices.Contains(locked.documentHashes, documentHash) {
			return true
		}
	}
	return false
}

func (s *Store) observationBatchPremiseSourcesTx(
	ctx context.Context,
	tx *loggedTx,
	batch PreparedObservationBatch,
) ([]int64, []int64, []peoplesweep.EvidenceRef, error) {
	roots := make([]int64, 0)
	for _, item := range batch.Items {
		if item.Failure != nil {
			continue
		}
		for _, premise := range item.Proposal.Premises {
			roots = append(roots, premise.ID)
		}
		reinforcementTargetIDs, err := s.observationBatchReinforcementTargetIDsTx(
			ctx, tx, batch.PersonID, item)
		if err != nil {
			return nil, nil, nil, err
		}
		roots = append(roots, reinforcementTargetIDs...)
	}
	slices.Sort(roots)
	roots = slices.Compact(roots)
	observationIDs := make([]int64, 0)
	messageIDs := make([]int64, 0)
	documentRefs := make([]peoplesweep.EvidenceRef, 0)
	for start := 0; start < len(roots); start += 400 {
		end := min(start+400, len(roots))
		rootRows := make([]string, 0, end-start)
		args := make([]any, 0, end-start+1)
		for _, rootID := range roots[start:end] {
			rootRows = append(rootRows, "(CAST(? AS BIGINT))")
			args = append(args, rootID)
		}
		args = append(args, batch.PersonID, batch.PersonID)
		rows, err := tx.QueryContext(ctx, s.Rebind(`WITH RECURSIVE roots(id) AS (
			VALUES `+strings.Join(rootRows, ",")+`
		), premise_ancestors(id) AS (
			SELECT observation.id FROM roots
			JOIN person_observations observation ON observation.id=roots.id
			WHERE observation.person_id=?
			UNION
			SELECT edge.premise_id
			FROM person_observation_premises edge
			JOIN premise_ancestors ancestor ON edge.derived_id=ancestor.id
			JOIN person_observations premise ON premise.id=edge.premise_id AND premise.person_id=?
		)
		SELECT DISTINCT ancestor.id, citation.message_id, fact.source_ref
		FROM premise_ancestors ancestor
		LEFT JOIN person_observation_evidence citation ON citation.observation_id=ancestor.id
		LEFT JOIN person_fact_evidence fact ON fact.id=citation.evidence_id
		ORDER BY ancestor.id, citation.message_id, fact.source_ref`), args...)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("read observation premise sources: %w", err)
		}
		for rows.Next() {
			var observationID int64
			var messageID sql.NullInt64
			var sourceRef sql.NullString
			if err := rows.Scan(&observationID, &messageID, &sourceRef); err != nil {
				_ = rows.Close()
				return nil, nil, nil, fmt.Errorf("scan observation premise source: %w", err)
			}
			observationIDs = append(observationIDs, observationID)
			if messageID.Valid {
				messageIDs = append(messageIDs, messageID.Int64)
			}
			if sourceRef.Valid {
				ref, err := peoplesweep.DecodePersonSweepEvidenceRef(sourceRef.String)
				if err == nil && ref.SourceLane == peoplesweep.SourceDocumentText {
					documentRefs = append(documentRefs, ref)
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, nil, nil, fmt.Errorf("read observation premise sources: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, nil, nil, fmt.Errorf("close observation premise sources: %w", err)
		}
	}
	slices.Sort(observationIDs)
	observationIDs = slices.Compact(observationIDs)
	slices.Sort(messageIDs)
	messageIDs = slices.Compact(messageIDs)
	return observationIDs, messageIDs, documentRefs, nil
}

func (s *Store) observationBatchReinforcementTargetIDsTx(
	ctx context.Context, tx *loggedTx, personID int64, item PreparedObservation,
) ([]int64, error) {
	targetIDs := make([]int64, 0, 2)
	if item.Proposal.ReinforcesID != nil {
		candidate, err := s.loadObservationTx(ctx, tx, *item.Proposal.ReinforcesID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil
			}
			return nil, err
		}
		if candidate.PersonID != personID || candidate.Level != item.Proposal.Level ||
			personobs.NormalizeContent(candidate.SubjectRef) != personobs.NormalizeContent(item.Proposal.SubjectRef) || candidate.Status == personobs.StatusRejected {
			return nil, nil
		}
		if candidate.Status != personobs.StatusSuperseded {
			targetIDs = append(targetIDs, candidate.ID)
		}
	}
	// A preceding item in this batch can supersede a currently valid explicit
	// target. The apply loop then falls back to the live content key, so include
	// that possible target and its retained evidence sources in the lock plan.
	matches, err := s.queryObservationRows(ctx, tx,
		`person_id=? AND content_key=? AND status IN ('active','unsupported')`,
		`id LIMIT 1`, personID, item.ContentKey)
	if err != nil {
		return nil, err
	}
	if len(matches) > 0 {
		targetIDs = append(targetIDs, matches[0].ID)
	}
	slices.Sort(targetIDs)
	return slices.Compact(targetIDs), nil
}

func (s *Store) nextObservationSequenceTx(ctx context.Context, tx *loggedTx, personID int64) (int64, error) {
	var sequence int64
	err := tx.QueryRowContext(ctx, `INSERT INTO person_observation_clocks(person_id,last_sequence) VALUES(?,1)
  ON CONFLICT(person_id) DO UPDATE SET last_sequence=person_observation_clocks.last_sequence+1 RETURNING last_sequence`, personID).Scan(&sequence)
	if err != nil {
		return 0, fmt.Errorf("advance person observation clock: %w", err)
	}
	return sequence, nil
}
func (s *Store) loadObservationTx(ctx context.Context, q contextRowsQuerier, id int64) (personobs.Observation, error) {
	rows, err := s.queryObservationRows(ctx, q, `id=?`, `id`, id)
	if err != nil {
		return personobs.Observation{}, err
	}
	if len(rows) == 0 {
		return personobs.Observation{}, sql.ErrNoRows
	}
	return rows[0], nil
}
func observationProtected(o personobs.Observation) bool {
	return o.Pinned || o.OwnerState == personobs.OwnerStateConfirmed
}

func (s *Store) recordObservationSupersedeBlockedTx(ctx context.Context, tx *loggedTx, personID, replacementID, targetID int64, actor string) error {
	if err := s.appendObservationEventTx(ctx, tx, personID, targetID, "supersede-blocked", actor, "", &replacementID); err != nil {
		return err
	}
	return s.appendObservationEventTx(ctx, tx, personID, replacementID, "supersede-blocked", actor, "", &targetID)
}

func observationNowAfter(previous time.Time) time.Time {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if !now.After(previous) {
		return previous.Add(time.Microsecond)
	}
	return now
}

// observationEventGenerationKey scopes provenance to the operation that triggered
// an event, including recursive support updates. Owner operations have no value.
type observationEventGenerationKey struct{}

// appendObservationEventTx advances the optimistic revision with the event.
// Explicit timestamps give SQLite the same precision as PostgreSQL.
func (s *Store) appendObservationEventTx(ctx context.Context, tx *loggedTx, personID, id int64, event, actor, detail string, related *int64) error {
	var previous requiredTimestamp
	if err := tx.QueryRowContext(ctx, `SELECT updated_at FROM person_observations WHERE id=? AND person_id=?`, id, personID).Scan(&previous); err != nil {
		return err
	}
	now := observationNowAfter(previous.Time)
	if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET updated_at=? WHERE id=?`, now, id); err != nil {
		return err
	}
	if actor == "" {
		actor = "system"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO person_observation_events(person_id,observation_id,generation_id,event,actor,detail,related_observation_id,created_at)
  SELECT person_id,id,?,?,?,?,?,? FROM person_observations WHERE id=?`, observationEventGeneration(ctx), event, actor, detail, personFactOptionalInt64(related), now, id)
	if err != nil {
		return fmt.Errorf("append observation event: %w", err)
	}
	return nil
}
func observationEventGeneration(ctx context.Context) any {
	generationID, _ := ctx.Value(observationEventGenerationKey{}).(int64)
	return observationOptionalGeneration(generationID)
}

func (s *Store) transitionObservationTx(ctx context.Context, tx *loggedTx, id int64, to personobs.Status, reason personobs.StatusReason, event, actor, detail string) error {
	var personID int64
	if err := tx.QueryRowContext(ctx, `UPDATE person_observations SET status=?,status_reason=? WHERE id=? RETURNING person_id`, to, reason, id).Scan(&personID); err != nil {
		return fmt.Errorf("transition observation: %w", err)
	}
	return s.appendObservationEventTx(ctx, tx, personID, id, event, actor, detail, nil)
}
func (s *Store) deletePersonObservationTx(ctx context.Context, tx *loggedTx, id int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM person_observations WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete person observation: %w", err)
	}
	return nil
}
func observationOptionalDate(date *personfacts.PartialDateValue) any {
	if date == nil {
		return nil
	}
	if date.Day > 0 {
		return fmt.Sprintf("%04d-%02d-%02d", date.Year, date.Month, date.Day)
	}
	if date.Month > 0 {
		return fmt.Sprintf("%04d-%02d", date.Year, date.Month)
	}
	return fmt.Sprintf("%04d", date.Year)
}
func observationOptionalText(value string) any {
	if value == "" {
		return nil
	}
	return value
}
func observationOptionalGeneration(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

func (s *Store) ApplyPersonObservationBatch(ctx context.Context, b PreparedObservationBatch, generationID int64, actor string) (personobs.BatchResult, error) {
	for restart := 0; ; restart++ {
		result, err := retryContendedWrite(ctx, s, "apply person observations", func() (*personobs.BatchResult, error) {
			var result personobs.BatchResult
			err := s.withTxLockedContext(ctx, nil, func(tx *loggedTx) error {
				// Person merges and splits can move participant ownership. Take
				// the identity fence before discovering or locking evidence sources,
				// and keep it through commit. PostgreSQL readers share the fence;
				// SQLite uses its writer reservation to serialize with identity writes.
				var err error
				if s.IsPostgreSQL() {
					err = s.shareIdentityLockTxContext(ctx, tx)
				} else {
					err = s.lockIdentityMutationTxContext(ctx, tx)
				}
				if err != nil {
					return err
				}
				if generationID != 0 {
					// Tracking transitions use this same per-person fence. Keep it
					// through commit so an already-prepared machine batch cannot
					// outlive an untrack operation.
					if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-fact-generation", b.PersonID); err != nil {
						return err
					}
					if err := verifyTrackedPersonFactPersonTx(ctx, tx, b.PersonID); err != nil {
						return err
					}
				}
				if s.personObservationTxBarrier != nil {
					s.personObservationTxBarrier("identity-locked")
				}
				return nil
			}, func(tx *loggedTx) error {
				var err error
				result, err = s.applyPersonObservationBatchTx(ctx, tx, b, generationID, actor)
				return err
			})
			return &result, err
		})
		if !errors.Is(err, errObservationSourceLockSetChanged) {
			if err != nil {
				return personobs.BatchResult{}, err
			}
			return *result, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return personobs.BatchResult{}, ctxErr
		}
		if restart >= maxObservationSourceLockRestarts {
			return personobs.BatchResult{}, fmt.Errorf(
				"apply person observations: source lock set kept changing after %d restarts: %w",
				maxObservationSourceLockRestarts, err,
			)
		}
	}
}
func (s *Store) applyPersonObservationBatchTx(ctx context.Context, tx *loggedTx, b PreparedObservationBatch, generationID int64, actor string) (personobs.BatchResult, error) {
	ctx = context.WithValue(ctx, observationEventGenerationKey{}, generationID)
	result := personobs.BatchResult{Items: make([]personobs.BatchItemResult, 0, len(b.Items))}
	if err := verifyPreparedObservationBatch(b); err != nil {
		return result, err
	}
	if (generationID == 0) != (b.Generation == nil) {
		return result, errors.New("observation generation ID does not match prepared provenance")
	}
	if generationID != 0 {
		var personID int64
		var fingerprint string
		if err := tx.QueryRowContext(ctx, `SELECT person_id,program_fingerprint FROM person_fact_generations WHERE id=?`, generationID).Scan(&personID, &fingerprint); err != nil {
			return result, fmt.Errorf("read observation generation: %w", err)
		}
		if personID != b.PersonID {
			return result, errors.New("observation generation belongs to another person")
		}
		if fingerprint != b.ProgramFingerprint {
			return result, errors.New("observation generation belongs to another program")
		}
	}
	// Source rows must be locked before the person row. That makes the
	// apply-time alignment check stable against message edits and reassignment.
	lockedSources, err := s.lockObservationSupportSourcesTx(ctx, tx, b)
	if err != nil {
		return result, err
	}
	if s.personObservationTxBarrier != nil {
		s.personObservationTxBarrier("source-rows-locked")
	}
	if err := s.lockPersonMutationTx(ctx, tx, b.PersonID); err != nil {
		return result, err
	}
	currentSources, err := s.observationSupportSourcePlanTx(ctx, tx, b)
	if err != nil {
		return result, err
	}
	if observationSourcePlanNeedsRestart(lockedSources, currentSources) {
		return result, errObservationSourceLockSetChanged
	}
	if s.personObservationTxBarrier != nil {
		s.personObservationTxBarrier("sources-revalidated")
	}
	premiseIDs := currentSources.premiseIDs
	for _, premiseID := range premiseIDs {
		_, changed, err := s.refreshObservationCitationExclusionsTx(ctx, tx, premiseID)
		if err != nil {
			return result, err
		}
		if changed {
			if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{premiseID}, "citation-exclusions-changed"); err != nil {
				return result, err
			}
		}
	}
	for index, item := range b.Items {
		out := personobs.BatchItemResult{Index: index, Failure: item.Failure}
		if item.Failure != nil {
			out.Outcome = "dropped-invalid"
			result.Items = append(result.Items, out)
			continue
		}
		for _, support := range item.Proposal.Support {
			alignment, err := (PersonSweepEvidenceAligner{Store: s}).alignTx(ctx, tx, support.Evidence)
			if err != nil {
				return result, fmt.Errorf("revalidate observation support: %w", err)
			}
			if !alignment.Accepted || support.Evidence.PersonID != b.PersonID || support.Evidence.SubjectPersonID == nil || *support.Evidence.SubjectPersonID != b.PersonID || support.Evidence.Directness != personfacts.DirectSelf {
				out.Outcome = "dropped-invalid"
				out.Failure = &personobs.ValidationError{Field: "support", Reason: "unaligned-evidence"}
				break
			}
		}
		if out.Failure != nil {
			result.Items = append(result.Items, out)
			continue
		}
		premises := make(map[int64]personobs.Observation, len(item.Proposal.Premises))
		for _, edge := range item.Proposal.Premises {
			premise, err := s.loadObservationTx(ctx, tx, edge.ID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					out.Failure = &personobs.ValidationError{Field: "premises", Reason: "premise-missing"}
					break
				}
				return result, err
			}
			supported, err := s.observationPremiseCurrentlySupportedTx(
				ctx, tx, b.PersonID, premise.ID, map[int64]bool{}, map[int64]bool{},
			)
			if err != nil {
				return result, err
			}
			if !supported && premise.Status == personobs.StatusActive {
				premise.Status = personobs.StatusUnsupported
			}
			premises[edge.ID] = premise
		}
		if out.Failure == nil {
			out.Failure = personobs.ValidateProposal(item.Proposal, premises)
		}
		if out.Failure != nil {
			out.Outcome = "dropped-invalid"
			result.Items = append(result.Items, out)
			continue
		}
		item.Categories = append([]personobs.Category(nil), item.Categories...)
		for _, edge := range item.Proposal.Premises {
			premise := premises[edge.ID]
			item.Categories = append(item.Categories, premise.Categories...)
			item.Proposal.Sensitive = item.Proposal.Sensitive || premise.Sensitive
		}
		slices.Sort(item.Categories)
		item.Categories = slices.Compact(item.Categories)
		item.Proposal.Sensitive = item.Proposal.Sensitive || len(item.Categories) > 0
		var existing personobs.Observation
		if item.Proposal.ReinforcesID != nil {
			var err error
			existing, err = s.loadObservationTx(ctx, tx, *item.Proposal.ReinforcesID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return result, err
			}
			if err != nil || existing.PersonID != b.PersonID || existing.Level != item.Proposal.Level || personobs.NormalizeContent(existing.SubjectRef) != personobs.NormalizeContent(item.Proposal.SubjectRef) || existing.Status == personobs.StatusRejected {
				out.Outcome = "dropped-invalid"
				out.Failure = &personobs.ValidationError{Field: "reinforces_id", Reason: "reinforcement-target"}
				result.Items = append(result.Items, out)
				continue
			}
			// A target superseded since preparation falls back to the current
			// live key, or a new row if that key is no longer live.
			if existing.Status == personobs.StatusSuperseded {
				existing = personobs.Observation{}
			}
		}
		if existing.ID == 0 {
			matches, err := s.queryObservationRows(ctx, tx, `person_id=? AND content_key=? AND status IN ('active','unsupported')`, `id LIMIT 1`, b.PersonID, item.ContentKey)
			if err != nil {
				return result, err
			}
			if len(matches) > 0 {
				existing = matches[0]
			}
		}
		for _, targetID := range item.Proposal.SupersedesIDs {
			if existing.ID != 0 && targetID == existing.ID {
				continue
			}
			target, err := s.loadObservationTx(ctx, tx, targetID)
			if errors.Is(err, sql.ErrNoRows) {
				out.Outcome = "dropped-invalid"
				out.Failure = &personobs.ValidationError{Field: "supersedes_ids", Reason: "target-missing"}
				break
			}
			if err != nil {
				return result, err
			}
			if target.PersonID != b.PersonID {
				out.Outcome = "dropped-invalid"
				out.Failure = &personobs.ValidationError{Field: "supersedes_ids", Reason: "target-person"}
				break
			}
		}
		if out.Failure != nil {
			result.Items = append(result.Items, out)
			continue
		}
		if existing.ID != 0 {
			for _, edge := range item.Proposal.Premises {
				premise := premises[edge.ID]
				_, failure, err := s.observationPremiseAppendStateTx(ctx, tx, existing, premise, edge.Role)
				if err != nil {
					return result, err
				}
				if failure != nil {
					out.Outcome = "dropped-invalid"
					out.Failure = failure
					break
				}
			}
		}
		if out.Failure != nil {
			result.Items = append(result.Items, out)
			continue
		}
		existingCitationsChanged := false
		if existing.ID != 0 {
			_, changed, err := s.refreshObservationCitationExclusionsTx(ctx, tx, existing.ID)
			if err != nil {
				return result, err
			}
			existingCitationsChanged = changed
		}
		candidate, err := s.observationSuppressionCandidateTx(ctx, tx, b.PersonID, item, premises)
		if err != nil {
			return result, err
		}
		itemSavepoint := fmt.Sprintf("person_observation_item_%d", index)
		if _, err := tx.ExecContext(ctx, "SAVEPOINT "+itemSavepoint); err != nil {
			return result, fmt.Errorf("save person observation batch item: %w", err)
		}
		verdict, err := s.DecideSuppressionTx(ctx, tx, b.PersonID, item.ContentKey, candidate)
		if err != nil {
			return result, err
		}
		if verdict == SuppressionSuppressed {
			if existingCitationsChanged {
				if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{existing.ID}, "citation-exclusions-changed"); err != nil {
					return result, err
				}
			}
			out.Outcome = "suppressed"
			if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+itemSavepoint); err != nil {
				return result, fmt.Errorf("release suppressed person observation batch item: %w", err)
			}
			result.Items = append(result.Items, out)
			continue
		}
		id := existing.ID
		ownerAssertion := id != 0 && item.Proposal.Origin == personobs.OriginOwner && existing.Origin != personobs.OriginOwner
		if ownerAssertion {
			matches, err := s.queryObservationRows(ctx, tx, `person_id=? AND content_key=? AND id<>? AND status IN ('active','unsupported')`, `id LIMIT 1`, b.PersonID, item.ContentKey, id)
			if err != nil {
				return result, err
			}
			if len(matches) > 0 {
				if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+itemSavepoint); err != nil {
					return result, fmt.Errorf("rollback colliding person observation batch item: %w", err)
				}
				if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+itemSavepoint); err != nil {
					return result, fmt.Errorf("release colliding person observation batch item: %w", err)
				}
				if existingCitationsChanged {
					if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{id}, "citation-exclusions-changed"); err != nil {
						return result, err
					}
				}
				out.Outcome = "dropped-invalid"
				out.Failure = &personobs.ValidationError{Field: "content", Reason: "live-key-collision"}
				result.Items = append(result.Items, out)
				continue
			}
		}
		if ownerAssertion {
			if err := s.promoteObservationToOwnerTx(ctx, tx, b.PersonID, id, item.Proposal, item.Stability); err != nil {
				return result, err
			}
		}
		if id == 0 {
			sequence, err := s.nextObservationSequenceTx(ctx, tx, b.PersonID)
			if err != nil {
				return result, err
			}
			source := personobs.StabilitySourceProgram
			if item.Proposal.Origin == personobs.OriginOwner {
				source = personobs.StabilitySourceOwner
			}
			now := observationNowAfter(time.Time{})
			err = tx.QueryRowContext(ctx, `INSERT INTO person_observations
    (person_id,sequence,generation_id,level,origin,kind,content,content_key,normalization_version,
     subject_ref,directness,sensitive,confidence_bp,valid_from,valid_until,occurs_on,program_fingerprint,
     status,stability,stability_source,target_kind,target_key,created_at,updated_at)
    VALUES(?,?,?,?,?,?,?,?,'obs-norm-v1',?,?,?,?,?,?,?,?,'active',?,?,?,?,?,?) RETURNING id`,
				b.PersonID, sequence, observationOptionalGeneration(generationID), item.Proposal.Level, item.Proposal.Origin, item.Proposal.Kind, item.Proposal.Content, item.ContentKey,
				item.Proposal.SubjectRef, personobs.DirectnessFor(item.Proposal.Level, item.Proposal.Origin, item.Proposal.Attribution), item.Proposal.Sensitive, item.Proposal.ConfidenceBP,
				item.Proposal.ValidFrom, item.Proposal.ValidUntil, observationOptionalDate(item.Proposal.OccursOn), b.ProgramFingerprint, item.Stability, source,
				observationOptionalText(item.Proposal.TargetKind), observationOptionalText(item.Proposal.TargetKey), now, now).Scan(&id)
			if err != nil {
				return result, fmt.Errorf("insert person observation: %w", err)
			}
			out.Outcome = "created"
			if err := s.appendObservationEventTx(ctx, tx, b.PersonID, id, "created", actor, "", nil); err != nil {
				return result, err
			}
		} else {
			out.Outcome = "reinforced"
		}
		if err := s.writeObservationCitationsTx(ctx, tx, b.PersonID, id, generationID, item); err != nil {
			return result, err
		}
		if err := s.unionObservationCategoriesTx(ctx, tx, b.PersonID, id, item, actor, out.Outcome == "created"); err != nil {
			return result, err
		}
		for _, edge := range item.Proposal.Premises {
			if err := s.appendObservationPremiseTx(ctx, tx, id, edge, generationID, actor); err != nil {
				return result, err
			}
		}
		if out.Outcome == "reinforced" {
			quotes := make([]string, 0, len(item.Proposal.Support))
			for _, support := range item.Proposal.Support {
				quotes = append(quotes, support.Quote)
			}
			detail, err := json.Marshal(struct {
				Quotes         []string `json:"quotes"`
				OwnerAssertion bool     `json:"owner_assertion,omitempty"`
			}{quotes, ownerAssertion})
			if err != nil {
				return result, err
			}
			if err := s.appendObservationEventTx(ctx, tx, b.PersonID, id, "reinforced", actor, string(detail), nil); err != nil {
				return result, err
			}
		}
		if verdict == SuppressionLifted {
			if err := s.ensureObservationSuppressionLiftEventTx(ctx, tx, b.PersonID, item.ContentKey, id); err != nil {
				return result, err
			}
		}
		if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{id}, "written"); err != nil {
			return result, err
		}
		out.ObservationID = id
		replacement, err := s.loadObservationTx(ctx, tx, id)
		if err != nil {
			return result, err
		}
		canSupersede := item.Proposal.Origin == personobs.OriginOwner
		if !canSupersede {
			canSupersede, err = s.observationPremiseCurrentlySupportedTx(
				ctx, tx, b.PersonID, id, map[int64]bool{}, map[int64]bool{},
			)
			if err != nil {
				return result, err
			}
		}
		if canSupersede {
			removedTargets := make(map[int64]bool)
			itemDropped := false
			for _, targetID := range item.Proposal.SupersedesIDs {
				if targetID == id {
					continue
				}
				target, err := s.loadObservationTx(ctx, tx, targetID)
				if err != nil {
					return result, err
				}
				if target.PersonID != b.PersonID {
					return result, errors.New("supersession target belongs to another person")
				}
				if target.Status != personobs.StatusActive && target.Status != personobs.StatusUnsupported {
					continue
				}
				out.AffectedIDs = append(out.AffectedIDs, targetID)
				if observationProtected(target) {
					if err := s.recordObservationSupersedeBlockedTx(ctx, tx, b.PersonID, id, targetID, actor); err != nil {
						return result, err
					}
					out.Outcome = "supersede-blocked"
					continue
				}
				excludedTargets := make(map[int64]bool, len(removedTargets)+1)
				for removedID := range removedTargets {
					excludedTargets[removedID] = true
				}
				excludedTargets[targetID] = true
				stillSupported, err := s.observationPremiseCurrentlySupportedExcludingTx(
					ctx, tx, b.PersonID, id, excludedTargets, map[int64]bool{}, map[int64]bool{},
				)
				if err != nil {
					return result, err
				}
				if !stillSupported {
					if err := s.recordObservationSupersedeBlockedTx(ctx, tx, b.PersonID, id, targetID, actor); err != nil {
						return result, err
					}
					out.Outcome = "supersede-blocked"
					continue
				}
				var end *time.Time
				end = replacement.ValidFrom
				if end == nil {
					end, err = s.observationLatestSupportedEvidenceTimeTx(ctx, tx, b.PersonID, id)
					if err != nil {
						return result, err
					}
				}
				if target.ValidUntil != nil && (end == nil || target.ValidUntil.Before(*end)) {
					end = target.ValidUntil
				}
				if end != nil && target.ValidFrom != nil && end.Before(*target.ValidFrom) {
					if _, err := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+itemSavepoint); err != nil {
						return result, fmt.Errorf("rollback invalid person observation batch item: %w", err)
					}
					if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+itemSavepoint); err != nil {
						return result, fmt.Errorf("release invalid person observation batch item: %w", err)
					}
					out.Outcome = "dropped-invalid"
					out.Failure = &personobs.ValidationError{Field: "supersedes_ids", Reason: "target-validity"}
					out.ObservationID = 0
					out.AffectedIDs = nil
					itemDropped = true
					break
				}
				if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET superseded_by_id=?,valid_until=COALESCE(?,valid_until) WHERE id=?`, id, end, targetID); err != nil {
					return result, err
				}
				if err := s.transitionObservationTx(ctx, tx, targetID, personobs.StatusSuperseded, personobs.StatusReasonKnowledgeUpdate, "superseded", actor, "knowledge-update"); err != nil {
					return result, err
				}
				if err := s.notifyObservationSupportChangedTx(ctx, tx, b.PersonID, []int64{targetID}, "superseded"); err != nil {
					return result, err
				}
				if out.Outcome != "supersede-blocked" {
					out.Outcome = "superseded-target"
				}
				removedTargets[targetID] = true
			}
			if itemDropped {
				result.Items = append(result.Items, out)
				continue
			}
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT "+itemSavepoint); err != nil {
			return result, fmt.Errorf("release person observation batch item: %w", err)
		}
		result.Items = append(result.Items, out)
	}
	return result, nil
}

func (s *Store) writeObservationCitationsTx(ctx context.Context, tx *loggedTx, personID, id, generationID int64, item PreparedObservation) error {
	if len(item.Citations) != len(item.Proposal.Support) {
		return errors.New("prepared observation citations do not match support")
	}
	for i, input := range item.Proposal.Support {
		key, err := personfacts.EvidenceKey(input.Evidence)
		if err != nil {
			return err
		}
		evidence, err := s.insertPersonFactEvidenceTx(ctx, tx, personID, key, input.Evidence)
		if err != nil {
			return err
		}
		c := item.Citations[i]
		c.Leaf, c.Exclusion, err = s.observationLeafTx(ctx, tx, c.Leaf.MessageID)
		if err != nil {
			return fmt.Errorf("refresh observation citation leaf: %w", err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_observation_evidence
   (observation_id,evidence_id,generation_id,quote_start,quote_end,quote_sha256,exclusion_reason,message_id,conversation_id,conversation_kind,authored_by_person,author_is_owner,event_time)
	   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT (observation_id,evidence_id) DO UPDATE SET exclusion_reason=excluded.exclusion_reason`, id, evidence.ID, observationOptionalGeneration(generationID), c.QuoteStart, c.QuoteEnd, c.QuoteSHA256, c.Exclusion, c.Leaf.MessageID, c.Leaf.ConversationID, c.Leaf.ConversationKind, c.Leaf.AuthoredByPerson, c.Leaf.AuthorIsOwner, c.Leaf.EventTime)
		if err != nil {
			return fmt.Errorf("write observation citation: %w", err)
		}
	}
	return s.recomputeObservationCitationSummaryTx(ctx, tx, id)
}

func (s *Store) refreshObservationCitationExclusionsTx(ctx context.Context, tx *loggedTx, observationID int64) ([]personobs.Citation, bool, error) {
	citations, err := s.observationCitations(ctx, tx, observationID)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for i := range citations {
		citation := citations[i]
		exclusion, err := s.observationCitationExclusionTx(ctx, tx, citation.Leaf.MessageID, citation.Exclusion)
		if err != nil {
			return nil, false, fmt.Errorf("refresh observation citation exclusion: %w", err)
		}
		citations[i].Exclusion = exclusion
		if exclusion == citation.Exclusion {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_observation_evidence SET exclusion_reason=? WHERE observation_id=? AND evidence_id=?`, exclusion, observationID, citation.EvidenceID); err != nil {
			return nil, false, fmt.Errorf("update observation citation exclusion: %w", err)
		}
		changed = true
	}
	if !changed {
		return citations, false, nil
	}
	if err := s.recomputeObservationCitationSummaryTx(ctx, tx, observationID); err != nil {
		return nil, false, err
	}
	return citations, true, nil
}

// observationPremiseCurrentlySupportedTx rechecks an observation's support
// graph while its source rows are locked. Stored support counts can be stale
// after a participant split or a citation status change.
func (s *Store) observationPremiseCurrentlySupportedTx(
	ctx context.Context,
	tx *loggedTx,
	personID, observationID int64,
	visiting, supportedCache map[int64]bool,
) (bool, error) {
	return s.observationPremiseCurrentlySupportedExcludingTx(ctx, tx, personID, observationID, nil, visiting, supportedCache)
}

// observationPremiseCurrentlySupportedExcludingTx evaluates the support graph
// as though excluded observations had already been superseded. This prevents a
// replacement from superseding a premise or ancestor that it still depends on.
func (s *Store) observationPremiseCurrentlySupportedExcludingTx(
	ctx context.Context,
	tx *loggedTx,
	personID, observationID int64,
	excluded map[int64]bool,
	visiting, supportedCache map[int64]bool,
) (bool, error) {
	if excluded[observationID] {
		return false, nil
	}
	if cached, ok := supportedCache[observationID]; ok {
		return cached, nil
	}
	if visiting[observationID] {
		return false, nil
	}
	visiting[observationID] = true
	defer delete(visiting, observationID)

	observation, err := s.loadObservationTx(ctx, tx, observationID)
	if errors.Is(err, sql.ErrNoRows) {
		supportedCache[observationID] = false
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if observation.PersonID != personID || !personobs.PremiseSupported(observation) {
		supportedCache[observationID] = false
		return false, nil
	}
	if observation.Level == personobs.LevelExplicit {
		if observation.Origin == personobs.OriginOwner {
			supportedCache[observationID] = true
			return true, nil
		}
		citations, err := s.observationCitations(ctx, tx, observationID)
		if err != nil {
			return false, err
		}
		for _, citation := range citations {
			ok, err := s.observationCitationCurrentlySupportedTx(ctx, tx, personID, citation)
			if err != nil {
				return false, err
			}
			if ok {
				supportedCache[observationID] = true
				return true, nil
			}
		}
		supportedCache[observationID] = false
		return false, nil
	}

	rows, err := tx.QueryContext(ctx, `SELECT premise_id,role FROM person_observation_premises WHERE derived_id=? ORDER BY premise_id`, observationID)
	if err != nil {
		return false, fmt.Errorf("read observation support graph: %w", err)
	}
	defer func() { _ = rows.Close() }()
	type supportEdge struct {
		premiseID int64
		role      personobs.PremiseRole
	}
	edges := make([]supportEdge, 0)
	for rows.Next() {
		var edge supportEdge
		if err := rows.Scan(&edge.premiseID, &edge.role); err != nil {
			return false, fmt.Errorf("scan observation support graph: %w", err)
		}
		edges = append(edges, edge)
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate observation support graph: %w", err)
	}
	if err := rows.Close(); err != nil {
		return false, fmt.Errorf("close observation support graph: %w", err)
	}
	supportEdges, supportedEdges := 0, 0
	for _, edge := range edges {
		parentSupported, err := s.observationPremiseCurrentlySupportedExcludingTx(
			ctx, tx, personID, edge.premiseID, excluded, visiting, supportedCache,
		)
		if err != nil {
			return false, err
		}
		if edge.role == personobs.PremiseRoleSupport {
			supportEdges++
			if !parentSupported {
				supportedCache[observationID] = false
				return false, nil
			}
		}
		if parentSupported {
			supportedEdges++
		}
	}
	minimum := 1
	switch observation.Level {
	case personobs.LevelExplicit:
		// Explicit observations return through the citation path above.
		return false, nil
	case personobs.LevelDeductive:
		minimum = 1
	case personobs.LevelInductive:
		minimum = 2
	case personobs.LevelContradiction:
		minimum = 2
		supportEdges = supportedEdges
	}
	current := supportEdges >= minimum
	supportedCache[observationID] = current
	return current, nil
}

func (s *Store) observationCitationCurrentlySupportedTx(
	ctx context.Context, tx *loggedTx, personID int64, citation personobs.Citation,
) (bool, error) {
	if citation.Exclusion != personobs.ExclusionReasonNone || !citation.Leaf.AuthoredByPerson {
		return false, nil
	}
	var evidenceKey string
	err := tx.QueryRowContext(ctx, `SELECT evidence_key FROM person_fact_evidence WHERE id=? AND person_id=?`,
		citation.EvidenceID, personID).Scan(&evidenceKey)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("resolve current observation citation: %w", err)
	}
	evidence, err := s.loadPersonFactEvidenceRowTx(ctx, tx, personID, evidenceKey)
	if err != nil {
		return false, err
	}
	status, err := s.latestEvidenceSupportedTx(ctx, tx, []int64{evidence.ID})
	if err != nil {
		return false, err
	}
	if !status[evidence.ID] {
		return false, nil
	}
	alignment, err := (PersonSweepEvidenceAligner{Store: s}).alignTx(ctx, tx, evidence.Input)
	if err != nil {
		return false, fmt.Errorf("revalidate observation citation identity: %w", err)
	}
	return alignment.Accepted, nil
}

func (s *Store) recomputeObservationCitationSummaryTx(ctx context.Context, tx *loggedTx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE person_observations SET
  mention_count=(SELECT COUNT(*) FROM (SELECT message_id FROM person_observation_evidence WHERE observation_id=? AND exclusion_reason='' GROUP BY message_id) mentions),
  first_seen_at=(SELECT MIN(event_time) FROM person_observation_evidence WHERE observation_id=? AND exclusion_reason=''),
  last_seen_at=(SELECT MAX(event_time) FROM person_observation_evidence WHERE observation_id=? AND exclusion_reason='') WHERE id=?`, id, id, id, id)
	return err
}

func (s *Store) observationCitationExclusionTx(ctx context.Context, tx *loggedTx, messageID int64, prepared personobs.ExclusionReason) (personobs.ExclusionReason, error) {
	if prepared != personobs.ExclusionReasonNone && prepared != personobs.ExclusionReasonList && prepared != personobs.ExclusionReasonDraft && prepared != personobs.ExclusionReasonHiddenText {
		return prepared, nil
	}
	var listID string
	var draft, hiddenText bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(list_id,''),draft_authored,hidden_text_present FROM messages WHERE id=?`, messageID).Scan(&listID, &draft, &hiddenText)
	if errors.Is(err, sql.ErrNoRows) {
		// Keep the prepared projection if its source message has since been deleted.
		return prepared, nil
	}
	if err != nil {
		return "", fmt.Errorf("read observation source exclusions: %w", err)
	}
	if draft {
		return personobs.ExclusionReasonDraft, nil
	}
	if hiddenText {
		return personobs.ExclusionReasonHiddenText, nil
	}
	if strings.TrimSpace(listID) != "" {
		return personobs.ExclusionReasonList, nil
	}
	return personobs.ExclusionReasonNone, nil
}

func (s *Store) unionObservationCategoriesTx(ctx context.Context, tx *loggedTx, personID, id int64, item PreparedObservation, actor string, created bool) error {
	changed := false
	lexicon := personobs.DetectCategories(item.Proposal.Content)
	for _, category := range item.Categories {
		source := "premise"
		if slices.Contains(lexicon, category) {
			source = "lexicon"
		}
		if slices.Contains(item.Proposal.ModelCategories, category) {
			source = "model"
		}
		if item.Proposal.Origin == personobs.OriginOwner {
			source = "owner"
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) VALUES(?,?,?) ON CONFLICT DO NOTHING`, id, category, source)
		if err != nil {
			return err
		}
		added, err := result.RowsAffected()
		if err != nil {
			return err
		}
		changed = changed || added > 0
	}
	sensitiveChanged := false
	if item.Proposal.Sensitive {
		result, err := tx.ExecContext(ctx, `UPDATE person_observations SET sensitive=TRUE WHERE id=? AND sensitive=FALSE`, id)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		sensitiveChanged = updated > 0
	}
	if !changed && !sensitiveChanged {
		return nil
	}
	// Creation already captures the initial set; every later change gets an event.
	if !created {
		if err := s.appendObservationEventTx(ctx, tx, personID, id, "sensitivity-changed", actor, "", nil); err != nil {
			return err
		}
		return s.propagateObservationClassificationsTx(ctx, tx, personID, id)
	}
	return nil
}

// propagateObservationClassificationsTx carries added categories and
// sensitivity through existing dependents. It visits the reachable graph once
// and revisits a dependent if another path adds a classification later.
func (s *Store) propagateObservationClassificationsTx(ctx context.Context, tx *loggedTx, personID, premiseID int64) error {
	pending := []int64{premiseID}
	queued := map[int64]bool{premiseID: true}
	processed := map[int64]bool{}
	for len(pending) > 0 {
		currentID := pending[0]
		pending = pending[1:]
		queued[currentID] = false
		processed[currentID] = true

		premise, err := s.loadObservationTx(ctx, tx, currentID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if premise.PersonID != personID {
			continue
		}

		rows, err := tx.QueryContext(ctx, `SELECT derived_id FROM person_observation_premises WHERE premise_id=? ORDER BY derived_id`, currentID)
		if err != nil {
			return fmt.Errorf("read observation dependents: %w", err)
		}
		var dependentIDs []int64
		for rows.Next() {
			var dependentID int64
			if err := rows.Scan(&dependentID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan observation dependent: %w", err)
			}
			dependentIDs = append(dependentIDs, dependentID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate observation dependents: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close observation dependents: %w", err)
		}

		for _, dependentID := range dependentIDs {
			dependent, err := s.loadObservationTx(ctx, tx, dependentID)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if dependent.PersonID != personID {
				continue
			}

			changed := false
			for _, category := range premise.Categories {
				result, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) VALUES(?,?,'premise') ON CONFLICT DO NOTHING`, dependentID, category)
				if err != nil {
					return fmt.Errorf("propagate observation category: %w", err)
				}
				added, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("count propagated observation categories: %w", err)
				}
				changed = changed || added > 0
			}
			if premise.Sensitive {
				result, err := tx.ExecContext(ctx, `UPDATE person_observations SET sensitive=TRUE WHERE id=? AND person_id=? AND sensitive=FALSE`, dependentID, personID)
				if err != nil {
					return fmt.Errorf("propagate observation sensitivity: %w", err)
				}
				updated, err := result.RowsAffected()
				if err != nil {
					return fmt.Errorf("count propagated observation sensitivity: %w", err)
				}
				changed = changed || updated > 0
			}
			if changed {
				if err := s.appendObservationEventTx(ctx, tx, personID, dependentID, "sensitivity-changed", "system", "premise-classification-changed", nil); err != nil {
					return err
				}
			}
			if (changed || !processed[dependentID]) && !queued[dependentID] {
				pending = append(pending, dependentID)
				queued[dependentID] = true
			}
		}
	}
	return nil
}

func (s *Store) promoteObservationToOwnerTx(ctx context.Context, tx *loggedTx, personID, id int64, proposal personobs.ProposedObservation, stability personobs.Stability) error {
	_, err := tx.ExecContext(ctx, `UPDATE person_observations SET
  generation_id=NULL,origin=?,kind=?,content=?,content_key=?,directness=?,
  confidence_bp=?,valid_from=?,valid_until=?,occurs_on=?,
  status='active',status_reason='',support_flag='',stability=?,stability_source=?,
  target_kind=?,target_key=?,program_fingerprint='',updated_at=? WHERE id=? AND person_id=?`,
		personobs.OriginOwner, proposal.Kind, proposal.Content, personobs.ContentKey(proposal.Level, proposal.SubjectRef, proposal.Content),
		personobs.DirectnessFor(proposal.Level, proposal.Origin, proposal.Attribution), proposal.ConfidenceBP,
		proposal.ValidFrom, proposal.ValidUntil, observationOptionalDate(proposal.OccursOn), stability, personobs.StabilitySourceOwner,
		observationOptionalText(proposal.TargetKind), observationOptionalText(proposal.TargetKey), time.Now().UTC().Truncate(time.Microsecond), id, personID)
	if err != nil {
		return fmt.Errorf("promote person observation to owner assertion: %w", err)
	}
	return nil
}
func (s *Store) appendObservationPremiseTx(ctx context.Context, tx *loggedTx, derivedID int64, p personobs.PremiseInput, generationID int64, actor string) error {
	derived, err := s.loadObservationTx(ctx, tx, derivedID)
	if err != nil {
		return err
	}
	premise, err := s.loadObservationTx(ctx, tx, p.ID)
	if err != nil {
		return err
	}
	exists, failure, err := s.observationPremiseAppendStateTx(ctx, tx, derived, premise, p.Role)
	if err != nil {
		return err
	}
	if failure != nil {
		return failure
	}
	if exists {
		return nil
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_observation_premises(derived_id,premise_id,role,ordinal,added_generation_id)
  VALUES(?,?,?,(SELECT COALESCE(MAX(ordinal),-1)+1 FROM person_observation_premises WHERE derived_id=?),?)`, derivedID, p.ID, p.Role, derivedID, observationOptionalGeneration(generationID))
	if err != nil {
		return fmt.Errorf("append observation premise: %w", err)
	}
	if err := s.appendObservationEventTx(ctx, tx, derived.PersonID, derivedID, "premises-appended", actor, "", &p.ID); err != nil {
		return err
	}
	return s.notifyObservationSupportChangedTx(ctx, tx, derived.PersonID, []int64{derivedID}, "premises-appended")
}

func (s *Store) observationPremiseAppendStateTx(
	ctx context.Context, tx *loggedTx, derived, premise personobs.Observation, role personobs.PremiseRole,
) (bool, *personobs.ValidationError, error) {
	if failure := personobs.ValidateAppend(derived, premise, role); failure != nil {
		return false, failure, nil
	}
	var existing personobs.PremiseRole
	err := tx.QueryRowContext(ctx, `SELECT role FROM person_observation_premises WHERE derived_id=? AND premise_id=?`, derived.ID, premise.ID).Scan(&existing)
	if err == nil {
		if existing != role {
			return false, &personobs.ValidationError{Field: "premises", Reason: "premise-role-immutable"}, nil
		}
		return true, nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, nil, fmt.Errorf("read observation premise: %w", err)
	}
	return false, nil, nil
}

func (s *Store) observationSuppressionCandidateTx(ctx context.Context, tx *loggedTx, personID int64, item PreparedObservation, premises map[int64]personobs.Observation) (SuppressionCandidate, error) {
	candidate := SuppressionCandidate{Origin: item.Proposal.Origin, Floor: 1}
	if item.Proposal.Level == personobs.LevelInductive || item.Proposal.Level == personobs.LevelContradiction {
		candidate.Floor = 2
	}
	var rejected requiredTimestamp
	err := tx.QueryRowContext(ctx, `SELECT rejected_at FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, personID, item.ContentKey).Scan(&rejected)
	if errors.Is(err, sql.ErrNoRows) {
		return candidate, nil
	}
	if err != nil {
		return candidate, err
	}
	if len(item.Citations) != len(item.Proposal.Support) {
		return candidate, errors.New("prepared observation citations do not match support")
	}
	type eligibleCitation struct {
		citation    personobs.Citation
		evidenceKey string
	}
	eligible := make([]eligibleCitation, 0, len(item.Citations))
	evidenceKeys := make([]string, 0, len(item.Citations))
	seenEvidenceKeys := make(map[string]struct{}, len(item.Citations))
	for i, c := range item.Citations {
		exclusion, err := s.observationCitationExclusionTx(ctx, tx, c.Leaf.MessageID, c.Exclusion)
		if err != nil {
			return candidate, fmt.Errorf("refresh observation proposal exclusion: %w", err)
		}
		if exclusion != personobs.ExclusionReasonNone || !c.Leaf.AuthoredByPerson || !c.Leaf.EventTime.After(rejected.Time) {
			continue
		}
		evidenceKey, err := personfacts.EvidenceKey(item.Proposal.Support[i].Evidence)
		if err != nil {
			return candidate, fmt.Errorf("resolve observation proposal evidence: %w", err)
		}
		eligible = append(eligible, eligibleCitation{citation: c, evidenceKey: evidenceKey})
		if _, seen := seenEvidenceKeys[evidenceKey]; !seen {
			seenEvidenceKeys[evidenceKey] = struct{}{}
			evidenceKeys = append(evidenceKeys, evidenceKey)
		}
	}
	existingIDs, err := s.personFactEvidenceIDsByKeyTx(ctx, tx, personID, evidenceKeys)
	if err != nil {
		return candidate, err
	}
	evidenceIDs := make([]int64, 0, len(existingIDs))
	for _, evidenceKey := range evidenceKeys {
		if evidenceID, exists := existingIDs[evidenceKey]; exists {
			evidenceIDs = append(evidenceIDs, evidenceID)
		}
	}
	latestSupport, err := s.latestEvidenceSupportedTx(ctx, tx, evidenceIDs)
	if err != nil {
		return candidate, err
	}
	messages := map[int64]bool{}
	for _, item := range eligible {
		if evidenceID, exists := existingIDs[item.evidenceKey]; exists && !latestSupport[evidenceID] {
			continue
		}
		messages[item.citation.Leaf.MessageID] = true
	}
	candidate.PostRejectionSupport = len(messages)
	if item.Proposal.Level != personobs.LevelExplicit {
		for _, edge := range item.Proposal.Premises {
			if edge.Role != personobs.PremiseRoleSupport && (item.Proposal.Level != personobs.LevelContradiction || edge.Role != personobs.PremiseRoleCounter) {
				continue
			}
			p := premises[edge.ID]
			if !personobs.PremiseSupported(p) {
				continue
			}
			fresh, err := s.observationHasFreshSupportedCitationTx(ctx, tx, personID, p.ID, rejected.Time)
			if err != nil {
				return candidate, err
			}
			if fresh {
				candidate.PostRejectionSupport++
			}
		}
	}
	return candidate, nil
}

func (s *Store) personFactEvidenceIDsByKeyTx(ctx context.Context, tx *loggedTx, personID int64, evidenceKeys []string) (map[string]int64, error) {
	ids := make(map[string]int64, len(evidenceKeys))
	keys := slices.Clone(evidenceKeys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	for start := 0; start < len(keys); start += 500 {
		chunk := keys[start:min(start+500, len(keys))]
		args := make([]any, 1, len(chunk)+1)
		args[0] = personID
		for _, key := range chunk {
			args = append(args, key)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id,evidence_key FROM person_fact_evidence WHERE person_id=? AND evidence_key IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("resolve existing person fact evidence: %w", err)
		}
		for rows.Next() {
			var id int64
			var key string
			if err := rows.Scan(&id, &key); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan existing person fact evidence: %w", err)
			}
			ids[key] = id
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate existing person fact evidence: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close existing person fact evidence: %w", err)
		}
	}
	return ids, nil
}

func (s *Store) observationHasFreshSupportedCitationTx(ctx context.Context, tx *loggedTx, personID, observationID int64, after time.Time) (bool, error) {
	latest, err := s.observationLatestSupportedEvidenceTimeTx(ctx, tx, personID, observationID)
	if err != nil {
		return false, err
	}
	return latest != nil && latest.After(after), nil
}

func (s *Store) observationLatestSupportedEvidenceTimeTx(ctx context.Context, tx *loggedTx, personID, observationID int64) (*time.Time, error) {
	pending := []int64{observationID}
	seenObservations := make(map[int64]struct{})
	citationsByEvidenceID := make(map[int64]personobs.Citation)
	for len(pending) > 0 {
		currentID := pending[0]
		pending = pending[1:]
		if _, seen := seenObservations[currentID]; seen {
			continue
		}
		seenObservations[currentID] = struct{}{}

		citations, changed, err := s.refreshObservationCitationExclusionsTx(ctx, tx, currentID)
		if err != nil {
			return nil, err
		}
		if changed {
			if err := s.notifyObservationSupportChangedTx(ctx, tx, personID, []int64{currentID}, "citation-exclusions-changed"); err != nil {
				return nil, err
			}
		}
		observation, err := s.loadObservationTx(ctx, tx, currentID)
		if errors.Is(err, sql.ErrNoRows) && currentID != observationID {
			continue
		}
		if err != nil {
			return nil, err
		}
		currentlySupported, err := s.observationPremiseCurrentlySupportedTx(
			ctx, tx, personID, observation.ID, map[int64]bool{}, map[int64]bool{},
		)
		if err != nil {
			return nil, err
		}
		if observation.PersonID != personID || !currentlySupported {
			continue
		}
		for _, citation := range citations {
			currentlySupported, err := s.observationCitationCurrentlySupportedTx(ctx, tx, personID, citation)
			if err != nil {
				return nil, err
			}
			if !currentlySupported {
				continue
			}
			current, exists := citationsByEvidenceID[citation.EvidenceID]
			if !exists || citation.Leaf.EventTime.After(current.Leaf.EventTime) {
				citationsByEvidenceID[citation.EvidenceID] = citation
			}
		}

		rolePredicate := `e.role=?`
		args := []any{currentID, personID, personobs.PremiseRoleSupport}
		if observation.Level == personobs.LevelContradiction {
			rolePredicate = `e.role IN (?,?)`
			args = append(args, personobs.PremiseRoleCounter)
		}
		rows, err := tx.QueryContext(ctx, `SELECT e.premise_id FROM person_observation_premises e
  JOIN person_observations derived ON derived.id=e.derived_id
  JOIN person_observations premise ON premise.id=e.premise_id AND premise.person_id=derived.person_id
  WHERE e.derived_id=? AND derived.person_id=? AND `+rolePredicate, args...)
		if err != nil {
			return nil, fmt.Errorf("read supported observation premises: %w", err)
		}
		for rows.Next() {
			var premiseID int64
			if err := rows.Scan(&premiseID); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan supported observation premise: %w", err)
			}
			pending = append(pending, premiseID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("iterate supported observation premises: %w", err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close supported observation premises: %w", err)
		}
	}

	evidenceIDs := make([]int64, 0, len(citationsByEvidenceID))
	for evidenceID := range citationsByEvidenceID {
		evidenceIDs = append(evidenceIDs, evidenceID)
	}
	slices.Sort(evidenceIDs)
	supported, err := s.latestEvidenceSupportedTx(ctx, tx, evidenceIDs)
	if err != nil {
		return nil, err
	}
	var latest *time.Time
	for evidenceID, citation := range citationsByEvidenceID {
		if !supported[evidenceID] || (latest != nil && !citation.Leaf.EventTime.After(*latest)) {
			continue
		}
		value := citation.Leaf.EventTime
		latest = &value
	}
	return latest, nil
}
func (s *Store) DecideSuppressionTx(ctx context.Context, tx *loggedTx, personID int64, contentKey string, c SuppressionCandidate) (SuppressionVerdict, error) {
	if c.Origin == personobs.OriginOwner {
		return SuppressionAllow, nil
	}
	var id int64
	var reason string
	err := tx.QueryRowContext(ctx, `SELECT id,reason FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, personID, contentKey).Scan(&id, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return SuppressionAllow, nil
	}
	if err != nil {
		return "", err
	}
	if reason != "outdated" || c.PostRejectionSupport < max(1, c.Floor) {
		return SuppressionSuppressed, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET cleared_at=? WHERE id=?`, time.Now().UTC(), id); err != nil {
		return "", err
	}
	var observationID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM person_observations WHERE person_id=? AND content_key=? ORDER BY id DESC LIMIT 1`, personID, contentKey).Scan(&observationID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if observationID != 0 {
		if err := s.ensureObservationSuppressionLiftEventTx(ctx, tx, personID, contentKey, observationID); err != nil {
			return "", err
		}
	}
	return SuppressionLifted, nil
}
func (s *Store) ensureObservationSuppressionLiftEventTx(ctx context.Context, tx *loggedTx, personID int64, key string, observationID int64) error {
	var suppressionID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NOT NULL ORDER BY id DESC LIMIT 1`, personID, key).Scan(&suppressionID); err != nil {
		return err
	}
	detail := fmt.Sprintf("outdated-new-support:suppression:%d", suppressionID)
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_observation_events WHERE person_id=? AND event='suppression-lifted' AND detail=?`, personID, detail).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return nil
	}
	return s.appendObservationEventTx(ctx, tx, personID, observationID, "suppression-lifted", "system", detail, nil)
}

// Support propagation replaces this hook in the next store layer.
func (s *Store) notifyObservationSupportChangedTx(_ context.Context, _ *loggedTx, _ int64, _ []int64, _ string) error {
	return nil
}
