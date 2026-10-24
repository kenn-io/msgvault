package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.kenn.io/msgvault/internal/personobs"
)

const observationTraversalBatchSize = 500

type observationTraversalBatchSizeKey struct{}

// refreshObservationSupportForEvidenceTx runs once after the batch's status
// events have landed, so observation state sees the complete new chronology.
func (s *Store) refreshObservationSupportForEvidenceTx(ctx context.Context, tx *loggedTx, personID int64, evidenceIDs []int64) error {
	if len(evidenceIDs) == 0 {
		return nil
	}
	if err := s.lockPersonMutationTx(ctx, tx, personID); err != nil {
		return err
	}
	affected := map[int64]bool{}
	for start := 0; start < len(evidenceIDs); start += 500 {
		chunk := evidenceIDs[start:min(start+500, len(evidenceIDs))]
		args := []any{personID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT o.id FROM person_observations o WHERE o.person_id=? AND o.level='explicit'
   AND EXISTS(SELECT 1 FROM person_observation_evidence e WHERE e.observation_id=o.id AND e.evidence_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)) ORDER BY o.id`, args...)
		if err != nil {
			return fmt.Errorf("find observations for evidence support: %w", err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			affected[id] = true
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	ids := make([]int64, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		citations, err := s.observationCitations(ctx, tx, id)
		if err != nil {
			return err
		}
		exclusionsChanged := false
		for _, c := range citations {
			exclusion, found, err := s.observationMessageExclusion(ctx, tx, c.Leaf.MessageID)
			if err != nil {
				return err
			}
			if !found {
				continue
			}
			if exclusion != c.Exclusion {
				exclusionsChanged = true
				if _, err := tx.ExecContext(ctx, `UPDATE person_observation_evidence SET exclusion_reason=? WHERE observation_id=? AND evidence_id=?`, exclusion, id, c.EvidenceID); err != nil {
					return err
				}
			}
		}
		if exclusionsChanged {
			if err := s.recomputeObservationCitationSummaryTx(ctx, tx, id); err != nil {
				return fmt.Errorf("refresh observation citation metadata: %w", err)
			}
		}
	}
	return s.notifyObservationSupportChangedTx(ctx, tx, personID, ids, "evidence-status")
}

// observationMessageExclusion uses the stored leaf coordinate, never source_ref.
// Missing messages retain their projection; their status event withdraws support.
func (s *Store) observationMessageExclusion(ctx context.Context, q contextRowsQuerier, messageID int64) (personobs.ExclusionReason, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(list_id,''),draft_authored FROM messages WHERE id=?`, messageID)
	if err != nil {
		return "", false, fmt.Errorf("read observation evidence exclusion: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var listID string
	var draft bool
	if err := rows.Scan(&listID, &draft); err != nil {
		return "", false, err
	}
	if draft {
		return personobs.ExclusionReasonDraft, true, nil
	}
	if strings.TrimSpace(listID) != "" {
		return personobs.ExclusionReasonList, true, nil
	}
	return personobs.ExclusionReasonNone, true, nil
}

// notifyObservationSupportChangedTx walks the reverse graph and evaluates the
// full affected DAG in bounded chunks, draining all chunks before this
// transaction commits.
func (s *Store) notifyObservationSupportChangedTx(ctx context.Context, tx *loggedTx, personID int64, ids []int64, cause string) error {
	batchSize := observationTraversalBatchSize
	if override, ok := ctx.Value(observationTraversalBatchSizeKey{}).(int); ok && override > 0 && override < batchSize {
		batchSize = override
	}
	pending := append([]int64(nil), ids...)
	seen := map[int64]bool{}
	included := map[int64]bool{}
	order := []int64{}
	root := map[int64]bool{}
	for _, id := range ids {
		root[id] = true
	}
	for len(pending) > 0 {
		id := pending[0]
		pending = pending[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		// Forget can pass an already-deleted root. Surviving dependents are passed
		// separately by its caller before the cascading premise edges disappear.
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM person_observations WHERE id=? AND person_id=?`, id, personID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			continue
		}
		included[id] = true
		order = append(order, id)
		rows, err := tx.QueryContext(ctx, `SELECT derived_id FROM person_observation_premises WHERE premise_id=? ORDER BY derived_id`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var child int64
			if err := rows.Scan(&child); err != nil {
				_ = rows.Close()
				return err
			}
			pending = append(pending, child)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	dependencies := map[int64][]int64{}
	for _, id := range order {
		rows, err := tx.QueryContext(ctx, `SELECT premise_id FROM person_observation_premises WHERE derived_id=? ORDER BY ordinal`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var premise int64
			if err := rows.Scan(&premise); err != nil {
				_ = rows.Close()
				return err
			}
			if included[premise] {
				dependencies[id] = append(dependencies[id], premise)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	done := map[int64]bool{}
	visiting := map[int64]bool{}
	topological := make([]int64, 0, len(order))
	var evaluate func(int64) error
	evaluate = func(id int64) error {
		if done[id] {
			return nil
		}
		if visiting[id] {
			return errors.New("observation premise graph contains a cycle")
		}
		visiting[id] = true
		for _, premise := range dependencies[id] {
			if err := evaluate(premise); err != nil {
				return err
			}
		}
		delete(visiting, id)
		done[id] = true
		topological = append(topological, id)
		return nil
	}
	for _, id := range order {
		if err := evaluate(id); err != nil {
			return err
		}
	}
	for start := 0; start < len(topological); start += batchSize {
		chunk := topological[start:min(start+batchSize, len(topological))]
		for _, id := range chunk {
			if err := s.refreshOneObservationTx(ctx, tx, personID, id, !root[id], cause); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) markObservationPremiseChangedTx(ctx context.Context, tx *loggedTx, personID, id int64, cause string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET premise_changed_at=? WHERE id=? AND person_id=?`, time.Now().UTC(), id, personID); err != nil {
		return err
	}
	return s.appendObservationEventTx(ctx, tx, personID, id, "premise-changed", "system", cause, nil)
}

func (s *Store) refreshOneObservationTx(ctx context.Context, tx *loggedTx, personID, id int64, descendant bool, cause string) error {
	o, err := s.loadObservationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if o.PersonID != personID {
		return errors.New("observation descendant belongs to another person")
	}
	if descendant {
		if err := s.markObservationPremiseChangedTx(ctx, tx, personID, id, cause); err != nil {
			return err
		}
	}
	// Owner terminal decisions never reverse automatically.
	terminal := o.Status == personobs.StatusRejected || o.Status == personobs.StatusSuperseded
	valid := true
	reason := personobs.StatusReasonPremisesWithdrawn
	if o.Level == personobs.LevelExplicit {
		if o.Origin == personobs.OriginOwner || terminal {
			return nil
		}
		valid = o.SupportedCount > 0
		reason = personobs.StatusReasonEvidenceUnsupported
	} else {
		type edge struct {
			id   int64
			role personobs.PremiseRole
		}
		edges := []edge{}
		rows, err := tx.QueryContext(ctx, `SELECT premise_id,role FROM person_observation_premises WHERE derived_id=? ORDER BY ordinal`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e edge
			if err := rows.Scan(&e.id, &e.role); err != nil {
				_ = rows.Close()
				return err
			}
			edges = append(edges, e)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		support, total, supported := 0, 0, 0
		categoryChanged := false
		sensitiveFromPremise := false
		for _, edge := range edges {
			premise, err := s.loadObservationTx(ctx, tx, edge.id)
			if err != nil {
				return err
			}
			for _, category := range premise.Categories {
				result, err := tx.ExecContext(ctx, `INSERT INTO person_observation_categories(observation_id,category,source) VALUES(?,?,'premise') ON CONFLICT DO NOTHING`, id, category)
				if err != nil {
					return err
				}
				added, err := result.RowsAffected()
				if err != nil {
					return err
				}
				categoryChanged = categoryChanged || added > 0
			}
			sensitiveFromPremise = sensitiveFromPremise || premise.Sensitive
			if personobs.PremiseSupported(premise) {
				total++
			}
			if edge.role != personobs.PremiseRoleSupport {
				continue
			}
			support++
			if personobs.PremiseSupported(premise) {
				supported++
			}
		}
		sensitivityChanged := sensitiveFromPremise && !o.Sensitive
		if categoryChanged || sensitivityChanged {
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET sensitive=TRUE WHERE id=?`, id); err != nil {
				return err
			}
			if err := s.appendObservationEventTx(ctx, tx, personID, id, "sensitivity-changed", "system", cause, nil); err != nil {
				return err
			}
		}
		if terminal {
			return nil
		}
		switch o.Level {
		case personobs.LevelExplicit:
			return errors.New("explicit observation reached derived support refresh")
		case personobs.LevelDeductive:
			valid = support > 0 && support == supported
		case personobs.LevelInductive:
			valid = supported >= 2
		case personobs.LevelContradiction:
			valid = total >= 2
		}
	}
	if observationProtected(o) && !valid {
		if o.SupportFlag != personobs.SupportFlagLapsed {
			if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET support_flag=? WHERE id=?`, personobs.SupportFlagLapsed, id); err != nil {
				return err
			}
			if err := s.appendObservationEventTx(ctx, tx, personID, id, "support-flagged", "system", cause, nil); err != nil {
				return err
			}
		}
		return nil
	}
	if !valid {
		// Citation eligibility refreshes support counts without changing an
		// explicit extraction's status. Evidence status events own that transition.
		if o.Level == personobs.LevelExplicit && cause == "citation-exclusions-changed" {
			return nil
		}
		if o.Status == personobs.StatusActive {
			return s.transitionObservationTx(ctx, tx, id, personobs.StatusUnsupported, reason, "unsupported", "system", cause)
		}
		return nil
	}
	if o.SupportFlag != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE person_observations SET support_flag='' WHERE id=?`, id); err != nil {
			return err
		}
		if err := s.appendObservationEventTx(ctx, tx, personID, id, "support-cleared", "system", cause, nil); err != nil {
			return err
		}
	}
	if o.Status == personobs.StatusUnsupported && (o.StatusReason == personobs.StatusReasonEvidenceUnsupported || o.StatusReason == personobs.StatusReasonPremisesWithdrawn) {
		return s.transitionObservationTx(ctx, tx, id, personobs.StatusActive, personobs.StatusReasonNone, "restored", "system", cause)
	}
	return nil
}
