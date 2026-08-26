package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"

	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
)

const maxPersonSweepFactStateItems = 1000

var _ peoplesweep.AssemblySource = (*Store)(nil)

// LoadPersonFactState returns the complete provider-visible projection and
// unresolved-claim state for the exact current catalog. All component reads
// share one snapshot so a packet never combines different ledger revisions.
func (s *Store) LoadPersonFactState(
	ctx context.Context, personID int64, catalog personfacts.Catalog,
) (peoplesweep.PersonFactState, error) {
	if personID <= 0 {
		return peoplesweep.PersonFactState{}, ErrPersonNotFound
	}
	state := peoplesweep.PersonFactState{
		Current: []peoplesweep.ProjectedValue{}, Unresolved: []personfacts.Claim{},
	}
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM persons WHERE id = ?`, personID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPersonNotFound
			}
			return fmt.Errorf("validate person sweep fact-state person: %w", err)
		}
		includeSensitive := slices.ContainsFunc(catalog.Targets,
			func(target personfacts.TargetDescriptor) bool { return target.Sensitive })
		expectedCatalog, err := s.buildPersonFactCatalogContext(ctx, tx, includeSensitive)
		if err != nil {
			return fmt.Errorf("build current person sweep fact-state catalog: %w", err)
		}
		if !reflect.DeepEqual(catalog, expectedCatalog) {
			return errors.New("person sweep fact-state catalog is not the exact current catalog")
		}

		for _, target := range catalog.Targets {
			projector, err := s.personFactProjectorSnapshotTx(ctx, tx, target)
			if err != nil {
				return fmt.Errorf("load person sweep fact-state target %q: %w", target.Key, err)
			}
			current, err := projector.loadCurrent(ctx, personID, target)
			if err != nil {
				return fmt.Errorf("load person sweep current projection for target %q: %w", target.Key, err)
			}
			if len(state.Current)+len(current) > maxPersonSweepFactStateItems {
				return errors.New("person sweep current projection exceeds the state limit")
			}
			for _, value := range current {
				effectiveAt := value.ActiveFrom.UTC()
				state.Current = append(state.Current, peoplesweep.ProjectedValue{
					TargetKey: target.Key, Value: append(json.RawMessage(nil), value.Normalized.JSON...),
					ValueFingerprint: value.Normalized.Fingerprint, EffectiveAt: &effectiveAt,
				})
			}

			remaining := maxPersonSweepFactStateItems - len(state.Unresolved)
			claims, err := s.loadPersonSweepUnresolvedClaimsTx(ctx, tx, personID, target, remaining)
			if err != nil {
				return err
			}
			state.Unresolved = append(state.Unresolved, claims...)
		}
		return nil
	})
	if err != nil {
		return peoplesweep.PersonFactState{}, err
	}
	sort.Slice(state.Current, func(i, j int) bool {
		left, right := state.Current[i], state.Current[j]
		if left.TargetKey != right.TargetKey {
			return left.TargetKey < right.TargetKey
		}
		if left.ValueFingerprint != right.ValueFingerprint {
			return left.ValueFingerprint < right.ValueFingerprint
		}
		return left.EffectiveAt.Before(*right.EffectiveAt)
	})
	sort.Slice(state.Unresolved, func(i, j int) bool {
		return state.Unresolved[i].ClaimKey < state.Unresolved[j].ClaimKey
	})
	return state, nil
}

func (s *Store) loadPersonSweepUnresolvedClaimsTx(
	ctx context.Context,
	tx *loggedTx,
	personID int64,
	target personfacts.TargetDescriptor,
	limit int,
) ([]personfacts.Claim, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+personFactClaimColumns+`
		FROM person_fact_claims c
		WHERE c.person_id = ?
		  AND c.target_kind = ? AND c.target_key = ? AND c.target_revision = ?
		  AND c.rejection_action IS NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM person_fact_decisions d
		      WHERE d.claim_id = c.id AND d.action IN
		          ('applied', 'superseded', 'invalid', 'identity-rejected',
		           'policy-rejected', 'conflict-rejected'))
		ORDER BY c.claim_key, c.id
		LIMIT ?`, personID, target.Kind, target.Key, target.Revision, limit+1)
	if err != nil {
		return nil, fmt.Errorf("load unresolved person sweep claims for target %q: %w", target.Key, err)
	}
	claims := make([]personfacts.Claim, 0)
	for rows.Next() {
		claim, scanErr := scanPersonFactClaim(rows)
		if scanErr != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan unresolved person sweep claim: %w", scanErr)
		}
		claims = append(claims, claim)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate unresolved person sweep claims: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close unresolved person sweep claims: %w", err)
	}
	if len(claims) > limit {
		return nil, errors.New("person sweep unresolved claims exceed the state limit")
	}
	for index := range claims {
		claim := &claims[index]
		generation, err := s.loadPersonFactGenerationByIDTx(ctx, tx, claim.GenerationID)
		if err != nil {
			return nil, err
		}
		if claim.PersonID != personID || generation.PersonID != personID ||
			claim.Target.Kind != target.Kind || claim.Target.Key != target.Key ||
			claim.Target.Revision != target.Revision || claim.Normalized == nil || claim.Failure != nil {
			return nil, errors.New("unresolved person sweep claim failed identity validation")
		}
		claim.Generation = generation
		claim.EvidenceIDs, err = loadPersonFactClaimEvidenceIDsTx(ctx, tx, claim.ID)
		if err != nil {
			return nil, err
		}
		if len(claim.EvidenceIDs) == 0 {
			return nil, errors.New("unresolved person sweep claim has no evidence")
		}
		var scopedEvidence int
		if err := tx.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM person_fact_claim_evidence ce
			JOIN person_fact_evidence e ON e.id = ce.evidence_id
			WHERE ce.claim_id = ? AND e.person_id = ?`, claim.ID, personID).Scan(&scopedEvidence); err != nil {
			return nil, fmt.Errorf("validate unresolved person sweep claim evidence: %w", err)
		}
		if scopedEvidence != len(claim.EvidenceIDs) {
			return nil, errors.New("unresolved person sweep claim evidence failed identity validation")
		}
	}
	return claims, nil
}
