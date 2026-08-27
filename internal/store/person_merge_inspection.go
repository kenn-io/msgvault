package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Store) ListPersonMergesContext(
	ctx context.Context, personID int64,
) ([]PersonMergeSummary, error) {
	return s.ListPersonMergesPageContext(ctx, personID, 100, 0)
}

func (s *Store) ListPersonMergesPageContext(
	ctx context.Context, personID int64, limit, offset int,
) ([]PersonMergeSummary, error) {
	if personID <= 0 {
		return nil, ErrPersonNotFound
	}
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	if offset < 0 {
		offset = 0
	}
	result := []PersonMergeSummary{}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		if _, err := s.getPersonTx(ctx, tx, personID); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT
			merge_record.id, merge_record.survivor_person_id_at_merge,
			merge_record.absorbed_person_id, merge_record.current_person_id,
			merge_record.survivor_uid, merge_record.absorbed_uid,
			merge_record.survivor_revision_before, merge_record.absorbed_revision_before,
			merge_record.survivor_revision_after, merge_record.actor,
			merge_record.snapshot_version, merge_record.snapshot_sha256, merge_record.created_at,
			(SELECT COUNT(*) FROM person_merge_participants lineage_count
			 WHERE lineage_count.merge_id = merge_record.id),
			(SELECT COUNT(*) FROM person_merge_rows row_count
			 WHERE row_count.merge_id = merge_record.id),
			(SELECT COUNT(*) FROM person_splits split_count
			 WHERE split_count.merge_id = merge_record.id),
			(SELECT COUNT(*) FROM person_merge_review_candidates candidate_count
			 WHERE candidate_count.merge_id = merge_record.id AND candidate_count.state = 'pending')
			FROM person_merges merge_record
			WHERE merge_record.current_person_id = ?
			   OR EXISTS (
				SELECT 1 FROM person_splits split_record
				WHERE split_record.merge_id = merge_record.id
				  AND (split_record.source_person_id = ? OR split_record.new_person_id = ?)
			   )
			   OR EXISTS (
				SELECT 1 FROM person_merge_participants lineage
				JOIN person_participants binding
				  ON binding.participant_id = lineage.participant_id
				WHERE lineage.merge_id = merge_record.id AND binding.person_id = ?
			   )
			ORDER BY merge_record.id DESC LIMIT ? OFFSET ?`,
			personID, personID, personID, personID, limit, offset)
		if err != nil {
			return fmt.Errorf("list person merge summaries: %w", err)
		}
		mergeIDs := []int64{}
		for rows.Next() {
			var summary PersonMergeSummary
			var currentID sql.NullInt64
			if err := rows.Scan(
				&summary.Merge.ID, &summary.Merge.SurvivorPersonID,
				&summary.Merge.AbsorbedPersonID, &currentID,
				&summary.Merge.SurvivorVCardUID, &summary.Merge.AbsorbedVCardUID,
				&summary.Merge.SurvivorRevisionBefore, &summary.Merge.AbsorbedRevisionBefore,
				&summary.Merge.SurvivorRevisionAfter, &summary.Merge.Actor,
				&summary.Merge.SnapshotVersion, &summary.Merge.SnapshotSHA256,
				&summary.Merge.CreatedAt, &summary.ParticipantCount, &summary.RowCount,
				&summary.SplitCount, &summary.PendingCandidateCount,
			); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan person merge summary: %w", err)
			}
			if currentID.Valid {
				summary.Merge.CurrentPersonID = &currentID.Int64
			}
			summary.RowActionCounts = map[string]int{}
			mergeIDs = append(mergeIDs, summary.Merge.ID)
			result = append(result, summary)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return fmt.Errorf("iterate person merge summaries: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close person merge summaries: %w", err)
		}
		if len(mergeIDs) == 0 {
			return nil
		}
		actionRows, err := tx.QueryContext(ctx, `SELECT merge_id, action, COUNT(*)
			FROM person_merge_rows WHERE merge_id IN (`+
			personMergeSnapshotPlaceholders(len(mergeIDs))+`)
			GROUP BY merge_id, action ORDER BY merge_id, action`,
			personMergeSnapshotIDArgs(mergeIDs)...)
		if err != nil {
			return fmt.Errorf("count person merge page row actions: %w", err)
		}
		summariesByID := make(map[int64]*PersonMergeSummary, len(result))
		for index := range result {
			summariesByID[result[index].Merge.ID] = &result[index]
		}
		for actionRows.Next() {
			var mergeID int64
			var action string
			var count int
			if err := actionRows.Scan(&mergeID, &action, &count); err != nil {
				_ = actionRows.Close()
				return fmt.Errorf("scan person merge page row action: %w", err)
			}
			summariesByID[mergeID].RowActionCounts[action] = count
		}
		if err := actionRows.Err(); err != nil {
			_ = actionRows.Close()
			return fmt.Errorf("iterate person merge page row actions: %w", err)
		}
		if err := actionRows.Close(); err != nil {
			return fmt.Errorf("close person merge page row actions: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) GetPersonMergeContext(
	ctx context.Context, mergeID int64,
) (*PersonMergeDetail, error) {
	if mergeID <= 0 {
		return nil, ErrPersonMergeNotFound
	}
	var detail *PersonMergeDetail
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		merge, err := s.getPersonMergeTx(ctx, tx, mergeID)
		if err != nil {
			return err
		}
		participants, err := listPersonMergeParticipantsTx(ctx, tx, mergeID)
		if err != nil {
			return err
		}
		rows, err := listPersonMergeRowsTx(ctx, tx, mergeID)
		if err != nil {
			return err
		}
		splits, err := listPersonSplitsTx(ctx, tx, mergeID)
		if err != nil {
			return err
		}
		candidates, err := listPersonMergeReviewCandidatesTx(ctx, tx, mergeID)
		if err != nil {
			return err
		}
		detail = &PersonMergeDetail{
			Merge: *merge, Participants: participants, Rows: rows,
			Splits: splits, ReviewCandidates: candidates,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return detail, nil
}

func listPersonMergeParticipantsTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) ([]PersonMergeParticipant, error) {
	rows, err := tx.QueryContext(ctx, `SELECT merge_id, participant_id, origin_side, split_id
		FROM person_merge_participants WHERE merge_id = ? ORDER BY participant_id`, mergeID)
	if err != nil {
		return nil, fmt.Errorf("list person merge participants: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []PersonMergeParticipant{}
	for rows.Next() {
		var item PersonMergeParticipant
		var splitID sql.NullInt64
		if err := rows.Scan(&item.MergeID, &item.ParticipantID, &item.OriginSide, &splitID); err != nil {
			return nil, fmt.Errorf("scan person merge participant: %w", err)
		}
		if splitID.Valid {
			item.SplitID = &splitID.Int64
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person merge participants: %w", err)
	}
	return result, nil
}

func listPersonMergeRowsTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) ([]PersonMergeRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT merge_id, table_name, original_row_id,
		original_row_key, current_row_id, current_row_key, origin_side,
		provenance_kind, participant_id, action, snapshot_path, split_id
		FROM person_merge_rows WHERE merge_id = ? ORDER BY table_name, original_row_key`, mergeID)
	if err != nil {
		return nil, fmt.Errorf("list person merge rows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []PersonMergeRow{}
	for rows.Next() {
		var item PersonMergeRow
		var originalID, currentID, participantID, splitID sql.NullInt64
		var currentKey sql.NullString
		if err := rows.Scan(
			&item.MergeID, &item.TableName, &originalID, &item.OriginalRowKey,
			&currentID, &currentKey, &item.OriginSide, &item.ProvenanceKind,
			&participantID, &item.Action, &item.SnapshotPath, &splitID,
		); err != nil {
			return nil, fmt.Errorf("scan person merge row: %w", err)
		}
		if originalID.Valid {
			item.OriginalRowID = &originalID.Int64
		}
		if currentID.Valid {
			item.CurrentRowID = &currentID.Int64
		}
		if currentKey.Valid {
			item.CurrentRowKey = &currentKey.String
		}
		if participantID.Valid {
			item.ParticipantID = &participantID.Int64
		}
		if splitID.Valid {
			item.SplitID = &splitID.Int64
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person merge rows: %w", err)
	}
	return result, nil
}

func listPersonSplitsTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) ([]PersonSplit, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, merge_id, source_person_id,
		new_person_id, new_person_uid, source_revision_before,
		source_revision_after, actor, is_exact_reversal, created_at
		FROM person_splits WHERE merge_id = ? ORDER BY id`, mergeID)
	if err != nil {
		return nil, fmt.Errorf("list person splits: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []PersonSplit{}
	for rows.Next() {
		var split PersonSplit
		if err := rows.Scan(
			&split.ID, &split.MergeID, &split.SourcePersonID, &split.NewPersonID,
			&split.NewPersonUID, &split.SourceRevisionBefore, &split.SourceRevisionAfter,
			&split.Actor, &split.ExactReversal, &split.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan person split: %w", err)
		}
		result = append(result, split)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person splits: %w", err)
	}
	return result, nil
}

func (s *Store) GetPersonMergeSnapshotContext(
	ctx context.Context, mergeID int64,
) (*PersonMergeSnapshotResponse, error) {
	if mergeID <= 0 {
		return nil, ErrPersonMergeNotFound
	}
	var blob []byte
	var version int
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_version, snapshot_sha256,
		snapshot_blob FROM person_merges WHERE id = ?`, mergeID).Scan(&version, &hash, &blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPersonMergeNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load person merge snapshot: %w", err)
	}
	snapshot, err := decodePersonMergeSnapshot(blob, hash)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode person merge snapshot response: %w", err)
	}
	return &PersonMergeSnapshotResponse{
		Version: version, SHA256: hash, JSON: json.RawMessage(canonical),
	}, nil
}

func (s *Store) DecidePersonMergeCandidateContext(
	ctx context.Context, request PersonMergeCandidateDecisionRequest,
) (*PersonMergeCandidateDecisionResult, error) {
	request.Actor = strings.TrimSpace(request.Actor)
	if request.CandidateID <= 0 || request.PersonID <= 0 ||
		request.ExpectedPersonRevision <= 0 || request.Actor == "" ||
		(request.Decision != PersonMergeCandidateAccept &&
			request.Decision != PersonMergeCandidateReject) {
		return nil, ErrPersonMergeInvalid
	}
	return retryContendedWrite(ctx, s, "decide person merge candidate",
		func() (*PersonMergeCandidateDecisionResult, error) {
			return s.decidePersonMergeCandidateOnce(ctx, request)
		})
}

func (s *Store) decidePersonMergeCandidateOnce(
	ctx context.Context, request PersonMergeCandidateDecisionRequest,
) (*PersonMergeCandidateDecisionResult, error) {
	var result *PersonMergeCandidateDecisionResult
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		var revision int64
		err := tx.QueryRowContext(ctx, `SELECT revision FROM persons WHERE id = ?`+
			s.dialect.SelectForUpdate(), request.PersonID).Scan(&revision)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		}
		if err != nil {
			return fmt.Errorf("lock merge candidate person: %w", err)
		}
		candidate, err := getPersonMergeReviewCandidateTx(
			ctx, tx, request.CandidateID, s.dialect.SelectForUpdate())
		if err != nil {
			return err
		}
		wantedState := "accepted"
		if request.Decision == PersonMergeCandidateReject {
			wantedState = "rejected"
		}
		if candidate.PersonID != request.PersonID {
			return ErrPersonMergeCandidateState
		}
		if candidate.State == wantedState {
			result = &PersonMergeCandidateDecisionResult{
				PersonMergeReviewCandidate: *candidate, PersonRevision: revision,
			}
			return nil
		}
		if candidate.State != "pending" {
			return ErrPersonMergeCandidateState
		}
		if revision != request.ExpectedPersonRevision {
			return ErrPersonRevisionConflict
		}
		var resolutionID any
		if request.Decision == PersonMergeCandidateAccept {
			insertedID, err := s.acceptPersonMergeCandidateValueTx(ctx, tx, candidate, request.Actor)
			if err != nil {
				return err
			}
			resolutionID = insertedID
		}
		decisionResult, err := tx.ExecContext(ctx, `UPDATE person_merge_review_candidates SET
			state = ?, resolution_value_id = ?, reviewed_by = ?, reviewed_at = ?
			WHERE id = ? AND state = 'pending'`, wantedState, resolutionID,
			request.Actor, time.Now().UTC(), candidate.ID)
		if err != nil {
			return fmt.Errorf("decide person merge candidate: %w", err)
		}
		if changed, err := decisionResult.RowsAffected(); err != nil {
			return fmt.Errorf("count person merge candidate decision: %w", err)
		} else if changed != 1 {
			return ErrPersonMergeCandidateState
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, request.PersonID); err != nil {
			return err
		}
		candidate, err = getPersonMergeReviewCandidateTx(ctx, tx, candidate.ID, "")
		if err != nil {
			return err
		}
		result = &PersonMergeCandidateDecisionResult{
			PersonMergeReviewCandidate: *candidate, PersonRevision: revision + 1,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func getPersonMergeReviewCandidateTx(
	ctx context.Context, tx *loggedTx, candidateID int64, lock string,
) (*PersonMergeReviewCandidate, error) {
	rows, err := listPersonMergeReviewCandidatesQueryTx(ctx, tx, `WHERE candidate.id = ?`+lock,
		candidateID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrPersonMergeCandidateNotFound
	}
	return &rows[0], nil
}

func listPersonMergeReviewCandidatesQueryTx(
	ctx context.Context, tx *loggedTx, suffix string, args ...any,
) ([]PersonMergeReviewCandidate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT candidate.id, candidate.merge_id,
		candidate.survivor_person_id, candidate.definition_id,
		candidate.survivor_value_id, candidate.absorbed_value_id, candidate.state,
		candidate.resolution_value_id, candidate.reviewed_by, candidate.reviewed_at,
		candidate.created_at FROM person_merge_review_candidates candidate `+suffix, args...)
	if err != nil {
		return nil, fmt.Errorf("load person merge review candidate: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []PersonMergeReviewCandidate{}
	for rows.Next() {
		var candidate PersonMergeReviewCandidate
		var resolution sql.NullInt64
		var reviewedBy sql.NullString
		var reviewedAt sql.NullTime
		if err := rows.Scan(
			&candidate.ID, &candidate.MergeID, &candidate.PersonID,
			&candidate.DefinitionID, &candidate.SurvivorValueID,
			&candidate.AbsorbedValueID, &candidate.State, &resolution,
			&reviewedBy, &reviewedAt, &candidate.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan person merge review candidate: %w", err)
		}
		if resolution.Valid {
			candidate.ResolutionValueID = &resolution.Int64
		}
		if reviewedBy.Valid {
			candidate.ReviewedBy = &reviewedBy.String
		}
		if reviewedAt.Valid {
			candidate.ReviewedAt = &reviewedAt.Time
		}
		result = append(result, candidate)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person merge review candidate: %w", err)
	}
	return result, nil
}

func (s *Store) acceptPersonMergeCandidateValueTx(
	ctx context.Context, tx *loggedTx,
	candidate *PersonMergeReviewCandidate, actor string,
) (int64, error) {
	absorbed, err := scanPersonAttributeValue(tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s
		FROM person_attribute_values v
		JOIN attribute_definitions d ON d.id = v.definition_id
		WHERE v.id = ?`, personAttributeValueColumns), candidate.AbsorbedValueID))
	if err != nil {
		return 0, fmt.Errorf("load absorbed merge candidate value: %w", err)
	}
	if absorbed.PersonID != candidate.PersonID || absorbed.DefinitionID != candidate.DefinitionID {
		return 0, ErrPersonMergeCandidateState
	}
	definition, err := s.getAttributeDefinitionBySlugTx(
		ctx, tx, AttributeObjectPerson, absorbed.DefinitionSlug)
	if err != nil {
		return 0, err
	}
	if err := writableAttributeDefinition(*definition); err != nil {
		return 0, fmt.Errorf("%w: %w", ErrPersonMergeCandidateState, err)
	}
	current, found, err := s.currentPersonAttributeValueTx(
		ctx, tx, candidate.PersonID, candidate.DefinitionID, absorbed.Ordinal)
	if err != nil {
		return 0, err
	}
	if !found || current.ID != candidate.SurvivorValueID {
		return 0, ErrPersonMergeCandidateState
	}
	now := time.Now().UTC()
	if _, err := s.closePersonAttributeValueTx(ctx, tx, current.ID, now, now); err != nil {
		return 0, err
	}
	if err := s.verifyAttributeRecordTargetTx(ctx, tx, absorbed.Value); err != nil {
		return 0, err
	}
	ordinal := absorbed.Ordinal
	inserted, err := s.insertPersonAttributeValueTx(ctx, tx, *definition,
		PersonAttributeValueInput{
			PersonID: candidate.PersonID, DefinitionSlug: absorbed.DefinitionSlug,
			Ordinal: &ordinal, Value: absorbed.Value, Source: absorbed.Source,
			SourceRef: absorbed.SourceRef, Confidence: absorbed.Confidence, Actor: &actor,
		}, absorbed.Ordinal, now, now)
	if err != nil {
		return 0, err
	}
	return inserted.ID, nil
}
