package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/personobs"
)

func (s *Store) lockObservationPeopleTx(ctx context.Context, tx *loggedTx, ids ...int64) error {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
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

func (s *Store) movePersonObservationsTx(ctx context.Context, tx *loggedTx, mergeID, survivorID, absorbedID int64) error {
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
			if err := s.refreshObservationMentionsTx(ctx, tx, row.ID); err != nil {
				return err
			}
		}
		if err := s.moveOwnerObservationTx(ctx, tx, survivorID, row.ID); err != nil {
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
func (s *Store) moveOwnerObservationTx(ctx context.Context, tx *loggedTx, survivorID, id int64) error {
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
			state := kept.OwnerState
			if row.OwnerState == personobs.OwnerStateConfirmed || state == "" {
				state = row.OwnerState
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET pinned=?,owner_state=? WHERE id=?`, kept.Pinned || row.Pinned, state, kept.ID); err != nil {
				return err
			}
			if err := s.appendObservationEventTx(ctx, tx, survivorID, kept.ID, "merge-moved", "owner", "decisions", &id); err != nil {
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
			state := kept.OwnerState
			if row.OwnerState == personobs.OwnerStateConfirmed || state == "" {
				state = row.OwnerState
			}
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET pinned=?,owner_state=? WHERE id=?`, kept.Pinned || row.Pinned, state, kept.ID); err != nil {
				return err
			}
			if err := s.appendObservationEventTx(ctx, tx, survivorID, kept.ID, "merge-moved", "owner", "decisions", &id); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) SELECT ?,category,'owner' FROM person_observation_categories WHERE observation_id=?`, id, row.ID); err != nil {
		return err
	}
	return s.appendObservationEventTx(ctx, tx, survivorID, id, "merge-converted", "owner", fmt.Sprintf("merge:%d;level:%s", mergeID, row.Level), &row.ID)
}

// Converted machine rows remain owner assertions after split: their old ledger
// has been discarded and cannot be restored as evidence.
func observationSplitSnapshotRow(row personMergeSnapshotRow) personMergeSnapshotRow {
	if row.TableName != "person_observations" || row.ProvenanceKind != personMergeProvenanceAbsorbedProfile || row.OriginSide != personMergeOriginAbsorbed {
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
func (s *Store) restoreObservationCopiesTx(ctx context.Context, tx *loggedTx, mergeID, sourceID, newPersonID, survivorSnapshotID, absorbedSnapshotID int64, snapshot personMergeSnapshot) error {
	for _, row := range snapshot.Rows {
		if row.TableName != "person_observations" || row.OriginSide != personMergeOriginAbsorbed || personSplitSnapshotRowText(row, "origin") == "owner" || personSplitSnapshotRowText(row, "level") == "explicit" {
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
		copy, err := s.loadObservationTx(ctx, tx, id)
		if err != nil {
			return err
		}
		var collision bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM person_observations WHERE person_id=? AND content_key=? AND status IN ('active','unsupported'))`, newPersonID, copy.ContentKey).Scan(&collision); err != nil {
			return err
		}
		status, reason := copy.Status, copy.StatusReason
		if collision && (copy.Status == personobs.StatusActive || copy.Status == personobs.StatusUnsupported) {
			if err := s.transitionObservationTx(ctx, tx, id, personobs.StatusSuperseded, personobs.StatusReasonPersonMergeCollision, "merge-moved", "owner", "person-merge-collision"); err != nil {
				return err
			}
			status, reason = personobs.StatusSuperseded, personobs.StatusReasonPersonMergeCollision
		} else if !collision && copy.Status == personobs.StatusSuperseded && copy.StatusReason == personobs.StatusReasonPersonMergeCollision {
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
			if copy.SubjectPersonID != nil && *copy.SubjectPersonID == mergedSubject {
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
