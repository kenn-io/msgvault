package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"sort"
	"time"

	"go.kenn.io/msgvault/internal/personfacts"
)

// PersonFactStaleness describes a current value and when its supporting
// evidence should next be verified. Reading this report never changes the value.
type PersonFactStaleness struct {
	Target        personfacts.TargetRef     `json:"target"`
	Slug          string                    `json:"slug"`
	Projection    personfacts.ProjectionRef `json:"projection"`
	Value         jsontext.Value            `json:"value"`
	Stability     personfacts.Stability     `json:"stability"`
	Declared      bool                      `json:"declared"`
	NewestSupport time.Time                 `json:"newest_support"`
	Stale         bool                      `json:"stale"`
	VerifyBy      *time.Time                `json:"verify_by,omitzero"`
}

// PersonFactStalenessContext reports current values using one read snapshot.
// Declared values age from their start date; machine values age from their
// newest still-supported evidence, falling back to the value's start date.
func (s *Store) PersonFactStalenessContext(
	ctx context.Context, personID int64, asOf time.Time, includeSensitive bool,
) ([]PersonFactStaleness, error) {
	if personID <= 0 {
		return nil, ErrPersonNotFound
	}
	asOf = asOf.UTC()
	items := make([]PersonFactStaleness, 0)
	policy := personfacts.DefaultPolicyV2()
	err := s.withReadSnapshotContext(ctx, func(tx *loggedTx) error {
		var found int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM persons WHERE id = ?`, personID).Scan(&found); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPersonNotFound
			}
			return fmt.Errorf("validate person fact staleness person: %w", err)
		}
		catalog, err := s.buildPersonFactCatalogContext(ctx, tx, includeSensitive)
		if err != nil {
			return fmt.Errorf("build person fact staleness catalog: %w", err)
		}
		for _, target := range catalog.Targets {
			_, eligibility, err := s.loadPersonFactTargetDescriptorSnapshotTx(ctx, tx, target.Kind, target.Key, &target, includeSensitive)
			if err != nil {
				return fmt.Errorf("load person fact staleness target: %w", err)
			}
			projector, err := s.personFactProjectorSnapshotTx(ctx, tx, target)
			if err != nil {
				return fmt.Errorf("load person fact staleness projector: %w", err)
			}
			current, err := projector.loadCurrent(ctx, personID, target)
			if err != nil {
				return fmt.Errorf("load person fact staleness current values: %w", err)
			}
			for _, value := range current {
				newest := value.ActiveFrom.UTC()
				if !value.Declared {
					support, err := s.newestPersonFactSupportTx(ctx, tx, personID, target, value.Normalized.Fingerprint)
					if err != nil {
						return err
					}
					if !support.IsZero() {
						newest = support
					}
				}
				item := PersonFactStaleness{
					Target: personfacts.TargetRef{Kind: target.Kind, Key: target.Key, Revision: target.Revision}, Slug: target.Slug,
					Projection: value.Ref, Value: append(jsontext.Value(nil), value.Normalized.JSON...), Stability: eligibility.Stability,
					Declared: value.Declared, NewestSupport: newest,
				}
				if horizon, ok := policy.StaleAfter(eligibility.Stability); ok {
					verifyBy := newest.Add(horizon)
					item.VerifyBy = &verifyBy
					item.Stale = asOf.Sub(newest) > horizon
				}
				items = append(items, item)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Target.Kind != b.Target.Kind {
			return a.Target.Kind < b.Target.Kind
		}
		if a.Target.Key != b.Target.Key {
			return a.Target.Key < b.Target.Key
		}
		return a.Projection.RowID < b.Projection.RowID
	})
	return items, nil
}

func (s *Store) newestPersonFactSupportTx(
	ctx context.Context, tx *loggedTx, personID int64, target personfacts.TargetDescriptor, fingerprint string,
) (time.Time, error) {
	rows, err := tx.QueryContext(ctx, `
  SELECT e.id,e.event_time FROM person_fact_evidence e
  WHERE e.person_id = ? AND EXISTS (
   SELECT 1 FROM person_fact_claim_evidence ce
   JOIN person_fact_claims c ON c.id = ce.claim_id
   WHERE ce.evidence_id = e.id AND c.person_id = e.person_id
     AND c.target_kind = ? AND c.target_key = ? AND c.relation = 'support'
     AND c.value_fingerprint = ? AND c.rejection_action IS NULL
  ) ORDER BY e.id`, personID, target.Kind, target.Key, fingerprint)
	if err != nil {
		return time.Time{}, fmt.Errorf("load person fact staleness evidence: %w", err)
	}
	ids := make([]int64, 0)
	times := make(map[int64]time.Time)
	for rows.Next() {
		var id int64
		var eventTime personFactTimestamp
		if err := rows.Scan(&id, &eventTime); err != nil {
			_ = rows.Close()
			return time.Time{}, fmt.Errorf("scan person fact staleness evidence: %w", err)
		}
		ids = append(ids, id)
		times[id] = eventTime.Time.UTC()
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return time.Time{}, fmt.Errorf("iterate person fact staleness evidence: %w", err)
	}
	if err := rows.Close(); err != nil {
		return time.Time{}, fmt.Errorf("close person fact staleness evidence: %w", err)
	}
	supported, err := s.latestEvidenceSupportedTx(ctx, tx, ids)
	if err != nil {
		return time.Time{}, err
	}
	var newest time.Time
	for _, id := range ids {
		if supported[id] && times[id].After(newest) {
			newest = times[id]
		}
	}
	return newest, nil
}
