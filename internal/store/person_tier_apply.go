package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/persontiers"
)

// ApplyPersonTierDecisionContext re-evaluates authority under the shared
// mutation gate. Signals belong to the frozen run; manual grants never do.
func (s *Store) ApplyPersonTierDecisionContext(ctx context.Context, run PersonTierRun, input PersonTierInput) error {
	_, err := s.applyPersonTierDecisionContext(ctx, run, input)
	return err
}

func (s *Store) applyPersonTierDecisionContext(ctx context.Context, run PersonTierRun, input PersonTierInput) (PersonTierInput, error) {
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		if err := s.checkPersonTierRunTx(ctx, tx, run); err != nil {
			return err
		}
		if err := s.lockProfileIdentityKeyTxContext(ctx, tx, "person-fact-generation", input.PersonID); err != nil {
			return err
		}
		if _, err := lockPersonEnrichmentPersonTx(ctx, tx, s.dialect, input.PersonID); errors.Is(err, sql.ErrNoRows) {
			return ErrPersonNotFound
		} else if err != nil {
			return err
		}
		var started nullableTimestamp
		var priorRunID string
		err := tx.QueryRowContext(ctx, `SELECT run_started_at,run_id FROM person_tier_state WHERE person_id=?`, input.PersonID).Scan(&started, &priorRunID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if started.Valid && started.Time.After(run.StartedAt) {
			return ErrPersonTierRunSuperseded
		}
		if run.Mode == "apply" && priorRunID == run.RunID {
			return nil
		}
		loc, _ := time.LoadLocation(run.Timezone)
		asOf := run.AsOf.In(loc)
		if err = s.loadPersonTierInputTx(ctx, tx, &input, loc); err != nil {
			return err
		}
		d := persontiers.Evaluate(input.Signals, run.Config, input.Prior, input.Manual, asOf, input.Readiness.Ready)
		input.Decision = d
		actor := run.Actor
		if actor == "" {
			actor = "people-tiers"
		}
		now := s.personEnrichmentTime().UTC()
		if run.Mode == "record_only" {
			if err = s.recordPersonTierObservationTx(ctx, tx, run, input, actor, now); err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, personTierFailureKey(input.PersonID))
			return err
		}
		wasEligible, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, input.PersonID)
		if err != nil {
			return err
		}
		since, err := time.ParseInLocation(time.DateOnly, d.TierSince, loc)
		if err != nil {
			return err
		}
		lanes, err := json.Marshal(d.AppliedLanes)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO person_tier_state
   (person_id,tier,tier_since,candidate_tier,candidate_since,lanes_json,enrichment_eligible,rule,rule_version,config_fingerprint,as_of_local_date,run_id,run_started_at,applied_at)
   VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(person_id) DO UPDATE SET
   tier=excluded.tier,tier_since=excluded.tier_since,candidate_tier=excluded.candidate_tier,candidate_since=excluded.candidate_since,
   lanes_json=excluded.lanes_json,enrichment_eligible=excluded.enrichment_eligible,rule=excluded.rule,rule_version=excluded.rule_version,
   config_fingerprint=excluded.config_fingerprint,as_of_local_date=excluded.as_of_local_date,run_id=excluded.run_id,run_started_at=excluded.run_started_at,applied_at=excluded.applied_at`,
			input.PersonID, d.Tier, since, nullableTier(d.CandidateTier), nullableTierDate(d.CandidateSince), string(lanes), slices.Contains(d.AppliedLanes, "enrichment"), d.Rule, persontiers.RuleVersion, run.ConfigFingerprint, asOf.Format(time.DateOnly), run.RunID, run.StartedAt, now)
		if err != nil {
			return fmt.Errorf("persist people tier authority: %w", err)
		}
		opts := personTierGrantOptions{Provenance: PersonTierProvenanceAuto, Actor: "people-tiers", At: now}
		// Remove briefs before tracking; create tracking before briefs.
		if d.Lanes["briefs"] == "removed" {
			if _, err = s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, input.PersonID, false, false, opts); err != nil {
				return err
			}
		}
		if action := d.Lanes["tracking"]; action == "created" || action == "removed" {
			if _, err = s.setPersonTrackingWithOptionsTx(ctx, tx, input.PersonID, action == "created", opts); err != nil {
				return err
			}
		}
		if d.Lanes["briefs"] == "created" {
			if _, err = s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, input.PersonID, true, false, opts); err != nil {
				return err
			}
		}
		eligible, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, input.PersonID)
		if err != nil {
			return err
		}
		if !eligible {
			if err = s.cancelScheduledPersonEnrichmentTx(ctx, tx, input.PersonID); err != nil {
				return err
			}
		}
		if !wasEligible && eligible {
			if err = s.publishPersonEnrichmentTx(ctx, tx, input.PersonID, personenrichment.TriggerTracked, "tier:"+run.RunID, now); err != nil {
				return err
			}
		}
		reason := personTierEventReason(d)
		if wasEligible != eligible && d.Reason == "" && d.DeferredReason == "" {
			reason = "config_change"
		}
		if reason != "" {
			if err = s.insertPersonTierEventTx(ctx, tx, run, input, d, reason, "apply", actor, now); err != nil {
				return err
			}
		}
		if err = s.recordPersonTierObservationTx(ctx, tx, run, input, actor, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key=?`, personTierFailureKey(input.PersonID))
		return err
	})
	return input, err
}
func personTierEventReason(d persontiers.Decision) string {
	if d.DeferredReason != "" {
		return "deferred"
	}
	if d.Reason != "" {
		if d.Reason == "config_change" {
			return d.Reason
		}
		if d.Rule == "minimum_tier" {
			return "minimum_tier"
		}
		return d.Reason
	}
	for _, a := range d.Lanes {
		if a == "created" || a == "removed" {
			return "config_change"
		}
	}
	for _, a := range d.Lanes {
		if a == "blocked_manual" || a == "blocked_opt_out" {
			return "manual_blocked"
		}
	}
	return ""
}
func (s *Store) recordPersonTierObservationTx(ctx context.Context, tx *loggedTx, run PersonTierRun, input PersonTierInput, actor string, now time.Time) error {
	loc, _ := time.LoadLocation(run.Timezone)
	asOf := run.AsOf.In(loc)
	var prior persontiers.Prior
	var candidate sql.NullInt64
	var date sql.NullString
	var previousDate string
	err := tx.QueryRowContext(ctx, `SELECT shadow_tier,shadow_since,shadow_candidate_tier,shadow_candidate_since,rule_version,config_fingerprint,as_of_local_date FROM person_tier_observations WHERE person_id=?`, input.PersonID).Scan(&prior.Tier, &prior.TierSince, &candidate, &date, &prior.RuleVersion, &prior.ConfigFingerprint, &previousDate)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	prior.CandidateTier = int(candidate.Int64)
	prior.CandidateSince = date.String
	// Shadows carry their own hysteresis and never borrow applied authority.
	d := persontiers.Evaluate(input.Signals, run.Config, prior, input.Manual, asOf, input.Readiness.Ready)
	signals, err := json.Marshal(struct {
		persontiers.Signals
		Readiness PersonTierReadiness `json:"readiness"`
	}{input.Signals, input.Readiness}, json.Deterministic(true))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO person_tier_observations
  (person_id,computed_tier,rule,signals_json,shadow_tier,shadow_since,shadow_candidate_tier,shadow_candidate_since,rule_version,config_fingerprint,as_of_local_date,activity_ready,evaluated_at)
  VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(person_id) DO UPDATE SET
  computed_tier=excluded.computed_tier,rule=excluded.rule,signals_json=excluded.signals_json,shadow_tier=excluded.shadow_tier,shadow_since=excluded.shadow_since,
  shadow_candidate_tier=excluded.shadow_candidate_tier,shadow_candidate_since=excluded.shadow_candidate_since,rule_version=excluded.rule_version,
  config_fingerprint=excluded.config_fingerprint,as_of_local_date=excluded.as_of_local_date,activity_ready=excluded.activity_ready,evaluated_at=excluded.evaluated_at`,
		input.PersonID, d.ComputedTier, d.Rule, string(signals), d.Tier, d.TierSince, nullableTier(d.CandidateTier), nullableTierDate(d.CandidateSince), persontiers.RuleVersion, run.ConfigFingerprint, asOf.Format(time.DateOnly), input.Readiness.Ready, now)
	if err != nil {
		return fmt.Errorf("record people tier observation: %w", err)
	}
	priorTier := prior.Tier
	if priorTier == 0 {
		priorTier = 3
	}
	due := d.DemotionDue != "" && previousDate < d.DemotionDue && asOf.Format(time.DateOnly) >= d.DemotionDue
	if run.Mode == "record_only" && (priorTier != d.Tier || due) {
		reason := personTierEventReason(d)
		if reason == "" {
			reason = "config_change"
		}
		input.Prior = prior
		return s.insertPersonTierEventTx(ctx, tx, run, input, d, reason, "preview", actor, now)
	}
	return nil
}

type PersonTierResetResult struct {
	Cleared  int                       `json:"cleared"`
	Deferred []PersonTierResetDeferral `json:"deferred"`
}
type PersonTierResetDeferral struct {
	PersonID int64  `json:"person_id"`
	RunID    string `json:"run_id"`
}

func (s *Store) ResetPersonTiersContext(ctx context.Context, actor string) (*PersonTierResetResult, error) {
	if err := validatePersonTierActor(actor); err != nil {
		return nil, err
	}
	out := &PersonTierResetResult{Deferred: []PersonTierResetDeferral{}}
	err := s.withTxContext(ctx, func(tx *loggedTx) error {
		if err := s.lockPersonEnrichmentAuthorityMutationTx(ctx, tx); err != nil {
			return err
		}
		now, err := s.advancePersonTierFenceTx(ctx, tx, s.personEnrichmentTime())
		if err != nil {
			return err
		}
		// Retain a scheduled run until its pending follow-up observes the fence.
		// Removing it here would let that follow-up start a fresh automatic run.
		if _, err = tx.ExecContext(ctx, `DELETE FROM archive_metadata WHERE key='people_tiers_manual_run'`); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM persons WHERE EXISTS(SELECT 1 FROM person_tier_state WHERE person_id=persons.id) OR EXISTS(SELECT 1 FROM person_tracking WHERE person_id=persons.id AND provenance='auto') OR EXISTS(SELECT 1 FROM person_brief_enrollments WHERE person_id=persons.id AND provenance='auto') ORDER BY id`)
		if err != nil {
			return err
		}
		ids := []int64{}
		for rows.Next() {
			var id int64
			if err = rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			var input PersonTierInput
			input.PersonID = id
			if err = s.loadPersonTierInputTx(ctx, tx, &input, time.UTC); err != nil {
				return err
			}
			if input.Manual.TrackingProvenance == PersonTierProvenanceAuto && input.Manual.OpenManualRunID != "" {
				out.Deferred = append(out.Deferred, PersonTierResetDeferral{id, input.Manual.OpenManualRunID})
				continue
			}
			opts := personTierGrantOptions{Provenance: PersonTierProvenanceAuto, Actor: actor, At: now}
			if input.Manual.BriefsProvenance == PersonTierProvenanceAuto {
				if _, err = s.setPersonBriefEnrollmentWithOptionsTx(ctx, tx, id, false, false, opts); err != nil {
					return err
				}
			}
			if input.Manual.TrackingProvenance == PersonTierProvenanceAuto {
				if _, err = s.setPersonTrackingWithOptionsTx(ctx, tx, id, false, opts); err != nil {
					return err
				}
			}
			if _, err = tx.ExecContext(ctx, `DELETE FROM person_tier_state WHERE person_id=?`, id); err != nil {
				return err
			}
			eligible, err := s.personEnrichmentScheduledEligibleTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if !eligible {
				if err = s.cancelScheduledPersonEnrichmentTx(ctx, tx, id); err != nil {
					return err
				}
			}
			d := persontiers.Decision{Prior: persontiers.Prior{Tier: 3}, Rule: "none", Lanes: map[string]string{"tracking": "unchanged", "briefs": "unchanged", "enrichment": "unchanged"}}
			for _, lane := range []string{"tracking", "briefs"} {
				provenance := input.Manual.TrackingProvenance
				if lane == "briefs" {
					provenance = input.Manual.BriefsProvenance
				}
				if provenance == "auto" {
					d.Lanes[lane] = "removed"
				}
			}
			run := PersonTierRun{RunID: "reset:" + uuid.NewString()}
			if err = s.insertPersonTierEventTx(ctx, tx, run, input, d, "reset", "apply", actor, now); err != nil {
				return err
			}
			out.Cleared++
		}
		return nil
	})
	return out, err
}

// PrunePersonTierPreviewsContext only removes record-only audit history.
func (s *Store) PrunePersonTierPreviewsContext(ctx context.Context, retentionDays int) error {
	if retentionDays < 1 {
		return ErrInvalidPersonTierRequest
	}
	_, err := s.db.ExecContext(ctx, s.Rebind(`DELETE FROM person_tier_events WHERE mode='preview' AND created_at<?`), s.personEnrichmentTime().AddDate(0, 0, -retentionDays))
	return err
}
