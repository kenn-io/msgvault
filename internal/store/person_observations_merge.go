package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

func (s *Store) lockObservationPeopleTx(ctx context.Context, tx *loggedTx, ids ...int64) error {
	slices.Sort(ids)
	// Fact generation locks the person row before refreshing observation support.
	// Serialize with it before taking observation keys or merge person-row locks.
	for _, id := range ids {
		if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-fact-generation", id); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if err := s.lockPersonMutationTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
func observationMergeProvenance(o personobs.Observation, absorbedID int64) personMergeProvenanceKind {
	if o.PersonID != absorbedID {
		return personMergeProvenanceInboundReference
	}
	if o.Origin == personobs.OriginOwner || (o.Level == personobs.LevelExplicit && observationProtected(o)) {
		return personMergeProvenanceAbsorbedProfile
	}
	return personMergeProvenanceDerived
}
func (s *Store) captureObservationChildrenTx(ctx context.Context, tx *loggedTx, spec personMergeTableSpec, survivorID, absorbedID int64) ([]personMergeSnapshotRow, error) {
	child := spec.ChildOf
	query := `SELECT * FROM ` + spec.TableName + ` WHERE EXISTS(SELECT 1 FROM ` + child.ParentTable + ` parent WHERE parent.id=` + spec.TableName + `.` + child.ParentKeyColumn + ` AND (parent.person_id=? OR parent.person_id=?)) ORDER BY ` + strings.Join(spec.keyColumns(), ",")
	rows, err := s.capturePersonMergeQueryTx(ctx, tx, spec, query, []any{survivorID, absorbedID}, absorbedID)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		parent, err := s.loadObservationTx(ctx, tx, personSplitSnapshotRowInteger(rows[i], child.ParentKeyColumn))
		if err != nil {
			return nil, err
		}
		rows[i].ProvenanceKind = observationMergeProvenance(parent, absorbedID)
		if parent.PersonID == absorbedID {
			rows[i].OriginSide = personMergeOriginAbsorbed
		} else {
			rows[i].OriginSide = personMergeOriginSurvivor
		}
		// The absorbed ledger cascades all citations, including reinforcement of owner assertions.
		if spec.TableName == "person_observation_evidence" && parent.PersonID == absorbedID {
			rows[i].ProvenanceKind = personMergeProvenanceDerived
		}
	}
	return rows, nil
}

func (s *Store) movePersonObservationsTx(ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64, snapshot personMergeSnapshot) error {
	observations, err := s.queryObservationRows(ctx, tx, `person_id=?`, `sequence,id`, absorbedID)
	if err != nil {
		return err
	}
	for _, row := range observations {
		if row.Origin != personobs.OriginOwner {
			if !observationProtected(row) {
				continue
			}
			if row.Level != personobs.LevelExplicit {
				if err := s.copyMergeProtectedObservationTx(ctx, tx, mergeID, survivorID, row); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET origin='owner',generation_id=NULL,program_fingerprint='',directness='direct-other',support_flag='',mention_count=0,first_seen_at=NULL,last_seen_at=NULL WHERE id=?`, row.ID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM person_observation_evidence WHERE observation_id=?`, row.ID); err != nil {
				return err
			}
			if err := s.appendObservationEventTx(ctx, tx, absorbedID, row.ID, "merge-converted", "owner", "explicit", nil); err != nil {
				return err
			}
		}
		if row.Origin == personobs.OriginOwner {
			if _, err := tx.ExecContext(ctx, `DELETE FROM person_observation_evidence WHERE observation_id=?`, row.ID); err != nil {
				return err
			}
			if err := s.recomputeObservationCitationSummaryTx(ctx, tx, row.ID); err != nil {
				return err
			}
		}
		if err := s.moveOwnerObservationTx(ctx, tx, mergeID, survivorID, row.ID); err != nil {
			return err
		}
	}
	// The absorbed ledger cannot support any remaining machine row after merge.
	// Deleting these rows also removes their premise edges before the person FK cascades.
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_observations WHERE person_id=? AND origin<>'owner'`, absorbedID); err != nil {
		return err
	}
	suppressions, err := personMergeRowIDsTx(ctx, tx, `SELECT id FROM person_observation_suppressions WHERE person_id=? ORDER BY id`, absorbedID)
	if err != nil {
		return err
	}
	for _, id := range suppressions {
		if err := s.moveObservationSuppressionTx(ctx, tx, survivorID, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET subject_person_id=? WHERE subject_person_id=?`, survivorID, absorbedID); err != nil {
		return err
	}
	if err := s.recordMergeAddedObservationCategoriesTx(ctx, tx, mergeID, survivorID, snapshot); err != nil {
		return err
	}
	if err := s.recordMergeAddedObservationSensitivityTx(ctx, tx, mergeID, survivorID, snapshot); err != nil {
		return err
	}
	// The absorbed clock is recreated from the snapshot on an exact split.
	var clock bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_observation_clocks WHERE person_id=?)`, absorbedID).Scan(&clock); err != nil {
		return err
	}
	if !clock {
		return nil
	}
	return s.setPersonMergeRowDispositionTx(ctx, tx, mergeID, "person_observation_clocks", absorbedID, "deleted_snapshot", nil)
}

// An outdated rejection can be lifted by new support. Keep a stricter
// rejection when keys collide; otherwise keep the latest rejection boundary.
func (s *Store) moveObservationSuppressionTx(ctx context.Context, tx *loggedTx, survivorID, id int64) error {
	var key, reason, actor string
	var rejected requiredTimestamp
	var cleared nullableTimestamp
	if err := tx.QueryRowContext(ctx, `SELECT content_key,reason,actor,rejected_at,cleared_at FROM person_observation_suppressions WHERE id=?`, id).Scan(&key, &reason, &actor, &rejected, &cleared); err != nil {
		return err
	}
	if !cleared.Valid {
		var keptID int64
		var keptReason string
		var keptRejected requiredTimestamp
		err := tx.QueryRowContext(ctx, `SELECT id,reason,rejected_at FROM person_observation_suppressions WHERE person_id=? AND content_key=? AND cleared_at IS NULL`, survivorID, key).Scan(&keptID, &keptReason, &keptRejected)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if keptReason == "outdated" && (reason != "outdated" || rejected.Time.After(keptRejected.Time)) {
				// Keep the survivor row active in place. Updating its decision metadata
				// lets the snapshot restore each side without a temporary live-key clash.
				if _, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET reason=?,actor=?,rejected_at=? WHERE id=?`, reason, actor, rejected.Time, keptID); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET cleared_at=CURRENT_TIMESTAMP WHERE id=?`, id); err != nil {
				return err
			}
		}
	}
	_, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions SET person_id=? WHERE id=?`, survivorID, id)
	return err
}
func (s *Store) moveOwnerObservationTx(ctx context.Context, tx *loggedTx, mergeID, survivorID, id int64) error {
	row, err := s.loadObservationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	sequence, err := s.nextObservationSequenceTx(ctx, tx, survivorID)
	if err != nil {
		return err
	}
	if row.Status == personobs.StatusActive || row.Status == personobs.StatusUnsupported {
		matches, err := s.queryObservationRows(ctx, tx, `person_id=? AND content_key=? AND status IN ('active','unsupported')`, `id LIMIT 1`, survivorID, row.ContentKey)
		if err != nil {
			return err
		}
		if len(matches) > 0 {
			kept := matches[0]
			if err := s.mergeOwnerObservationCollisionTx(ctx, tx, mergeID, survivorID, kept.ID, id, row.Status == personobs.StatusActive || row.Status == personobs.StatusUnsupported); err != nil {
				return err
			}
			if err := s.transitionObservationTx(ctx, tx, id, personobs.StatusSuperseded, personobs.StatusReasonPersonMergeCollision, "merge-moved", "owner", "person-merge-collision"); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET person_id=?,sequence=? WHERE id=?`, survivorID, sequence, id); err != nil {
		return fmt.Errorf("move owner observation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_observation_events SET person_id=? WHERE observation_id=?`, survivorID, id); err != nil {
		return err
	}
	return s.appendObservationEventTx(ctx, tx, survivorID, id, "merge-moved", "owner", "", nil)
}
func (s *Store) copyMergeProtectedObservationTx(ctx context.Context, tx *loggedTx, mergeID, survivorID int64, row personobs.Observation) error {
	sequence, err := s.nextObservationSequenceTx(ctx, tx, survivorID)
	if err != nil {
		return err
	}
	status := row.Status
	reason := row.StatusReason
	if status == personobs.StatusActive || status == personobs.StatusUnsupported {
		status = personobs.StatusSuperseded
		reason = personobs.StatusReasonPersonMergeCollision
	}
	key := personobs.ContentKey(personobs.LevelExplicit, row.SubjectRef, row.Content)
	var id int64
	err = tx.QueryRowContext(ctx, `INSERT INTO person_observations(person_id,sequence,level,origin,kind,content,content_key,normalization_version,subject_ref,subject_person_id,directness,sensitive,confidence_bp,valid_from,valid_until,occurs_on,stability,stability_source,status,status_reason,owner_state,pinned)
 VALUES(?,?,'explicit','owner',?,?,?,'obs-norm-v1',?,?,'direct-other',?,?,?,?,?,?,'owner',?,?,?,?) RETURNING id`, survivorID, sequence, row.Kind, row.Content, key, row.SubjectRef, row.SubjectPersonID, row.Sensitive, 1000, row.ValidFrom, row.ValidUntil, observationOptionalDate(row.OccursOn), row.Stability, status, reason, row.OwnerState, row.Pinned).Scan(&id)
	if err != nil {
		return fmt.Errorf("copy protected derived observation: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) SELECT ?,category,'owner' FROM person_observation_categories WHERE observation_id=?`, id, row.ID); err != nil {
		return err
	}
	if row.Status == personobs.StatusActive || row.Status == personobs.StatusUnsupported {
		matches, err := s.queryObservationRows(ctx, tx, `person_id=? AND content_key=? AND status IN ('active','unsupported')`, `id LIMIT 1`, survivorID, key)
		if err != nil {
			return err
		}
		if len(matches) == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET status=?,status_reason=? WHERE id=?`, row.Status, row.StatusReason, id); err != nil {
				return err
			}
		} else {
			kept := matches[0]
			if err := s.mergeOwnerObservationCollisionTx(ctx, tx, mergeID, survivorID, kept.ID, id, row.Status == personobs.StatusActive || row.Status == personobs.StatusUnsupported); err != nil {
				return err
			}
		}
	}
	return s.appendObservationEventTx(ctx, tx, survivorID, id, "merge-converted", "owner", fmt.Sprintf("merge:%d;level:%s", mergeID, row.Level), &row.ID)
}

func (s *Store) mergeOwnerObservationCollisionTx(ctx context.Context, tx *loggedTx, mergeID, personID, keptID, incomingID int64, incomingLive bool) error {
	kept, err := s.loadObservationTx(ctx, tx, keptID)
	if err != nil {
		return err
	}
	incoming, err := s.loadObservationTx(ctx, tx, incomingID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) SELECT ?,category,source FROM person_observation_categories WHERE observation_id=? ON CONFLICT DO NOTHING`, keptID, incomingID); err != nil {
		return err
	}
	state := kept.OwnerState
	if incoming.OwnerState == personobs.OwnerStateConfirmed || state == "" {
		state = incoming.OwnerState
	}
	sensitive := kept.Sensitive || incoming.Sensitive || len(kept.Categories)+len(incoming.Categories) > 0
	status := kept.Status
	statusReason := kept.StatusReason
	if incomingLive {
		status, statusReason = personobs.StatusActive, personobs.StatusReasonNone
	}
	if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET origin='owner',generation_id=NULL,program_fingerprint='',directness='direct-other',support_flag='',sensitive=?,pinned=?,owner_state=?,status=?,status_reason=? WHERE id=?`, sensitive, kept.Pinned || incoming.Pinned, state, status, statusReason, keptID); err != nil {
		return err
	}
	evidenceIDs, err := personMergeRowIDsTx(ctx, tx, `SELECT evidence_id FROM person_observation_evidence WHERE observation_id=? ORDER BY evidence_id`, keptID)
	if err != nil {
		return fmt.Errorf("load merge-collided observation evidence: %w", err)
	}
	for _, evidenceID := range evidenceIDs {
		originalKey, err := personMergeIntegerRowKey([]string{personObservationIDColumn, personObservationEvidenceIDColumn}, keptID, evidenceID)
		if err != nil {
			return err
		}
		if err := s.setPersonMergeRowKeyDispositionTx(
			ctx, tx, mergeID, "person_observation_evidence", originalKey, "deleted_snapshot", nil,
		); err != nil {
			return fmt.Errorf("record merge-collided observation evidence: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_observation_evidence WHERE observation_id=?`, keptID); err != nil {
		return err
	}
	if err := s.recomputeObservationCitationSummaryTx(ctx, tx, keptID); err != nil {
		return err
	}
	if err := s.appendObservationEventTx(ctx, tx, personID, keptID, "merge-moved", "owner", fmt.Sprintf("merge:%d;owner-collision", mergeID), &incomingID); err != nil {
		return err
	}
	return s.notifyObservationSupportChangedTx(ctx, tx, personID, []int64{keptID}, fmt.Sprintf("person-merge:%d", mergeID))
}

func (s *Store) recordMergeAddedObservationCategoriesTx(ctx context.Context, tx *loggedTx, mergeID, survivorID int64, snapshot personMergeSnapshot) error {
	survivorObservations := map[int64]struct{}{}
	originalCategories := map[string]struct{}{}
	for _, row := range snapshot.Rows {
		// OriginSide can be absorbed when this survivor-owned row references the
		// absorbed person. The stored person_id determines which profile owns it.
		if row.TableName == personObservationsTableName && personSplitSnapshotRowInteger(row, personMergePersonIDColumn) == survivorID {
			survivorObservations[row.RowID] = struct{}{}
		}
		if row.TableName == "person_observation_categories" {
			observationID := personSplitSnapshotRowInteger(row, "observation_id")
			if _, ok := survivorObservations[observationID]; ok || row.OriginSide == personMergeOriginSurvivor {
				originalCategories[fmt.Sprintf("%d:%s", observationID, personSplitSnapshotRowText(row, "category"))] = struct{}{}
			}
		}
	}
	for observationID := range survivorObservations {
		rows, err := tx.QueryContext(ctx, `SELECT category FROM person_observation_categories WHERE observation_id=? ORDER BY category`, observationID)
		if err != nil {
			return err
		}
		categories := []string{}
		for rows.Next() {
			var category string
			if err := rows.Scan(&category); err != nil {
				_ = rows.Close()
				return err
			}
			categories = append(categories, category)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, category := range categories {
			if _, ok := originalCategories[fmt.Sprintf("%d:%s", observationID, category)]; ok {
				continue
			}
			postRows, err := s.capturePersonMergeQueryTx(
				ctx, tx, personMergeTableRegistry["person_observation_categories"],
				`SELECT * FROM person_observation_categories WHERE observation_id=? AND category=?`,
				[]any{observationID, category}, survivorID,
			)
			if err != nil {
				return fmt.Errorf("capture merge-added observation category: %w", err)
			}
			if len(postRows) != 1 {
				return fmt.Errorf("%w: merge-added observation category disappeared", ErrPersonMergeInvalid)
			}
			postJSON, err := json.Marshal(postRows[0])
			if err != nil {
				return fmt.Errorf("encode merge-added observation category: %w", err)
			}
			key, err := canonicalPersonMergeSnapshotRowKey([]personMergeSnapshotColumn{{Name: "observation_id", Value: personMergeSnapshotValue{Kind: personMergeSnapshotInteger, Integer: &observationID}}, {Name: "category", Value: personMergeSnapshotValue{Kind: personMergeSnapshotText, Text: &category}}}, []string{"observation_id", "category"})
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_rows(merge_id,table_name,original_row_id,original_row_key,current_row_id,current_row_key,origin_side,provenance_kind,action,snapshot_path,post_merge_row_json) VALUES(?,'person_observation_categories',NULL,?,NULL,?,'survivor','derived','recomputed','merge-added-observation-category',?)`, mergeID, key, key, string(postJSON)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) recordMergeAddedObservationSensitivityTx(
	ctx context.Context,
	tx *loggedTx,
	mergeID, survivorID int64,
	snapshot personMergeSnapshot,
) error {
	spec := personMergeTableRegistry[personObservationsTableName]
	for _, row := range snapshot.Rows {
		if row.TableName != personObservationsTableName ||
			personSplitSnapshotRowInteger(row, personMergePersonIDColumn) != survivorID ||
			observationSnapshotSensitive(row) {
			continue
		}
		postRows, err := s.capturePersonMergeQueryTx(
			ctx, tx, spec,
			`SELECT * FROM person_observations WHERE id=? AND person_id=?`,
			[]any{row.RowID, survivorID}, survivorID,
		)
		if err != nil {
			return fmt.Errorf("capture merge-added observation sensitivity: %w", err)
		}
		if len(postRows) == 0 {
			continue
		}
		if !observationSnapshotSensitive(postRows[0]) {
			continue
		}
		postJSON, err := json.Marshal(postRows[0])
		if err != nil {
			return fmt.Errorf("encode merge-added observation sensitivity: %w", err)
		}
		key, err := canonicalPersonMergeSnapshotRowKey(postRows[0].Columns, spec.keyColumns())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO person_merge_rows
			(merge_id,table_name,original_row_id,original_row_key,current_row_id,current_row_key,
			 origin_side,provenance_kind,action,snapshot_path,post_merge_row_json)
			VALUES(?,? ,NULL,?,?,?,'survivor','derived','recomputed','merge-added-observation-sensitivity',?)`,
			mergeID, personObservationsTableName, key, row.RowID, key, string(postJSON)); err != nil {
			return fmt.Errorf("journal merge-added observation sensitivity: %w", err)
		}
	}
	return nil
}

func observationSnapshotSensitive(row personMergeSnapshotRow) bool {
	for _, column := range row.Columns {
		if column.Name == "sensitive" && column.Value.Boolean != nil {
			return *column.Value.Boolean
		}
	}
	return false
}

// Converted machine rows remain owner assertions after split: their old ledger
// has been discarded and cannot be restored as evidence.
func observationSplitSnapshotRow(row personMergeSnapshotRow) personMergeSnapshotRow {
	if row.TableName != personObservationsTableName || row.ProvenanceKind != personMergeProvenanceAbsorbedProfile || row.OriginSide != personMergeOriginAbsorbed {
		return row
	}
	owner := personSplitSnapshotRowText(row, "origin") == "owner"
	if !owner && personSplitSnapshotRowText(row, "level") != "explicit" {
		return row
	}
	row.Columns = append([]personMergeSnapshotColumn(nil), row.Columns...)
	for i, column := range row.Columns {
		// All absorbed citations belonged to the discarded ledger. Owner rows
		// keep their assertion but cannot recover its old citation projections.
		switch column.Name {
		case "first_seen_at", "last_seen_at":
			row.Columns[i].Value = personMergeSnapshotValue{Kind: personMergeSnapshotNull}
			continue
		case "mention_count":
			zero := int64(0)
			row.Columns[i].Value = personMergeSnapshotValue{Kind: personMergeSnapshotInteger, Integer: &zero}
			continue
		}
		if owner {
			continue
		}
		value := ""
		switch column.Name {
		case "origin":
			value = "owner"
		case "directness":
			value = string(personfacts.DirectOther)
		case "program_fingerprint", "support_flag":
		case "generation_id":
			row.Columns[i].Value = personMergeSnapshotValue{Kind: personMergeSnapshotNull}
			continue
		default:
			continue
		}
		row.Columns[i].Value = personMergeSnapshotValue{Kind: personMergeSnapshotText, Text: &value}
	}
	return row
}

func (s *Store) restoreMergeAddedObservationCategoriesTx(
	ctx context.Context,
	tx *loggedTx,
	journal []personSplitJournalRow,
	snapshot personMergeSnapshot,
) error {
	const snapshotPath = "merge-added-observation-category"
	spec := personMergeTableRegistry["person_observation_categories"]
	snapshotEvents := make(map[int64]struct{})
	for _, row := range snapshot.Rows {
		if row.TableName == "person_observation_events" {
			snapshotEvents[row.RowID] = struct{}{}
		}
	}
	observations := make(map[int64]struct{})
	for _, row := range journal {
		if row.tableName != "person_observation_categories" || row.snapshotPath != snapshotPath {
			continue
		}
		if !row.currentKey.Valid || !row.postMergeJSON.Valid {
			return fmt.Errorf("%w: incomplete merge-added observation category", ErrPersonMergeSnapshotCorrupt)
		}
		where, args, err := personSplitCurrentRowWhere(spec, row)
		if err != nil {
			return err
		}
		currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
			`SELECT * FROM person_observation_categories WHERE `+where, args, 0)
		if err != nil {
			return err
		}
		if len(currentRows) > 1 {
			return fmt.Errorf("%w: merge-added observation category is ambiguous", ErrPersonSplitParticipants)
		}
		if len(currentRows) == 0 {
			continue
		}
		var post personMergeSnapshotRow
		if err := json.Unmarshal([]byte(row.postMergeJSON.String), &post); err != nil {
			return fmt.Errorf("%w: decode merge-added observation category: %w", ErrPersonMergeSnapshotCorrupt, err)
		}
		observationID := personSplitSnapshotRowInteger(post, personObservationIDColumn)
		observations[observationID] = struct{}{}
		if !reflect.DeepEqual(currentRows[0].Columns, post.Columns) {
			continue
		}
		laterReaffirmation, err := s.hasPostMergeObservationReaffirmationTx(ctx, tx, observationID, snapshotEvents)
		if err != nil {
			return fmt.Errorf("check later observation category assertion: %w", err)
		}
		if laterReaffirmation {
			continue
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM person_observation_categories WHERE `+where, args...)
		if err != nil {
			return fmt.Errorf("remove merge-added observation category: %w", err)
		}
		if changed, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("count removed merge-added observation category: %w", err)
		} else if changed != 1 {
			return fmt.Errorf("%w: merge-added observation category changed during split", ErrPersonSplitParticipants)
		}
	}
	for observationID := range observations {
		if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET sensitive=TRUE
			WHERE id=? AND EXISTS (SELECT 1 FROM person_observation_categories WHERE observation_id=?)`,
			observationID, observationID); err != nil {
			return fmt.Errorf("restore sensitivity from retained observation categories: %w", err)
		}
	}
	return nil
}

func (s *Store) restoreMergeAddedObservationSensitivityTx(
	ctx context.Context,
	tx *loggedTx,
	journal []personSplitJournalRow,
	snapshot personMergeSnapshot,
) error {
	const snapshotPath = "merge-added-observation-sensitivity"
	spec := personMergeTableRegistry[personObservationsTableName]
	snapshotEvents := make(map[int64]struct{})
	for _, row := range snapshot.Rows {
		if row.TableName == "person_observation_events" {
			snapshotEvents[row.RowID] = struct{}{}
		}
	}
	for _, row := range journal {
		if row.tableName != personObservationsTableName || row.snapshotPath != snapshotPath {
			continue
		}
		if !row.currentKey.Valid || !row.postMergeJSON.Valid {
			return fmt.Errorf("%w: incomplete merge-added observation sensitivity", ErrPersonMergeSnapshotCorrupt)
		}
		where, args, err := personSplitCurrentRowWhere(spec, row)
		if err != nil {
			return err
		}
		currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
			`SELECT * FROM person_observations WHERE `+where, args, 0)
		if err != nil {
			return err
		}
		if len(currentRows) > 1 {
			return fmt.Errorf("%w: merge-added observation sensitivity is ambiguous", ErrPersonSplitParticipants)
		}
		if len(currentRows) == 0 || !observationSnapshotSensitive(currentRows[0]) {
			continue
		}
		var post personMergeSnapshotRow
		if err := json.Unmarshal([]byte(row.postMergeJSON.String), &post); err != nil {
			return fmt.Errorf("%w: decode merge-added observation sensitivity: %w", ErrPersonMergeSnapshotCorrupt, err)
		}
		observationID := personSplitSnapshotRowInteger(post, "id")
		laterReaffirmation, err := s.hasPostMergeObservationReaffirmationTx(ctx, tx, observationID, snapshotEvents)
		if err != nil {
			return fmt.Errorf("check later sensitive observation reaffirmation: %w", err)
		}
		if laterReaffirmation {
			continue
		}
		var hasCategories bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM person_observation_categories WHERE observation_id=?)`, observationID).Scan(&hasCategories); err != nil {
			return fmt.Errorf("check retained observation sensitivity categories: %w", err)
		}
		if hasCategories {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET sensitive=FALSE WHERE id=? AND sensitive=TRUE`, observationID); err != nil {
			return fmt.Errorf("restore pre-merge observation sensitivity: %w", err)
		}
	}
	return nil
}

func (s *Store) hasPostMergeObservationOwnerAssertionTx(
	ctx context.Context,
	tx *loggedTx,
	observationID int64,
	snapshotEvents map[int64]struct{},
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM person_observation_events
		WHERE observation_id=? AND event='owner-assertion-confirmed' ORDER BY id`, observationID)
	if err != nil {
		return false, fmt.Errorf("read post-merge owner assertion events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var eventID int64
		if err := rows.Scan(&eventID); err != nil {
			return false, fmt.Errorf("scan post-merge owner assertion event: %w", err)
		}
		if _, existedAtMerge := snapshotEvents[eventID]; !existedAtMerge {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("read post-merge owner assertion events: %w", err)
	}
	return false, nil
}

func (s *Store) hasPostMergeObservationReinforcementTx(
	ctx context.Context,
	tx *loggedTx,
	observationID int64,
	snapshotEvents map[int64]struct{},
) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id,actor FROM person_observation_events
		WHERE observation_id=? AND event='reinforced' ORDER BY id`, observationID)
	if err != nil {
		return false, fmt.Errorf("read observation reinforcement events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var eventID int64
		var actor string
		if err := rows.Scan(&eventID, &actor); err != nil {
			return false, err
		}
		if _, existedAtMerge := snapshotEvents[eventID]; !existedAtMerge &&
			(actor == "owner" || actor == "api") {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) hasPostMergeObservationReaffirmationTx(
	ctx context.Context,
	tx *loggedTx,
	observationID int64,
	snapshotEvents map[int64]struct{},
) (bool, error) {
	reinforced, err := s.hasPostMergeObservationReinforcementTx(ctx, tx, observationID, snapshotEvents)
	if err != nil || reinforced {
		return reinforced, err
	}
	return s.hasPostMergeObservationOwnerAssertionTx(ctx, tx, observationID, snapshotEvents)
}

func (s *Store) restoreObservationCopiesTx(ctx context.Context, tx *loggedTx, mergeID, sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64, snapshot personMergeSnapshot) error {
	for _, row := range snapshot.Rows {
		if row.TableName != personObservationsTableName || row.OriginSide != personMergeOriginAbsorbed || personSplitSnapshotRowText(row, "origin") == "owner" || personSplitSnapshotRowText(row, "level") == "explicit" {
			continue
		}
		var id int64
		err := tx.QueryRowContext(ctx, `SELECT o.id FROM person_observations o WHERE o.person_id=? AND EXISTS(SELECT 1 FROM person_observation_events e WHERE e.observation_id=o.id AND e.event='merge-converted' AND e.related_observation_id=? AND e.detail=?)`, sourceID, row.RowID, fmt.Sprintf("merge:%d;level:%s", mergeID, personSplitSnapshotRowText(row, "level"))).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		// The original owner row may have been restored before this converted
		// copy. Preserve both records while allowing only one live key.
		converted, err := s.loadObservationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		var collision bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_observations WHERE person_id=? AND content_key=? AND status IN ('active','unsupported'))`, newPersonID, converted.ContentKey).Scan(&collision); err != nil {
			return err
		}
		status, reason := converted.Status, converted.StatusReason
		if collision && (converted.Status == personobs.StatusActive || converted.Status == personobs.StatusUnsupported) {
			if err := s.transitionObservationTx(ctx, tx, id, personobs.StatusSuperseded, personobs.StatusReasonPersonMergeCollision, "merge-moved", "owner", "person-merge-collision"); err != nil {
				return err
			}
			status, reason = personobs.StatusSuperseded, personobs.StatusReasonPersonMergeCollision
		} else if !collision && converted.Status == personobs.StatusSuperseded && converted.StatusReason == personobs.StatusReasonPersonMergeCollision {
			status = personobs.Status(personSplitSnapshotRowText(row, "status"))
			reason = personobs.StatusReason(personSplitSnapshotRowText(row, "status_reason"))
		}
		originalSubject := personSplitSnapshotRowInteger(row, "subject_person_id")
		if originalSubject != 0 {
			restoredSubject, present, err := s.personSplitResolvePersonReferenceTx(ctx, tx, originalSubject, sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID)
			if err != nil {
				return err
			}
			mergedSubject := restoredSubject
			if originalSubject == absorbedSnapshotID {
				mergedSubject = sourceID
			}
			// Only reverse the merge's rewrite. Preserve an owner edit made later.
			if converted.SubjectPersonID != nil && *converted.SubjectPersonID == mergedSubject {
				var subject any
				if present {
					subject = restoredSubject
				}
				if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET subject_person_id=? WHERE id=?`, subject, id); err != nil {
					return err
				}
			}
		}

		if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET person_id=?,sequence=?,status=?,status_reason=? WHERE id=?`, newPersonID, personSplitSnapshotRowInteger(row, "sequence"), status, reason, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE person_observation_events SET person_id=? WHERE observation_id=?`, newPersonID, id); err != nil {
			return err
		}
	}
	ids, err := personMergeRowIDsTx(ctx, tx, `SELECT id FROM person_observations WHERE person_id=? ORDER BY id`, newPersonID)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE person_observation_events SET person_id=? WHERE observation_id=?`, newPersonID, id); err != nil {
			return err
		}
		if err := s.appendObservationEventTx(ctx, tx, newPersonID, id, "split-restored", "owner", "", nil); err != nil {
			return err
		}
	}
	return nil
}

type personMergeObservationSuppressionDecision struct {
	id               int64
	contentKey       string
	reason           string
	actor            string
	rejected         requiredTimestamp
	cleared          nullableTimestamp
	originSide       personMergeOriginSide
	baselineReason   string
	baselineActor    string
	baselineRejected time.Time
	baselineCleared  nullableTimestamp
	hasBaseline      bool
}

func (s *Store) capturePostMergeObservationSuppressionsTx(
	ctx context.Context,
	tx *loggedTx,
	sourceID int64,
	journal []personSplitJournalRow,
	snapshot personMergeSnapshot,
) ([]personMergeObservationSuppressionDecision, error) {
	absorbedObservationKeys := make(map[string]struct{})
	baselineByID := make(map[int64]personMergeSnapshotRow)
	originSideByID := make(map[int64]personMergeOriginSide)
	for _, row := range snapshot.Rows {
		if row.TableName == personObservationsTableName && row.OriginSide == personMergeOriginAbsorbed {
			absorbedObservationKeys[personSplitSnapshotRowText(row, "content_key")] = struct{}{}
		}
		if row.TableName == "person_observation_suppressions" {
			baselineByID[row.RowID] = row
			originSideByID[row.RowID] = row.OriginSide
		}
	}
	if len(absorbedObservationKeys) == 0 {
		return nil, nil
	}
	for _, row := range journal {
		if row.tableName != "person_observation_suppressions" || !row.postMergeJSON.Valid {
			continue
		}
		var post personMergeSnapshotRow
		if err := json.Unmarshal([]byte(row.postMergeJSON.String), &post); err != nil {
			return nil, fmt.Errorf("%w: decode post-merge observation suppression: %w", ErrPersonMergeSnapshotCorrupt, err)
		}
		baselineByID[post.RowID] = post
		if row.originSide != "" {
			originSideByID[post.RowID] = row.originSide
		}
	}

	type currentSuppression struct {
		id         int64
		contentKey string
		reason     string
		actor      string
		rejected   requiredTimestamp
		cleared    nullableTimestamp
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,content_key,reason,actor,rejected_at,cleared_at
		FROM person_observation_suppressions WHERE person_id=? ORDER BY id`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("read merged observation suppressions: %w", err)
	}
	current := []currentSuppression{}
	for rows.Next() {
		var row currentSuppression
		if err := rows.Scan(&row.id, &row.contentKey, &row.reason, &row.actor, &row.rejected, &row.cleared); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan merged observation suppression: %w", err)
		}
		current = append(current, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("read merged observation suppressions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close merged observation suppressions: %w", err)
	}

	spec := personMergeTableRegistry["person_observation_suppressions"]
	decisions := make(map[string]personMergeObservationSuppressionDecision)
	for _, row := range current {
		if _, belongsToAbsorbed := absorbedObservationKeys[row.contentKey]; !belongsToAbsorbed {
			continue
		}
		currentRows, err := s.capturePersonMergeQueryTx(ctx, tx, spec,
			`SELECT * FROM person_observation_suppressions WHERE id=?`, []any{row.id}, sourceID)
		if err != nil {
			return nil, err
		}
		if len(currentRows) != 1 {
			return nil, fmt.Errorf("%w: merged observation suppression disappeared", ErrPersonSplitParticipants)
		}
		if baseline, ok := baselineByID[row.id]; ok && sameObservationSuppressionState(currentRows[0], baseline) {
			continue
		}
		decision := personMergeObservationSuppressionDecision{
			id: row.id, contentKey: row.contentKey, reason: row.reason,
			actor: row.actor, rejected: row.rejected, cleared: row.cleared,
			originSide: originSideByID[row.id],
		}
		if baseline, ok := baselineByID[row.id]; ok {
			values := personSplitSnapshotColumnsByName(baseline.Columns)
			rejectedAt, err := time.Parse(time.RFC3339Nano, personSplitSnapshotRowText(baseline, "rejected_at"))
			if err != nil {
				return nil, fmt.Errorf("%w: decode survivor observation suppression boundary: %w", ErrPersonMergeSnapshotCorrupt, err)
			}
			clearedAt := nullableTimestamp{}
			clearedValue := values["cleared_at"]
			if clearedValue.Kind != personMergeSnapshotNull {
				if clearedValue.Text == nil {
					return nil, fmt.Errorf("%w: missing survivor suppression clear timestamp", ErrPersonMergeSnapshotCorrupt)
				}
				clearedAt.Time, err = time.Parse(time.RFC3339Nano, *clearedValue.Text)
				if err != nil {
					return nil, fmt.Errorf("%w: decode survivor suppression clear timestamp: %w", ErrPersonMergeSnapshotCorrupt, err)
				}
				clearedAt.Time = clearedAt.Time.UTC()
				clearedAt.Valid = true
			}
			decision.baselineReason = personSplitSnapshotRowText(baseline, "reason")
			decision.baselineActor = personSplitSnapshotRowText(baseline, "actor")
			decision.baselineRejected = rejectedAt.UTC()
			decision.baselineCleared = clearedAt
			decision.hasBaseline = true
		}
		previous, exists := decisions[row.contentKey]
		if !exists || observationSuppressionDecisionAfter(decision, previous) {
			decisions[row.contentKey] = decision
		}
	}
	result := make([]personMergeObservationSuppressionDecision, 0, len(decisions))
	for _, decision := range decisions {
		result = append(result, decision)
	}
	slices.SortFunc(result, func(left, right personMergeObservationSuppressionDecision) int {
		return strings.Compare(left.contentKey, right.contentKey)
	})
	return result, nil
}

func sameObservationSuppressionState(current, baseline personMergeSnapshotRow) bool {
	currentValues := personSplitSnapshotColumnsByName(current.Columns)
	baselineValues := personSplitSnapshotColumnsByName(baseline.Columns)
	for _, name := range []string{"content_key", "reason", "actor", "rejected_at", "cleared_at"} {
		if !reflect.DeepEqual(currentValues[name], baselineValues[name]) {
			return false
		}
	}
	return true
}

func observationSuppressionDecisionAfter(candidate, previous personMergeObservationSuppressionDecision) bool {
	candidateAt := candidate.rejected.Time
	if candidate.cleared.Valid && candidate.cleared.Time.After(candidateAt) {
		candidateAt = candidate.cleared.Time
	}
	previousAt := previous.rejected.Time
	if previous.cleared.Valid && previous.cleared.Time.After(previousAt) {
		previousAt = previous.cleared.Time
	}
	if !candidateAt.Equal(previousAt) {
		return candidateAt.After(previousAt)
	}
	return candidate.id > previous.id
}

func (s *Store) restorePostMergeObservationSuppressionsTx(
	ctx context.Context,
	tx *loggedTx,
	sourceID int64,
	newPersonID int64,
	decisions []personMergeObservationSuppressionDecision,
) error {
	for _, decision := range decisions {
		clearedAt := any(nil)
		if decision.cleared.Valid {
			clearedAt = decision.cleared.Time
		}
		var restoredSuppressionID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM person_observation_suppressions
			WHERE person_id=? AND content_key=? ORDER BY rejected_at DESC,id DESC LIMIT 1`,
			newPersonID, decision.contentKey).Scan(&restoredSuppressionID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("find restored observation suppression: %w", err)
		}
		var sourceSuppressionID int64
		sourceErr := tx.QueryRowContext(ctx, `SELECT id FROM person_observation_suppressions
			WHERE id=? AND person_id=?`, decision.id, sourceID).Scan(&sourceSuppressionID)
		sourceExists := sourceErr == nil
		if sourceErr != nil && !errors.Is(sourceErr, sql.ErrNoRows) {
			return fmt.Errorf("find merged observation suppression: %w", sourceErr)
		}
		if err == nil {
			if _, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions
				SET reason=?,actor=?,rejected_at=?,cleared_at=? WHERE id=?`,
				decision.reason, decision.actor, decision.rejected.Time, clearedAt, restoredSuppressionID); err != nil {
				return fmt.Errorf("restore post-merge observation suppression state: %w", err)
			}
			if sourceExists && decision.originSide == personMergeOriginSurvivor {
				if err := s.restoreSurvivorObservationSuppressionBaselineTx(ctx, tx, sourceID, decision); err != nil {
					return err
				}
			} else if sourceExists && decision.originSide == "" {
				if _, err := tx.ExecContext(ctx, `DELETE FROM person_observation_suppressions WHERE id=? AND person_id=?`,
					decision.id, sourceID); err != nil {
					return fmt.Errorf("move post-merge observation suppression to restored profile: %w", err)
				}
			}
			continue
		}
		if sourceExists && decision.originSide != personMergeOriginSurvivor {
			if _, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions
				SET person_id=?,reason=?,actor=?,rejected_at=?,cleared_at=? WHERE id=? AND person_id=?`,
				newPersonID, decision.reason, decision.actor, decision.rejected.Time, clearedAt,
				decision.id, sourceID); err != nil {
				return fmt.Errorf("move post-merge observation suppression to restored profile: %w", err)
			}
			continue
		}
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO person_observation_suppressions
				(person_id,content_key,reason,actor,rejected_at,cleared_at) VALUES(?,?,?,?,?,?)`,
				newPersonID, decision.contentKey, decision.reason, decision.actor,
				decision.rejected.Time, clearedAt); err != nil {
				return fmt.Errorf("copy post-merge observation suppression: %w", err)
			}
			if sourceExists && decision.originSide == personMergeOriginSurvivor {
				if err := s.restoreSurvivorObservationSuppressionBaselineTx(ctx, tx, sourceID, decision); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *Store) restoreSurvivorObservationSuppressionBaselineTx(
	ctx context.Context,
	tx *loggedTx,
	sourceID int64,
	decision personMergeObservationSuppressionDecision,
) error {
	if !decision.hasBaseline {
		return fmt.Errorf("%w: missing survivor observation suppression baseline", ErrPersonMergeSnapshotCorrupt)
	}
	clearedAt := any(nil)
	if decision.baselineCleared.Valid {
		clearedAt = decision.baselineCleared.Time
	}
	result, err := tx.ExecContext(ctx, `UPDATE person_observation_suppressions
		SET reason=?,actor=?,rejected_at=?,cleared_at=? WHERE id=? AND person_id=?`,
		decision.baselineReason, decision.baselineActor, decision.baselineRejected,
		clearedAt, decision.id, sourceID)
	if err != nil {
		return fmt.Errorf("restore survivor observation suppression baseline: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count restored survivor observation suppression baseline: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("%w: survivor observation suppression changed during split", ErrPersonSplitParticipants)
	}
	return nil
}
