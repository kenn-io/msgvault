package store

import (
	"context"
	"fmt"

	"go.kenn.io/msgvault/internal/personfacts"
)

// Capture before moving or hiding rows, while their old provenance is still
// available. The person row is also the publication approval fence. Callers
// retain their existing projection UPDATE in this same transaction.
func (s *Store) captureInferenceExportPeopleTx(
	ctx context.Context, tx *loggedTx, ids ...int64,
) (map[int64]personInferenceExportProjection, error) {
	ids = sortedUniqueInt64s(ids...)
	if err := s.lockEmploymentPeopleTx(ctx, tx, ids...); err != nil {
		return nil, err
	}
	before := make(map[int64]personInferenceExportProjection, len(ids))
	for _, id := range ids {
		projection, err := s.loadPersonInferenceExportProjectionTx(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		before[id] = projection
	}
	return before, nil
}

// Exposure changes have no new source argument. Compare inferred contributors
// only: an unrelated declared value must not manufacture inference debt.
func (s *Store) invalidateInferenceExportChangesTx(
	ctx context.Context, tx *loggedTx, before map[int64]personInferenceExportProjection,
) error {
	ids := make([]int64, 0, len(before))
	for id := range before {
		ids = append(ids, id)
	}
	for _, id := range sortedUniqueInt64s(ids...) {
		after, err := s.loadPersonInferenceExportProjectionTx(ctx, tx, id)
		if err != nil {
			return err
		}
		changed, err := inferenceExportProjectionChanged(before[id], after)
		if err != nil {
			return err
		}
		if changed {
			if err := s.advancePersonInferenceExportRevisionTx(ctx, tx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func inferredExportContributors(projection personInferenceExportProjection) personInferenceExportProjection {
	result := personInferenceExportProjection{Attributes: make([]personInferenceExportAttribute, 0), Employments: make([]personInferenceExportEmployment, 0)}
	for _, a := range projection.Attributes {
		if a.Inferred {
			result.Attributes = append(result.Attributes, a)
		}
	}
	for _, e := range projection.Employments {
		if e.Inferred {
			result.Employments = append(result.Employments, e)
		}
	}
	return result
}

func (s *Store) captureDefinitionInferenceExportTx(
	ctx context.Context, tx *loggedTx, id int64,
) (map[int64]personInferenceExportProjection, error) {
	// Exposure also bumps every person's native projection. Coordinate before
	// taking any person lock, including people using an unrelated definition.
	if err := s.lockAttributeDefinitionCatalogTx(ctx, tx, true); err != nil {
		return nil, err
	}
	if err := s.lockRowsTx(ctx, tx, `SELECT id FROM persons ORDER BY id`,
		"definition exposure people", 1, nil); err != nil {
		return nil, err
	}
	return s.captureMatchingInferenceExportPeopleTx(ctx, tx, `
  SELECT id FROM persons WHERE EXISTS (
   SELECT 1 FROM person_attribute_values v
   WHERE v.person_id = persons.id AND v.definition_id = ?
  ) ORDER BY id`, id)
}

func (s *Store) captureOrganizationInferenceExportTx(
	ctx context.Context, tx *loggedTx, ids ...int64,
) (map[int64]personInferenceExportProjection, error) {
	placeholders, args := sortedIDPlaceholders(ids)
	return s.captureMatchingInferenceExportPeopleTx(ctx, tx, `
  SELECT id FROM persons WHERE EXISTS (
   SELECT 1 FROM employments e
   WHERE e.person_id = persons.id AND e.organization_id IN (`+placeholders+`)
  ) ORDER BY id`, args...)
}

func (s *Store) captureMatchingInferenceExportPeopleTx(
	ctx context.Context, tx *loggedTx, query string, args ...any,
) (map[int64]personInferenceExportProjection, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list inference export contributors: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, err
	}
	return s.captureInferenceExportPeopleTx(ctx, tx, ids...)
}

// A supported system winner can replace an inferred slot during resolution.
// That is a deterministic replacement, not inference-driven retirement.
// Explicit supported system retirements carry the exact applied projection
// reference, since there is no surviving contributor to identify them. Slot
// identity is used only to attribute these replacements; it is never part of
// the semantic comparison or the native vCard fingerprint.
func (s *Store) invalidateResolverInferenceExportTx(
	ctx context.Context, tx *loggedTx, personID int64, before personInferenceExportProjection,
	systemRetirements map[personfacts.ProjectionRef]struct{},
) error {
	after, err := s.loadPersonInferenceExportProjectionTx(ctx, tx, personID)
	if err != nil {
		return err
	}
	declaredAttributes := make(map[string]bool)
	for _, attribute := range after.Attributes {
		if !attribute.Inferred {
			declaredAttributes[attribute.Slot] = true
		}
	}
	declaredEmployments := make(map[int64]bool)
	for _, employment := range after.Employments {
		if !employment.Inferred {
			declaredEmployments[employment.RowID] = true
		}
	}
	exportable := personInferenceExportProjection{}
	for _, attribute := range before.Attributes {
		_, retired := systemRetirements[personfacts.ProjectionRef{Kind: "person_attribute", RowID: attribute.RowID}]
		if !declaredAttributes[attribute.Slot] && !retired {
			exportable.Attributes = append(exportable.Attributes, attribute)
		}
	}
	for _, employment := range before.Employments {
		_, retired := systemRetirements[personfacts.ProjectionRef{Kind: personFactProjectionKindEmployment, RowID: employment.RowID}]
		if !declaredEmployments[employment.RowID] && !retired {
			exportable.Employments = append(exportable.Employments, employment)
		}
	}
	changed, err := inferenceExportProjectionChanged(exportable, after)
	if err != nil {
		return err
	}
	if changed {
		return s.advancePersonInferenceExportRevisionTx(ctx, tx, personID)
	}
	return nil
}
