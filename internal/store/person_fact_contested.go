package store

import (
	"context"
	"fmt"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// PersonFactContest is the latest unresolved contest for a target.
type PersonFactContest struct {
	Target             personfacts.TargetDescriptor    `json:"target"`
	ResolvedAt         time.Time                       `json:"resolved_at"`
	ResolutionRevision string                          `json:"resolution_revision"`
	Current            []personfacts.CurrentProjection `json:"current"`
	Claims             []PersonFactContestedClaim      `json:"claims"`
}

type PersonFactContestedClaim struct {
	Claim    personfacts.ResolvedClaim `json:"claim"`
	Decision personfacts.Decision      `json:"decision"`
}

// ListPersonFactContestsContext reads the latest resolutions and effective evidence
// in one snapshot without changing the archive.
func (s *Store) ListPersonFactContestsContext(ctx context.Context, personID int64, includeSensitive bool) ([]PersonFactContest, error) {
	contests := make([]PersonFactContest, 0)
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		catalog, err := s.buildPersonFactCatalogContext(ctx, tx, includeSensitive)
		if err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT r.id, r.target_kind, r.target_key, r.target_revision, r.resolved_at
   FROM person_fact_resolutions r WHERE r.person_id = ? AND r.id = (
    SELECT MAX(other.id) FROM person_fact_resolutions other
    WHERE other.person_id = r.person_id AND other.target_kind = r.target_kind AND other.target_key = r.target_key)
   ORDER BY r.target_kind, r.target_key`, personID)
		if err != nil {
			return fmt.Errorf("list latest person fact resolutions: %w", err)
		}
		shells := make([]personfacts.ResolutionResult, 0)
		for rows.Next() {
			var shell personfacts.ResolutionResult
			var at personFactTimestamp
			if err := rows.Scan(&shell.ID, &shell.Target.Kind, &shell.Target.Key, &shell.Target.Revision, &at); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan person fact resolution: %w", err)
			}
			shell.ResolvedAt = at.Time
			shells = append(shells, shell)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		applyThreshold := personfacts.DefaultPolicyV1().ApplyThreshold
		for _, shell := range shells {
			var target *personfacts.TargetDescriptor
			for i := range catalog.Targets {
				item := &catalog.Targets[i]
				if item.Kind == shell.Target.Kind && item.Key == shell.Target.Key {
					target = item
					break
				}
			}
			if target == nil {
				continue
			}
			shell, err = s.hydratePersonFactResolutionTx(ctx, tx, shell)
			if err != nil {
				return err
			}
			selected := make(map[string]bool)
			for _, d := range shell.Decisions {
				if d.Action == personfacts.DecisionAmbiguousRetained ||
					d.Action == personfacts.DecisionConflictRejected ||
					(d.Action == personfacts.DecisionRetained && d.Reason != personfacts.ReasonPinRetained && d.Score.Total >= applyThreshold) {
					selected[d.ClaimKey] = true
					if d.CompetingClaimKey != "" {
						selected[d.CompetingClaimKey] = true
					}
				}
			}
			if len(selected) == 0 {
				continue
			}
			claims, _, err := s.loadPersonFactResolvedClaimsTx(ctx, tx, personID, *target, nil)
			if err != nil {
				return err
			}
			byKey := make(map[string]personfacts.ResolvedClaim, len(claims))
			for _, c := range claims {
				byKey[c.ClaimKey] = c
			}
			contest := PersonFactContest{Target: *target, ResolvedAt: shell.ResolvedAt, ResolutionRevision: shell.Target.Revision, Current: []personfacts.CurrentProjection{}, Claims: []PersonFactContestedClaim{}}
			for _, d := range shell.Decisions {
				if !selected[d.ClaimKey] {
					continue
				}
				claim, ok := byKey[d.ClaimKey]
				if !ok {
					continue
				}
				evidence := make([]personfacts.Evidence, 0)
				for _, item := range claim.Evidence {
					if item.Supported {
						evidence = append(evidence, item)
					}
				}
				if len(evidence) == 0 {
					continue
				}
				claim.Evidence = evidence
				claim.Claim.Evidence = nil
				contest.Claims = append(contest.Claims, PersonFactContestedClaim{Claim: claim, Decision: d})
			}
			if len(contest.Claims) == 0 {
				continue
			}
			projector, err := s.personFactProjectorSnapshotTx(ctx, tx, *target)
			if err != nil {
				return err
			}
			current, err := projector.loadCurrent(ctx, personID, *target)
			if err != nil {
				return err
			}
			pin, err := s.effectivePersonFactPinTx(ctx, tx, personID, *target, current)
			if err != nil {
				return err
			}
			if pin.Pinned {
				continue
			}
			if target.Kind == personfacts.TargetEmployment {
				employments, err := s.listAllEmploymentsContext(ctx, tx, EmploymentFilter{PersonID: personID})
				if err != nil {
					return err
				}
				active := make(map[int64]bool)
				for _, e := range employments {
					active[e.ID] = e.IsCurrent
				}
				for _, c := range current {
					if active[c.Ref.RowID] {
						contest.Current = append(contest.Current, c)
					}
				}
			} else {
				contest.Current = append(contest.Current, current...)
			}
			contests = append(contests, contest)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return contests, nil
}
