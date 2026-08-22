package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

const (
	personSplitAliasRetargeted = "retired_uid_alias_retargeted"
	personSplitAliasUnchanged  = "retired_uid_alias_unchanged"
)

type personSplitLineage struct {
	participantID int64
	originSide    personMergeOriginSide
	splitID       sql.NullInt64
	personID      sql.NullInt64
}

type personSplitAncestorRestoration struct {
	merge          *PersonMerge
	snapshot       personMergeSnapshot
	absorbedRoot   personMergeSnapshotPerson
	participantIDs []int64
}

type personSplitJournalRow struct {
	tableName     string
	originalRowID sql.NullInt64
	originalKey   string
	currentRowID  sql.NullInt64
	currentKey    sql.NullString
	provenance    personMergeProvenanceKind
	originSide    personMergeOriginSide
	participantID sql.NullInt64
	action        string
	snapshotPath  string
	postMergeJSON sql.NullString
}

// SplitPersonMergeContext moves selected absorbed-origin participant
// lineages from a merged person into a fresh person. Aggregate profile rows
// are restored when the selection completes their owning merge; a partial
// split otherwise moves only participant-exact evidence and reports the rows
// left behind.
func (s *Store) SplitPersonMergeContext(
	ctx context.Context, request PersonSplitRequest,
) (*PersonSplitResult, error) {
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor = strings.TrimSpace(request.Actor)
	if err := request.validate(); err != nil {
		return nil, err
	}
	request.ParticipantIDs = request.canonicalParticipantIDs()
	return retryBusyWrite(ctx, s, "split person merge", func() (*PersonSplitResult, error) {
		return s.splitPersonMergeOnce(ctx, request)
	})
}

func (s *Store) splitPersonMergeOnce(
	ctx context.Context, request PersonSplitRequest,
) (*PersonSplitResult, error) {
	requestHash, err := personSplitRequestHash(request)
	if err != nil {
		return nil, err
	}
	var result *PersonSplitResult
	err = s.withTxContext(ctx, func(tx *loggedTx) error {
		if s.personOperationBeforeIdentityLockHook != nil {
			s.personOperationBeforeIdentityLockHook()
		}
		if err := s.lockIdentityMutationTxContext(ctx, tx); err != nil {
			return err
		}
		replayed, found, err := s.personSplitByIdempotencyKeyTx(
			ctx, tx, request.IdempotencyKey, requestHash,
		)
		if err != nil {
			return err
		}
		if found {
			result = replayed
			return nil
		}

		var sourceRevision int64
		var sourceUID string
		err = tx.QueryRowContext(ctx, `SELECT revision, vcard_uid FROM persons WHERE id = ?`+
			s.dialect.SelectForUpdate(), request.SourcePersonID).Scan(&sourceRevision, &sourceUID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		}
		if err != nil {
			return fmt.Errorf("lock split source person: %w", err)
		}
		if sourceRevision != request.ExpectedSourceRevision {
			return ErrPersonSplitRevision
		}

		merge, snapshot, err := s.loadPersonSplitMergeTx(ctx, tx, request.MergeID)
		if err != nil {
			return err
		}
		if merge.CurrentPersonID == nil {
			var splitExists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
				SELECT 1 FROM person_splits WHERE merge_id = ?
			)`, request.MergeID).Scan(&splitExists); err != nil {
				return fmt.Errorf("inspect completed person split: %w", err)
			}
			if splitExists {
				return ErrPersonMergeAlreadySplit
			}
			return ErrPersonSplitOwnership
		}
		if *merge.CurrentPersonID != request.SourcePersonID {
			return ErrPersonSplitOwnership
		}
		lineage, err := s.loadPersonSplitLineageTx(ctx, tx, request.MergeID)
		if err != nil {
			return err
		}
		exact, err := validatePersonSplitLineage(
			lineage, request.SourcePersonID, request.ParticipantIDs,
		)
		if err != nil {
			return err
		}
		if exact {
			reviewed, err := personSplitHasPostMergeAcceptedCandidateTx(
				ctx, tx, request.MergeID, snapshot,
			)
			if err != nil {
				return err
			}
			if reviewed {
				return ErrPersonSplitReviewed
			}
		}
		ancestorRestorations, transferredMergeIDs, err :=
			s.loadPersonSplitAncestorRestorationsTx(ctx, tx, request)
		if err != nil {
			return err
		}

		absorbedRoot, err := absorbedPersonMergeSnapshotRoot(snapshot)
		if err != nil {
			return err
		}
		newUID, err := newVCardUID()
		if err != nil {
			return err
		}
		var displayName any
		if exact && absorbedRoot.DisplayName != nil {
			displayName = *absorbedRoot.DisplayName
		}
		var newPersonID int64
		if err := tx.QueryRowContext(ctx,
			`INSERT INTO persons (vcard_uid, display_name) VALUES (?, ?) RETURNING id`,
			newUID, displayName,
		).Scan(&newPersonID); err != nil {
			return fmt.Errorf("create split person: %w", err)
		}
		var splitID int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO person_splits (
			merge_id, idempotency_key, request_hash, source_person_id, new_person_id,
			new_person_uid, source_revision_before, source_revision_after, actor,
			is_exact_reversal
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			request.MergeID, request.IdempotencyKey, requestHash,
			request.SourcePersonID, newPersonID, newUID,
			sourceRevision, sourceRevision+1, request.Actor, false,
		).Scan(&splitID); err != nil {
			if s.dialect.IsConflictError(err) {
				return ErrPersonSplitIdempotency
			}
			return fmt.Errorf("insert person split: %w", err)
		}

		if err := s.deletePersonSplitCrossingLinksTx(ctx, tx, request.ParticipantIDs); err != nil {
			return err
		}
		args := []any{newPersonID, request.SourcePersonID}
		args = append(args, personMergeSnapshotIDArgs(request.ParticipantIDs)...)
		bindingResult, err := tx.ExecContext(ctx, `UPDATE person_participants
			SET person_id = ? WHERE person_id = ? AND participant_id IN (`+
			personMergeSnapshotPlaceholders(len(request.ParticipantIDs))+`)`, args...)
		if err != nil {
			return fmt.Errorf("move split participant bindings: %w", err)
		}
		if moved, err := bindingResult.RowsAffected(); err != nil {
			return fmt.Errorf("count split participant bindings: %w", err)
		} else if moved != int64(len(request.ParticipantIDs)) {
			return fmt.Errorf("%w: participant binding changed during split", ErrPersonSplitParticipants)
		}

		unrestored := []PersonMergeRowRef{}
		ambiguous, err := s.restorePersonSplitRowsTx(
			ctx, tx, request.MergeID, splitID, request.SourcePersonID,
			newPersonID, snapshot.Persons[0].ID, absorbedRoot.ID, sourceUID, newUID,
			request.ParticipantIDs, exact, snapshot, &unrestored,
		)
		if err != nil {
			return err
		}
		for _, ancestor := range ancestorRestorations {
			ancestorAmbiguous, err := s.restorePersonSplitRowsTx(
				ctx, tx, ancestor.merge.ID, splitID, request.SourcePersonID,
				newPersonID, ancestor.snapshot.Persons[0].ID, ancestor.absorbedRoot.ID,
				sourceUID, newUID, ancestor.participantIDs, true, ancestor.snapshot,
				&unrestored,
			)
			if err != nil {
				return err
			}
			ambiguous = append(ambiguous, ancestorAmbiguous...)
			if ancestor.absorbedRoot.DisplayName != nil {
				if _, err := tx.ExecContext(ctx, `UPDATE persons
					SET display_name = COALESCE(display_name, ?) WHERE id = ?`,
					*ancestor.absorbedRoot.DisplayName, newPersonID); err != nil {
					return fmt.Errorf("restore ancestor split display name: %w", err)
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_merge_review_candidates SET
				state = 'rejected', reviewed_by = ?, reviewed_at = `+s.dialect.Now()+`
				WHERE merge_id = ? AND state = 'pending'`, request.Actor, ancestor.merge.ID); err != nil {
				return fmt.Errorf("finalize ancestor-split review candidates: %w", err)
			}
			aliasResult, err := tx.ExecContext(ctx, `UPDATE person_uid_aliases
				SET surviving_person_id = ? WHERE retired_uid = ? AND surviving_person_id = ?`,
				newPersonID, ancestor.merge.AbsorbedVCardUID, request.SourcePersonID)
			if err != nil {
				return fmt.Errorf("retarget ancestor split retired UID alias: %w", err)
			}
			if changed, err := aliasResult.RowsAffected(); err != nil {
				return fmt.Errorf("count ancestor split retired UID alias: %w", err)
			} else if changed != 1 {
				return fmt.Errorf("%w: ancestor absorbed UID alias is not owned by source",
					ErrPersonSplitParticipants)
			}
		}
		if exact {
			if _, err := tx.ExecContext(ctx, `UPDATE person_merge_review_candidates SET
				state = 'rejected', reviewed_by = ?, reviewed_at = `+s.dialect.Now()+`
				WHERE merge_id = ? AND state = 'pending'`, request.Actor, request.MergeID); err != nil {
				return fmt.Errorf("finalize exact-split review candidates: %w", err)
			}
		}
		if err := s.reconcilePersonSplitCounterpartProjectionsTx(
			ctx, tx, splitID, request.SourcePersonID, newPersonID,
		); err != nil {
			return err
		}
		aliasDisposition := personSplitAliasUnchanged
		if exact {
			aliasResult, err := tx.ExecContext(ctx, `UPDATE person_uid_aliases
				SET surviving_person_id = ? WHERE retired_uid = ? AND surviving_person_id = ?`,
				newPersonID, merge.AbsorbedVCardUID, request.SourcePersonID)
			if err != nil {
				return fmt.Errorf("retarget split retired UID alias: %w", err)
			}
			if changed, err := aliasResult.RowsAffected(); err != nil {
				return fmt.Errorf("count split retired UID alias: %w", err)
			} else if changed != 1 {
				return fmt.Errorf("%w: absorbed UID alias is not owned by source", ErrPersonSplitParticipants)
			}
			aliasDisposition = personSplitAliasRetargeted
		}
		for _, mergeID := range transferredMergeIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE person_merges
				SET current_person_id = ? WHERE id = ? AND current_person_id = ?`,
				newPersonID, mergeID, request.SourcePersonID); err != nil {
				return fmt.Errorf("transfer nested merge lineage: %w", err)
			}
		}

		lineageArgs := []any{splitID, request.SourcePersonID}
		lineageArgs = append(lineageArgs, personMergeSnapshotIDArgs(request.ParticipantIDs)...)
		if _, err := tx.ExecContext(ctx, `UPDATE person_merge_participants SET split_id = ?
			WHERE split_id IS NULL AND merge_id IN (
				SELECT id FROM person_merges WHERE current_person_id = ?
			) AND participant_id IN (`+
			personMergeSnapshotPlaceholders(len(request.ParticipantIDs))+`)`, lineageArgs...); err != nil {
			return fmt.Errorf("mark split participant lineage: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_merges
			SET current_person_id = NULL
			WHERE current_person_id = ? AND NOT EXISTS (
				SELECT 1 FROM person_merge_participants lineage
				WHERE lineage.merge_id = person_merges.id
				  AND lineage.origin_side = 'absorbed'
				  AND lineage.split_id IS NULL
			)`, request.SourcePersonID); err != nil {
			return fmt.Errorf("close fully split merge lineage: %w", err)
		}
		identityRevision, err := s.bumpIdentityRevisionContext(ctx, tx)
		if err != nil {
			return err
		}
		accountRevision, err := readAccountIdentityRevision(tx)
		if err != nil {
			return err
		}
		if err := s.recomputePersonSplitActivityTx(
			ctx, tx, request.SourcePersonID, newPersonID, ContactRevisions{
				IdentityRevision: identityRevision, AccountIdentityRevision: accountRevision,
			},
		); err != nil {
			return err
		}
		if err := s.bumpPersonRevisionsTx(ctx, tx, request.SourcePersonID, newPersonID); err != nil {
			return err
		}
		exactReversal := exact && len(unrestored) == 0
		if _, err := tx.ExecContext(ctx, `UPDATE person_splits
			SET is_exact_reversal = ? WHERE id = ?`, exactReversal, splitID); err != nil {
			return fmt.Errorf("record exact person split outcome: %w", err)
		}

		split, err := s.getPersonSplitTx(ctx, tx, splitID)
		if err != nil {
			return err
		}
		source, err := s.getPersonTx(ctx, tx, request.SourcePersonID)
		if err != nil {
			return err
		}
		created, err := s.getPersonTx(ctx, tx, newPersonID)
		if err != nil {
			return err
		}
		result = &PersonSplitResult{
			Split: *split, SourcePerson: *source, NewPerson: *created,
			ExactReversal: exactReversal, UIDAliasDisposition: aliasDisposition,
			AmbiguousRows: ambiguous, UnrestoredRows: unrestored,
			IdentityRevision: identityRevision,
		}
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return fmt.Errorf("encode person split idempotency result: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_splits
			SET result_json = ?, identity_revision = ? WHERE id = ?`,
			string(resultJSON), identityRevision, splitID); err != nil {
			return fmt.Errorf("store person split idempotency result: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func personSplitHasPostMergeAcceptedCandidateTx(
	ctx context.Context, tx *loggedTx, mergeID int64, snapshot personMergeSnapshot,
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT candidate.merge_id, journal.snapshot_path
		FROM person_merge_review_candidates candidate
		LEFT JOIN person_merge_rows journal
		  ON journal.merge_id = ?
		 AND journal.table_name = 'person_merge_review_candidates'
		 AND journal.origin_side = 'absorbed'
		 AND journal.original_row_id = candidate.id
		WHERE candidate.state = 'accepted'
		  AND (candidate.merge_id = ? OR journal.snapshot_path IS NOT NULL)
		ORDER BY candidate.id`, mergeID, mergeID)
	if err != nil {
		return false, fmt.Errorf("inspect exact-split review candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	snapshotByPath := make(map[string]personMergeSnapshotRow, len(snapshot.Rows))
	for index, row := range snapshot.Rows {
		snapshotByPath["rows/"+strconv.Itoa(index)] = row
	}
	for rows.Next() {
		var candidateMergeID int64
		var snapshotPath sql.NullString
		if err := rows.Scan(&candidateMergeID, &snapshotPath); err != nil {
			return false, fmt.Errorf("scan exact-split review candidate: %w", err)
		}
		if candidateMergeID == mergeID {
			return true, nil
		}
		snapshotRow, ok := snapshotByPath[snapshotPath.String]
		if !ok {
			return false, fmt.Errorf("%w: missing candidate snapshot path %q",
				ErrPersonMergeSnapshotCorrupt, snapshotPath.String)
		}
		if personSplitSnapshotRowText(snapshotRow, "state") != "accepted" {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate exact-split review candidates: %w", err)
	}
	return false, nil
}

func personSplitRequestHash(request PersonSplitRequest) (string, error) {
	canonical, err := json.Marshal(struct {
		SourcePersonID         int64   `json:"source_person_id"`
		MergeID                int64   `json:"merge_id"`
		ParticipantIDs         []int64 `json:"participant_ids"`
		ExpectedSourceRevision int64   `json:"expected_source_revision"`
		Actor                  string  `json:"actor"`
	}{request.SourcePersonID, request.MergeID, request.ParticipantIDs,
		request.ExpectedSourceRevision, request.Actor})
	if err != nil {
		return "", fmt.Errorf("encode person split request: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func (s *Store) loadPersonSplitMergeTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) (*PersonMerge, personMergeSnapshot, error) {
	merge, err := s.getPersonMergeTx(ctx, tx, mergeID)
	if err != nil {
		return nil, personMergeSnapshot{}, err
	}
	var blob []byte
	var hash string
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_blob, snapshot_sha256
		FROM person_merges WHERE id = ?`+s.dialect.SelectForUpdate(), mergeID).Scan(&blob, &hash); err != nil {
		return nil, personMergeSnapshot{}, fmt.Errorf("load person merge snapshot: %w", err)
	}
	snapshot, err := decodePersonMergeSnapshot(blob, hash)
	if err != nil {
		return nil, personMergeSnapshot{}, err
	}
	return merge, snapshot, nil
}

func (s *Store) loadPersonSplitLineageTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) ([]personSplitLineage, error) {
	locked, err := tx.QueryContext(ctx, `SELECT participant_id
		FROM person_merge_participants WHERE merge_id = ? ORDER BY participant_id`+
		s.dialect.SelectForUpdate(), mergeID)
	if err != nil {
		return nil, fmt.Errorf("lock person split lineage: %w", err)
	}
	for locked.Next() {
		var participantID int64
		if err := locked.Scan(&participantID); err != nil {
			_ = locked.Close()
			return nil, fmt.Errorf("scan locked person split lineage: %w", err)
		}
	}
	if err := locked.Err(); err != nil {
		_ = locked.Close()
		return nil, fmt.Errorf("iterate locked person split lineage: %w", err)
	}
	if err := locked.Close(); err != nil {
		return nil, fmt.Errorf("close locked person split lineage: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT lineage.participant_id, lineage.origin_side,
		lineage.split_id, binding.person_id
		FROM person_merge_participants lineage
		LEFT JOIN person_participants binding ON binding.participant_id = lineage.participant_id
		WHERE lineage.merge_id = ? ORDER BY lineage.participant_id`, mergeID)
	if err != nil {
		return nil, fmt.Errorf("load person split lineage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var result []personSplitLineage
	for rows.Next() {
		var item personSplitLineage
		if err := rows.Scan(&item.participantID, &item.originSide, &item.splitID, &item.personID); err != nil {
			return nil, fmt.Errorf("scan person split lineage: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person split lineage: %w", err)
	}
	return result, nil
}

func (s *Store) loadPersonSplitAncestorRestorationsTx(
	ctx context.Context, tx *loggedTx, request PersonSplitRequest,
) ([]personSplitAncestorRestoration, []int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM person_merges
		WHERE current_person_id = ? AND id < ? ORDER BY id`,
		request.SourcePersonID, request.MergeID)
	if err != nil {
		return nil, nil, fmt.Errorf("load earlier active person merges: %w", err)
	}
	mergeIDs := []int64{}
	for rows.Next() {
		var mergeID int64
		if err := rows.Scan(&mergeID); err != nil {
			_ = rows.Close()
			return nil, nil, fmt.Errorf("scan earlier active person merge: %w", err)
		}
		mergeIDs = append(mergeIDs, mergeID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, nil, fmt.Errorf("iterate earlier active person merges: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, fmt.Errorf("close earlier active person merges: %w", err)
	}

	selected := make(map[int64]struct{}, len(request.ParticipantIDs))
	for _, participantID := range request.ParticipantIDs {
		selected[participantID] = struct{}{}
	}
	restorations := []personSplitAncestorRestoration{}
	transfers := []int64{}
	for _, mergeID := range mergeIDs {
		lineage, err := s.loadPersonSplitLineageTx(ctx, tx, mergeID)
		if err != nil {
			return nil, nil, err
		}
		sourceBindings := 0
		selectedBindings := 0
		absorbedSelected := []int64{}
		survivorSelected := false
		for _, item := range lineage {
			if !item.personID.Valid || item.personID.Int64 != request.SourcePersonID {
				continue
			}
			sourceBindings++
			if _, ok := selected[item.participantID]; !ok {
				continue
			}
			selectedBindings++
			if item.originSide == personMergeOriginAbsorbed && !item.splitID.Valid {
				absorbedSelected = append(absorbedSelected, item.participantID)
			}
			survivorSelected = survivorSelected || item.originSide == personMergeOriginSurvivor
		}
		if selectedBindings == 0 {
			continue
		}
		if selectedBindings == sourceBindings {
			transfers = append(transfers, mergeID)
			continue
		}
		if survivorSelected {
			return nil, nil, ErrPersonSplitParticipants
		}
		if len(absorbedSelected) == 0 {
			continue
		}
		exact, err := validatePersonSplitLineage(
			lineage, request.SourcePersonID, absorbedSelected,
		)
		if err != nil {
			return nil, nil, err
		}
		if !exact {
			return nil, nil, ErrPersonSplitParticipants
		}
		merge, snapshot, err := s.loadPersonSplitMergeTx(ctx, tx, mergeID)
		if err != nil {
			return nil, nil, err
		}
		reviewed, err := personSplitHasPostMergeAcceptedCandidateTx(
			ctx, tx, mergeID, snapshot,
		)
		if err != nil {
			return nil, nil, err
		}
		if reviewed {
			return nil, nil, ErrPersonSplitReviewed
		}
		absorbedRoot, err := absorbedPersonMergeSnapshotRoot(snapshot)
		if err != nil {
			return nil, nil, err
		}
		restorations = append(restorations, personSplitAncestorRestoration{
			merge: merge, snapshot: snapshot, absorbedRoot: absorbedRoot,
			participantIDs: absorbedSelected,
		})
	}
	return restorations, transfers, nil
}

func validatePersonSplitLineage(
	lineage []personSplitLineage, sourceID int64, selected []int64,
) (bool, error) {
	wanted := make(map[int64]struct{}, len(selected))
	for _, id := range selected {
		wanted[id] = struct{}{}
	}
	absorbedUnsplit := 0
	absorbedTotal := 0
	sourceBindings := 0
	matched := 0
	alreadySplit := false
	survivorLineageIntact := true
	for _, item := range lineage {
		if item.personID.Valid && item.personID.Int64 == sourceID {
			sourceBindings++
		}
		if item.originSide == personMergeOriginSurvivor &&
			(!item.personID.Valid || item.personID.Int64 != sourceID) {
			survivorLineageIntact = false
		}
		if item.originSide == personMergeOriginAbsorbed {
			absorbedTotal++
			if !item.splitID.Valid {
				absorbedUnsplit++
			}
		}
		if _, ok := wanted[item.participantID]; !ok {
			continue
		}
		matched++
		if item.originSide != personMergeOriginAbsorbed ||
			(!item.splitID.Valid && (!item.personID.Valid || item.personID.Int64 != sourceID)) {
			return false, ErrPersonSplitParticipants
		}
		alreadySplit = alreadySplit || item.splitID.Valid
	}
	if matched != len(selected) {
		return false, ErrPersonSplitParticipants
	}
	if alreadySplit {
		return false, ErrPersonMergeAlreadySplit
	}
	if sourceBindings <= len(selected) {
		return false, ErrPersonSplitParticipants
	}
	return survivorLineageIntact && len(selected) == absorbedUnsplit &&
		absorbedUnsplit == absorbedTotal, nil
}

func absorbedPersonMergeSnapshotRoot(
	snapshot personMergeSnapshot,
) (personMergeSnapshotPerson, error) {
	if len(snapshot.Persons) != 2 {
		return personMergeSnapshotPerson{}, fmt.Errorf("%w: merge snapshot roots", ErrPersonMergeSnapshotCorrupt)
	}
	return snapshot.Persons[1], nil
}

func (s *Store) deletePersonSplitCrossingLinksTx(
	ctx context.Context, tx *loggedTx, selected []int64,
) error {
	if err := s.rejectAcceptedIdentityMatchesAcrossPersonSplitTx(ctx, tx, selected); err != nil {
		return err
	}
	args := append(personMergeSnapshotIDArgs(selected), personMergeSnapshotIDArgs(selected)...)
	placeholders := personMergeSnapshotPlaceholders(len(selected))
	if _, err := tx.ExecContext(ctx, `DELETE FROM participant_links
		WHERE (participant_a IN (`+placeholders+`) AND participant_b NOT IN (`+placeholders+`))
		   OR (participant_b IN (`+placeholders+`) AND participant_a NOT IN (`+placeholders+`))`,
		append(args, args...)...); err != nil {
		return fmt.Errorf("cut split identity links: %w", err)
	}
	return nil
}

func (s *Store) restorePersonSplitRowsTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID, splitID, sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
	selected []int64,
	exact bool,
	snapshot personMergeSnapshot,
	unrestored *[]PersonMergeRowRef,
) ([]PersonMergeRowRef, error) {
	journal, err := loadPersonSplitJournalTx(ctx, tx, mergeID)
	if err != nil {
		return nil, err
	}
	selectedSet := make(map[int64]struct{}, len(selected))
	for _, participantID := range selected {
		selectedSet[participantID] = struct{}{}
	}
	rowsByPath := make(map[string]personMergeSnapshotRow, len(snapshot.Rows))
	for index, row := range snapshot.Rows {
		rowsByPath["rows/"+strconv.Itoa(index)] = row
	}

	move := make([]personSplitJournalRow, 0, len(journal))
	ambiguous := make([]PersonMergeRowRef, 0)
	for _, row := range journal {
		eligible := exact
		if !exact && row.provenance == personMergeProvenanceParticipantExact &&
			row.participantID.Valid {
			_, eligible = selectedSet[row.participantID.Int64]
		}
		if eligible {
			move = append(move, row)
			continue
		}
		if row.originSide == personMergeOriginAbsorbed {
			ambiguous = append(ambiguous, personSplitJournalRef(row))
		}
	}
	unsupportedCandidates, err := s.personSplitUnsupportedGeneratedCandidatesTx(
		ctx, tx, move, rowsByPath,
	)
	if err != nil {
		return nil, err
	}
	unsupportedEvidence, err := s.personSplitUnsupportedGeneratedEvidenceTx(
		ctx, tx, move, rowsByPath, unsupportedCandidates,
	)
	if err != nil {
		return nil, err
	}

	// Recreate deleted and removed deduplicated parent rows first. Existing dependent
	// rows may need their merge-time foreign-key remaps reversed afterward.
	slices.SortStableFunc(move, func(left, right personSplitJournalRow) int {
		return personSplitRestorePriority(left.tableName) - personSplitRestorePriority(right.tableName)
	})
	for _, row := range move {
		if row.provenance == personMergeProvenanceDerived || row.action == "recomputed" {
			if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
				return nil, err
			}
			continue
		}
		if row.action != "deleted_snapshot" &&
			(row.action != personMergeActionDeduplicated || personSplitJournalRowWasRetained(row)) {
			continue
		}
		snapshotRow, ok := rowsByPath[row.snapshotPath]
		if !ok {
			return nil, fmt.Errorf("%w: missing split snapshot path %q",
				ErrPersonMergeSnapshotCorrupt, row.snapshotPath)
		}
		if personSplitSnapshotHasUnsupportedCandidate(
			snapshotRow, unsupportedCandidates, unsupportedEvidence,
		) {
			appendPersonSplitUnrestoredRow(unrestored, exact, row)
			if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
				ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
				survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, false,
			); err != nil {
				return nil, err
			}
			if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
				return nil, err
			}
			continue
		}
		if row.action == personMergeActionDeduplicated {
			var restore bool
			snapshotRow, restore, err = s.preparePersonSplitDeduplicatedRowTx(
				ctx, tx, row, snapshotRow,
			)
			if err != nil {
				return nil, err
			}
			if !restore {
				appendPersonSplitUnrestoredRow(unrestored, exact, row)
				// Deleting or reassigning the surviving merged row is post-merge
				// user state. Do not recreate a duplicate under stale ownership.
				if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
					ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
					survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, false,
				); err != nil {
					return nil, err
				}
				if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
					return nil, err
				}
				continue
			}
		}
		dependenciesPresent, err := s.personSplitSnapshotDependenciesPresentTx(
			ctx, tx, snapshotRow, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return nil, err
		}
		recordTargetPresent, err := s.personSplitSnapshotRecordTargetPresentTx(
			ctx, tx, snapshotRow, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return nil, err
		}
		dependenciesPresent = dependenciesPresent && recordTargetPresent
		if !dependenciesPresent {
			appendPersonSplitUnrestoredRow(unrestored, exact, row)
			// Dependency removal after the merge is user state. Preserve the
			// deleted row instead of turning an exact split into an FK failure.
			if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
				ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
				survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, false,
			); err != nil {
				return nil, err
			}
			if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
				return nil, err
			}
			continue
		}
		if err := s.insertPersonSplitSnapshotRowTx(
			ctx, tx, snapshotRow, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
		); err != nil {
			return nil, err
		}
		if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
			ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, false,
		); err != nil {
			return nil, err
		}
		if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
			return nil, err
		}
	}
	for _, row := range move {
		if row.provenance == personMergeProvenanceDerived || row.action == "recomputed" ||
			row.action == "deleted_snapshot" ||
			(row.action == personMergeActionDeduplicated && !personSplitJournalRowWasRetained(row)) {
			continue
		}
		snapshotRow, ok := rowsByPath[row.snapshotPath]
		if !ok {
			return nil, fmt.Errorf("%w: missing split snapshot path %q",
				ErrPersonMergeSnapshotCorrupt, row.snapshotPath)
		}
		if personSplitSnapshotHasUnsupportedCandidate(
			snapshotRow, unsupportedCandidates, unsupportedEvidence,
		) {
			appendPersonSplitUnrestoredRow(unrestored, exact, row)
			if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
				ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
				survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, false,
			); err != nil {
				return nil, err
			}
			if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
				return nil, err
			}
			continue
		}
		replaced, err := s.personSplitTrackingRowReplacedTx(ctx, tx, row)
		if err != nil {
			return nil, err
		}
		if replaced {
			if err := s.insertPersonSplitSnapshotRowTx(
				ctx, tx, snapshotRow, sourceID, newPersonID,
				survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
			); err != nil {
				return nil, err
			}
			if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
				ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
				survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, true,
			); err != nil {
				return nil, err
			}
			if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
				return nil, err
			}
			continue
		}
		if err := s.restoreExistingPersonSplitRowTx(
			ctx, tx, row, snapshotRow, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
		); err != nil {
			return nil, err
		}
		if err := s.rebasePriorPersonMergeRowsAfterSplitTx(
			ctx, tx, mergeID, row, snapshotRow, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID, false,
		); err != nil {
			return nil, err
		}
		if err := markPersonSplitJournalRowTx(ctx, tx, mergeID, splitID, row); err != nil {
			return nil, err
		}
	}
	return ambiguous, nil
}

func appendPersonSplitUnrestoredRow(
	rows *[]PersonMergeRowRef, exact bool, row personSplitJournalRow,
) {
	if !exact {
		return
	}
	var originalID *int64
	if row.originalRowID.Valid {
		id := row.originalRowID.Int64
		originalID = &id
	}
	*rows = append(*rows, PersonMergeRowRef{
		TableName: row.tableName, OriginalRowID: originalID,
		OriginalKey: row.originalKey, Action: row.action,
	})
}

func (s *Store) personSplitUnsupportedGeneratedCandidatesTx(
	ctx context.Context,
	tx *loggedTx,
	journal []personSplitJournalRow,
	rowsByPath map[string]personMergeSnapshotRow,
) (map[int64]struct{}, error) {
	candidates := make(map[int64]personMergeSnapshotRow)
	supportSources := make(map[int64][]int64)
	for _, item := range journal {
		row, ok := rowsByPath[item.snapshotPath]
		if !ok {
			return nil, fmt.Errorf("%w: missing split snapshot path %q",
				ErrPersonMergeSnapshotCorrupt, item.snapshotPath)
		}
		switch row.TableName {
		case identityMatchCandidatesTableName:
			candidates[row.RowID] = row
		case identityMatchCandidateSourcesTableName:
			candidateID := personSplitSnapshotRowInteger(row, "candidate_id")
			sourceID := personSplitSnapshotRowInteger(row, sourceIDColumnName)
			if candidateID > 0 && sourceID > 0 {
				supportSources[candidateID] = append(supportSources[candidateID], sourceID)
			}
		}
	}
	unsupported := make(map[int64]struct{})
	for candidateID, candidate := range candidates {
		source := Provenance(personSplitSnapshotRowText(candidate, "source"))
		if source != ProvenanceArchiveObservation && source != ProvenanceExtraction &&
			source != ProvenanceEnrichment {
			continue
		}
		if personSplitSnapshotRowText(candidate, "decided_by") == string(ProvenanceUser) {
			continue
		}
		var supported bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM identity_match_candidate_sources support
			JOIN sources source ON source.id = support.source_id
			WHERE support.candidate_id = ?
		)`, candidateID).Scan(&supported); err != nil {
			return nil, fmt.Errorf("inspect current split candidate support: %w", err)
		}
		for _, sourceID := range supportSources[candidateID] {
			if supported {
				break
			}
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM sources WHERE id = ?)`, sourceID,
			).Scan(&supported); err != nil {
				return nil, fmt.Errorf("inspect restored split candidate support: %w", err)
			}
		}
		if !supported {
			unsupported[candidateID] = struct{}{}
		}
	}
	return unsupported, nil
}

func (s *Store) personSplitUnsupportedGeneratedEvidenceTx(
	ctx context.Context,
	tx *loggedTx,
	journal []personSplitJournalRow,
	rowsByPath map[string]personMergeSnapshotRow,
	unsupportedCandidates map[int64]struct{},
) (map[int64]struct{}, error) {
	evidenceRows := make(map[int64]personMergeSnapshotRow)
	supportSources := make(map[int64][]int64)
	for _, item := range journal {
		row, ok := rowsByPath[item.snapshotPath]
		if !ok {
			return nil, fmt.Errorf("%w: missing split snapshot path %q",
				ErrPersonMergeSnapshotCorrupt, item.snapshotPath)
		}
		switch row.TableName {
		case identityMatchEvidenceTableName:
			evidenceRows[row.RowID] = row
		case identityMatchEvidenceSourcesTableName:
			evidenceID := personSplitSnapshotRowInteger(row, "evidence_id")
			sourceID := personSplitSnapshotRowInteger(row, sourceIDColumnName)
			if evidenceID > 0 && sourceID > 0 {
				supportSources[evidenceID] = append(supportSources[evidenceID], sourceID)
			}
		}
	}
	unsupported := make(map[int64]struct{})
	for evidenceID, evidence := range evidenceRows {
		candidateID := personSplitSnapshotRowInteger(evidence, "candidate_id")
		if _, skip := unsupportedCandidates[candidateID]; skip {
			unsupported[evidenceID] = struct{}{}
			continue
		}
		source := Provenance(personSplitSnapshotRowText(evidence, "source"))
		if source != ProvenanceArchiveObservation && source != ProvenanceExtraction &&
			source != ProvenanceEnrichment {
			continue
		}
		var supported bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM identity_match_evidence_sources support
			JOIN sources source ON source.id = support.source_id
			WHERE support.evidence_id = ?
		)`, evidenceID).Scan(&supported); err != nil {
			return nil, fmt.Errorf("inspect current split evidence support: %w", err)
		}
		for _, sourceID := range supportSources[evidenceID] {
			if supported {
				break
			}
			if err := tx.QueryRowContext(ctx,
				`SELECT EXISTS (SELECT 1 FROM sources WHERE id = ?)`, sourceID,
			).Scan(&supported); err != nil {
				return nil, fmt.Errorf("inspect restored split evidence support: %w", err)
			}
		}
		if !supported {
			unsupported[evidenceID] = struct{}{}
		}
	}
	return unsupported, nil
}

func personSplitSnapshotHasUnsupportedCandidate(
	row personMergeSnapshotRow,
	unsupportedCandidates, unsupportedEvidence map[int64]struct{},
) bool {
	switch row.TableName {
	case identityMatchCandidatesTableName:
		_, skip := unsupportedCandidates[row.RowID]
		return skip
	case identityMatchCandidateSourcesTableName:
		_, skip := unsupportedCandidates[personSplitSnapshotRowInteger(row, "candidate_id")]
		return skip
	case identityMatchEvidenceTableName:
		_, candidateUnsupported := unsupportedCandidates[personSplitSnapshotRowInteger(row, "candidate_id")]
		_, evidenceUnsupported := unsupportedEvidence[row.RowID]
		return candidateUnsupported || evidenceUnsupported
	case identityMatchEvidenceSourcesTableName:
		_, skip := unsupportedEvidence[personSplitSnapshotRowInteger(row, "evidence_id")]
		return skip
	case "identity_match_candidate_redirects":
		_, skip := unsupportedCandidates[personSplitSnapshotRowInteger(row, "surviving_candidate_id")]
		return skip
	default:
		return false
	}
}

type personSplitSnapshotDependency struct {
	column, table, key string
}

var errPersonSplitReferenceMissing = errors.New("person split reference target is missing")

var personSplitSnapshotDependencies = map[string][]personSplitSnapshotDependency{
	personRelationshipsTableName: {
		{column: "relationship_type_id", table: "relationship_types", key: "id"},
	},
	identityMatchCandidateSourcesTableName: {
		{column: sourceIDColumnName, table: "sources", key: "id"},
	},
	identityMatchEvidenceSourcesTableName: {
		{column: sourceIDColumnName, table: "sources", key: "id"},
	},
}

var personSplitSnapshotColumnDependencies = map[string]map[string]personSplitSnapshotDependency{
	personRelationshipReviewsTableName: {
		"accepted_relationship_id": {
			column: "accepted_relationship_id", table: personRelationshipsTableName, key: "id",
		},
		"matched_person_id": {
			column: "matched_person_id", table: "persons", key: "id",
		},
	},
	identityMatchCandidatesTableName: {
		"service_id": {
			column: "service_id", table: "communication_services", key: "id",
		},
	},
}

func (s *Store) personSplitSnapshotDependenciesPresentTx(
	ctx context.Context,
	tx *loggedTx,
	row personMergeSnapshotRow,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
) (bool, error) {
	for _, dependency := range personSplitSnapshotDependencies[row.TableName] {
		value := personSplitSnapshotRowInteger(row, dependency.column)
		if value <= 0 {
			return false, fmt.Errorf("%w: missing %s dependency %q",
				ErrPersonMergeSnapshotCorrupt, row.TableName, dependency.column)
		}
		var present bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+
			personSplitIdentifier(dependency.table)+` WHERE `+
			personSplitIdentifier(dependency.key)+` = ?)`, value).Scan(&present); err != nil {
			return false, fmt.Errorf("inspect %s split dependency %s: %w",
				row.TableName, dependency.column, err)
		}
		if !present {
			return false, nil
		}
	}
	spec, ok := personMergeTableRegistry[row.TableName]
	if !ok {
		return false, fmt.Errorf("%w: unregistered split table %q",
			ErrPersonMergeInvalid, row.TableName)
	}
	for _, reference := range spec.PersonReferences {
		if reference.Kind == personMergeReferencePolymorphic &&
			personSplitSnapshotRowText(row, reference.KindColumn) != reference.KindValue {
			continue
		}
		historicalID := personSplitSnapshotRowInteger(row, reference.IDColumn)
		if historicalID <= 0 {
			continue
		}
		_, present, err := s.personSplitResolvePersonReferenceTx(
			ctx, tx, historicalID, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return false, err
		}
		if !present && !personSplitNullablePersonReference(row.TableName, reference.IDColumn) {
			return false, nil
		}
	}
	return true, nil
}

func (s *Store) personSplitSnapshotColumnDependencyPresentTx(
	ctx context.Context,
	tx *loggedTx,
	row personMergeSnapshotRow,
	column personMergeSnapshotColumn,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
) (bool, error) {
	dependencies := personSplitSnapshotColumnDependencies[row.TableName]
	dependency, ok := dependencies[column.Name]
	if !ok || column.Value.Integer == nil {
		return true, nil
	}
	if dependency.table == "persons" {
		_, present, err := s.personSplitResolvePersonReferenceTx(
			ctx, tx, *column.Value.Integer, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		return present, err
	}
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+
		personSplitIdentifier(dependency.table)+` WHERE `+
		personSplitIdentifier(dependency.key)+` = ?)`, *column.Value.Integer).Scan(&present); err != nil {
		return false, fmt.Errorf("inspect %s split column dependency %s: %w",
			row.TableName, dependency.column, err)
	}
	return present, nil
}

func (s *Store) personSplitSnapshotRecordTargetPresentTx(
	ctx context.Context,
	tx *loggedTx,
	row personMergeSnapshotRow,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
) (bool, error) {
	if (row.TableName != personAttributeValuesTableName &&
		row.TableName != "organization_attribute_values") ||
		personSplitSnapshotRowText(row, "value_record_type") != "person" {
		return true, nil
	}
	targetID := personSplitSnapshotRowInteger(row, "value_record_id")
	if targetID <= 0 {
		return false, fmt.Errorf("%w: missing %s record target",
			ErrPersonMergeSnapshotCorrupt, row.TableName)
	}
	_, present, err := s.personSplitResolvePersonReferenceTx(
		ctx, tx, targetID, sourceID, newPersonID,
		survivorSnapshotID, absorbedSnapshotID,
	)
	if err != nil {
		return false, fmt.Errorf("inspect %s split record target: %w", row.TableName, err)
	}
	return present, nil
}

func personSplitNullablePersonReference(table, column string) bool {
	return (table == personRelationshipReviewsTableName && column == "matched_person_id") ||
		(table == "person_uid_aliases" && column == "surviving_person_id") ||
		(table == "person_merges" && column == "current_person_id")
}

func (s *Store) personSplitResolvePersonReferenceTx(
	ctx context.Context,
	tx *loggedTx,
	historicalID, sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
) (int64, bool, error) {
	switch historicalID {
	case survivorSnapshotID:
		return sourceID, true, nil
	case absorbedSnapshotID:
		return newPersonID, true, nil
	}
	var resolvedID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM persons WHERE id = ?`, historicalID).
		Scan(&resolvedID)
	if err == nil {
		return resolvedID, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("resolve split person reference %d: %w", historicalID, err)
	}
	err = tx.QueryRowContext(ctx, `SELECT
		COALESCE(alias.surviving_person_id, merge_record.current_person_id)
		FROM person_merges merge_record
		LEFT JOIN person_uid_aliases alias
		  ON alias.retired_uid = merge_record.absorbed_uid
		JOIN persons current_person
		  ON current_person.id = COALESCE(alias.surviving_person_id, merge_record.current_person_id)
		WHERE merge_record.absorbed_person_id = ?
		ORDER BY merge_record.id DESC LIMIT 1`, historicalID).Scan(&resolvedID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("resolve split person merge lineage %d: %w", historicalID, err)
	}
	return resolvedID, true, nil
}

func personSplitJournalRowWasRetained(row personSplitJournalRow) bool {
	if row.action != personMergeActionDeduplicated {
		return false
	}
	if row.originalRowID.Valid && row.currentRowID.Valid {
		return row.originalRowID.Int64 == row.currentRowID.Int64
	}
	return !row.originalRowID.Valid && !row.currentRowID.Valid &&
		row.currentKey.Valid && row.originalKey == row.currentKey.String
}

func (s *Store) reconcilePersonSplitCounterpartProjectionsTx(
	ctx context.Context, tx *loggedTx, splitID, sourceID, newPersonID int64,
) error {
	// A fresh person ID can reverse the stable ordering used by symmetric
	// relationship rows, so restore their canonical endpoint order first.
	if _, err := tx.ExecContext(ctx, `UPDATE person_relationships SET
		source_person_id = target_person_id,
		target_person_id = source_person_id
		WHERE id IN (
			SELECT original_row_id FROM person_merge_rows
			WHERE split_id = ? AND table_name = 'person_relationships'
			  AND original_row_id IS NOT NULL
		)
		AND source_person_id > target_person_id
		AND relationship_type_id IN (
			SELECT id FROM relationship_types WHERE is_symmetric = TRUE
		)`, splitID); err != nil {
		return fmt.Errorf("canonicalize split relationships: %w", err)
	}
	queries := []struct {
		query string
		args  []any
	}{
		{
			query: `SELECT source_person_id FROM person_relationships WHERE id IN (
				SELECT original_row_id FROM person_merge_rows
				WHERE split_id = ? AND table_name = 'person_relationships'
			) UNION SELECT target_person_id FROM person_relationships WHERE id IN (
				SELECT original_row_id FROM person_merge_rows
				WHERE split_id = ? AND table_name = 'person_relationships'
			)`,
			args: []any{splitID, splitID},
		},
		{
			query: `SELECT value.person_id FROM person_attribute_values value
				JOIN person_merge_rows journal ON journal.original_row_id = value.id
				WHERE journal.split_id = ? AND journal.table_name = 'person_attribute_values'
				  AND journal.provenance_kind = 'inbound_reference'`,
			args: []any{splitID},
		},
		{
			query: `SELECT employment.person_id
				FROM organization_attribute_values value
				JOIN person_merge_rows journal ON journal.original_row_id = value.id
				JOIN employments employment ON employment.organization_id = value.organization_id
				WHERE journal.split_id = ?
				  AND journal.table_name = 'organization_attribute_values'
				  AND journal.provenance_kind = 'inbound_reference'`,
			args: []any{splitID},
		},
		{
			query: `SELECT review.person_id
				FROM person_relationship_reviews review
				JOIN person_merge_rows journal ON journal.original_row_id = review.id
				WHERE journal.split_id = ?
				  AND journal.table_name = 'person_relationship_reviews'`,
			args: []any{splitID},
		},
	}
	people := []int64{}
	for _, item := range queries {
		ids, err := personMergeRowIDsTx(ctx, tx, item.query, item.args...)
		if err != nil {
			return fmt.Errorf("load split counterpart projections: %w", err)
		}
		for _, personID := range ids {
			if personID != sourceID && personID != newPersonID {
				people = append(people, personID)
			}
		}
	}
	return s.bumpPersonVCardProjectionsTx(ctx, tx, people...)
}

func loadPersonSplitJournalTx(
	ctx context.Context, tx *loggedTx, mergeID int64,
) ([]personSplitJournalRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT table_name, original_row_id,
		original_row_key, current_row_id, current_row_key, provenance_kind, origin_side,
		participant_id, action, snapshot_path, post_merge_row_json
		FROM person_merge_rows WHERE merge_id = ? AND split_id IS NULL
		ORDER BY table_name, original_row_key`, mergeID)
	if err != nil {
		return nil, fmt.Errorf("load person split journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := []personSplitJournalRow{}
	for rows.Next() {
		var row personSplitJournalRow
		if err := rows.Scan(
			&row.tableName, &row.originalRowID, &row.originalKey,
			&row.currentRowID, &row.currentKey, &row.provenance, &row.originSide,
			&row.participantID, &row.action, &row.snapshotPath, &row.postMergeJSON,
		); err != nil {
			return nil, fmt.Errorf("scan person split journal: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate person split journal: %w", err)
	}
	return result, nil
}

func personSplitJournalRef(row personSplitJournalRow) PersonMergeRowRef {
	var id *int64
	if row.originalRowID.Valid {
		value := row.originalRowID.Int64
		id = &value
	}
	return PersonMergeRowRef{
		TableName: row.tableName, OriginalRowID: id,
		OriginalKey: row.originalKey, Action: row.action,
	}
}

func personSplitRestorePriority(table string) int {
	switch table {
	case identityMatchCandidatesTableName, personRelationshipsTableName, personAttributeValuesTableName:
		return 10
	case "identity_match_candidate_redirects", identityMatchCandidateSourcesTableName,
		identityMatchEvidenceTableName, personRelationshipReviewsTableName,
		personMergeReviewCandidatesTableName:
		return 20
	case identityMatchEvidenceSourcesTableName:
		return 30
	default:
		return 15
	}
}

func (s *Store) restoreExistingPersonSplitRowTx(
	ctx context.Context,
	tx *loggedTx,
	journal personSplitJournalRow,
	row personMergeSnapshotRow,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
) error {
	spec, ok := personMergeTableRegistry[row.TableName]
	if !ok {
		return fmt.Errorf("%w: unregistered split table %q", ErrPersonMergeInvalid, row.TableName)
	}
	where, whereArgs, err := personSplitCurrentRowWhere(spec, journal)
	if err != nil {
		return err
	}
	currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
		`SELECT * FROM `+personSplitIdentifier(row.TableName)+` WHERE `+where,
		whereArgs, absorbedSnapshotID)
	if err != nil {
		return err
	}
	if len(currentRows) == 0 {
		// A supported post-merge deletion is user state, not merge damage.
		// Leave the row absent and close its journal entry below.
		return nil
	}
	if len(currentRows) != 1 {
		return fmt.Errorf("%w: current %s row is missing", ErrPersonSplitParticipants, row.TableName)
	}
	recordTargetPresent, err := s.personSplitSnapshotRecordTargetPresentTx(
		ctx, tx, row, sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID,
	)
	if err != nil {
		return err
	}
	currentByName := personSplitSnapshotColumnsByName(currentRows[0].Columns)
	postByName := map[string]personMergeSnapshotValue{}
	if journal.postMergeJSON.Valid {
		var post personMergeSnapshotRow
		if err := json.Unmarshal([]byte(journal.postMergeJSON.String), &post); err != nil {
			return fmt.Errorf("%w: decode %s post-merge row: %w",
				ErrPersonMergeSnapshotCorrupt, row.TableName, err)
		}
		postByName = personSplitSnapshotColumnsByName(post.Columns)
	}
	keyColumns := make(map[string]struct{}, len(spec.keyColumns()))
	for _, name := range spec.keyColumns() {
		keyColumns[name] = struct{}{}
	}
	assignments := make([]string, 0, len(row.Columns))
	args := make([]any, 0, len(row.Columns)+3)
	for _, column := range row.Columns {
		if !recordTargetPresent &&
			(column.Name == "active_until" || column.Name == "superseded_at") {
			continue
		}
		if personSplitRevisionedTable(row.TableName) &&
			(column.Name == "revision" || column.Name == "updated_at") {
			continue
		}
		dependencyPresent, err := s.personSplitSnapshotColumnDependencyPresentTx(
			ctx, tx, row, column, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return err
		}
		if !dependencyPresent {
			continue
		}
		value, restore, personReference, err := s.personSplitExistingColumnValueTx(
			ctx, tx, column, row, spec, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
		)
		if err != nil {
			return err
		}
		if restore && personReference && journal.postMergeJSON.Valid {
			postValue, hasPost := postByName[column.Name]
			currentValue, hasCurrent := currentByName[column.Name]
			if hasPost && hasCurrent && !reflect.DeepEqual(currentValue, postValue) {
				restore = false
			}
		}
		if !restore && !personReference && journal.postMergeJSON.Valid {
			_, isKey := keyColumns[column.Name]
			postValue, hasPost := postByName[column.Name]
			currentValue, hasCurrent := currentByName[column.Name]
			// A repointed composite row is the same physical fact under a
			// merge-created key. Restore that key only while it still matches
			// the recorded post-merge state; ordinary keys remain immutable.
			if (!isKey || journal.action == "repointed") && hasPost && hasCurrent &&
				!reflect.DeepEqual(column.Value, postValue) &&
				reflect.DeepEqual(currentValue, postValue) {
				value = personSplitSnapshotValue(column.Value)
				restore = true
			}
		}
		if !restore {
			continue
		}
		assignments = append(assignments, personSplitIdentifier(column.Name)+" = ?")
		args = append(args, value)
	}
	if personSplitRevisionedTable(row.TableName) {
		assignments = append(assignments,
			"revision = revision + 1", "updated_at = "+s.dialect.Now())
	}
	if len(assignments) == 0 {
		return nil
	}
	args = append(args, whereArgs...)
	result, err := tx.ExecContext(ctx, `UPDATE `+personSplitIdentifier(row.TableName)+
		` SET `+strings.Join(assignments, ", ")+` WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("restore split row %s: %w", row.TableName, err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("count restored split row %s: %w", row.TableName, err)
	} else if changed != 1 {
		return fmt.Errorf("%w: current %s row is missing", ErrPersonSplitParticipants, row.TableName)
	}
	return nil
}

func personSplitSnapshotColumnsByName(
	columns []personMergeSnapshotColumn,
) map[string]personMergeSnapshotValue {
	result := make(map[string]personMergeSnapshotValue, len(columns))
	for _, column := range columns {
		result[column.Name] = column.Value
	}
	return result
}

func rebasePersonMergePostRowReferences(
	encoded sql.NullString,
	current personMergeSnapshotRow,
	spec personMergeTableSpec,
	replacements map[int64]int64,
) (sql.NullString, bool, error) {
	if !encoded.Valid || encoded.String == "" {
		return encoded, false, nil
	}
	var post personMergeSnapshotRow
	if err := json.Unmarshal([]byte(encoded.String), &post); err != nil {
		return sql.NullString{}, false, fmt.Errorf("%w: decode rebased %s post-merge row: %w",
			ErrPersonMergeSnapshotCorrupt, spec.TableName, err)
	}
	currentByName := personSplitSnapshotColumnsByName(current.Columns)
	changed := false
	for _, reference := range spec.PersonReferences {
		if reference.Kind == personMergeReferencePolymorphic &&
			personSplitSnapshotRowText(post, reference.KindColumn) != reference.KindValue {
			continue
		}
		for index := range post.Columns {
			column := &post.Columns[index]
			if column.Name != reference.IDColumn || column.Value.Integer == nil {
				continue
			}
			target, ok := replacements[*column.Value.Integer]
			if !ok {
				continue
			}
			currentValue, ok := currentByName[column.Name]
			if !ok || currentValue.Integer == nil || *currentValue.Integer != target {
				continue
			}
			column.Value = currentValue
			changed = true
		}
	}
	if !changed {
		return encoded, false, nil
	}
	postByName := personSplitSnapshotColumnsByName(post.Columns)
	keyColumns := make([]personMergeSnapshotColumn, 0, len(spec.keyColumns()))
	for _, name := range spec.keyColumns() {
		value, ok := postByName[name]
		if !ok {
			return sql.NullString{}, false, fmt.Errorf("%w: rebased %s post-merge key",
				ErrPersonMergeSnapshotCorrupt, spec.TableName)
		}
		keyColumns = append(keyColumns, personMergeSnapshotColumn{Name: name, Value: value})
	}
	rowKey, err := canonicalPersonMergeSnapshotRowKey(keyColumns, spec.keyColumns())
	if err != nil {
		return sql.NullString{}, false, err
	}
	post.RowKey = rowKey
	rebased, err := json.Marshal(post)
	if err != nil {
		return sql.NullString{}, false, fmt.Errorf("encode rebased %s post-merge row: %w",
			spec.TableName, err)
	}
	return sql.NullString{String: string(rebased), Valid: true}, true, nil
}

func (s *Store) personSplitExistingColumnValueTx(
	ctx context.Context,
	tx *loggedTx,
	column personMergeSnapshotColumn,
	row personMergeSnapshotRow,
	spec personMergeTableSpec,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
) (any, bool, bool, error) {
	if spec.TableName == "vcard_resource_envelopes" && column.Name == "canonical_person_uid" {
		switch personSplitSnapshotRowInteger(row, "person_id") {
		case absorbedSnapshotID:
			return newUID, true, false, nil
		case survivorSnapshotID:
			return sourceUID, true, false, nil
		}
	}
	for _, reference := range spec.PersonReferences {
		if reference.IDColumn != column.Name {
			continue
		}
		if reference.Kind == personMergeReferencePolymorphic &&
			personSplitSnapshotRowText(row, reference.KindColumn) != reference.KindValue {
			continue
		}
		if column.Value.Integer == nil {
			return nil, true, true, nil
		}
		resolvedID, present, err := s.personSplitResolvePersonReferenceTx(
			ctx, tx, *column.Value.Integer, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return nil, false, true, err
		}
		if !present {
			return nil, false, true, nil
		}
		return resolvedID, true, true, nil
	}
	return nil, false, false, nil
}

func (s *Store) insertPersonSplitSnapshotRowTx(
	ctx context.Context,
	tx *loggedTx,
	row personMergeSnapshotRow,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
) error {
	spec, ok := personMergeTableRegistry[row.TableName]
	if !ok {
		return fmt.Errorf("%w: unregistered split table %q", ErrPersonMergeInvalid, row.TableName)
	}
	if row.TableName == identityMatchCandidatesTableName {
		if _, err := tx.ExecContext(ctx, `DELETE FROM identity_match_candidate_redirects
			WHERE retired_candidate_id = ?`, row.RowID); err != nil {
			return fmt.Errorf("remove split candidate redirect: %w", err)
		}
	}
	columns := make([]string, 0, len(row.Columns))
	args := make([]any, 0, len(row.Columns))
	for _, column := range row.Columns {
		if personSplitRevisionedTable(row.TableName) && column.Name == "updated_at" {
			continue
		}
		columns = append(columns, personSplitIdentifier(column.Name))
		dependencyPresent, err := s.personSplitSnapshotColumnDependencyPresentTx(
			ctx, tx, row, column, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return err
		}
		var value any
		if dependencyPresent {
			value, err = s.personSplitSnapshotColumnValueTx(
				ctx, tx, column, row, spec, sourceID, newPersonID,
				survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
			)
			if errors.Is(err, errPersonSplitReferenceMissing) {
				value = nil
			} else if err != nil {
				return err
			}
		}
		if personSplitRevisionedTable(row.TableName) && column.Name == "revision" {
			revision, ok := value.(int64)
			if !ok {
				return fmt.Errorf("%w: invalid %s revision", ErrPersonMergeSnapshotCorrupt, row.TableName)
			}
			value = revision + 1
		}
		args = append(args, value)
	}
	table := personSplitIdentifier(row.TableName)
	insert := `INSERT INTO ` + table
	if s.IsPostgreSQL() {
		insert += ` (` + strings.Join(columns, ", ") + `) OVERRIDING SYSTEM VALUE`
	} else {
		insert += ` (` + strings.Join(columns, ", ") + `)`
	}
	insert += ` VALUES (` + personMergeSnapshotPlaceholders(len(columns)) + `)`
	if _, err := tx.ExecContext(ctx, insert, args...); err != nil {
		return fmt.Errorf("recreate split row %s: %w", row.TableName, err)
	}
	return nil
}

func personSplitRevisionedTable(table string) bool {
	switch table {
	case "vcard_resource_envelopes", personRelationshipsTableName, "employments":
		return true
	default:
		return false
	}
}

func (s *Store) personSplitSnapshotColumnValueTx(
	ctx context.Context,
	tx *loggedTx,
	column personMergeSnapshotColumn,
	row personMergeSnapshotRow,
	spec personMergeTableSpec,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
) (any, error) {
	value := personSplitSnapshotValue(column.Value)
	for _, reference := range spec.PersonReferences {
		if reference.IDColumn != column.Name {
			continue
		}
		if reference.Kind == personMergeReferencePolymorphic &&
			personSplitSnapshotRowText(row, reference.KindColumn) != reference.KindValue {
			continue
		}
		if column.Value.Integer == nil {
			return value, nil
		}
		resolvedID, present, err := s.personSplitResolvePersonReferenceTx(
			ctx, tx, *column.Value.Integer, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID,
		)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, errPersonSplitReferenceMissing
		}
		return resolvedID, nil
	}
	if spec.TableName == "vcard_resource_envelopes" && column.Name == "canonical_person_uid" {
		switch personSplitSnapshotRowInteger(row, "person_id") {
		case absorbedSnapshotID:
			return newUID, nil
		case survivorSnapshotID:
			return sourceUID, nil
		}
	}
	return value, nil
}

func personSplitSnapshotRowText(row personMergeSnapshotRow, name string) string {
	for _, column := range row.Columns {
		if column.Name == name && column.Value.Text != nil {
			return *column.Value.Text
		}
	}
	return ""
}

func personSplitSnapshotRowInteger(row personMergeSnapshotRow, name string) int64 {
	for _, column := range row.Columns {
		if column.Name == name && column.Value.Integer != nil {
			return *column.Value.Integer
		}
	}
	return 0
}

func personSplitSnapshotValue(value personMergeSnapshotValue) any {
	switch value.Kind {
	case personMergeSnapshotNull:
		return nil
	case personMergeSnapshotInteger:
		if value.Integer != nil {
			return *value.Integer
		}
	case personMergeSnapshotReal:
		if value.Real != nil {
			return *value.Real
		}
	case personMergeSnapshotBoolean:
		if value.Boolean != nil {
			return *value.Boolean
		}
	case personMergeSnapshotText:
		if value.Text != nil {
			return *value.Text
		}
	case personMergeSnapshotBytes:
		return value.Bytes
	}
	return nil
}

type priorPersonMergeJournalRow struct {
	mergeID       int64
	originalRowID sql.NullInt64
	originalKey   string
	currentRowID  sql.NullInt64
	currentKey    sql.NullString
	postMergeJSON sql.NullString
}

// rebasePriorPersonMergeRowsAfterSplitTx keeps older unsplit journals pointed
// at the physical row produced by this split. Merge reconciliation performs
// the forward rebase when rows move or collapse; the inverse operation must do
// the same or a later reversal of the older merge follows a stale key.
func (s *Store) rebasePriorPersonMergeRowsAfterSplitTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID int64,
	journal personSplitJournalRow,
	snapshotRow personMergeSnapshotRow,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
	replacement bool,
) error {
	spec, ok := personMergeTableRegistry[journal.tableName]
	if !ok {
		return fmt.Errorf("%w: unregistered split table %q", ErrPersonMergeInvalid, journal.tableName)
	}
	newRowID, newRowKey, exists, err := s.personSplitRestoredRowLocatorTx(
		ctx, tx, spec, snapshotRow, sourceID, newPersonID,
		survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
	)
	if err != nil {
		return err
	}
	var rebasedCurrent *personMergeSnapshotRow
	if exists {
		where, args, err := personSplitCurrentRowWhere(spec, personSplitJournalRow{
			currentRowID: newRowID, currentKey: newRowKey,
		})
		if err != nil {
			return err
		}
		currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
			`SELECT * FROM `+personSplitIdentifier(spec.TableName)+` WHERE `+where,
			args, absorbedSnapshotID)
		if err != nil {
			return err
		}
		if len(currentRows) != 1 {
			return fmt.Errorf("%w: rebased %s row is missing",
				ErrPersonSplitParticipants, journal.tableName)
		}
		rebasedCurrent = &currentRows[0]
	}

	rows, err := tx.QueryContext(ctx, `SELECT merge_id, original_row_id,
		original_row_key, current_row_id, current_row_key, post_merge_row_json
		FROM person_merge_rows
		WHERE merge_id <> ? AND table_name = ? AND split_id IS NULL
		ORDER BY merge_id, original_row_key`, mergeID, journal.tableName)
	if err != nil {
		return fmt.Errorf("load prior %s split journals: %w", journal.tableName, err)
	}
	defer func() { _ = rows.Close() }()
	prior := []priorPersonMergeJournalRow{}
	for rows.Next() {
		var item priorPersonMergeJournalRow
		if err := rows.Scan(
			&item.mergeID, &item.originalRowID, &item.originalKey,
			&item.currentRowID, &item.currentKey, &item.postMergeJSON,
		); err != nil {
			return fmt.Errorf("scan prior %s split journal: %w", journal.tableName, err)
		}
		prior = append(prior, item)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate prior %s split journals: %w", journal.tableName, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close prior %s split journals: %w", journal.tableName, err)
	}

	recreated := replacement || journal.action == "deleted_snapshot" ||
		(journal.action == personMergeActionDeduplicated && !personSplitJournalRowWasRetained(journal))
	for _, item := range prior {
		locatorMatch := personSplitJournalLocatorsEqual(
			item.currentRowID, item.currentKey, journal.currentRowID, journal.currentKey,
		)
		lineageMatch, err := priorPersonMergeJournalMatchesSplitOriginal(item, spec, journal)
		if err != nil {
			return err
		}
		if (recreated && !lineageMatch) || (!recreated && !locatorMatch) {
			continue
		}
		action := "deleted_snapshot"
		var currentRowID, currentRowKey any
		if exists {
			action = "moved"
			if newRowID.Valid {
				currentRowID = newRowID.Int64
			}
			if newRowKey.Valid {
				currentRowKey = newRowKey.String
			}
		}
		var rebasedPostMergeJSON sql.NullString
		if !exists {
			rebasedPostMergeJSON = sql.NullString{}
		} else {
			var err error
			rebasedPostMergeJSON, _, err = rebasePersonMergePostRowReferences(
				item.postMergeJSON, *rebasedCurrent, spec, map[int64]int64{
					absorbedSnapshotID: newPersonID,
					sourceID:           newPersonID,
				},
			)
			if err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_merge_rows SET
			action = ?, current_row_id = ?, current_row_key = ?, post_merge_row_json = ?
			WHERE merge_id = ? AND table_name = ? AND original_row_key = ?
			  AND split_id IS NULL`, action, currentRowID, currentRowKey, rebasedPostMergeJSON,
			item.mergeID, journal.tableName, item.originalKey); err != nil {
			return fmt.Errorf("rebase prior %s journal after split: %w", journal.tableName, err)
		}
		if exists {
			if err := syncPersonMergeRowPersonRefsTx(
				ctx, tx, item.mergeID, journal.tableName, item.originalKey,
				*rebasedCurrent, spec,
			); err != nil {
				return err
			}
		} else if _, err := tx.ExecContext(ctx, `DELETE FROM person_merge_row_person_refs
			WHERE merge_id = ? AND table_name = ? AND original_row_key = ?`,
			item.mergeID, journal.tableName, item.originalKey); err != nil {
			return fmt.Errorf("clear prior %s split person references: %w", journal.tableName, err)
		}
	}
	return nil
}

func (s *Store) personSplitTrackingRowReplacedTx(
	ctx context.Context, tx *loggedTx, journal personSplitJournalRow,
) (bool, error) {
	if journal.tableName != "person_tracking" || !journal.postMergeJSON.Valid {
		return false, nil
	}
	var post personMergeSnapshotRow
	if err := json.Unmarshal([]byte(journal.postMergeJSON.String), &post); err != nil {
		return false, fmt.Errorf("%w: decode tracking post-merge row: %w",
			ErrPersonMergeSnapshotCorrupt, err)
	}
	spec := personMergeTableRegistry["person_tracking"]
	where, args, err := personSplitCurrentRowWhere(spec, journal)
	if err != nil {
		return false, err
	}
	current, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
		`SELECT * FROM person_tracking WHERE `+where, args, 0)
	if err != nil {
		return false, err
	}
	if len(current) == 0 {
		return false, nil
	}
	if len(current) != 1 {
		return false, fmt.Errorf("%w: current person_tracking row is ambiguous",
			ErrPersonSplitParticipants)
	}
	currentColumns := personSplitSnapshotColumnsByName(current[0].Columns)
	postColumns := personSplitSnapshotColumnsByName(post.Columns)
	return !reflect.DeepEqual(currentColumns["tracked_at"], postColumns["tracked_at"]), nil
}

func (s *Store) personSplitRestoredRowLocatorTx(
	ctx context.Context,
	tx *loggedTx,
	spec personMergeTableSpec,
	row personMergeSnapshotRow,
	sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64,
	sourceUID, newUID string,
) (sql.NullInt64, sql.NullString, bool, error) {
	columnsByName := make(map[string]personMergeSnapshotColumn, len(row.Columns))
	for _, column := range row.Columns {
		columnsByName[column.Name] = column
	}
	keyNames := spec.keyColumns()
	keyColumns := make([]personMergeSnapshotColumn, 0, len(keyNames))
	for _, name := range keyNames {
		column, ok := columnsByName[name]
		if !ok {
			return sql.NullInt64{}, sql.NullString{}, false,
				fmt.Errorf("%w: missing %s split key %q", ErrPersonMergeSnapshotCorrupt, spec.TableName, name)
		}
		value, err := s.personSplitSnapshotColumnValueTx(
			ctx, tx, column, row, spec, sourceID, newPersonID,
			survivorSnapshotID, absorbedSnapshotID, sourceUID, newUID,
		)
		if errors.Is(err, errPersonSplitReferenceMissing) {
			return sql.NullInt64{}, sql.NullString{}, false, nil
		}
		if err != nil {
			return sql.NullInt64{}, sql.NullString{}, false, err
		}
		normalized, err := normalizePersonMergeSnapshotValue(value, string(column.Value.Kind))
		if err != nil {
			return sql.NullInt64{}, sql.NullString{}, false,
				fmt.Errorf("%w: normalize %s split key: %w", ErrPersonMergeSnapshotCorrupt, spec.TableName, err)
		}
		keyColumns = append(keyColumns, personMergeSnapshotColumn{Name: name, Value: normalized})
	}
	encoded, err := canonicalPersonMergeSnapshotRowKey(keyColumns, keyNames)
	if err != nil {
		return sql.NullInt64{}, sql.NullString{}, false, err
	}
	where, args, err := personSplitRowKeyWhere(spec, encoded)
	if err != nil {
		return sql.NullInt64{}, sql.NullString{}, false, err
	}
	var present int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+personSplitIdentifier(spec.TableName)+
		` WHERE `+where+` LIMIT 1`, args...).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, sql.NullString{}, false, nil
	}
	if err != nil {
		return sql.NullInt64{}, sql.NullString{}, false,
			fmt.Errorf("locate restored %s split row: %w", spec.TableName, err)
	}
	rowID := sql.NullInt64{}
	if len(keyColumns) == 1 && keyColumns[0].Value.Integer != nil {
		rowID = sql.NullInt64{Int64: *keyColumns[0].Value.Integer, Valid: true}
	}
	return rowID, sql.NullString{String: encoded, Valid: true}, true, nil
}

func personSplitJournalLocatorsEqual(
	leftID sql.NullInt64, leftKey sql.NullString,
	rightID sql.NullInt64, rightKey sql.NullString,
) bool {
	if leftID.Valid || rightID.Valid {
		return leftID.Valid && rightID.Valid && leftID.Int64 == rightID.Int64
	}
	return leftKey.Valid && rightKey.Valid && leftKey.String == rightKey.String
}

func priorPersonMergeJournalMatchesSplitOriginal(
	prior priorPersonMergeJournalRow,
	spec personMergeTableSpec,
	current personSplitJournalRow,
) (bool, error) {
	if prior.originalRowID.Valid && current.originalRowID.Valid &&
		prior.originalRowID.Int64 == current.originalRowID.Int64 {
		return true, nil
	}
	if prior.originalKey == current.originalKey {
		return true, nil
	}
	if !prior.postMergeJSON.Valid || prior.postMergeJSON.String == "" {
		return false, nil
	}
	var post personMergeSnapshotRow
	if err := json.Unmarshal([]byte(prior.postMergeJSON.String), &post); err != nil {
		return false, fmt.Errorf("%w: decode prior %s post-merge row: %w",
			ErrPersonMergeSnapshotCorrupt, spec.TableName, err)
	}
	postKey := post.RowKey
	if postKey == "" {
		columns := personSplitSnapshotColumnsByName(post.Columns)
		keyColumns := make([]personMergeSnapshotColumn, 0, len(spec.keyColumns()))
		for _, name := range spec.keyColumns() {
			value, ok := columns[name]
			if !ok {
				return false, fmt.Errorf("%w: prior %s post-merge key",
					ErrPersonMergeSnapshotCorrupt, spec.TableName)
			}
			keyColumns = append(keyColumns, personMergeSnapshotColumn{Name: name, Value: value})
		}
		var err error
		postKey, err = canonicalPersonMergeSnapshotRowKey(keyColumns, spec.keyColumns())
		if err != nil {
			return false, err
		}
	}
	return postKey == current.originalKey, nil
}

func personSplitCurrentRowWhere(
	spec personMergeTableSpec, row personSplitJournalRow,
) (string, []any, error) {
	if row.currentRowID.Valid && len(spec.keyColumns()) == 1 {
		return personSplitIdentifier(spec.keyColumns()[0]) + " = ?",
			[]any{row.currentRowID.Int64}, nil
	}
	if !row.currentKey.Valid {
		return "", nil, fmt.Errorf("%w: current split row key is missing", ErrPersonSplitParticipants)
	}
	return personSplitRowKeyWhere(spec, row.currentKey.String)
}

func (s *Store) preparePersonSplitDeduplicatedRowTx(
	ctx context.Context,
	tx *loggedTx,
	journal personSplitJournalRow,
	original personMergeSnapshotRow,
) (personMergeSnapshotRow, bool, error) {
	spec, ok := personMergeTableRegistry[journal.tableName]
	if !ok {
		return original, false,
			fmt.Errorf("%w: unregistered split table %q", ErrPersonMergeInvalid, journal.tableName)
	}
	where, args, err := personSplitCurrentRowWhere(spec, journal)
	if err != nil {
		return original, false, err
	}
	currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
		`SELECT * FROM `+personSplitIdentifier(journal.tableName)+` WHERE `+where,
		args, 0)
	if err != nil {
		return original, false, err
	}
	if len(currentRows) == 0 || !journal.postMergeJSON.Valid {
		return original, false, nil
	}
	if len(currentRows) != 1 {
		return original, false, fmt.Errorf("%w: current %s row is ambiguous",
			ErrPersonSplitParticipants, journal.tableName)
	}
	var post personMergeSnapshotRow
	if err := json.Unmarshal([]byte(journal.postMergeJSON.String), &post); err != nil {
		return original, false, fmt.Errorf("%w: decode deduplicated %s post-merge row: %w",
			ErrPersonMergeSnapshotCorrupt, journal.tableName, err)
	}
	currentByName := personSplitSnapshotColumnsByName(currentRows[0].Columns)
	postByName := personSplitSnapshotColumnsByName(post.Columns)
	protected := make(map[string]struct{}, len(spec.keyColumns())+len(spec.PersonReferences)*2)
	for _, name := range spec.keyColumns() {
		protected[name] = struct{}{}
	}
	for _, reference := range spec.PersonReferences {
		protected[reference.IDColumn] = struct{}{}
		if reference.Kind == personMergeReferencePolymorphic {
			protected[reference.KindColumn] = struct{}{}
		}
	}
	for name := range protected {
		currentValue, hasCurrent := currentByName[name]
		postValue, hasPost := postByName[name]
		if hasCurrent != hasPost || (hasCurrent && !reflect.DeepEqual(currentValue, postValue)) {
			return original, false, nil
		}
	}
	reconciled := original
	reconciled.Columns = slices.Clone(original.Columns)
	for index := range reconciled.Columns {
		name := reconciled.Columns[index].Name
		if _, fixed := protected[name]; fixed || name == "updated_at" {
			continue
		}
		currentValue, hasCurrent := currentByName[name]
		postValue, hasPost := postByName[name]
		if hasCurrent && hasPost && !reflect.DeepEqual(currentValue, postValue) {
			reconciled.Columns[index].Value = currentValue
		}
	}
	return reconciled, true, nil
}

func personSplitRowKeyWhere(
	spec personMergeTableSpec, encoded string,
) (string, []any, error) {
	var key []personMergeSnapshotColumn
	if err := json.Unmarshal([]byte(encoded), &key); err != nil {
		return "", nil, fmt.Errorf("%w: decode split row key: %w", ErrPersonMergeSnapshotCorrupt, err)
	}
	wanted := spec.keyColumns()
	if len(key) != len(wanted) {
		return "", nil, fmt.Errorf("%w: split row key shape", ErrPersonMergeSnapshotCorrupt)
	}
	predicates := make([]string, len(key))
	args := make([]any, len(key))
	for index, column := range key {
		if column.Name != wanted[index] {
			return "", nil, fmt.Errorf("%w: split row key column", ErrPersonMergeSnapshotCorrupt)
		}
		predicates[index] = personSplitIdentifier(column.Name) + " = ?"
		args[index] = personSplitSnapshotValue(column.Value)
	}
	return strings.Join(predicates, " AND "), args, nil
}

func personSplitIdentifier(value string) string {
	// All callers use the closed merge-table registry or schema-derived column
	// names. Quote them defensively anyway so malformed snapshot data can never
	// turn validation into a process-wide panic.
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func markPersonSplitJournalRowTx(
	ctx context.Context, tx *loggedTx, mergeID, splitID int64, row personSplitJournalRow,
) error {
	result, err := tx.ExecContext(ctx, `UPDATE person_merge_rows SET split_id = ?
		WHERE merge_id = ? AND table_name = ? AND original_row_key = ? AND split_id IS NULL`,
		splitID, mergeID, row.tableName, row.originalKey)
	if err != nil {
		return fmt.Errorf("mark split row journal: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("count split row journal: %w", err)
	} else if changed != 1 {
		return fmt.Errorf("%w: split row journal changed", ErrPersonMergeAlreadySplit)
	}
	return nil
}

func (s *Store) personSplitByIdempotencyKeyTx(
	ctx context.Context, tx *loggedTx, key, requestHash string,
) (*PersonSplitResult, bool, error) {
	var splitID int64
	var storedHash string
	var storedResult sql.NullString
	var storedIdentityRevision sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT id, request_hash, result_json, identity_revision
		FROM person_splits WHERE idempotency_key = ?`, key).Scan(
		&splitID, &storedHash, &storedResult, &storedIdentityRevision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("load person split idempotency key: %w", err)
	}
	if storedHash != requestHash {
		return nil, false, ErrPersonSplitIdempotency
	}
	if storedResult.Valid && storedResult.String != "" {
		var result PersonSplitResult
		if err := json.Unmarshal([]byte(storedResult.String), &result); err != nil {
			return nil, false, fmt.Errorf("decode person split idempotency result: %w", err)
		}
		if !storedIdentityRevision.Valid || storedIdentityRevision.Int64 <= 0 ||
			result.IdentityRevision != storedIdentityRevision.Int64 {
			return nil, false, errors.New("person split idempotency revision is missing or inconsistent")
		}
		return &result, true, nil
	}
	return nil, false, errors.New("person split idempotency result is missing")
}

func (s *Store) getPersonSplitTx(
	ctx context.Context, tx *loggedTx, splitID int64,
) (*PersonSplit, error) {
	var split PersonSplit
	err := tx.QueryRowContext(ctx, `SELECT id, merge_id, source_person_id,
		new_person_id, new_person_uid, source_revision_before,
		source_revision_after, actor, is_exact_reversal, created_at
		FROM person_splits WHERE id = ?`, splitID).Scan(
		&split.ID, &split.MergeID, &split.SourcePersonID, &split.NewPersonID,
		&split.NewPersonUID, &split.SourceRevisionBefore, &split.SourceRevisionAfter,
		&split.Actor, &split.ExactReversal, &split.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrPersonSplitNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get person split %d: %w", splitID, err)
	}
	return &split, nil
}

// recomputePersonSplitActivityTx reclassifies materialized activity from its
// native participant evidence after bindings are divided. Messages remain
// immutable; only the derived activity links and contact aggregates change.
func (s *Store) recomputePersonSplitActivityTx(
	ctx context.Context,
	tx *loggedTx,
	sourceID, newPersonID int64,
	revisions ContactRevisions,
) error {
	contactIDs, err := s.reclassifyPersonActivityTx(
		ctx, tx, []int64{sourceID, newPersonID}, revisions,
	)
	if err != nil {
		return err
	}
	for _, personID := range contactIDs {
		if err := s.recomputeContactStateTx(ctx, tx, personID, revisions, true); err != nil {
			return err
		}
	}
	return nil
}
