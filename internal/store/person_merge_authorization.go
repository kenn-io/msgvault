package store

import (
	"context"
	"errors"
	"fmt"
)

// Resolve owners whose rows or projections the native merge changes. Historical
// references count because merge repoints them too; outbound targets do not.
func (s *Store) personMergeScopeTx(ctx context.Context, tx *loggedTx, survivorID, absorbedID int64) (*IdentityGrantSelection, error) {
	var organizationReference bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
 SELECT 1 FROM organization_attribute_values
 WHERE value_record_type = 'person' AND value_record_id = ?)`, absorbedID).Scan(&organizationReference); err != nil {
		return nil, fmt.Errorf("resolve person merge organization effects: %w", err)
	}
	if organizationReference {
		return nil, ErrPersonMergeScopeUnsupported
	}
	var people []int64
	err := identityReadRows(ctx, tx, `SELECT p.id FROM persons p
 WHERE p.id IN (?, ?) OR EXISTS (
 SELECT 1 FROM person_relationships r
 WHERE (r.source_person_id = p.id AND r.target_person_id IN (?, ?))
 OR (r.target_person_id = p.id AND r.source_person_id IN (?, ?))) OR EXISTS (
 SELECT 1 FROM person_attribute_values v WHERE v.person_id = p.id
 AND v.value_record_type = 'person' AND v.value_record_id = ?) OR EXISTS (
 SELECT 1 FROM person_relationship_reviews review WHERE review.person_id = p.id
 AND (review.matched_person_id = ? OR EXISTS (
 SELECT 1 FROM person_relationships r WHERE r.id = review.accepted_relationship_id
 AND (r.source_person_id = ? OR r.target_person_id = ?))))
 ORDER BY p.id LIMIT 101`, []any{survivorID, absorbedID, survivorID, absorbedID, survivorID, absorbedID, absorbedID, absorbedID, absorbedID, absorbedID}, func(rows *loggedRows) error {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		people = append(people, id)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("resolve person merge affected owners: %w", err)
	}
	return s.personEditSelectionTx(ctx, tx, people)
}

func (s *Store) authorizePersonMergeReplayTx(ctx context.Context, tx *loggedTx, receipt *PersonMergeResult, authorize PersonEditAuthorizer) error {
	if authorize == nil {
		return nil
	}
	merge, err := s.getPersonMergeTx(ctx, tx, receipt.Merge.ID)
	if err != nil {
		return err
	}
	if merge.CurrentPersonID == nil || *merge.CurrentPersonID != merge.SurvivorPersonID || merge.Actor != receipt.Merge.Actor {
		return ErrPersonMergeLineageConflict
	}
	var split bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM person_splits WHERE merge_id = ?)`, merge.ID).Scan(&split); err != nil {
		return fmt.Errorf("check person merge replay splits: %w", err)
	}
	if split {
		return ErrPersonMergeLineageConflict
	}
	survivor, err := s.getPersonTx(ctx, tx, merge.SurvivorPersonID)
	if errors.Is(err, ErrPersonNotFound) {
		return ErrPersonMergeLineageConflict
	}
	if err != nil {
		return err
	}
	if survivor.VCardUID != merge.SurvivorVCardUID {
		return ErrPersonMergeLineageConflict
	}
	_, err = s.getPersonTx(ctx, tx, merge.AbsorbedPersonID)
	if err == nil {
		return ErrPersonMergeLineageConflict
	}
	if !errors.Is(err, ErrPersonNotFound) {
		return err
	}
	scope, err := s.personEditSelectionTx(ctx, tx, []int64{merge.SurvivorPersonID})
	if err != nil {
		return err
	}
	scope.Persons = append(scope.Persons, IdentityPersonEvidence{
		ID: merge.AbsorbedPersonID, UID: merge.AbsorbedVCardUID, Revision: merge.AbsorbedRevisionBefore,
	})
	if len(scope.Persons)+len(scope.AddressBooks) > 100 {
		return ErrIdentityOperationTooLarge
	}
	return authorize(ctx, scope)
}
